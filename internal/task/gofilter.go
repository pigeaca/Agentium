package task

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pigeaca/agentium/internal/source"
)

// A verify command such as `go test ./... -skip 'TestSlow'` decides which tests grading runs. When its -skip pattern
// matches one of the task's own hidden tests, or its -run pattern leaves one out, grading never runs that test: the
// task may still be valid (another hidden test fails on the base), but it checks less than its solution's tests do.
// FilteredHiddenTests finds that case, for a warning: the user may mean it (a slow test kept out on purpose).

// goTestFilter is one `go test` invocation in a verify command, with its -run and -skip patterns ("" when not given,
// or not known here).
type goTestFilter struct {
	command   string
	run, skip string
}

// filtered reports whether the invocation names a pattern.
func (f goTestFilter) filtered() bool { return f.run != "" || f.skip != "" }

// runs reports whether the invocation runs the top-level test name. Only a pattern's first element (before an
// unbracketed "/") decides: a -skip pattern with more elements skips subtests only, and a -run pattern's later
// elements narrow the subtests a matching test runs. A pattern that does not compile is taken to run everything (`go
// test` would fail on it instead).
func (f goTestFilter) runs(name string) bool {
	if f.skip != "" && len(splitPattern(f.skip)) == 1 {
		if re := firstElement(f.skip); re != nil && re.MatchString(name) {
			return false
		}
	}
	if f.run != "" {
		if re := firstElement(f.run); re != nil && !re.MatchString(name) {
			return false
		}
	}
	return true
}

// describe is the invocation and its patterns in words: "`go test -skip 'Slow' ./...` (-skip "Slow")".
func (f goTestFilter) describe() string {
	var flags []string
	if f.run != "" {
		flags = append(flags, fmt.Sprintf("-run %q", f.run))
	}
	if f.skip != "" {
		flags = append(flags, fmt.Sprintf("-skip %q", f.skip))
	}
	return fmt.Sprintf("`%s` (%s)", f.command, strings.Join(flags, ", "))
}

// FilteredHiddenTests warns of the hidden Go tests (top-level Test, Fuzz and Example functions the solution adds or
// changes in its hidden _test.go files; not TestMain, nor an example without an output comment, which `go test` only
// compiles) that no `go test` invocation of the verify commands runs: an invocation without patterns runs them all,
// so a test one invocation filters out and another runs is graded. Commands it cannot read (a pattern from a shell
// variable) count as running everything. It returns at most one warning.
func FilteredHiddenTests(verify, hiddenTests []string, base, solution source.Source) []string {
	var invocations []goTestFilter
	for _, command := range verify {
		invocations = append(invocations, goTestFilters(command)...)
	}
	if !slices.ContainsFunc(invocations, goTestFilter.filtered) || solution == nil {
		return nil
	}
	missed := slices.DeleteFunc(ownGoTests(hiddenTests, base, solution), func(name string) bool {
		return slices.ContainsFunc(invocations, func(f goTestFilter) bool { return f.runs(name) })
	})
	if len(missed) == 0 {
		return nil
	}
	var filters []string
	for _, f := range invocations {
		filters = append(filters, f.describe())
	}
	return []string{fmt.Sprintf("no `go test` in the verify commands runs hidden test(s) %s: %s filter(s) them out, so grading never runs them",
		strings.Join(missed, ", "), strings.Join(filters, " and "))}
}

// firstElement compiles a -run or -skip pattern's first element, as `go test` matches it against a top-level test's
// name (unanchored); nil when it matches every name (an empty element) or does not compile.
func firstElement(pattern string) *regexp.Regexp {
	first := splitPattern(pattern)[0]
	if first == "" {
		return nil
	}
	re, err := regexp.Compile(first)
	if err != nil {
		return nil
	}
	return re
}

