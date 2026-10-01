package task

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// InlineRustTests lists the Rust source files (reference files ending in .rs) in which the solution adds, removes or
// changes test code: an item marked #[cfg(test)] (usually mod tests { ... }) or a function marked #[test] (or a
// #[tokio::test]-style attribute). Such tests sit beside the code in one file, which Split puts in the reference, so
// the agent would see the hidden tests or the solution would lose them. Doc tests (``` blocks in /// comments) are not
// detected. Files that cannot be scanned reliably (unbalanced
// braces) are listed too, with the reason. The caller refuses the task.
func InlineRustTests(ctx context.Context, base, solution string, reference []string, where ...string) ([]string, error) {
	var files []string
	var sol, bs source.Source
	for _, p := range reference {
		if path.Ext(p) != ".rs" {
			continue
		}
		if sol == nil {
			var err error
			if sol, err = source.Commit(ctx, solution, where...); err != nil {
				return nil, fmt.Errorf("rust tests: %w", err)
			}
			if bs, err = source.Commit(ctx, base, where...); err != nil {
				return nil, fmt.Errorf("rust tests: %w", err)
			}
		}
		read := func(s source.Source) (string, error) {
			if !source.Has(s, p) {
				return "", nil
			}
			b, err := s.ReadFile(p)
			return string(b), err
		}
		after, err := read(sol)
		if err != nil {
			return nil, fmt.Errorf("rust tests: %w", err)
		}
		before, err := read(bs)
		if err != nil {
			return nil, fmt.Errorf("rust tests: %w", err)
		}
		a, errA := rustTestItems(after)
		b, errB := rustTestItems(before)
		switch {
		case errA != nil:
			files = append(files, p+" ("+errA.Error()+")")
		case errB != nil:
			files = append(files, p+" (base: "+errB.Error()+")")
		case !slices.Equal(a, b):
			files = append(files, p)
		}
	}
	return files, nil
}

// rustTestItems returns the source text of each test item in a Rust file, in order: an item preceded by #[cfg(test)]
// (also cfg(all(test, ...)); cfg(not(test)) is ordinary code), #[test] or a path ending in ::test (#[tokio::test]).
// The item runs from its first attribute to its closing brace, or to the semicolon of a braceless item. A leading
// #![cfg(test)] makes the whole file one test item. Comparing these lists before and after finds a changed test
// without a diff. Limits: macros that generate tests without a visible attribute are not seen, and neither are doc tests (``` blocks
// in /// comments), which run as tests but live in comments.
func rustTestItems(src string) ([]string, error) {
	var items []string
	for i := 0; i < len(src); {
		if end, _, _, kind := lexAt(src, i, langRust); kind != tokNone {
			i = max(end, i+1)
			continue
		}
		if src[i] != '#' {
			i++
			continue
		}
		inner := i+1 < len(src) && src[i+1] == '!'
		open := i + 1
		if inner {
			open++
		}
		if open >= len(src) || src[open] != '[' {
			i++
			continue
		}
		attrEnd, err := matchClose(src, open)
		if err != nil {
			return nil, err
		}
		if !isTestAttr(src[open+1 : attrEnd]) {
			i = attrEnd + 1
			continue
		}
		if inner { // #![cfg(test)] marks the enclosing module, in a file's head the file
			items = append(items, src)
			return items, nil
		}
		end, err := itemEnd(src, attrEnd+1)
		if err != nil {
			return nil, err
		}
		items = append(items, src[i:end])
		i = end
	}
	return items, nil
}

// lineOf is the 1-based line of offset i.
func lineOf(src string, i int) int { return 1 + strings.Count(src[:min(i, len(src))], "\n") }

// isTestAttr reports whether the text inside #[...] marks test code: a test attribute by name (test, tokio::test,
// wasm_bindgen_test or any *_test, test_case, rstest) or a cfg that requires the word test (cfg(test),
// cfg(all(test, not(miri)))). Quoted strings (feature = "test-util") and not(...) groups are ignored.
func isTestAttr(attr string) bool {
	attr = strings.TrimSpace(attr)
	name, _, _ := strings.Cut(attr, "(")
	name, _, _ = strings.Cut(name, "=")
	name = strings.TrimSpace(name)
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = strings.TrimSpace(name[i+2:])
	}
	switch {
	case name == "test", name == "test_case", name == "rstest", strings.HasSuffix(name, "_test"):
		return true
	case name != "cfg":
		return false
	}
	var plain strings.Builder // the attribute without its strings
	for i := 0; i < len(attr); {
		if end, _, _, kind := lexAt(attr, i, langRust); kind == tokString || kind == tokRaw {
			i = max(end, i+1)
			plain.WriteByte(' ')
			continue
		}
		plain.WriteByte(attr[i])
		i++
	}
	text := plain.String()
	for { // drop not(...) groups, innermost or not, by depth
		i := strings.Index(text, "not(")
		for i > 0 && isIdentByte(text[i-1]) { // all_not( is not a negation
			j := strings.Index(text[i+1:], "not(")
			if j < 0 {
				i = -1
				break
			}
			i += 1 + j
		}
		if i < 0 {
			break
		}
		depth, j := 0, i+3
		for ; j < len(text); j++ {
			if text[j] == '(' {
				depth++
			} else if text[j] == ')' {
				if depth--; depth == 0 {
					break
				}
			}
		}
		text = text[:i] + " " + text[min(j+1, len(text)):]
	}
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return r < 128 && !isIdentByte(byte(r)) }) {
		if w == "test" {
			return true
		}
	}
	return false
}

