package task

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/source"
)

// Gap kinds.
const (
	GapLiteral    = "literal"    // a text the hidden tests compare against and the solution produces, stated nowhere the agent can see
	GapIdentifier = "identifier" // a Go name the hidden tests use that the solution adds and the base lacks
)

// Gap is one thing the hidden tests require that neither the instruction nor the base code states.
type Gap struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	File string `json:"file"` // the hidden test file that needs it
}

func (g Gap) String() string {
	if g.Kind == GapIdentifier {
		return fmt.Sprintf("identifier %s (%s)", g.Text, g.File)
	}
	return fmt.Sprintf("text %q (%s)", g.Text, g.File)
}

// minLiteral is the length under which a piece of text is too generic to count ("ok", "--flag").
const minLiteral = 8

var (
	// quoted matches the string literals of Python, Ruby, JavaScript and TypeScript on one line.
	quoted = regexp.MustCompile("\"(?:[^\"\\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\\n]|\\\\.)*'|`[^`]*`")
	// sourceExt lists the test-file extensions scanned; other hidden files (data, snapshots) are not.
	sourceExt = map[string]bool{".go": true, ".py": true, ".rb": true, ".js": true, ".jsx": true, ".ts": true,
		".tsx": true, ".mjs": true, ".cjs": true, ".mts": true, ".cts": true}
	spaces = regexp.MustCompile(`\s+`)
	verbs  = regexp.MustCompile(`%[-+# 0-9.*]*[a-zA-Z%]`)
	// messageCalls are Go calls whose first argument is a format or a name, not an expectation (t.Run, t.Errorf, fmt.Sprintf...).
	messageCalls = map[string]bool{"Run": true, "Errorf": true, "Fatalf": true, "Logf": true, "Skipf": true, "Sprintf": true}
)

// Fairness finds what hidden tests require that the agent cannot learn from the instruction or the base code. One
// value serves one command: it caches sources and searches per commit, so repeated checks against a base are cheap.
// All git access is local and goes through internal/gitx.
type Fairness struct {
	where []string // git location, e.g. "--git-dir", bare
	srcs  map[string]source.Source
	found map[string]bool
}

// NewFairness returns a checker for the repository located by where (for example "--git-dir", bare).
func NewFairness(where ...string) *Fairness {
	return &Fairness{where: where, srcs: map[string]source.Source{}, found: map[string]bool{}}
}

// FairnessInput describes a task: commits in the repository, the instruction, and the solution's split.
type FairnessInput struct {
	Base, Solution         string
	Instruction            string
	HiddenTests, Reference []string
}

