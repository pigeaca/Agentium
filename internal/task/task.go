// Package task defines coding tasks: a base commit, an instruction, verification commands and, optionally, a solution
// commit. The solution's test-file changes are the hidden tests (the agent never sees them) and its other changes the
// reference solution. Validation proves a task can tell a solution from none.
package task

import (
	"context"
	"path"
	"regexp"
	"strings"

	"github.com/pigeaca/agentium/internal/gitx"
)

// jsTest matches JavaScript and TypeScript test files: name.test.ts, name.spec.jsx and so on.
var jsTest = regexp.MustCompile(`\.(test|spec)\.(js|jsx|ts|tsx|mjs|cjs|mts|cts)$`)

// IsTestFile reports whether p holds tests or test data: Go (_test.go), Python (test_*.py, *_test.py, conftest.py),
// JavaScript and TypeScript (*.test.*, *.spec.*), and anything in a test, tests, __tests__, testdata, __snapshots__ or
// e2e folder.
func IsTestFile(p string) bool {
	for _, dir := range strings.Split(path.Dir(p), "/") {
		switch dir {
		case "test", "tests", "__tests__", "testdata", "__snapshots__", "e2e":
			return true
		}
	}
	base := path.Base(p)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.HasSuffix(base, ".py"):
		return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "conftest.py"
	default:
		return jsTest.MatchString(base)
	}
}

// Split lists the files that differ between base and solution in the repository located by where (for example
// "--git-dir", bare): test files become the hidden tests, the rest the reference solution.
func Split(ctx context.Context, base, solution string, where ...string) (hiddenTests, reference []string, err error) {
	out, err := gitx.Output(ctx, nil, append(append([]string{}, where...), "diff", "--name-only", "-z", "--no-renames", base, solution)...)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range strings.Split(string(out), "\x00") {
		switch {
		case p == "":
		case IsTestFile(p):
			hiddenTests = append(hiddenTests, p)
		default:
			reference = append(reference, p)
		}
	}
	return hiddenTests, reference, nil
}