// itemKeywords start an item whose head may contain commas (generics, where clauses) before its body.
var itemKeywords = map[string]bool{"fn": true, "mod": true, "impl": true, "struct": true, "enum": true, "trait": true, "use": true,
	"const": true, "static": true, "type": true, "async": true, "unsafe": true, "extern": true, "union": true, "macro_rules": true}

// headIsItem reports whether the thing starting at from (after its test attribute) is an item rather than a struct
// field, an enum variant or a match arm, which a comma ends: further attributes and a visibility are skipped, then
// the first word is looked up.
func headIsItem(src string, from int) bool {
	i := from
	for i < len(src) {
		switch {
		case src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r':
			i++
		case strings.HasPrefix(src[i:], "#["):
			e, err := matchClose(src, i+1)
			if err != nil {
				return true
			}
			i = e + 1
		case strings.HasPrefix(src[i:], "//") || strings.HasPrefix(src[i:], "/*"):
			end, _, _, _ := lexAt(src, i, langRust)
			i = max(end, i+1)
		default:
			j := i
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			word := src[i:j]
			if word == "pub" {
				i = j
				for i < len(src) && src[i] == ' ' {
					i++
				}
				if i < len(src) && src[i] == '(' {
					e, err := matchClose(src, i)
					if err != nil {
						return true
					}
					i = e + 1
				}
				continue
			}
			return itemKeywords[word]
		}
	}
	return true
}

// itemEnd finds where the item that starts at from (after its test attribute) ends: further attributes and the item's
// head are scanned until a { (matched to its }) or a ; outside parentheses and brackets. A field, variant or match
// arm (not an item) also ends at its comma, and anything ends where the enclosing block closes, so a #[cfg(test)]
// field or arm does not swallow the code after it.
func itemEnd(src string, from int) (int, error) {
	depth := 0
	item := headIsItem(src, from)
	for i := from; i < len(src); {
		if end, _, _, kind := lexAt(src, i, langRust); kind != tokNone {
			i = max(end, i+1)
			continue
		}
		switch src[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ';':
			if depth <= 0 {
				return i + 1, nil
			}
		case ',':
			if depth <= 0 && !item {
				return i + 1, nil
			}
		case '}':
			if depth <= 0 {
				return i, nil
			}
		case '{':
			if depth <= 0 {
				e, err := matchClose(src, i)
				return e + 1, err
			}
		}
		i++
	}
	return 0, errors.New("a test item has no end")
}

// matchClose returns the offset of the bracket that closes the one at src[open], skipping comments, strings, raw
// strings and character literals (a lifetime is not one).
func matchClose(src string, open int) (int, error) {
	pairs := map[byte]byte{'(': ')', '[': ']', '{': '}'}
	var stack []byte
	for i := open; i < len(src); {
		if end, _, _, kind := lexAt(src, i, langRust); kind != tokNone {
			i = max(end, i+1)
			continue
		}
		switch c := src[i]; c {
		case '(', '[', '{':
			stack = append(stack, pairs[c])
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return 0, fmt.Errorf("unbalanced %q on line %d", c, lineOf(src, i))
			}
			if stack = stack[:len(stack)-1]; len(stack) == 0 {
				return i, nil
			}
		}
		i++
	}
	return 0, fmt.Errorf("unbalanced brackets: %q opened on line %d is never closed", src[open], lineOf(src, open))
}

// RefuseInlineRustTests returns the error the task commands give for a solution that changes inline Rust tests, or
// nil. where locates the repository holding both commits (for example "--git-dir", bare).
func RefuseInlineRustTests(ctx context.Context, base, solution string, reference []string, where ...string) error {
	files, err := InlineRustTests(ctx, base, solution, reference, where...)
	if err != nil || len(files) == 0 {
		return err
	}
	return fmt.Errorf("the solution changes Rust tests inside source files (%s): #[cfg(test)] and #[test] code would land in the "+
		"reference solution, so the hidden tests could not be kept from the agent or kept in the grading; move them to a "+
		"file under tests/ or choose another solution", strings.Join(files, ", "))
}
