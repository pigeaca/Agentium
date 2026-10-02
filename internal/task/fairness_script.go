package task

import (
	"regexp"
	"strings"
)

// Declaration patterns for Python and for TypeScript or JavaScript, matched on code with comments and strings
// blanked. Like the JVM and Rust patterns they are a pattern match, not a parser.
var (
	pyDef = regexp.MustCompile(`\b(?:def|class)\s+(\w+)`)
	// a module-level assignment (NAME = ..., NAME: T = ...); a class-level or annotated one is pyField
	pyModuleAssign = regexp.MustCompile(`(?m)^(\w+)[ \t]*(?::[^=\n]+)?=(?:[^=]|$)`)
	// an annotated name in any body (a dataclass or NamedTuple field, "retries: int = 3" or "retries: int")
	pyField = regexp.MustCompile(`(?m)^[ \t]+(\w+)[ \t]*:[ \t]*[^=\s][^=\n]*(?:=[^=]|$)`)
	// names a test binds locally (hide only): assignment targets, for targets, "as" names, walrus, parameters
	pyLocal = []*regexp.Regexp{
		regexp.MustCompile(`(?m)^[ \t]*(\w+(?:[ \t]*,[ \t]*\w+)*)[ \t]*(?::[^=\n]+)?[-+*/|&@%]?=(?:[^=]|$)`),
		regexp.MustCompile(`\bfor\s+([\w, \t()]+?)\s+in\b`),
		regexp.MustCompile(`\bas\s+(\w+)`),
		regexp.MustCompile(`\b(\w+)\s*:=`),
		regexp.MustCompile(`\bdef\s+\w+\s*\(([^()]*)\)`),
		regexp.MustCompile(`\blambda\b([^:]*):`),
	}

	tsDecl = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:function\s*\*?|class|interface|type|enum|namespace|module)\s+(\w+)`),
		// exported or top-level variables; locals inside functions are not declarations of the module
		regexp.MustCompile(`(?m)^(?:export\s+)?(?:declare\s+)?(?:default\s+)?(?:const|let|var)\s+(\w+)`),
		regexp.MustCompile(`\bexport\s+(?:declare\s+)?(?:default\s+)?(?:const|let|var)\s+(\w+)`),
		// a member written with a modifier
		regexp.MustCompile(`(?m)^[ \t]+(?:(?:public|private|protected|static|readonly|abstract|async|override|declare|get|set)\s+)+\*?(\w+)`),
		// a method with a body, an interface member with a type
		tsMethod,
		regexp.MustCompile(`(?m)^[ \t]+(\w+)\??[ \t]*:[ \t]*[^;\n]+;`),
	}
	tsMethod  = regexp.MustCompile(`(?m)^[ \t]+(\w+)[ \t]*\??[ \t]*(?:<[^>\n]*>)?\([^;\n]*\)[ \t]*(?::[^;{\n]+)?\{`)
	tsKeyword = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "return": true, "function": true,
		"constructor": true, "super": true, "with": true, "do": true, "else": true, "await": true, "default": true, "case": true, "break": true, "continue": true, "throw": true, "yield": true, "typeof": true, "new": true}
	tsExportList = regexp.MustCompile(`\bexport\s*\{([^}]*)\}`)
	tsLocal      = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:const|let|var)\s+(\w+)`),
		regexp.MustCompile(`\b(?:const|let|var)\s*[{\[]([^}\]]*)[}\]]`),
		regexp.MustCompile(`\bfunction\s*\*?\s*\w*\s*(?:<[^>]*>)?\(([^()]*)\)`),
		regexp.MustCompile(`\(([^()]*)\)\s*(?::\s*[^=;{]+?)?\s*=>`),
		regexp.MustCompile(`\b(\w+)\s*=>`),
		regexp.MustCompile(`\bcatch\s*\(\s*(\w+)`),
	}
	// overridden TypeScript members name standard methods, not the reference's own vocabulary.
	tsStandard = map[string]bool{"toString": true, "valueOf": true, "toJSON": true, "constructor": true}
)

// scriptNames lists the names a Python or TypeScript or JavaScript source declares (see declaredNames). With
// skipOverrides it keeps what a reference declares and leaves out Python dunder methods and standard TypeScript
// methods; without it it adds locals and parameters, for a test file's own names.
func scriptNames(code string, l lang, skipOverrides bool) map[string]bool {
	out := map[string]bool{}
	add := func(n string) {
		if n == "" || skipOverrides && (strings.HasPrefix(n, "__") && strings.HasSuffix(n, "__") || tsStandard[n]) {
			return
		}
		out[n] = true
	}
	addList := func(list string) {
		for _, n := range nameToken.FindAllString(list, -1) {
			add(n)
		}
	}
	if l == langPython {
		for _, re := range []*regexp.Regexp{pyDef, pyModuleAssign, pyField} {
			for _, m := range re.FindAllStringSubmatch(code, -1) {
				add(m[1])
			}
		}
		if !skipOverrides {
			for _, re := range pyLocal {
				for _, m := range re.FindAllStringSubmatch(code, -1) {
					addList(paramNames(m[1], l))
				}
			}
		}
		return out
	}
	for _, re := range tsDecl {
		for _, m := range re.FindAllStringSubmatch(code, -1) {
			if !tsKeyword[m[1]] {
				add(m[1])
			}
		}
	}
	for _, m := range tsExportList.FindAllStringSubmatch(code, -1) {
		for _, part := range strings.Split(m[1], ",") {
			f := strings.Fields(part)
			if len(f) > 0 {
				add(f[len(f)-1]) // "a as b" exports b
			}
		}
	}
	if !skipOverrides {
		for _, re := range tsLocal {
			for _, m := range re.FindAllStringSubmatch(code, -1) {
				addList(paramNames(m[1], l))
			}
		}
	}
	return out
}

// paramNames keeps, as one string, the identifiers of a parameter or target list that are bound names and not type
// annotations or default values: the part of each comma-separated item before its ":" or "=". A destructured
// TypeScript parameter keeps every identifier in it (it can only hide a name, never flag one).
func paramNames(list string, l lang) string {
	var keep []string
	depth, start := 0, 0
	flush := func(end int) {
		part := list[start:end]
		if l == langTS && strings.ContainsAny(part, "{[") {
			keep = append(keep, part)
			return
		}
		if i := strings.IndexAny(part, ":="); i >= 0 {
			part = part[:i]
		}
		keep = append(keep, part)
	}
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}', '>':
			depth--
		case ',':
			if depth <= 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(list))
	return strings.Join(keep, " ")
}
