package task

import (
	"context"
	"errors"
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
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/source"
)

// Gap kinds.
const (
	GapLiteral    = "literal"    // a text the hidden tests compare against and the solution produces, stated nowhere the agent can see
	GapIdentifier = "identifier" // a Go, Java, Kotlin or Rust name the hidden tests use that the solution adds and the base lacks
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
		".tsx": true, ".mjs": true, ".cjs": true, ".mts": true, ".cts": true, ".java": true, ".kt": true, ".rs": true}
	moduleLine = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)"?`)
	spaces     = regexp.MustCompile(`\s+`)
	verbs      = regexp.MustCompile(`%[-+# 0-9.*]*[a-zA-Z%]`)
	// messageCalls are Go calls whose first argument is a format or a name, not an expectation (t.Run, t.Errorf, fmt.Sprintf...).
	messageCalls = map[string]bool{"Run": true, "Errorf": true, "Fatalf": true, "Logf": true, "Skipf": true, "Sprintf": true}
)

// Fairness finds what hidden tests require that the agent cannot learn from the instruction or the base code. One
// value serves one command: it keeps each task's gaps once worked out, each search's answer, and each object it read
// of the repository (source.Objects), so a check asked again starts no git process. It is safe for concurrent use
// (PrepareGaps checks several tasks at a time): whoever asks for what another goroutine is working out waits for it.
// All git access is local and goes through internal/gitx.
type Fairness struct {
	where   []string        // git location, e.g. "--git-dir", bare
	objects *source.Objects // the repository's commits, each object read once
	srcs    kept[source.Source]
	found   kept[bool]
	stmts   kept[map[string]map[string]bool] // baseFields by commit and directories
	gaps    kept[[]Gap]                      // Gaps by input
	// unread counts the reads of f's sources that failed. A check skips a file it cannot read (a link that leaves the
	// repository never can be read), so an answer worked out while the count rose may lack something: it is given, as
	// it always was, but not kept, in case the next read succeeds.
	unread atomic.Int64
	cache  *GapsCache // gaps kept between commands (Keep); nil keeps none
}

// Keep makes f take gaps from c, where an earlier command of the same checker put them, and put there the complete
// ones it works out for commits named by their full IDs. Call it before f is used; the command saves c when it ends.
func (f *Fairness) Keep(c *GapsCache) { f.cache = c }

// NewFairness returns a checker for the repository located by where (for example "--git-dir", bare).
func NewFairness(where ...string) *Fairness {
	return &Fairness{where: slices.Clip(slices.Clone(where)), objects: source.NewObjects(where...)}
}

// kept holds values worked out once per key, for several goroutines: the first to ask for a key works its value out
// and the others wait for it, each for as long as its own context lives. A value is kept only when its work says so;
// a failure, or an answer that is not to be kept, leaves the key free, and each caller that waited then works the
// value out itself and gets its own answer or error. Working a value out may ask another kept for a value, never its
// own: Fairness's four are asked in one order (gaps, then stmts, then srcs or found), so no two goroutines wait for
// each other.
type kept[V any] struct {
	mu sync.Mutex // guards m and its values; never held while a value is worked out
	m  map[string]*keptValue[V]
}

type keptValue[V any] struct {
	v       V
	ok      bool          // v is kept
	working chan struct{} // closed when the attempt under way ends; nil when none is
}

// get returns key's kept value, or else what work returns, which is kept when work says so and fails with no error.
// A caller whose ctx ends while it waits for another's work gets ctx's error.
func (k *kept[V]) get(ctx context.Context, key string, work func() (v V, keep bool, err error)) (V, error) {
	for {
		k.mu.Lock()
		if k.m == nil {
			k.m = map[string]*keptValue[V]{}
		}
		e := k.m[key]
		if e == nil {
			e = &keptValue[V]{}
			k.m[key] = e
		}
		if e.ok {
			v := e.v
			k.mu.Unlock()
			return v, nil
		}
		if under := e.working; under != nil {
			k.mu.Unlock()
			select {
			case <-under: // kept by now, or free to work out
				continue
			case <-ctx.Done():
				var none V
				return none, fmt.Errorf("fairness: %w", ctx.Err())
			}
		}
		done := make(chan struct{})
		e.working = done
		k.mu.Unlock()
		var (
			v    V
			keep bool
			err  error
		)
		func() {
			defer func() { // also when work panics: those who wait must not wait for ever
				k.mu.Lock()
				if err == nil && keep {
					e.v, e.ok = v, true
				}
				e.working = nil
				k.mu.Unlock()
				close(done)
			}()
			v, keep, err = work()
		}()
		return v, err
	}
}

