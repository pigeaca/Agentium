package task

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// vitestInSource is the guard of a Vitest in-source test block: if (import.meta.vitest) { ... }.
const vitestInSource = "import.meta.vitest"

// InlineTests lists the reference files in which the solution adds, removes or changes tests that sit inside
// ordinary source files, by kind: Rust (#[cfg(test)], #[test]; see InlineRustTests), Python doctests (">>>" examples in
// the strings of a non-test .py file) and Vitest in-source tests (a block guarded by import.meta.vitest in a non-test
// TypeScript or JavaScript file). Split puts such files in the reference, so the agent would see the hidden tests or
// the solution would lose them. Files that cannot be scanned reliably are listed with the reason.
func InlineTests(ctx context.Context, base, solution string, reference []string, where ...string) (rust, python, script []string, err error) {
	if rust, err = InlineRustTests(ctx, base, solution, reference, where...); err != nil {
		return nil, nil, nil, err
	}
	if python, err = changedItems(ctx, base, solution, reference, where, "python doctests",
		func(ext string) bool { return ext == ".py" }, pythonDoctests); err != nil {
		return nil, nil, nil, err
	}
	script, err = changedItems(ctx, base, solution, reference, where, "in-source tests",
		func(ext string) bool { return slices.Contains(jsFamily, ext) }, vitestInSourceBlocks)
	return rust, python, script, err
}

// pythonDoctests returns the doctest examples in a Python file's string literals (docstrings and any other
// string), in order: each ">>>" line and the lines that follow it up to a blank line, unindented. Doctests in
// comments are not run and are not seen. Limits: examples in .rst, .md and .txt files (pytest --doctest-glob) are
// not looked at, and a ">>>" in a string that is not a doctest counts as one (the task is refused, never missed).
func pythonDoctests(src string) ([]string, error) {
	if !strings.Contains(src, ">>>") {
		return nil, nil
	}
	var out []string
	for i := 0; i < len(src); {
		end, bs, be, kind := lexScript(src, i, langPython)
		if (kind == tokString || kind == tokRaw) && strings.Contains(src[bs:be], ">>>") {
			var block []string
			flush := func() {
				if len(block) > 0 {
					out = append(out, strings.Join(block, "\n"))
					block = nil
				}
			}
			for _, line := range strings.Split(src[bs:be], "\n") {
				line = strings.TrimSpace(line)
				switch {
				case line == "":
					flush()
				case strings.HasPrefix(line, ">>>"):
					flush()
					block = append(block, line)
				case len(block) > 0:
					block = append(block, line)
				}
			}
			flush()
		}
		i = max(end, i+1)
	}
	return out, nil
}

// vitestInSourceBlocks returns the text of each import.meta.vitest block of a TypeScript or JavaScript file, in
// order: from the guard to the closing brace of the block that follows it (to the end of the line when no block
// follows). An unbalanced block is an error, and so is a file whose raw guard count differs from the guards found as
// code (a comment or string mention, or a regular-expression literal the lexer mistook for a string or comment).
// Limit: a test written outside such a block (a bare "describe" in a source file) is not seen.
func vitestInSourceBlocks(src string) ([]string, error) {
	if !strings.Contains(src, vitestInSource) {
		return nil, nil
	}
	var out []string
	for i := 0; i < len(src); {
		if end, _, _, kind := lexScript(src, i, langTS); kind != tokNone {
			i = max(end, i+1)
			continue
		}
		if !strings.HasPrefix(src[i:], vitestInSource) {
			i++
			continue
		}
		stop := strings.IndexByte(src[i:], '\n')
		if stop < 0 {
			stop = len(src) - i
		}
		open := strings.IndexByte(src[i:i+stop], '{')
		if open < 0 { // a guard alone on its line, the block starting on the next
			window := len(src) - i // the scan stops at the next guard, so a file of guard lines stays linear
			if next := strings.Index(src[i+len(vitestInSource):], vitestInSource); next >= 0 {
				window = len(vitestInSource) + next
			}
			if open = strings.IndexByte(src[i:i+window], '{'); open < 0 || strings.Contains(src[i:i+open], ";") {
				out = append(out, src[i:i+stop])
				i += stop
				continue
			}
		}
		closeAt, err := matchBrace(src, i+open)
		if err != nil {
			return nil, err
		}
		out = append(out, src[i:closeAt+1])
		i = closeAt + 1
	}
	// The lexer has no regular-expression literals, so a backtick or "/*" inside one can hide a guard (a template
	// literal or comment that never ends). Every raw occurrence must be one the lexer found as code; otherwise the
	// file is refused, which is also what a guard in a comment or string costs (the caller refuses; nothing is missed).
	parsed := 0 // occurrences inside the blocks found (the block's own guard and uses such as "= import.meta.vitest")
	for _, b := range out {
		parsed += strings.Count(b, vitestInSource)
	}
	if raw := strings.Count(src, vitestInSource); raw != parsed {
		return nil, fmt.Errorf("an in-source test guard (%s) was seen %d times but %d could be parsed", vitestInSource, raw, parsed)
	}
	return out, nil
}

// matchBrace returns the offset of the "}" that closes the "{" at src[open], skipping comments, strings and
// template literals.
func matchBrace(src string, open int) (int, error) {
	depth := 0
	for i := open; i < len(src); {
		if end, _, _, kind := lexScript(src, i, langTS); kind != tokNone {
			i = max(end, i+1)
			continue
		}
		switch src[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return i, nil
			}
		}
		i++
	}
	return 0, fmt.Errorf("unbalanced braces: %q opened on line %d is never closed", "{", lineOf(src, open))
}

// RefuseInlineRustTests returns the error the task commands give for a solution that changes tests inside source
// files, or nil: Rust inline tests, Python doctests and Vitest in-source tests (the name is kept for its callers).
// where locates the repository holding both commits (for example "--git-dir", bare).
func RefuseInlineRustTests(ctx context.Context, base, solution string, reference []string, where ...string) error {
	rust, python, script, err := InlineTests(ctx, base, solution, reference, where...)
	if err != nil {
		return err
	}
	var reasons []string
	if len(rust) > 0 {
		reasons = append(reasons, fmt.Sprintf("the solution changes Rust tests inside source files (%s): #[cfg(test)] and #[test] code would land in the "+
			"reference solution, so the hidden tests could not be kept from the agent or kept in the grading; move them to a "+
			"file under tests/ or choose another solution", strings.Join(rust, ", ")))
	}
	if len(python) > 0 {
		reasons = append(reasons, fmt.Sprintf("the solution changes Python doctests inside source files (%s): doctest examples would land in the "+
			"reference solution, so the hidden tests could not be kept from the agent or kept in the grading; move them to a "+
			"file under tests/ or choose another solution", strings.Join(python, ", ")))
	}
	if len(script) > 0 {
		reasons = append(reasons, fmt.Sprintf("the solution changes Vitest in-source tests (import.meta.vitest) inside source files (%s): they would land in the "+
			"reference solution, so the hidden tests could not be kept from the agent or kept in the grading; move them to a "+
			"*.test.ts file or choose another solution", strings.Join(script, ", ")))
	}
	if len(reasons) == 0 {
		return nil
	}
	return errors.New(strings.Join(reasons, "; "))
}
