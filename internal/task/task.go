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

// jvmTest matches the base name of a Java or Kotlin test class: a lowercase letter or digit, then Test, Tests or IT.
// The letter before the suffix keeps GIT, AUDIT, ABTest and a bare Test.java (production names) out.
var jvmTest = regexp.MustCompile(`[a-z0-9](Test|Tests|IT)\.(java|kt)$`)

// jsTest matches JavaScript and TypeScript test files: name.test.ts, name.spec.jsx and so on.
var jsTest = regexp.MustCompile(`\.(test|spec)\.(js|jsx|ts|tsx|mjs|cjs|mts|cts)$`)

// IsTestFile reports whether p holds tests or test data: Go (_test.go), Python (test_*.py, *_test.py, tests.py,
// conftest.py), Ruby (*_spec.rb), JavaScript and TypeScript (*.test.*, *.spec.*), Java and Kotlin test classes
// (a name like ParserTest, ParserTests or ParserIT outside a src/main folder: Maven and Gradle keep tests in src/test,
// and a production class that is merely named like a test, such as RetryingTest.java, must stay in the reference or the
// hidden tests would hand it to agents), Rust's tests.rs, and anything in a test, tests, spec,
// __tests__, __mocks__, testdata, fixtures, __fixtures__, __snapshots__ or e2e folder. Other test inputs (a golden file
// beside the code, say) land in the reference, which validation cannot notice because the reference supplies them.
// Rust's inline #[cfg(test)] code sits in ordinary source files and cannot be told apart by name: InlineRustTests
// finds it, and the task commands refuse such solutions.
func IsTestFile(p string) bool {
	for _, dir := range strings.Split(path.Dir(p), "/") {
		switch dir {
		case "test", "tests", "spec", "__tests__", "__mocks__", "testdata", "fixtures", "__fixtures__", "__snapshots__", "e2e":
			return true
		}
	}
	base := path.Base(p)
	switch {
	case base == "tests.rs": // the file of a `#[cfg(test)] mod tests;` declaration; InlineRustTests refuses the declaration's change
		return true
	case strings.HasSuffix(base, "_test.go"), strings.HasSuffix(base, "_spec.rb"):
		return true
	case strings.HasSuffix(base, ".py"):
		return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "tests.py" || base == "conftest.py"
	case strings.HasSuffix(base, ".java"), strings.HasSuffix(base, ".kt"):
		return jvmTest.MatchString(base) && !inMainSource(p)
	default:
		return jsTest.MatchString(base)
	}
}

// inMainSource reports whether p has a src/main folder pair (Maven and Gradle production code).
func inMainSource(p string) bool {
	parts := strings.Split(p, "/")
	for i := 0; i+1 < len(parts)-1; i++ {
		if parts[i] == "src" && parts[i+1] == "main" {
			return true
		}
	}
	return false
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