// counted is one of f's sources: it counts the reads that fail (Fairness.unread).
type counted struct {
	source.Source
	unread *atomic.Int64
}

func (c counted) ReadFile(p string) ([]byte, error) {
	data, err := c.Source.ReadFile(p)
	if err != nil {
		c.unread.Add(1)
	}
	return data, err
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
//     A text a reference format string produces (fmt.Sprintf("slash commands differ (%d added)"), each verb matching
//     any text) counts as produced by the reference; it is a gap unless the format's fixed pieces are in the base or
//     the instruction. Only formats the solution added or changed in non-test files are used.
//     Matching ignores case, collapsed whitespace and surrounding punctuation; base and reference are searched with
//     git grep, so whitespace inside a piece must match exactly there. Limitation: the format argument of a
//     call in a hidden test is skipped, so an expectation a test builds with fmt.Sprintf is not checked.
//   - For Java, Kotlin, Rust, Python and TypeScript or JavaScript (fairness_names.go, fairness_script.go): names a hidden test file newly uses anywhere in its code (imports,
//     static imports, qualified references, Type::item) that the reference declares (class, interface, enum, record,
//     object, fun, val, struct, trait, fn, const, mod, field and so on, found by pattern), that the test does not declare
//     itself, that the instruction does not mention and that no base .java or .kt (or .rs) file contains as a word.
//     Only names the reference declares can be flagged, so JDK, standard-library and dependency names never are.
//     Overrides (@Override, Kotlin override, methods inside a Rust "impl Trait for Type") are not declarations, and the
//     test's own locals, parameters and lambda parameters (Java "Type name", Kotlin "name:" and "name ->", Rust
//     "let name" and "name:") hide a name.
//     Limits: names under 4 characters are skipped; a new name equal to any old word in the base is missed; enum
//     constants are not seen as declarations; Kotlin generic functions with nested ">" (fun <T : List<X>> f), Kotlin
//     constructor parameters without val or var, and Rust "pub use" re-exports are missed; no scoping, so a name the
//     reference declares in one place and the test takes from a dependency is flagged only if the base lacks the word.
//     Python and TypeScript declarations are def, class, module-level assignments and annotated fields (Python) and
//     function, class, interface, type, enum, exported or top-level const, export lists and members with a modifier,
//     a body or a type (TypeScript); Python dunder methods and TypeScript toString and the like are not. A test's
//     own names are its definitions, assignments, parameters and destructured names. Python and TypeScript limits:
//     plain class-level assignments (enum members, class constants), TypeScript enum members, object-literal methods
//     and re-exports (from x import y in an __init__, export * from) are not seen as declarations; a Python name
//     reached only through __all__ strings is not a declaration; a regular-expression literal with a quote or JSX
//     text with an apostrophe can blank the rest of its line; .js, .jsx, .mjs and .cjs files join the TypeScript family.
//   - For Go: names the tests newly use as selectors (x.Name), composite-literal keys (T{Name: v}) or called functions
//     that a changed non-test Go file declares at package level or as a struct or interface member, that the base's Go
//     files in that directory lack, and that the instruction does not mention. Only names the reference declares can be
//     flagged, so standard-library names never are. A key of a composite literal that names its type (T{Name: v},
//     &T{...}, pkg.T{...}) is checked against the fields of the base's struct T in T's own directory (pkg through the
//     module path in go.mod; a type that cannot be resolved falls back to the word search), so a field Name that another base type has is
//     still flagged, and a key the base's test already sets on that type is not. Selectors (x.Name) and keys of literals with an elided type have no known receiver without
//     go/types, so they use the word search: a new name equal to any old word in its directory is missed there.
//
// The gaps of an input are worked out once and kept: the input says all they depend on (f takes a commit's name to
// mean one commit for as long as it lives, as its kept sources and searches always have). They are not kept when a
// read failed while they were worked out, or when ctx ended on the way: something may be missing from them.
//
// A caller whose ctx has ended gets an error, never an answer. Its check still runs, so an interrupted command fails
// as it always did, at its first git call; what the kept sources and searches of another context would still answer
// is refused when the check returns. A caller whose ctx ends while it waits for another goroutine's check of the same
// input gets the error then.
func (f *Fairness) Gaps(ctx context.Context, in FairnessInput) ([]Gap, error) {
	var gaps []Gap
	var err error
	if ctx.Err() != nil {
		gaps, err = f.check(ctx, in, new(bool))
	} else {
		gaps, err = f.gaps.get(ctx, in.key(), func() ([]Gap, bool, error) {
			if gaps, ok := f.cache.lookup(in); ok {
				return gaps, true, nil
			}
			unread := f.unread.Load()
			lasting := true
			gaps, err := f.check(ctx, in, &lasting)
			keep := f.unread.Load() == unread && ctx.Err() == nil
			if err == nil && keep && lasting && f.cache != nil && f.pinned(ctx, in) {
				f.cache.store(in, gaps)
			}
			return gaps, keep, err
		})
	}
	if err == nil && ctx.Err() != nil {
		return nil, fmt.Errorf("fairness: %w", ctx.Err())
	}
	return slices.Clone(gaps), err // the caller's own: it may sort or cut them
}

// pinned reports whether in names both its commits by their full IDs. Only then does in say all its gaps depend on
// whenever it is asked, so that they may outlive f (GapsCache).
func (f *Fairness) pinned(ctx context.Context, in FairnessInput) bool {
	for _, commit := range []string{in.Base, in.Solution} {
		src, err := f.source(ctx, commit) // kept by the check that just ended
		if err != nil {
			return false
		}
		if c, ok := src.(counted); !ok || !source.Pinned(c.Source) {
			return false
		}
	}
	return true
}

// key tells inputs apart exactly: every text is written with its length, so no two inputs share a key.
func (in FairnessInput) key() string {
	var b strings.Builder
	for _, texts := range [][]string{{in.Base, in.Solution, in.Instruction}, in.HiddenTests, in.Reference} {
		fmt.Fprintf(&b, "%d\x00", len(texts))
		for _, text := range texts {
			fmt.Fprintf(&b, "%d\x00%s", len(text), text)
		}
	}
	return b.String()
}

// check works out Gaps' answer. It sets *lasting to false when the answer is good for this command only: the check
// asked git, ignoring case, for text outside ASCII. How such text matches depends on what git loads to match it (PCRE2
// and its Unicode tables, or the C library's), which nothing Agentium can read names, so another command may be told
// otherwise by the same git. ASCII matches the same in all of them.
func (f *Fairness) check(ctx context.Context, in FairnessInput, lasting *bool) ([]Gap, error) {
	var gaps []Gap
	stated := normalize(in.Instruction)
	newNames, newTypes, err := f.newNames(ctx, in) // name (type) -> directories of the reference files declaring it
	if err != nil {
		return nil, err
	}
	formats, err := f.formats(ctx, in)
	if err != nil {
		return nil, err
	}
	refNames, err := f.referenceNames(ctx, in)
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
		for _, lit := range literalsOf(before, path.Ext(file), false) {
			old[lit] = true
		}
		seen := map[string]bool{}
		for _, lit := range literalsOf(after, path.Ext(file), false) {
			pieces := piecesOf(lit)
			produced := false
			if format, ok := matchFormat(formats, strings.TrimSpace(lit)); ok { // built by the reference from a format string
				pieces, produced = piecesOf(format), true
			}
			key := strings.Join(pieces, "\x00")
			if old[lit] || len(pieces) == 0 || seen[key] || !worthChecking(lit) || allIn(stated, pieces) {
				continue
			}
			seen[key] = true
			if !isASCII(pieces) {
				*lasting = false
			}
			if produced {
				// nothing to look up in the reference: the format is what it has
			} else if ok, err := f.allFound(ctx, in.Solution, pieces, in.Reference); err != nil {
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
			names, err := f.unstatedNames(ctx, in, file, after, before, stated, refNames)
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				gaps = append(gaps, Gap{Kind: GapIdentifier, Text: name, File: file})
			}
			continue
		}
		var oldUse usage
		if bf, err := parser.ParseFile(token.NewFileSet(), file, before, parser.SkipObjectResolution); err == nil && len(before) > 0 {
			oldUse = used(bf)
		}
		af, err := parser.ParseFile(token.NewFileSet(), file, after, parser.SkipObjectResolution)
		if err != nil {
			continue // not valid Go: literals were still checked by pattern
		}
		use := used(af)
		flagged := map[string]bool{}
		flag := func(name string) {
			if !flagged[name] {
				flagged[name] = true
				gaps = append(gaps, Gap{Kind: GapIdentifier, Text: name, File: file})
			}
		}
		for name := range use.plain {
			dirs, isNew := newNames[name]
			if !isNew || oldUse.plain[name] || wordIn(stated, name) {
				continue
			}
			inBase, err := f.baseHasName(ctx, in.Base, name, dirs)
			if err != nil {
				return nil, err
			}
			if !inBase {
				flag(name)
			}
		}
		dir := path.Dir(file)
		for name, refs := range use.keyed { // T{Name: v}: is Name a field the base's T lacks?
			dirs, isNew := newNames[name]
			if !isNew || wordIn(stated, name) {
				continue
			}
			for ref := range refs {
				if oldUse.keyed[name][ref] {
					continue // the base's test already sets it on this type
				}
				known := false
				typeDir, ok := f.typeDir(ctx, in, af, dir, ref)
				var fields map[string]map[string]bool
				if ok {
					var err error
					if fields, err = f.baseFields(ctx, in.Base, []string{typeDir}); err != nil {
						return nil, err
					}
					_, inBase := fields[ref.Name]
					known = inBase || slices.Contains(newTypes[ref.Name], typeDir)
				}
				if known {
					if slices.Contains(dirs, typeDir) && !fields[ref.Name][name] {
						flag(name) // the field is declared with the type, and the base's struct lacks it
					}
					continue
				}
				// The type is not resolved (a dot import, a package name that differs from its directory, another
				// module): treat the key like a selector.
				inBase, err := f.baseHasName(ctx, in.Base, name, dirs)
				if err != nil {
					return nil, err
				}
				if !inBase {
					flag(name)
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

func (f *Fairness) source(ctx context.Context, commit string) (source.Source, error) {
	return f.srcs.get(ctx, commit, func() (source.Source, bool, error) {
		s, err := f.objects.Commit(ctx, commit)
		if err != nil {
			return nil, false, fmt.Errorf("fairness: %w", err)
		}
		return counted{Source: s, unread: &f.unread}, true, nil
	})
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
// reports as an *ExitError with that status and an empty message. A git that a signal ended (killed, or crashed) has
// an empty message too, and found out nothing: that is an error, never "no match".
func (f *Fairness) grep(ctx context.Context, commit, pattern string, word bool, pathspecs []string) (bool, error) {
	key := fmt.Sprintf("%s\x00%s\x00%t\x00%s", commit, pattern, word, strings.Join(pathspecs, "\x00"))
	return f.found.get(ctx, key, func() (bool, bool, error) {
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
		if ctx.Err() != nil { // a cancelled git also exits without a message: never keep that as "no match"
			return false, false, fmt.Errorf("fairness: %w", ctx.Err())
		}
		var exit *gitx.ExitError
		if noMatch := errors.As(err, &exit) && exit.Code == 1 && exit.Stderr == ""; err != nil && !noMatch {
			return false, false, fmt.Errorf("fairness: %w", err)
		}
		return err == nil && len(out) > 0, true, nil
	})
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

// format is a reference format string made into a pattern: every verb matches any text.
type format struct {
	text  string
	re    *regexp.Regexp
	fixed int // characters in the fixed pieces: how specific the pattern is
}

// formats collects the format strings (literals with %-verbs) the solution added or changed in its non-test source
// files, from the message calls too.
func (f *Fairness) formats(ctx context.Context, in FairnessInput) ([]format, error) {
	sol, err := f.source(ctx, in.Solution)
	if err != nil {
		return nil, err
	}
	base, err := f.source(ctx, in.Base)
	if err != nil {
		return nil, err
	}
	var out []format
	for _, p := range in.Reference {
		if !sourceExt[path.Ext(p)] || strings.HasSuffix(p, "_test.go") || !source.Has(sol, p) {
			continue
		}
		after, err := sol.ReadFile(p)
		if err != nil {
			continue
		}
		var before []byte
		if source.Has(base, p) {
			before, _ = base.ReadFile(p)
		}
		old := map[string]bool{}
		for _, lit := range literalsOf(before, path.Ext(p), true) {
			old[lit] = true
		}
		for _, lit := range literalsOf(after, path.Ext(p), true) {
			pieces := piecesOf(lit)
			if old[lit] || !verbs.MatchString(lit) || len(pieces) == 0 { // "%d" or "%s: %v" would match almost any text
				continue
			}
			fixed := 0
			for _, p := range pieces {
				fixed += len(p)
			}
			parts := verbs.Split(lit, -1)
			for i := range parts {
				parts[i] = regexp.QuoteMeta(parts[i])
			}
			out = append(out, format{text: lit, fixed: fixed, re: regexp.MustCompile(`(?s)^` + strings.Join(parts, `.*`) + `$`)})
		}
	}
	return out, nil
}

// matchFormat returns the most specific format (the most fixed characters) whose pattern matches lit in full.
func matchFormat(formats []format, lit string) (string, bool) {
	best, found := format{}, false
	for _, fm := range formats {
		if fm.re.MatchString(lit) && (!found || fm.fixed > best.fixed) {
			best, found = fm, true
		}
	}
	return best.text, found
}

// typeDir finds the directory of the package that declares the type a composite literal names: the test file's own
// for T, and for pkg.T the directory the import of pkg maps to through the module path in go.mod (the alias, or else the
// last element of the import path, names pkg). It reports false when the package is not in this module.
func (f *Fairness) typeDir(ctx context.Context, in FairnessInput, file *ast.File, dir string, ref typeRef) (string, bool) {
	if ref.Qual == "" {
		return dir, true
	}
	sol, err := f.source(ctx, in.Solution)
	if err != nil || !source.Has(sol, "go.mod") {
		return "", false
	}
	mod, err := sol.ReadFile("go.mod")
	if err != nil {
		return "", false
	}
	m := moduleLine.FindSubmatch(mod)
	if m == nil {
		return "", false
	}
	module := string(m[1])
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != ref.Qual {
			continue
		}
		switch {
		case p == module:
			return ".", true
		case strings.HasPrefix(p, module+"/"):
			return strings.TrimPrefix(p, module+"/"), true
		}
	}
	return "", false
}

// baseFields returns the fields of the struct types the base's Go files in dirs declare, by type name.
func (f *Fairness) baseFields(ctx context.Context, commit string, dirs []string) (map[string]map[string]bool, error) {
	key := commit + "\x00" + strings.Join(dirs, "\x00")
	return f.stmts.get(ctx, key, func() (map[string]map[string]bool, bool, error) {
		src, err := f.source(ctx, commit)
		if err != nil {
			return nil, false, err
		}
		unread := f.unread.Load()
		out := map[string]map[string]bool{} // kept only once filled: its readers never change it
		for _, p := range src.Paths() {
			if path.Ext(p) != ".go" || !slices.Contains(dirs, path.Dir(p)) {
				continue
			}
			data, err := src.ReadFile(p)
			if err != nil {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), p, data, parser.SkipObjectResolution)
			if err != nil {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if ts, ok := n.(*ast.TypeSpec); ok {
					if st, ok := ts.Type.(*ast.StructType); ok {
						if out[ts.Name.Name] == nil {
							out[ts.Name.Name] = map[string]bool{}
						}
						for _, fl := range st.Fields.List {
							for _, id := range fl.Names {
								out[ts.Name.Name][id.Name] = true
							}
						}
					}
				}
				return true
			})
		}
		if ctx.Err() != nil { // a cancelled read skips its file: the fields of the files that were left are no answer
			return nil, false, fmt.Errorf("fairness: %w", ctx.Err())
		}
		return out, f.unread.Load() == unread, nil // a file that could not be read may be readable next time
	})
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
// declare (not parameters or locals), with the directories declaring each; types is the subset that are type names.
func (f *Fairness) newNames(ctx context.Context, in FairnessInput) (names, types map[string][]string, err error) {
	names, types = map[string][]string{}, map[string][]string{}
	sol, err := f.source(ctx, in.Solution)
	if err != nil {
		return nil, nil, err
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
						if !slices.Contains(types[s.Name.Name], path.Dir(p)) {
							types[s.Name.Name] = append(types[s.Name.Name], path.Dir(p))
						}
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
	return names, types, nil
}

func normalize(s string) string { return strings.ToLower(spaces.ReplaceAllString(s, " ")) }

// piecesOf cuts a literal at newlines and %-verbs and keeps the pieces of at least minLiteral characters, trimmed of
// surrounding space and punctuation. A literal with no such piece is too generic to check.
func piecesOf(lit string) []string {
	var out []string
	lit = strings.Map(func(r rune) rune { // control characters (NUL, tab, newline) separate pieces; git grep cannot take NUL
		if unicode.IsControl(r) {
			return '\n'
		}
		return r
	}, lit)
	for _, chunk := range strings.Split(verbs.ReplaceAllString(lit, "\n"), "\n") {
		chunk = strings.Trim(spaces.ReplaceAllString(strings.TrimSpace(chunk), " "), " .,;:!?\"'`()[]")
		if len([]rune(chunk)) >= minLiteral {
			out = append(out, chunk)
		}
	}
	return out
}

// isASCII reports whether no piece has a byte outside ASCII.
func isASCII(pieces []string) bool {
	for _, piece := range pieces {
		for i := 0; i < len(piece); i++ {
			if piece[i] >= utf8.RuneSelf {
				return false
			}
		}
	}
	return true
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

// literalsOf returns the string literals of a source file of the given extension: Java, Kotlin and Rust through the
// small lexer in lex.go (multi-line text blocks, raw strings and byte strings included), others through literals.
// A Rust {} placeholder is not treated as a format verb, so a reference format string such as "got {}" only matches
// the exact text.
func literalsOf(src []byte, ext string, keepMessages bool) []string {
	switch ext {
	case ".java":
		return stringLiterals(string(src), langJava)
	case ".kt":
		return stringLiterals(string(src), langKotlin)
	case ".rs":
		return stringLiterals(string(src), langRust)
	}
	return literals(src, ext == ".go", keepMessages)
}

// literals returns the string literals of a source file: through the Go parser for Go (skipping import paths and the
// first argument of messageCalls unless keepMessages; falling back to the regular expression when the file does not parse), and by
// regular expression elsewhere (single-line strings only).
func literals(src []byte, isGo, keepMessages bool) []string {
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
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && !keepMessages && messageCalls[sel.Sel.Name] && len(n.Args) > 0 {
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

// usage is what a Go file uses of the package's names.
type usage struct {
	plain map[string]bool             // selectors (x.Name), called functions and keys of composite literals of unknown type
	keyed map[string]map[typeRef]bool // key -> the struct types named in literals it is a key of (T{Key: v}, &T{...})
}

// used collects the names a file uses. A selector's receiver type is unknown without type information, so selectors
// are plain names; a composite literal that names its type (T{Key: v}, pkg.T{...}, &T{...}) says which type has the key.
func used(f *ast.File) usage {
	u := usage{plain: map[string]bool{}, keyed: map[string]map[typeRef]bool{}}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			u.plain[n.Sel.Name] = true
		case *ast.CompositeLit:
			typ := literalType(n.Type)
			for _, el := range n.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						if typ.Name == "" {
							u.plain[id.Name] = true
						} else {
							if u.keyed[id.Name] == nil {
								u.keyed[id.Name] = map[typeRef]bool{}
							}
							u.keyed[id.Name][typ] = true
						}
					}
				}
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok {
				u.plain[id.Name] = true
			}
		}
		return true
	})
	return u
}

// typeRef names a type in source: T, or pkg.T.
type typeRef struct{ Qual, Name string }

// literalType names the struct type of a composite literal: T, pkg.T or a generic T[...]; empty when it is elided or not a name.
func literalType(e ast.Expr) typeRef {
	switch e := e.(type) {
	case *ast.Ident:
		return typeRef{Name: e.Name}
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return typeRef{Qual: id.Name, Name: e.Sel.Name}
		}
	case *ast.IndexExpr:
		return literalType(e.X)
	case *ast.IndexListExpr:
		return literalType(e.X)
	}
	return typeRef{}
}