// Gaps lists the gaps of a task, sorted by file, kind and text. It is a heuristic that reads only what the solution
// added or changed in the hidden test files:
//
//   - String literals, in any supported language, that a hidden test file adds or changes and that the reference
//     (the solution's non-test changes) also contains, but that appear nowhere in the base and not in the instruction:
//     the exact messages and notes the implementation must produce. Literals without a letter, and the format or name
//     argument of t.Run, t.Errorf, t.Fatalf, t.Logf, t.Skipf, fmt.Sprintf and fmt.Errorf, do not count. A text is cut
//     at newlines and %-verbs; each piece of 8 or more characters must be found, and a text with none is too generic.
//     Matching ignores case, collapsed whitespace and surrounding punctuation; base and reference are searched with
//     git grep, so whitespace inside a piece must match exactly there. Limitations: the format argument of fmt.Sprintf
//     and fmt.Errorf is skipped, so an expected text built with them is not checked; a reference message built with a
//     verb inside quotes (fmt.Errorf("value %q is empty")) is not matched by the formatted text a test compares to.
//   - For Go: names the tests newly use as selectors (x.Name), composite-literal keys (T{Name: v}) or called functions
//     that a changed non-test Go file declares at package level or as a struct or interface member, that the base's Go
//     files in that directory lack, and that the instruction does not mention. Only names the reference declares can be
//     flagged, so standard-library names never are; a new name equal to an old one in its directory is missed.
func (f *Fairness) Gaps(ctx context.Context, in FairnessInput) ([]Gap, error) {
	var gaps []Gap
	stated := normalize(in.Instruction)
	newNames, err := f.newNames(ctx, in) // name -> directories of the reference files declaring it
	if err != nil {
		return nil, err
	}
	for _, file := range in.HiddenTests {
		if !sourceExt[path.Ext(file)] {
			continue
		}
		after, before, err := f.versions(ctx, in, file)
		if err != nil {
			return nil, err
		}
		isGo := path.Ext(file) == ".go"
		old := map[string]bool{}
		for _, lit := range literals(before, isGo) {
			old[lit] = true
		}
		seen := map[string]bool{}
		for _, lit := range literals(after, isGo) {
			pieces := piecesOf(lit)
			key := strings.Join(pieces, "\x00")
			if old[lit] || len(pieces) == 0 || seen[key] || !worthChecking(lit) || allIn(stated, pieces) {
				continue
			}
			seen[key] = true
			if ok, err := f.allFound(ctx, in.Solution, pieces, in.Reference); err != nil {
				return nil, err
			} else if !ok {
				continue // the implementation does not produce it: a test's own message
			}
			if ok, err := f.allFound(ctx, in.Base, pieces, nil); err != nil {
				return nil, err
			} else if !ok {
				gaps = append(gaps, Gap{Kind: GapLiteral, Text: strings.TrimSpace(lit), File: file})
			}
		}
		if !isGo {
			continue
		}
		oldNames := map[string]bool{}
		if bf, err := parser.ParseFile(token.NewFileSet(), file, before, parser.SkipObjectResolution); err == nil && len(before) > 0 {
			oldNames = used(bf)
		}
		af, err := parser.ParseFile(token.NewFileSet(), file, after, parser.SkipObjectResolution)
		if err != nil {
			continue // not valid Go: literals were still checked by pattern
		}
		for name := range used(af) {
			dirs, isNew := newNames[name]
			if !isNew || oldNames[name] || wordIn(stated, name) {
				continue
			}
			inBase, err := f.baseHasName(ctx, in.Base, name, dirs)
			if err != nil {
				return nil, err
			}
			if !inBase {
				gaps = append(gaps, Gap{Kind: GapIdentifier, Text: name, File: file})
			}
		}
	}
	sort.SliceStable(gaps, func(i, j int) bool {
		a, b := gaps[i], gaps[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Text < b.Text
	})
	return gaps, nil
}

func (f *Fairness) source(ctx context.Context, commit string) (source.Source, error) {
	if s, ok := f.srcs[commit]; ok {
		return s, nil
	}
	s, err := source.Commit(ctx, commit, f.where...)
	if err != nil {
		return nil, fmt.Errorf("fairness: %w", err)
	}
	f.srcs[commit] = s
	return s, nil
}

// versions reads a hidden test file as the solution has it and as the base had it (nil if new).
func (f *Fairness) versions(ctx context.Context, in FairnessInput, file string) (after, before []byte, err error) {
	sol, err := f.source(ctx, in.Solution)
	if err != nil {
		return nil, nil, err
	}
	if after, err = sol.ReadFile(file); err != nil {
		return nil, nil, fmt.Errorf("fairness: %w", err)
	}
	base, err := f.source(ctx, in.Base)
	if err != nil {
		return nil, nil, err
	}
	if source.Has(base, file) {
		if before, err = base.ReadFile(file); err != nil {
			return nil, nil, fmt.Errorf("fairness: %w", err)
		}
	}
	return after, before, nil
}

// grep reports whether commit has pattern (fixed string; case-insensitive text, or a case-sensitive whole word when word is set) in the files
// matching pathspecs (all files when none). git grep exits 1 with no output when nothing matches, which gitx
// reports as an error with an empty message.
func (f *Fairness) grep(ctx context.Context, commit, pattern string, word bool, pathspecs []string) (bool, error) {
	key := fmt.Sprintf("%s\x00%s\x00%t\x00%s", commit, pattern, word, strings.Join(pathspecs, "\x00"))
	if v, ok := f.found[key]; ok {
		return v, nil
	}
	args := append([]string{}, f.where...)
	args = append(args, "grep", "-F", "-l")
	if word { // Go names are case-sensitive: Wait does not state Timeout
		args = append(args, "-w")
	} else {
		args = append(args, "-i")
	}
	args = append(args, "-e", pattern, commit, "--")
	args = append(args, pathspecs...)
	out, err := gitx.Output(ctx, nil, args...)
	if ctx.Err() != nil { // a cancelled git also exits without a message: never cache that as "no match"
		return false, fmt.Errorf("fairness: %w", ctx.Err())
	}
	if err != nil && !strings.HasSuffix(err.Error(), ": ") {
		return false, fmt.Errorf("fairness: %w", err)
	}
	found := err == nil && len(out) > 0
	f.found[key] = found
	return found, nil
}

// allFound reports whether every piece is in the given files of commit (any file when files is empty).
func (f *Fairness) allFound(ctx context.Context, commit string, pieces, files []string) (bool, error) {
	var specs []string
	for _, p := range files {
		specs = append(specs, ":(literal)"+p)
	}
	for _, piece := range pieces {
		ok, err := f.grep(ctx, commit, piece, false, specs)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// baseHasName reports whether the base's Go files in dirs mention name as a word.
func (f *Fairness) baseHasName(ctx context.Context, base, name string, dirs []string) (bool, error) {
	var specs []string
	for _, d := range dirs {
		specs = append(specs, ":(glob)"+path.Join(d, "*.go"))
	}
	return f.grep(ctx, base, name, true, specs)
}

// newNames collects, from the reference's Go files, the package-level names and struct and interface members they
// declare (not parameters or locals), with the directories declaring each.
func (f *Fairness) newNames(ctx context.Context, in FairnessInput) (map[string][]string, error) {
	names := map[string][]string{}
	sol, err := f.source(ctx, in.Solution)
	if err != nil {
		return nil, err
	}
	for _, p := range in.Reference {
		if path.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") || !source.Has(sol, p) {
			continue
		}
		data, err := sol.ReadFile(p)
		if err != nil {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), p, data, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		add := func(id *ast.Ident) {
			if id.Name != "_" && !slices.Contains(names[id.Name], path.Dir(p)) {
				names[id.Name] = append(names[id.Name], path.Dir(p))
			}
		}
		members := func(fields *ast.FieldList) {
			if fields != nil {
				for _, fl := range fields.List {
					for _, id := range fl.Names {
						add(id)
					}
				}
			}
		}
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				add(d.Name)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, id := range s.Names {
							add(id)
						}
					case *ast.TypeSpec:
						add(s.Name)
						ast.Inspect(s.Type, func(n ast.Node) bool {
							switch n := n.(type) {
							case *ast.StructType:
								members(n.Fields)
							case *ast.InterfaceType:
								members(n.Methods)
							}
							return true
						})
					}
				}
			}
		}
	}
	return names, nil
}

