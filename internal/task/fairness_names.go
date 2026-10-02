package task

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// minName is the length under which a Java, Kotlin or Rust name is too short to be worth a word search.
const minName = 4

var (
	importLine = regexp.MustCompile(`(?m)^[ \t]*(?:import|package)\b.*$`) // "import static a.B.C;" is not a declaration
	nameToken  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	// Declarations, matched on code with comments, strings and characters blanked out.
	javaDecl = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:class|interface|enum|record)\s+(\w+)`),
		// a member written with a modifier: the last word before "(", "=" or ";" is its name
		regexp.MustCompile(`\b(?:public|protected|private|static|final|abstract|default|synchronized|native)\b[^;={()]*?\b(\w+)\s*[(=;]`),
	}
	kotlinDecl = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:class|interface|object|fun|val|var|typealias)\s+(?:<[^>]*>\s*)?(?:\w+\.)?(\w+)`),
	}
	rustDecl = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:fn|struct|enum|trait|type|mod|union|const|static)\s+(?:(?:fn|mut|unsafe)\s+)?(\w+)`),
		regexp.MustCompile(`\bmacro_rules!\s*(\w+)`),
		regexp.MustCompile(`\bpub(?:\([^)]*\))?\s+(\w+)\s*:`), // a public field
	}
)

// nameFamily groups the files whose names can refer to each other: Java and Kotlin share the JVM; Rust stands alone.
// It returns the language of the extension, the extensions of its family and whether the family is supported.
func nameFamily(ext string) (l lang, exts []string, ok bool) {
	switch ext {
	case ".java":
		return langJava, []string{".java", ".kt"}, true
	case ".kt":
		return langKotlin, []string{".java", ".kt"}, true
	case ".rs":
		return langRust, []string{".rs"}, true
	}
	return 0, nil, false
}

// codeOnly blanks comments, string literals and character literals so that names in them are not read as code.
func codeOnly(src string, l lang) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		end, _, _, kind := lexAt(src, i, l)
		if end <= i {
			b.WriteByte(src[i])
			i++
			continue
		}
		if kind == tokNone {
			b.WriteString(src[i:end])
		} else {
			b.WriteString(` "" `)
		}
		i = end
	}
	return b.String()
}

// declaredNames lists the names a Java, Kotlin or Rust source declares: types, functions, methods, constants and
// fields. It is a pattern match, not a parser, so it misses interface methods without a modifier, enum constants and
// the like; a name it misses is never flagged.
func declaredNames(src string, l lang) map[string]bool {
	var res []*regexp.Regexp
	switch l {
	case langJava:
		res = javaDecl
	case langKotlin:
		res = kotlinDecl
	default:
		res = rustDecl
	}
	code := importLine.ReplaceAllString(codeOnly(src, l), "")
	out := map[string]bool{}
	for _, re := range res {
		for _, m := range re.FindAllStringSubmatch(code, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// usedNames lists every name that appears in the code of a source file, imports included.
func usedNames(src string, l lang) map[string]bool {
	out := map[string]bool{}
	for _, n := range nameToken.FindAllString(codeOnly(src, l), -1) {
		out[n] = true
	}
	return out
}

// referenceNames collects the names the solution's non-test Java, Kotlin or Rust files declare, by family. Only these
// can be flagged, so names from the JDK, the standard library and dependencies never are.
func (f *Fairness) referenceNames(ctx context.Context, in FairnessInput) (map[string]map[string]bool, error) {
	sol, err := f.source(ctx, in.Solution)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]bool{}
	for _, p := range in.Reference {
		l, exts, ok := nameFamily(path.Ext(p))
		if !ok || !source.Has(sol, p) {
			continue
		}
		data, err := sol.ReadFile(p)
		if err != nil {
			continue
		}
		key := strings.Join(exts, ",")
		if out[key] == nil {
			out[key] = map[string]bool{}
		}
		for n := range declaredNames(string(data), l) {
			out[key][n] = true
		}
	}
	return out, nil
}

// unstatedNames returns, sorted, the names a Java, Kotlin or Rust hidden test file newly uses that the reference
// declares, that the test does not declare itself, that the instruction does not mention and that no base file of the
// same language family contains as a word. It reports nothing for other languages.
func (f *Fairness) unstatedNames(ctx context.Context, in FairnessInput, file string, after, before []byte, stated string, ref map[string]map[string]bool) ([]string, error) {
	l, exts, ok := nameFamily(path.Ext(file))
	if !ok {
		return nil, nil
	}
	declared := ref[strings.Join(exts, ",")]
	if len(declared) == 0 {
		return nil, nil
	}
	own := declaredNames(string(after), l)
	old := usedNames(string(before), l)
	var specs []string
	for _, e := range exts {
		specs = append(specs, ":(glob)**/*"+e)
	}
	var out []string
	for name := range usedNames(string(after), l) {
		if len(name) < minName || !declared[name] || own[name] || old[name] || wordIn(stated, name) {
			continue
		}
		inBase, err := f.grep(ctx, in.Base, name, true, specs)
		if err != nil {
			return nil, err
		}
		if !inBase {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