// splitPattern splits a test pattern at the slashes outside brackets and parentheses, as the testing package does.
func splitPattern(pattern string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '[', '(':
			depth++
		case ']', ')':
			depth = max(depth-1, 0)
		case '\\':
			i++
		case '/':
			if depth == 0 {
				parts = append(parts, pattern[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, pattern[start:])
}

// goTestFilters reads every `go test` invocation of a shell command, with its -run and -skip patterns (the last one of
// each, as the flag package keeps). Flags after -args belong to the test binary and are not read.
func goTestFilters(command string) []goTestFilter {
	words, ok := shellWords(command)
	if !ok {
		return nil
	}
	var out []goTestFilter
	for i := 0; i+1 < len(words); i++ {
		if path.Base(words[i]) != "go" || words[i+1] != "test" {
			continue
		}
		f := goTestFilter{command: command}
		for j := i + 2; j < len(words) && !isShellOperator(words[j]); j++ {
			w := words[j]
			if w == "-args" || w == "--args" {
				break
			}
			if value, next, found := flagValue(words, j, "run"); found {
				f.run, j = known(value), next
			} else if value, next, found := flagValue(words, j, "skip"); found {
				f.skip, j = known(value), next
			}
		}
		out = append(out, f)
	}
	return out
}

// goValueFlags are the `go test` flags that take a value, from `go help testflag`, `go help build` and `go help test`
// (go 1.27): their value is the next word unless given as -flag=value. Test flags are also known by their -test. names.
var goValueFlags = map[string]bool{
	// go help testflag
	"bench": true, "benchtime": true, "blockprofile": true, "blockprofilerate": true, "count": true, "covermode": true, "coverpkg": true,
	"coverprofile": true, "cpu": true, "cpuprofile": true, "fuzz": true, "fuzzminimizetime": true, "fuzztime": true, "list": true,
	"memprofile": true, "memprofilerate": true, "mutexprofile": true, "mutexprofilefraction": true, "outputdir": true, "parallel": true,
	"run": true, "shuffle": true, "skip": true, "timeout": true, "trace": true, "vet": true,
	// go help build
	"C": true, "p": true, "asmflags": true, "buildmode": true, "compiler": true, "gccgoflags": true, "gcflags": true, "installsuffix": true,
	"ldflags": true, "mod": true, "modfile": true, "overlay": true, "pgo": true, "pkgdir": true, "tags": true, "toolexec": true,
	// go help test
	"exec": true, "o": true,
}

// goBoolFlags are the `go test` flags that take no value, from the same pages: with goValueFlags, every flag go test
// knows. Any other flag is the test binary's.
var goBoolFlags = map[string]bool{
	"a": true, "n": true, "race": true, "msan": true, "asan": true, "cover": true, "v": true, "work": true, "x": true, "buildvcs": true,
	"json": true, "linkshared": true, "modcacherw": true, "trimpath": true, "c": true, "i": true, "failfast": true, "fullpath": true,
	"short": true, "benchmem": true, "artifacts": true,
}

// goTestTargets lists the packages a shell command's `go test` invocations name, each as written, with the folder of
// its -C flag ("" without one). It reads the arguments as go test does: the package list is the first run of words
// that are not flags; the value of a flag that takes one is never a package; after an unknown flag (the test binary's),
// -args or --, and once a flag has followed the package list, the remaining words are the test binary's.
func goTestTargets(command string) (targets []goTarget) {
	words, ok := shellWords(command)
	if !ok {
		return nil
	}
	for i := 0; i+1 < len(words); i++ {
		if path.Base(words[i]) != "go" || words[i+1] != "test" {
			continue
		}
		var pkgs []string
		chdir, listed, inList := "", false, false
	args:
		for j := i + 2; j < len(words) && !isShellOperator(words[j]); j++ {
			w := words[j]
			if w == "--" {
				break
			}
			if !strings.HasPrefix(w, "-") || w == "-" {
				if listed && !inList {
					break // the test binary's
				}
				listed, inList = true, true
				pkgs = append(pkgs, w)
				continue
			}
			inList = false
			name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(w, "-"), "-"), "=")
			name = strings.TrimPrefix(name, "test.")
			switch {
			case name == "args":
				break args
			case goValueFlags[name]:
				if !hasValue && j+1 < len(words) && !isShellOperator(words[j+1]) {
					j++
					value = words[j]
				}
				if name == "C" {
					chdir = value
				}
			case goBoolFlags[name]:
			default:
				listed = true // an unknown flag: the package list, if any, is complete
			}
		}
		for _, p := range pkgs {
			if !strings.Contains(p, substitution) && !strings.Contains(chdir, substitution) {
				targets = append(targets, goTarget{dir: chdir, pkg: p})
			}
		}
	}
	return targets
}