func normalize(s string) string { return strings.ToLower(spaces.ReplaceAllString(s, " ")) }

// piecesOf cuts a literal at newlines and %-verbs and keeps the pieces of at least minLiteral characters, trimmed of
// surrounding space and punctuation. A literal with no such piece is too generic to check.
func piecesOf(lit string) []string {
	var out []string
	for _, chunk := range strings.Split(verbs.ReplaceAllString(lit, "\n"), "\n") {
		chunk = strings.Trim(spaces.ReplaceAllString(strings.TrimSpace(chunk), " "), " .,;:!?\"'`")
		if len([]rune(chunk)) >= minLiteral {
			out = append(out, chunk)
		}
	}
	return out
}

// allIn reports whether every piece is in the (normalized) text.
func allIn(text string, pieces []string) bool {
	for _, p := range pieces {
		if !strings.Contains(text, normalize(p)) {
			return false
		}
	}
	return true
}

// worthChecking drops literals with no letter: numbers, separators.
func worthChecking(lit string) bool {
	return strings.IndexFunc(lit, func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127 }) >= 0
}

func wordIn(text, name string) bool {
	return regexp.MustCompile(`(?i)(^|[^\w])` + regexp.QuoteMeta(name) + `($|[^\w])`).MatchString(text)
}

// literals returns the string literals of a source file: through the Go parser for Go (skipping import paths and the
// first argument of messageCalls; falling back to the regular expression when the file does not parse), and by
// regular expression elsewhere (single-line strings only).
func literals(src []byte, isGo bool) []string {
	if len(src) == 0 {
		return nil
	}
	var out []string
	if isGo {
		if f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution); err == nil {
			skip := map[*ast.BasicLit]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.ImportSpec:
					skip[n.Path] = true
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && messageCalls[sel.Sel.Name] && len(n.Args) > 0 {
						if lit, ok := n.Args[0].(*ast.BasicLit); ok {
							skip[lit] = true
						}
					}
				}
				return true
			})
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && !skip[lit] {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						out = append(out, s)
					}
				}
				return true
			})
			return out
		}
	}
	for _, m := range quoted.FindAll(src, -1) {
		body := string(m[1 : len(m)-1])
		out = append(out, strings.NewReplacer(`\"`, `"`, `\'`, `'`, `\n`, "\n", `\t`, "\t").Replace(body))
	}
	return out
}

// used collects the names a file uses as selectors, composite-literal keys and called functions.
func used(f *ast.File) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			names[n.Sel.Name] = true
		case *ast.CompositeLit:
			for _, el := range n.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						names[id.Name] = true
					}
				}
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok {
				names[id.Name] = true
			}
		}
		return true
	})
	return names
}
