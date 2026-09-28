package task

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
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
		out = append(out, s.Arm+"/"+s.Stage+"="+s.Want+":"+passFail(s.Passed))
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
	if err != nil || withSetup.Status != StatusValid ||
		!slices.Equal(stages(withSetup), []string{"base/setup=pass:pass", "base/hidden-tests=fail:fail", "base/reference=pass:pass"}) {
		t.Errorf("with setup: %v (%s), %v", stages(withSetup), withSetup.Status, err)
	}
	needsAsset.Setup = []string{"exit 7"}
	if broken, _ := v.Validate(ctx, needsAsset, []Arm{{Name: "base"}}); broken.Status != StatusInvalid || !slices.Equal(stages(broken), []string{"base/setup=pass:fail"}) {
		t.Errorf("failing setup: %v", stages(broken))
	}

	v.Timeout = 100 * time.Millisecond
	slow, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{"sleep 5"}}, []Arm{{Name: "base"}})
	if err != nil || slow.Status != StatusInvalid || !slow.Stages[0].Commands[0].TimedOut {
		t.Errorf("timeout: %+v, %v", slow, err)
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
