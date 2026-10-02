package task

import (
	"fmt"
	"go/ast"
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

// goTestFilter is the -run and -skip patterns of one `go test` invocation in a verify command; "" when not given.
type goTestFilter struct {
	command   string
	run, skip string
}

// FilteredHiddenTests warns, once per verify command, of the hidden Go tests (top-level Test, Fuzz and Example
// functions the solution adds or changes in its hidden _test.go files) that the command's `go test` -skip pattern
// matches or its -run pattern does not. Only the first element of a pattern (before an unbracketed "/") is checked:
// a -skip pattern with more elements skips subtests only, and one with a run pattern's later elements narrows the
// subtests a matching test runs. Commands it cannot read (a pattern from a shell variable, one that does not
// compile) are left alone.
func FilteredHiddenTests(verify, hiddenTests []string, base, solution source.Source) []string {
	var filters []goTestFilter
	for _, command := range verify {
		filters = append(filters, goTestFilters(command)...)
	}
	if len(filters) == 0 || solution == nil {
		return nil
	}
	names := ownGoTests(hiddenTests, base, solution)
	if len(names) == 0 {
		return nil
	}
	var warnings []string
	for _, f := range filters {
		if f.skip != "" && len(splitPattern(f.skip)) == 1 { // with more elements, it skips subtests only
			if re := firstElement(f.skip); re != nil {
				if hit := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !re.MatchString(n) }); len(hit) > 0 {
					warnings = append(warnings, fmt.Sprintf("the verify command `%s` skips hidden test(s) %s: its -skip pattern %q matches them, so grading never runs them",
						f.command, strings.Join(hit, ", "), f.skip))
				}
			}
		}
		if f.run != "" {
			if re := firstElement(f.run); re != nil {
				if miss := slices.DeleteFunc(slices.Clone(names), re.MatchString); len(miss) > 0 {
					warnings = append(warnings, fmt.Sprintf("the verify command `%s` leaves out hidden test(s) %s: its -run pattern %q does not match them, so grading never runs them",
						f.command, strings.Join(miss, ", "), f.run))
				}
			}
		}
	}
	return warnings
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

// goTestFilters reads the `go test` invocations of a shell command and their -run and -skip patterns (the last one of
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
		if f.run != "" || f.skip != "" {
			out = append(out, f)
		}
	}
	return out
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
		var before map[string]string
		if base != nil {
			if src, err := base.ReadFile(p); err == nil {
				before, _ = goTestFuncs(src)
			}
		}
		for name, body := range now {
			if old, had := before[name]; !had || old != body {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// goTestFuncs maps a Go test file's top-level test functions to their source text.
func goTestFuncs(src []byte) (map[string]string, bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x_test.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, false
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !isGoTestName(fn.Name.Name) {
			continue
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