// goTarget is a package a `go test` names, as written, and the -C folder it is named from.
type goTarget struct{ dir, pkg string }

// namedGoPackage reports whether a `go test` of the verify commands names folder dir (a slash path from the repository's
// root) by a relative path from the module's folder, or from the folder its -C flag names ("./testdata/p"): the go tool
// runs a folder its wildcards skip only when it is named so (even "./testdata/..." matches nothing). Import paths and a
// `cd` in the command are not read.
func namedGoPackage(verify []string, module, dir string) bool {
	for _, command := range verify {
		for _, t := range goTestTargets(command) {
			w := t.pkg
			if path.IsAbs(t.dir) || !(w == "." || w == ".." || strings.HasPrefix(w, "./") || strings.HasPrefix(w, "../")) {
				continue
			}
			if path.Join(module, t.dir, w) == path.Clean(dir) {
				return true
			}
		}
	}
	return false
}

// hasSkippedElement reports whether a slash path has a file or folder name the go tool's wildcards skip: testdata, or
// one that starts with "_" or ".".
func hasSkippedElement(p string) bool {
	for _, el := range strings.Split(path.Clean(p), "/") {
		if el != "." && el != ".." && (el == "testdata" || strings.HasPrefix(el, "_") || strings.HasPrefix(el, ".")) {
			return true
		}
	}
	return false
}

// known is a pattern, or "" when a substitution makes it unknown here (shellWords marks one with substitution).
func known(pattern string) string {
	if strings.Contains(pattern, substitution) {
		return ""
	}
	return pattern
}

// flagValue reads flag name at words[i] in any of `go test`'s spellings (-name, --name, -test.name; the value after
// "=" or in the next word) and returns its value and the index of the last word it used.
func flagValue(words []string, i int, name string) (value string, last int, found bool) {
	w := words[i]
	for _, prefix := range []string{"-" + name, "--" + name, "-test." + name, "--test." + name} {
		switch {
		case w == prefix && i+1 < len(words) && !isShellOperator(words[i+1]):
			return words[i+1], i + 1, true
		case strings.HasPrefix(w, prefix+"="):
			return strings.TrimPrefix(w, prefix+"="), i, true
		}
	}
	return "", i, false
}

// shellOperators separate commands; shellWords returns each as a word of its own.
var shellOperators = []string{"&&", "||", ";;", ";", "|", "&", "(", ")", "\n"}

func isShellOperator(w string) bool { return slices.Contains(shellOperators, w) }

// substitution marks where a word has a parameter, command or arithmetic substitution, whose value is not known.
const substitution = "\x00"

// startsSubstitution reports whether a "$" outside single quotes, followed by s, begins a substitution; otherwise it
// is a literal "$" (as at a word's end, where a test pattern's anchor sits).
func startsSubstitution(s string) bool {
	return s != "" && (s[0] == '{' || s[0] == '(' || s[0] == '_' || unicode.IsLetter(rune(s[0])) || strings.IndexByte("0123456789@*#?$!-", s[0]) >= 0)
}

