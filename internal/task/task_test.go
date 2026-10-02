package task

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/snapshot"
)

func TestIsTestFile(t *testing.T) {
	for p, want := range map[string]bool{
		"pkg/parser_test.go": true, "pkg/parser.go": false, "pkg/testdata/in.json": true,
		"test_app.py": true, "app/app_test.py": true, "tests/conftest.py": true, "app/testing.py": false,
		"src/a.test.ts": true, "src/a.spec.jsx": true, "src/__tests__/a.js": true, "src/a.ts": false,
		"src/__snapshots__/a.snap": true, "e2e/login.ts": true, "scripts/test_harness.py": true,
		"docs/testing.md": false, "contest.py": false, "latest/x.go": false,
		"app/tests.py": true, "spec/models/user_spec.rb": true, "lib/user_spec.rb": true, "src/__mocks__/fs.js": true,
		"fixtures/users.json": true, "app/fixture.go": false, "specs.md": false,
		// Python and TypeScript (plan 2026-10-02-python-ts): the new rules and the production names they must not take.
		"pkg/unit_tests.py": true, "pkg/test_utils/helpers.py": true, "tests/test_utils/test_x.py": true,
		"src/click/testing.py": false, "src/flask/testing.py": false, "numpy/testing/_private/utils.py": false,
		"src/testing.py": false, "src/test_utils.py": true, "pkg/utils_test.py": true, "pkg/latest_tests.py": true,
		"pkg/tests_data.py": false, "pkg/testutils.py": false,
		"src/a.test-d.ts": true, "src/a.spec-d.mts": true, "bench/a.bench.ts": true, "src/a.bench.js": true,
		"src/a.d.ts": false, "src/benchmark.ts": false, "src/a.bench.json": false, "src/test-d.ts": false, "src/a.testd.ts": false,
		"src/testing.ts": false, "src/test-utils.ts": false,
	} {
		if got := IsTestFile(p); got != want {
			t.Errorf("IsTestFile(%q) = %v, want %v", p, got, want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = gitx.Environ(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, repo string, files map[string]string, message string) string {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(repo, name)
		if body == "" {
			os.Remove(full)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", message)
	return git(t, repo, "rev-parse", "HEAD")
}

// fixture is a repository whose checks are shell tests: run_tests.sh runs tests/*.sh and requires CLAUDE.md to name
// the rules (the repository's own documentation check, which a trimmed context can break).
type fixture struct {
	bare, base, solution, weakSolution string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	user := t.TempDir()
	git(t, user, "init", "-q", "-b", "main")
	base := commit(t, user, map[string]string{
		"run_tests.sh": "grep -q Rules CLAUDE.md || { echo 'CLAUDE.md lost its rules'; exit 1; }\n" +
			"for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n",
		"CLAUDE.md": "# Rules\nKeep tests green.\n",
		"value.txt": "old\n",
	}, "base")
	solution := commit(t, user, map[string]string{"tests/value_test.sh": "grep -q new value.txt\n", "value.txt": "new\n"}, "make value new")
	git(t, user, "checkout", "-q", base)
	weak := commit(t, user, map[string]string{"tests/always_test.sh": "true\n", "value.txt": "newer\n"}, "tests that never fail")
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{base, solution, weak} {
		if err := gitx.FetchCommit(ctx, user, c, gitx.SourceRef(c), "--git-dir", bare); err != nil {
			t.Fatal(err)
		}
	}
	return fixture{bare: bare, base: base, solution: solution, weakSolution: weak}
}

func validator(t *testing.T, bare string) (Validator, *bytes.Buffer) {
	var progress bytes.Buffer
	return Validator{Bare: bare, WorkDir: t.TempDir(), LogDir: t.TempDir(), Timeout: 30 * time.Second, Progress: &progress,
		Now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }}, &progress
}

func stages(v Validation) []string {
	var out []string
	for _, s := range v.Stages {
		got := passFail(s.Passed)
		if s.SetupFailed {
			got = "setup-failed"
		}
		out = append(out, s.Arm+"/"+s.Stage+"="+s.Want+":"+got)
	}
	return out
}

func TestSplitAndValidate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	hidden, reference, err := Split(ctx, f.base, f.solution, "--git-dir", f.bare)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hidden, []string{"tests/value_test.sh"}) || !slices.Equal(reference, []string{"value.txt"}) {
		t.Fatalf("split = %v / %v", hidden, reference)
	}
	spec := Spec{Base: f.base, Solution: f.solution, HiddenTests: hidden, Reference: reference, Verify: []string{"sh run_tests.sh"}}

	// Two context arms: a trimmed CLAUDE.md that keeps the rules heading, and one that breaks the documentation check.
	var arms []Arm
	for name, claude := range map[string]string{"trimmed": "# Rules\n", "broken": "Be brief.\n"} {
		commitID, _, err := snapshot.Build(ctx, f.bare, snapSource{"CLAUDE.md": claude}, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		arms = append(arms, Arm{Name: name, Snapshot: commitID})
	}
	sort.Slice(arms, func(i, j int) bool { return arms[i].Name > arms[j].Name }) // trimmed, broken
	v, progress := validator(t, f.bare)
	result, err := v.Validate(ctx, spec, append([]Arm{{Name: "base"}}, arms...))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"base/hidden-tests=fail:fail", "base/reference=pass:pass",
		"trimmed/hidden-tests=fail:fail", "trimmed/reference=pass:pass",
		"broken/hidden-tests=fail:fail", "broken/reference=pass:fail", // the arm breaks the repository's own check
	}
	if !slices.Equal(stages(result), want) || result.Status != StatusInvalid {
		t.Errorf("stages = %v (status %s)\nwant %v", stages(result), result.Status, want)
	}
	if !strings.Contains(result.Summary(), "broken/reference wanted pass") || !strings.Contains(progress.String(), "NOT OK") {
		t.Errorf("summary %q, progress:\n%s", result.Summary(), progress.String())
	}
	log, _ := os.ReadFile(result.Stages[5].Log)
	if !strings.Contains(string(log), "CLAUDE.md lost its rules") {
		t.Errorf("the log should show why: %s", log)
	}
	if entries, _ := os.ReadDir(v.WorkDir); len(entries) != 0 {
		t.Errorf("checkouts left behind: %v", entries)
	}

	valid, err := v.Validate(ctx, spec, []Arm{{Name: "base"}, arms[0]})
	if err != nil || valid.Status != StatusValid {
		t.Errorf("without the broken arm: %s, %v", valid.Summary(), err)
	}
}

func TestValidateRejectsTestsThatDoNotFailAndHandlesMissingSolutions(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	hidden, reference, err := Split(ctx, f.base, f.weakSolution, "--git-dir", f.bare)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := validator(t, f.bare)
	weak, err := v.Validate(ctx, Spec{Base: f.base, Solution: f.weakSolution, HiddenTests: hidden, Reference: reference,
		Verify: []string{"sh run_tests.sh"}}, []Arm{{Name: "base"}})
	if err != nil {
		t.Fatal(err)
	}
	if weak.Status != StatusInvalid || !slices.Equal(stages(weak), []string{"base/hidden-tests=fail:pass"}) {
		t.Errorf("hidden tests that pass on the base: %v (%s)", stages(weak), weak.Status)
	}
	manual, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{"sh run_tests.sh"}}, []Arm{{Name: "base"}})
	if err != nil || manual.Status != StatusUnchecked || len(manual.Stages) != 1 {
		t.Errorf("no solution: %+v, %v", manual, err)
	}
	// Setup runs first in each checkout: here it creates what the checks need, like built assets.
	needsAsset := Spec{Base: f.base, Solution: f.solution, HiddenTests: []string{"tests/value_test.sh"}, Reference: []string{"value.txt"},
		Verify: []string{"test -f build/asset", "sh run_tests.sh"}}
	if without, _ := v.Validate(ctx, needsAsset, []Arm{{Name: "base"}}); without.Status != StatusInvalid {
		t.Errorf("without setup: %v", stages(without))
	}
	needsAsset.Setup = []string{"mkdir -p build && touch build/asset"}
	withSetup, err := v.Validate(ctx, needsAsset, []Arm{{Name: "base"}})
	if err != nil || withSetup.Status != StatusValid || len(withSetup.Stages[1].Setup) != 1 ||
		!slices.Equal(stages(withSetup), []string{"base/hidden-tests=fail:fail", "base/reference=pass:pass"}) {
		t.Errorf("with setup: %v (%s), %v", stages(withSetup), withSetup.Status, err)
	}
	needsAsset.Setup = []string{"exit 7"}
	broken, _ := v.Validate(ctx, needsAsset, []Arm{{Name: "base"}})
	if broken.Status != StatusInvalid || !slices.Equal(stages(broken), []string{"base/hidden-tests=fail:setup-failed"}) ||
		!strings.Contains(broken.Summary(), "base/hidden-tests setup failed") {
		t.Errorf("failing setup: %v, %q", stages(broken), broken.Summary())
	}

	v.Timeout = 100 * time.Millisecond
	slow, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{"sleep 5"}}, []Arm{{Name: "base"}})
	if err != nil || slow.Status != StatusInvalid || !slow.Stages[0].Commands[0].TimedOut {
		t.Errorf("timeout: %+v, %v", slow, err)
	}
	// Hidden tests must fail, but a timeout is not that failure.
	hung, err := v.Validate(ctx, Spec{Base: f.base, Solution: f.solution, HiddenTests: []string{"tests/value_test.sh"},
		Reference: []string{"value.txt"}, Verify: []string{"sleep 5"}}, []Arm{{Name: "base"}})
	if err != nil || hung.Status != StatusInvalid || !strings.Contains(hung.Summary(), "base/hidden-tests timed out") {
		t.Errorf("timed-out hidden tests: %v, %q, %v", stages(hung), hung.Summary(), err)
	}
}

