package task

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// InlineRustTests lists the Rust source files (reference files ending in .rs) in which the solution adds, removes or
// changes test code: an item marked #[cfg(test)] (usually mod tests { ... }) or a function marked #[test] (or a
// #[tokio::test]-style attribute). Such tests sit beside the code in one file, which Split puts in the reference, so
// the agent would see the hidden tests or the solution would lose them. Files that cannot be scanned reliably (unbalanced
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
// without a diff. Macros that generate tests without a visible #[test] cannot be seen.
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

// isTestAttr reports whether the text inside #[...] marks test code.
func isTestAttr(attr string) bool {
	attr = strings.Join(strings.Fields(attr), "")
	switch {
	case attr == "test", strings.HasSuffix(attr, "::test"):
		return true
	case strings.HasPrefix(attr, "cfg(") && !strings.Contains(attr, "not("):
		for _, w := range strings.FieldsFunc(attr, func(r rune) bool {
			return !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
		}) {
			if w == "test" {
				return true
			}
		}
	}
	return false
}

// itemEnd finds where the item that starts at from (after its test attribute) ends: further attributes and the item's
// head are scanned until a { (matched to its }) or a ; outside parentheses and brackets.
func itemEnd(src string, from int) (int, error) {
	depth := 0
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
		case '{':
			if depth <= 0 {
				e, err := matchClose(src, i)
				return e + 1, err
			}
		}
		i++
	}
	return 0, fmt.Errorf("a test item has no end")
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
				return 0, fmt.Errorf("unbalanced %q at offset %d", c, i)
			}
			if stack = stack[:len(stack)-1]; len(stack) == 0 {
				return i, nil
			}
		}
		i++
	}
	return 0, fmt.Errorf("unbalanced brackets: %q is never closed", src[open])
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
