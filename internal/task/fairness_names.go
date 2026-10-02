package task

import (
	"context"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// minName is the length under which a Java, Kotlin, Rust, Python or TypeScript name is too short to be worth a word search.
const minName = 4

var (
	importLine = regexp.MustCompile(`(?m)^[ \t]*(?:import|package)\b.*$`) // "import static a.B.C;" is not a declaration
	nameToken  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	// Declarations, matched on code with comments, strings and characters blanked out. The name is the last group.
	javaDecl = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:class|interface|enum|record)\s+(\w+)`),
		// a member written with a modifier: the last word before "(", "=" or ";" is its name
		regexp.MustCompile(`\b(?:public|protected|private|static|final|abstract|synchronized|native)\b[^;={()]*?\b(\w+)\s*[(=;]`),
		regexp.MustCompile(`\bdefault\s+[\w<>\[\]]+\s+(\w+)\s*\(`), // an interface's default method (not a switch's default:)
		regexp.MustCompile(`\b(\w+)\s*\(\s*\)\s*default\b`),        // an annotation element: int jitterSeed() default 0;
		javaAbstract,
	}
	// javaAbstract matches a method without a body or modifier ("long delay(int x);", in an interface); group 1 is the
	// words before the name, which must not be a statement keyword.
	javaAbstract = regexp.MustCompile(`(?m)^[ \t]*((?:[\w<>\[\],.?]+[ \t]+)+)(\w+)[ \t]*\([^;{}()]*\)[ \t]*(?:throws[^;{}]*)?;`)
	notAType     = regexp.MustCompile(`\b(?:return|new|throw|else|yield|assert|case|goto)\b`)
	kotlinDecl   = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:class|interface|object|fun|val|var|typealias)\s+(?:<[^>]*>\s*)?(?:\w+\.)?(\w+)`),
	}
	rustDecl = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:fn|struct|enum|trait|type|mod|union|const|static)\s+(?:(?:fn|mut|unsafe)\s+)?(\w+)`),
		regexp.MustCompile(`\bmacro_rules!\s*(\w+)`),
		regexp.MustCompile(`\bpub(?:\([^)]*\))?\s+(\w+)\s*:`), // a public field
	}
	// overridden is the text before a declaration that marks it an override of a standard or trait method.
	overridden    = regexp.MustCompile(`(?:@Override|\boverride)\b[\w\s]*$`)
	rustTraitImpl = regexp.MustCompile(`\bimpl\b[^{;]*\bfor\b[^{;]*\{`)
	// Names a test binds locally: they can only hide a gap, never create one.
	localDecl = map[lang][]*regexp.Regexp{
		langJava:   {regexp.MustCompile(`\b[A-Za-z_][\w.]*(?:<[^<>;()]*>)?(?:\[\])*[ \t]+(\w+)[ \t]*[=;,)]`)},
		langKotlin: {regexp.MustCompile(`\b(\w+)\s*:(?:[^:]|$)`), regexp.MustCompile(`\b(\w+)\s*->`)},
		langRust:   {regexp.MustCompile(`\blet\s+(?:mut\s+)?(\w+)`), regexp.MustCompile(`\b(\w+)\s*:(?:[^:]|$)`)},
	}
)

// jsFamily is the TypeScript and JavaScript family: a TypeScript test imports names from JavaScript files and back.
var jsFamily = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// nameFamily groups the files whose names can refer to each other: Java and Kotlin share the JVM; Rust and Python
// stand alone; TypeScript and JavaScript share one family.
// It returns the language of the extension, the extensions of its family and whether the family is supported.
func nameFamily(ext string) (l lang, exts []string, ok bool) {
	switch ext {
	case ".java":
		return langJava, []string{".java", ".kt"}, true
	case ".kt":
		return langKotlin, []string{".java", ".kt"}, true
	case ".rs":
		return langRust, []string{".rs"}, true
	case ".py":
		return langPython, []string{".py"}, true
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return langTS, jsFamily, true
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

// declaredNames lists the names a Java, Kotlin or Rust source declares: types, functions, methods, constants,
// annotation elements and fields. With skipOverrides it leaves out overrides (@Override, Kotlin override, methods in a
// Rust "impl Trait for Type" block), which name standard or trait methods; without it it adds locals and parameters
// too, for a test file's own names. It is a pattern match, not a parser, so it misses enum constants and the like; a
// name it misses is never flagged.
func declaredNames(src string, l lang, skipOverrides bool) map[string]bool {
	if l == langPython || l == langTS {
		return scriptNames(importLine.ReplaceAllString(codeOnly(src, l), ""), l, skipOverrides)
	}
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
	var skip [][2]int // Rust trait impl bodies
	if skipOverrides && l == langRust {
		for _, m := range rustTraitImpl.FindAllStringIndex(code, -1) {
			depth, j := 1, m[1]
			for ; j < len(code) && depth > 0; j++ {
				switch code[j] {
				case '{':
					depth++
				case '}':
					depth--
				}
			}
			skip = append(skip, [2]int{m[0], j})
		}
	}
	out := map[string]bool{}
	for _, re := range res {
		for _, m := range re.FindAllStringSubmatchIndex(code, -1) {
			n := len(m)/2 - 1
			if re == javaAbstract && notAType.MatchString(code[m[2]:m[3]]) {
				continue
			}
			if skipOverrides {
				if before := code[max(0, m[0]-60):m[0]]; overridden.MatchString(before) {
					continue
				}
				if slices.ContainsFunc(skip, func(r [2]int) bool { return m[0] >= r[0] && m[0] < r[1] }) {
					continue
				}
			}
			out[code[m[2*n]:m[2*n+1]]] = true
		}
	}
	if !skipOverrides {
		for _, re := range localDecl[l] {
			for _, m := range re.FindAllStringSubmatch(code, -1) {
				out[m[1]] = true
			}
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
		for n := range declaredNames(string(data), l, true) {
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
	own := declaredNames(string(after), l, false)
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