// snapSource is an in-memory repository state for building snapshots.
type snapSource map[string]string

func (m snapSource) Paths() []string {
	var paths []string
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
func (m snapSource) ReadFile(p string) ([]byte, error) {
	if data, ok := m[p]; ok {
		return []byte(data), nil
	}
	return nil, os.ErrNotExist
}
func (m snapSource) Executable(string) bool { return false }
func (m snapSource) Describe() string       { return "memory" }

// history makes a repository with a base commit and solutions branching from it, copies them into a bare repository
// and returns it with the commit ids.
func history(t *testing.T, base map[string]string, solutions ...map[string]string) (string, string, []string) {
	t.Helper()
	ctx := context.Background()
	user := t.TempDir()
	git(t, user, "init", "-q", "-b", "main")
	baseCommit := commit(t, user, base, "base")
	var ids []string
	for i, files := range solutions {
		git(t, user, "checkout", "-q", baseCommit)
		ids = append(ids, commit(t, user, files, "solution "+strconv.Itoa(i)))
	}
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	for _, c := range append([]string{baseCommit}, ids...) {
		if err := gitx.FetchCommit(ctx, user, c, gitx.SourceRef(c), "--git-dir", bare); err != nil {
			t.Fatal(err)
		}
	}
	return bare, baseCommit, ids
}

var checkedRepo = map[string]string{
	"run_tests.sh": "grep -q Rules CLAUDE.md || { echo 'CLAUDE.md lost its rules'; exit 1; }\n" +
		"for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n",
	"CLAUDE.md": "# Rules\nKeep tests green.\n",
	"value.txt": "old\n",
}

// A hidden test that fails once and then passes (like a snapshot test writing its snapshot) must not make a reference
// that fixes nothing look valid: each stage gets a fresh checkout.
func TestStagesDoNotShareState(t *testing.T) {
	ctx := context.Background()
	bare, base, ids := history(t, checkedRepo, map[string]string{
		"tests/seen_test.sh": "test -f .seen || { touch .seen; exit 1; }\n",
		"unrelated.txt":      "the reference fixes nothing\n",
	})
	v, _ := validator(t, bare)
	result, err := v.Validate(ctx, Spec{Base: base, Solution: ids[0], HiddenTests: []string{"tests/seen_test.sh"},
		Reference: []string{"unrelated.txt"}, Verify: []string{"sh run_tests.sh"}}, []Arm{{Name: "base"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusInvalid || !slices.Equal(stages(result), []string{"base/hidden-tests=fail:fail", "base/reference=pass:fail"}) {
		t.Errorf("stages = %v (%s): state leaked from the hidden-tests run into the reference run", stages(result), result.Status)
	}
}

// A solution that also edits context files must not undo an arm's context: the arm keeps its version, so an arm whose
// context breaks the repository's checks stays invalid.
func TestSolutionFilesKeepTheArmsContext(t *testing.T) {
	ctx := context.Background()
	bare, base, ids := history(t, checkedRepo, map[string]string{
		"tests/value_test.sh": "grep -q new value.txt\n",
		"value.txt":           "new\n",
		"CLAUDE.md":           "# Rules\nKeep tests green. Values are new.\n",
	})
	broken, _, err := snapshot.Build(ctx, bare, snapSource{"CLAUDE.md": "Be brief.\n"}, "broken", nil)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := validator(t, bare)
	result, err := v.Validate(ctx, Spec{Base: base, Solution: ids[0], HiddenTests: []string{"tests/value_test.sh"},
		Reference: []string{"CLAUDE.md", "value.txt"}, Verify: []string{"sh run_tests.sh"}}, []Arm{{Name: "base"}, {Name: "broken", Snapshot: broken}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"base/hidden-tests=fail:fail", "base/reference=pass:pass", "broken/hidden-tests=fail:fail", "broken/reference=pass:fail"}
	if !slices.Equal(stages(result), want) || result.Status != StatusInvalid {
		t.Errorf("stages = %v (%s), want %v", stages(result), result.Status, want)
	}
	if !slices.Equal(result.ContextKept["broken"], []string{"CLAUDE.md"}) || len(result.ContextKept["base"]) != 0 {
		t.Errorf("context kept = %v", result.ContextKept)
	}
}

func TestIsTestFileJavaAndKotlinClasses(t *testing.T) {
	for p, want := range map[string]bool{
		"src/main/java/app/ParserTest.java": false, "app/ParserTests.java": true, "app/ParserIT.java": true,
		"core/UserServiceTest.kt": true, "core/UserServiceIT.kt": true, "src/test/java/app/Helper.java": true,
		"src/main/java/app/Parser.java": false, "app/Contest.java": false, "app/Test.txt": false, "app/ParserTest.xml": false,
		"app/ParserTester.java": false, "app/Wait.kt": false, "app/ParserTest.scala": false,
		"src/main/java/com/x/ABTest.java": false, "app/LoadTest.kt": true, "src/GIT.java": false, "src/AUDIT.kt": false,
		"src/main/java/Test.java": false, "x/IT.java": false, "src/main/java/RetryingTest.java": false,
		"mod/src/main/kotlin/CartesianTest.kt": false, "src/test/java/RetryingTest.java": true, "src/Web3Test.java": true,
	} {
		if got := IsTestFile(p); got != want {
			t.Errorf("IsTestFile(%q) = %v, want %v", p, got, want)
		}
	}
}

// A Java test class outside a test folder is a hidden test, not part of the reference.
func TestSplitJavaTestClassAnywhere(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	b := commit(t, repo, map[string]string{"app/Parser.java": "class Parser {}\n"}, "base")
	s := commit(t, repo, map[string]string{"app/Parser.java": "class Parser { int x; }\n", "app/ParserTest.java": "class ParserTest {}\n", "app/Run.kt": "fun x() {}\n"}, "solution")
	hidden, reference, err := Split(context.Background(), b, s, "-C", repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hidden, []string{"app/ParserTest.java"}) || !slices.Equal(reference, []string{"app/Parser.java", "app/Run.kt"}) {
		t.Errorf("split = %v / %v", hidden, reference)
	}
}

// Each checkout's build tools keep their caches in Agentium's cache folder, whatever tool the task's repository uses;
// without a cache root the environment is the caller's alone.
func TestValidatorEnvFollowsTheCheckoutsBuildTools(t *testing.T) {
	dir := t.TempDir()
	v := Validator{Env: []string{"A=1"}, Cache: "/data/cache"}
	if got := v.envFor(dir); !slices.Equal(got, []string{"A=1"}) {
		t.Errorf("a checkout with no marker: %q", got)
	}
	for file, want := range map[string]string{"build.gradle.kts": "GRADLE_USER_HOME=/data/cache/gradle", "pom.xml": "MAVEN_ARGS=-Dmaven.repo.local=/data/cache/m2 -Dmaven.build.cache.enabled=false",
		"Cargo.toml": "RUSTC_WRAPPER="} {
		if err := os.WriteFile(filepath.Join(dir, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := v.envFor(dir); !slices.Contains(got, want) || got[0] != "A=1" {
			t.Errorf("with %s: %q lacks %s", file, got, want)
		}
	}
	if got := (Validator{Env: []string{"A=1"}}).envFor(dir); !slices.Equal(got, []string{"A=1"}) {
		t.Errorf("without a cache root: %q", got)
	}
	if len(v.Env) != 1 {
		t.Error("envFor changed the validator's own environment")
	}
}