// shellWords splits a POSIX shell command into words, with quotes removed and backslash escapes applied; operators
// outside quotes are words of their own. It expands nothing: a substitution is marked (substitution) in its word. ok
// is false for an unterminated quote.
func shellWords(command string) (words []string, ok bool) {
	var word strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == '\'':
			end := strings.IndexByte(command[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			word.WriteString(command[i+1 : i+1+end])
			inWord, i = true, i+1+end
		case c == '"':
			j := i + 1
			for ; j < len(command) && command[j] != '"'; j++ {
				switch {
				case command[j] == '\\' && j+1 < len(command) && strings.IndexByte("\"\\$`\n", command[j+1]) >= 0:
					j++
				case command[j] == '`' || command[j] == '$' && startsSubstitution(command[j+1:]):
					word.WriteString(substitution)
				}
				word.WriteByte(command[j])
			}
			if j >= len(command) {
				return nil, false
			}
			inWord, i = true, j
		case c == '\\' && i+1 < len(command):
			if command[i+1] != '\n' {
				word.WriteByte(command[i+1])
				inWord = true
			}
			i++
		case c == '#' && !inWord:
			end := strings.IndexByte(command[i:], '\n')
			if end < 0 {
				return words, true
			}
			i += end - 1
		case c == ' ' || c == '\t':
			flush()
		default:
			if op := operatorAt(command[i:]); op != "" {
				flush()
				words = append(words, op)
				i += len(op) - 1
				continue
			}
			if c == '`' || c == '$' && startsSubstitution(command[i+1:]) {
				word.WriteString(substitution)
			}
			word.WriteByte(c)
			inWord = true
		}
	}
	flush()
	return words, true
}

// operatorAt is the shell operator s starts with, or "".
func operatorAt(s string) string {
	for _, op := range shellOperators {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	return ""
}

// ownGoTests lists, sorted, the top-level test functions (Test, Fuzz and Example) of the hidden _test.go files that
// the solution adds, or changes from the base's version. A file that does not parse is skipped.
func ownGoTests(hiddenTests []string, base, solution source.Source) []string {
	var names []string
	tests, _ := hiddenGoTests(hiddenTests, base, solution)
	for _, t := range tests {
		if t.own {
			names = append(names, t.name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// goTest is a top-level test function of a hidden _test.go file: its name, the file's folder (a slash path from the
// repository's root, "." for the root), and whether it is the task's own (the solution adds it or changes it from the
// base's version).
type goTest struct {
	dir, name string
	own       bool
}

// hiddenGoTests lists the top-level test functions `go test` runs (goTestFuncs) of the hidden _test.go files as the
// solution has them, each with its folder. A file the solution removes, or that does not parse, is skipped; parsed
// counts the files read.
func hiddenGoTests(hiddenTests []string, base, solution source.Source) (tests []goTest, parsed int) {
	if solution == nil {
		return nil, 0
	}
	for _, p := range hiddenTests {
		if !strings.HasSuffix(p, "_test.go") {
			continue
		}
		after, err := solution.ReadFile(p)
		if err != nil {
			continue // removed by the solution
		}
		now, ok := goTestFuncs(after)
		if !ok {
			continue
		}
		parsed++
		var before map[string]string
		if base != nil {
			if src, err := base.ReadFile(p); err == nil {
				before, _ = goTestFuncs(src)
			}
		}
		for name, body := range now {
			old, had := before[name]
			tests = append(tests, goTest{dir: path.Dir(p), name: name, own: !had || old != body})
		}
	}
	return tests, parsed
}

// goTestFuncs maps a Go test file's top-level test functions that `go test` runs (TestMain and examples without an
// output comment are not) to their source text. Whether an example runs is Go's own rule (go/doc.Examples): an output
// comment ("// Output:", "// Unordered output:") that ends its body, never a line that only looks like one (in a string,
// say).
func goTestFuncs(src []byte) (map[string]string, bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x_test.go", src, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		return nil, false
	}
	runnable := map[string]bool{}
	for _, ex := range doc.Examples(file) {
		if ex.Output != "" || ex.EmptyOutput {
			runnable["Example"+ex.Name] = true
		}
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !isGoTestName(fn.Name.Name) || fn.Name.Name == "TestMain" {
			continue
		}
		if strings.HasPrefix(fn.Name.Name, "Example") && !runnable[fn.Name.Name] {
			continue // compiled, never run
		}
		out[fn.Name.Name] = string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
	}
	return out, true
}

// isGoTestName reports whether name is one `go test` runs with -run: Test, Fuzz or Example, alone or followed by a
// character that is not a lowercase letter.
func isGoTestName(name string) bool {
	for _, prefix := range []string{"Test", "Fuzz", "Example"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			if rest == "" {
				return true
			}
			r, _ := utf8.DecodeRuneInString(rest)
			return !unicode.IsLower(r)
		}
	}
	return false
}
