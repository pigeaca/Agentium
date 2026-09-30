package task

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// Gap kinds.
const (
	GapLiteral    = "literal"    // a string the hidden tests compare against, stated nowhere the agent can see
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

// minLiteral is the length under which a string literal is too generic to count ("ok", "--flag").
const minLiteral = 8

// maxScanned caps the size of a base file searched for literals.
const maxScanned = 1 << 20

var (
	// quoted matches the string literals of Python, Ruby, JavaScript and TypeScript on one line.
	quoted = regexp.MustCompile("\"(?:[^\"\\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\\n]|\\\\.)*'|`[^`]*`")
	// sourceExt lists the test-file extensions scanned; other hidden files (data, snapshots) are not.
	sourceExt = map[string]bool{".go": true, ".py": true, ".rb": true, ".js": true, ".jsx": true, ".ts": true,
		".tsx": true, ".mjs": true, ".cjs": true, ".mts": true, ".cts": true}
	spaces = regexp.MustCompile(`\s+`)
)

// Fairness lists what the hidden tests need that the agent cannot learn from the instruction or the base code. It is a
// heuristic that reads only what the solution added or changed in the hidden test files:
//
//   - String literals of at least 8 characters with a letter in them, in any supported language, that do not occur in
//     the file's base version, in any base file, or in the instruction (compared with whitespace collapsed and case
//     ignored). These are exact messages and texts a test compares against.
//   - For Go only: names the tests use as selectors (x.Name), as composite-literal keys (T{Name: v}) or as called
//     functions (Name(...)) that the solution's non-test files of the package declare (functions, methods, types,
//     fields, constants, variables) but the base package does not, and that the instruction does not mention. Names the
//     tests use from the standard library or dependencies are never flagged, because only names the solution
//     declares can be flagged; a new name equal to an unrelated existing one is missed.
//
// Gaps come sorted by file, kind and text. Solution-less tasks have no hidden tests and so no gaps.
func Fairness(base, solution source.Source, instruction string, hiddenTests []string) ([]Gap, error) {
	var gaps []Gap
	var baseText string // lazily built: every base file's text, for literal lookups
	corpus := func() string {
		if baseText == "" {
			var b strings.Builder
			for _, p := range base.Paths() {
				if data, err := base.ReadFile(p); err == nil && len(data) <= maxScanned && !bytes.Contains(data, []byte{0}) {
					b.WriteString(string(data))
					b.WriteByte('\n')
				}
			}
			baseText = normalize(b.String())
		}
		return baseText
	}
	stated := normalize(instruction)
	for _, file := range hiddenTests {
		if !sourceExt[path.Ext(file)] {
			continue
		}
		after, err := solution.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("fairness: %w", err)
		}
		var before []byte
		if source.Has(base, file) {
			if before, err = base.ReadFile(file); err != nil {
				return nil, fmt.Errorf("fairness: %w", err)
			}
		}
		isGo := path.Ext(file) == ".go"
		old := map[string]bool{}
		for _, lit := range literals(before, isGo) {
			old[lit] = true
		}
		seen := map[string]bool{}
		for _, lit := range literals(after, isGo) {
			key := normalize(lit)
			if old[lit] || seen[key] || !worthChecking(lit) || strings.Contains(stated, key) || strings.Contains(corpus(), key) {
				continue
			}
			seen[key] = true
			gaps = append(gaps, Gap{Kind: GapLiteral, Text: lit, File: file})
		}
		if isGo {
			names, err := missingNames(base, solution, file, after, before)
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				if !wordIn(stated, name) {
					gaps = append(gaps, Gap{Kind: GapIdentifier, Text: name, File: file})
				}
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

func normalize(s string) string { return strings.ToLower(spaces.ReplaceAllString(s, " ")) }

// worthChecking drops literals that are short or have no letter: separators, numbers, flags, format verbs.
func worthChecking(lit string) bool {
	if len([]rune(strings.TrimSpace(lit))) < minLiteral {
		return false
	}
	return strings.IndexFunc(lit, func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127 }) >= 0
}

func wordIn(text, name string) bool {
	return regexp.MustCompile(`(?i)(^|[^\w])` + regexp.QuoteMeta(name) + `($|[^\w])`).MatchString(text)
}

// literals returns the string literals of a source file: through the Go parser for Go (falling back to the regular
// expression when the file does not parse), and by regular expression elsewhere (single-line strings only).
func literals(src []byte, isGo bool) []string {
	if len(src) == 0 {
		return nil
	}
	var out []string
	if isGo {
		fset := token.NewFileSet()
		if f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution); err == nil {
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						out = append(out, s)
					}
				}
				if imp, ok := n.(*ast.ImportSpec); ok && imp != nil {
					return false // import paths are not texts
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

// missingNames finds the Go names the hidden test file uses that the solution's package declares and the base's does not.
func missingNames(base, solution source.Source, file string, after, before []byte) ([]string, error) {
	dir := path.Dir(file)
	baseDecl := declared(base, dir, true)
	newDecl := declared(solution, dir, false)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, after, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil // not valid Go: literals were still checked by pattern
	}
	old := map[string]bool{}
	if len(before) > 0 {
		if bf, err := parser.ParseFile(token.NewFileSet(), file, before, parser.SkipObjectResolution); err == nil {
			for name := range used(bf) {
				old[name] = true
			}
		}
	}
	var out []string
	for name := range used(f) {
		if newDecl[name] && !baseDecl[name] && !old[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
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

// declared collects the names the Go files directly in dir declare: functions, methods, types, struct fields,
// interface methods, constants and variables. withTests also reads _test.go files (helpers a hidden test may rely on).
func declared(src source.Source, dir string, withTests bool) map[string]bool {
	names := map[string]bool{}
	for _, p := range src.Paths() {
		if path.Dir(p) != dir || path.Ext(p) != ".go" || (!withTests && strings.HasSuffix(p, "_test.go")) {
			continue
		}
		data, err := src.ReadFile(p)
		if err != nil {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, data, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				names[n.Name.Name] = true
			case *ast.TypeSpec:
				names[n.Name.Name] = true
			case *ast.ValueSpec:
				for _, id := range n.Names {
					names[id.Name] = true
				}
			case *ast.Field:
				for _, id := range n.Names {
					names[id.Name] = true
				}
			}
			return true
		})
	}
	return names
}
