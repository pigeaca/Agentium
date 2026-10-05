package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/task"
)

// The agents of issue #160, as shell run in the agent's checkout: each leaves Value() returning 0, the hidden test
// wants 1. Only goSolves fixes it.
const (
	goIdle     = ""
	goTestMain = `printf 'package fixture\n\nimport (\n\t"os"\n\t"testing"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(0) }\n' > bypass_test.go`
	goInit     = `printf 'package fixture\n\nimport "os"\n\nfunc Value() int { return 0 }\n\nfunc init() { os.Exit(0) }\n' > value.go`
	goSolves   = `printf 'package fixture\n\nfunc Value() int { return 1 }\n' > value.go`
)

// newGoOnce is a Go module (fixture) whose Value returns 0 at the base; the solution adds the hidden test value_test.go
// (TestHidden wants Value() == 1) and makes Value return 1. The verification is `go test -count=1 ./...`; a fake Claude
// Code runs agent in its checkout. Commands find the go that runs the tests, with this machine's build cache (no
// download: GOTOOLCHAIN=local, GOPROXY=off).
func newGoOnce(t *testing.T, agent string) moduleOnce {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	cache, err := exec.Command(goBin, "env", "GOCACHE").Output()
	if err != nil {
		t.Skipf("go env GOCACHE: %v", err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	user := filepath.Join(dir, "user")
	gitIn := func(args ...string) string {
		t.Helper()
		out, err := gitx.Run(ctx, append([]string{"-C", user, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	must(t, os.MkdirAll(user, 0o700))
	gitIn("init", "-q", "-b", "main")
	writeFile(t, filepath.Join(user, "go.mod"), "module example.com/fixture\n\ngo 1.22\n")
	writeFile(t, filepath.Join(user, "value.go"), "package fixture\n\nfunc Value() int { return 0 }\n")
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "base")
	base := gitIn("rev-parse", "HEAD")
	writeFile(t, filepath.Join(user, "value_test.go"), "package fixture\n\nimport \"testing\"\n\nfunc TestHidden(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal(Value())\n\t}\n}\n")
	writeFile(t, filepath.Join(user, "value.go"), "package fixture\n\nfunc Value() int { return 1 }\n")
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "make the value 1")
	solution := gitIn("rev-parse", "HEAD")

	layout, err := home.Resolve(func(key string) string { return map[string]string{"AGENTIUM_HOME": filepath.Join(dir, "data")}[key] })
	must(t, err)
	must(t, layout.Ensure())
	bare := filepath.Join(layout.Root, "projects", "1", "repo.git")
	must(t, gitx.InitBare(ctx, bare))
	for _, c := range []string{base, solution} {
		must(t, gitx.FetchCommit(ctx, user, c, gitx.SourceRef(c), "--git-dir", bare))
	}
	cli := filepath.Join(dir, "claude")
	writeFile(t, cli, "#!/bin/sh\n"+agent+"\n"+`cat <<'EOF'
{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5","permissionMode":"acceptEdits","tools":["Bash"],"skills":[],"slash_commands":[]}
{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.25,"num_turns":2,"duration_ms":1000,"modelUsage":{}}
EOF
`)
	must(t, os.Chmod(cli, 0o755))
	homeDir := filepath.Join(dir, "home")
	must(t, os.MkdirAll(homeDir, 0o700))
	id, err := NewID(time.Now())
	must(t, err)
	env := Env{ID: id, Layout: layout, Bare: bare, ProjectRoot: user, CLI: cli, Home: homeDir,
		Environ: []string{"PATH=" + filepath.Dir(goBin) + ":/usr/bin:/bin", "HOME=" + homeDir, "GOCACHE=" + strings.TrimSpace(string(cache)),
			"GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=mod"},
		SignIn: claude.SignInLogin, VerifyTimeout: 2 * time.Minute, Grace: time.Second, Now: time.Now}
	spec := Spec{TaskName: "value", Instruction: "Make Value return 1.", Arm: task.Arm{Name: "base"}, Model: "claude-sonnet-5", BudgetUSD: 1,
		Timeout: time.Minute, Task: task.Spec{Base: base, Solution: solution, HiddenTests: []string{"value_test.go"}, Reference: []string{"value.go"},
			Verify: []string{"go test -count=1 ./..."}}}
	return moduleOnce{env: env, spec: spec}
}

// gradeGo runs f's agent once and returns its record and verify.log.
func gradeGo(t *testing.T, f moduleOnce) (Record, string) {
	t.Helper()
	rec, err := Once(context.Background(), f.env, f.spec)
	if skipErr != nil {
		skipErr(t, err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != agent.OutcomeOK || rec.Passed == nil || rec.Grader != task.GraderOf(f.env.Grader) ||
		(rec.Grader == task.GraderSandbox) != (rec.Sandbox != nil && rec.Sandbox.Canary == task.CanaryPassed) {
		t.Fatalf("outcome %s, passed %v, grader %s, sandbox %+v, notes %v", rec.Outcome, rec.Passed, rec.Grader, rec.Sandbox, rec.Notes)
	}
	log, _ := os.ReadFile(filepath.Join(rec.RecordsDir, "verify.log"))
	return rec, string(log)
}

// skipErr, when set (the sandbox's tests, on macOS), may skip a test for an error Once returned.
var skipErr func(t *testing.T, err error)

// notRun reports whether a record carries the note of a pass without the proof.
func notRun(rec Record) bool {
	for _, n := range rec.Notes {
		if strings.HasPrefix(n, task.NoteHiddenTestsNotRun+":") {
			return true
		}
	}
	return false
}

// checkIssue160 grades the issue's agents in f's mode (made by fixture) and checks each grade: unchanged code fails on
// the test itself (the proof never runs after a failed verification); a TestMain or an init that exits 0 passes the
// verification but not the proof, so each fails with the note; a correct change passes, proven.
func checkIssue160(t *testing.T, fixture func(t *testing.T, agent string) moduleOnce) {
	t.Helper()
	rec, log := gradeGo(t, fixture(t, goIdle))
	if *rec.Passed || rec.Proof != nil || notRun(rec) || strings.Contains(log, "go test -json") {
		t.Errorf("unchanged code: passed %v, proof %+v, notes %v\n%s", *rec.Passed, rec.Proof, rec.Notes, log)
	}
	for name, agentCmd := range map[string]string{"TestMain exits 0": goTestMain, "init exits 0": goInit} {
		rec, log := gradeGo(t, fixture(t, agentCmd))
		if *rec.Passed || !notRun(rec) || rec.Proof == nil || rec.Proof.Proven() || len(rec.Verify) != 1 || rec.Verify[0].ExitCode != 0 {
			t.Errorf("%s: passed %v, verify %+v, proof %+v, notes %v\n%s", name, *rec.Passed, rec.Verify, rec.Proof, rec.Notes, log)
			continue
		}
		if m := rec.Proof.Missing; len(m) != 1 || m[0].Test != "TestHidden" || m[0].Saw != task.SawNone {
			t.Errorf("%s: missing %+v", name, m)
		}
		if !strings.Contains(log, "$ go test -json -count=1 -run '^(TestHidden)$' .\n") ||
			!strings.Contains(log, "[agentium] the hidden tests did not run: go test -json showed no pass for TestHidden in . (not run)") {
			t.Errorf("%s: verify.log:\n%s", name, log)
		}
		if _, err := os.Stat(filepath.Join(rec.RecordsDir, task.ProofEvents)); err != nil {
			t.Errorf("%s: the proof's events: %v", name, err)
		}
	}
	rec, log = gradeGo(t, fixture(t, goSolves))
	if !*rec.Passed || !rec.Proof.Proven() || notRun(rec) || !strings.Contains(log, "[agentium] the hidden tests ran: go test -json showed each of the task's 1 hidden test(s) pass") {
		t.Errorf("a correct change: passed %v, proof %+v, notes %v\n%s", *rec.Passed, rec.Proof, rec.Notes, log)
	}
	events, _ := os.ReadFile(filepath.Join(rec.RecordsDir, task.ProofEvents))
	if !strings.Contains(string(events), `"Action":"pass","Package":"example.com/fixture","Test":"TestHidden"`) {
		t.Errorf("the proof's events:\n%s", events)
	}
}

// Issue #160 on the host.
func TestOnceNeedsTheProofThatTheHiddenTestsRan(t *testing.T) {
	checkIssue160(t, newGoOnce)
}

// An experiment locked before the proof keeps its rule: the exit codes alone grade it, so the TestMain bypass passes
// as it did, and nothing about the proof is run or recorded.
func TestOnceByTheExitCodesRunsNoProof(t *testing.T) {
	f := newGoOnce(t, goTestMain)
	f.env.ExitCodeOnly = true
	rec, log := gradeGo(t, f)
	if !*rec.Passed || rec.Proof != nil || notRun(rec) || strings.Contains(log, "go test -json") {
		t.Errorf("passed %v, proof %+v, notes %v\n%s", *rec.Passed, rec.Proof, rec.Notes, log)
	}
	if _, err := os.Stat(filepath.Join(rec.RecordsDir, task.ProofEvents)); err == nil {
		t.Error("a proof's events under the exit-code rule")
	}
}

// The proof does not depend on the verification commands: with two of them it follows both, and with one that is not
// go test at all (which passes whatever the code does) it still fails a change whose hidden test fails.
func TestOnceProvesWhateverTheVerificationIs(t *testing.T) {
	f := newGoOnce(t, goTestMain)
	f.spec.Task.Verify = []string{"go vet ./...", "go test -count=1 ./..."}
	if rec, log := gradeGo(t, f); *rec.Passed || !notRun(rec) || len(rec.Verify) != 2 {
		t.Errorf("two commands, TestMain exits 0: passed %v, verify %+v, notes %v\n%s", *rec.Passed, rec.Verify, rec.Notes, log)
	}
	f = newGoOnce(t, goSolves)
	f.spec.Task.Verify = []string{"go vet ./...", "go test -count=1 ./..."}
	if rec, log := gradeGo(t, f); !*rec.Passed || !rec.Proof.Proven() {
		t.Errorf("two commands, solved: passed %v, proof %+v\n%s", *rec.Passed, rec.Proof, log)
	}

	f = newGoOnce(t, goIdle)
	f.spec.Task.Verify = []string{"true"}
	rec, log := gradeGo(t, f)
	if *rec.Passed || !notRun(rec) || rec.Proof == nil || len(rec.Proof.Missing) != 1 || rec.Proof.Missing[0].Saw != task.SawFail ||
		!strings.Contains(log, "no pass for TestHidden in . (failed)") {
		t.Errorf("not go test, unchanged code: passed %v, proof %+v, notes %v\n%s", *rec.Passed, rec.Proof, rec.Notes, log)
	}
	f = newGoOnce(t, goSolves)
	f.spec.Task.Verify = []string{"true"}
	if rec, log := gradeGo(t, f); !*rec.Passed || !rec.Proof.Proven() {
		t.Errorf("not go test, solved: passed %v, proof %+v\n%s", *rec.Passed, rec.Proof, log)
	}
}

// A task in a module folder proves its tests from there, and the proof tests are read from the task's commits.
func TestOnceProvesInTheTasksModule(t *testing.T) {
	f := newModuleOnceWith(t, "svc", "decoy", "printf 'package svc\\n\\nfunc Value() int { return 1 }\\n' > svc/value.go", map[string]string{
		"svc/go.mod": "module example.com/svc\n\ngo 1.22\n", "svc/value.go": "package svc\n\nfunc Value() int { return 0 }\n"})
	g := newGoOnce(t, goIdle) // for its environment
	f.env.Environ = g.env.Environ
	// The hidden test is in the module's folder; the fixture's own shell solution is not used.
	solution := commitOnto(t, f, map[string]string{"svc/value_test.go": "package svc\n\nimport \"testing\"\n\nfunc TestHidden(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal(Value())\n\t}\n}\n",
		"svc/value.go": "package svc\n\nfunc Value() int { return 1 }\n"})
	f.spec.Task.Solution, f.spec.Task.HiddenTests, f.spec.Task.Reference = solution, []string{"svc/value_test.go"}, []string{"svc/value.go"}
	f.spec.Task.Verify = []string{"go test -count=1 ./..."}
	rec, log := gradeGo(t, f)
	if !*rec.Passed || !rec.Proof.Proven() || !strings.Contains(log, "$ go test -json -count=1 -run '^(TestHidden)$' .\n") {
		t.Errorf("passed %v, proof %+v, notes %v\n%s", *rec.Passed, rec.Proof, rec.Notes, log)
	}
}

// commitOnto commits files on top of f's base in the user's repository and copies the commit into f's bare one.
func commitOnto(t *testing.T, f moduleOnce, files map[string]string) string {
	t.Helper()
	ctx := context.Background()
	user := f.env.ProjectRoot
	git := func(args ...string) string {
		t.Helper()
		out, err := gitx.Run(ctx, append([]string{"-C", user, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git("checkout", "-q", f.spec.Task.Base)
	for p, c := range files {
		writeFile(t, filepath.Join(user, filepath.FromSlash(p)), c)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "the Go solution")
	id := git("rev-parse", "HEAD")
	must(t, gitx.FetchCommit(ctx, user, id, gitx.SourceRef(id), "--git-dir", f.env.Bare))
	return id
}

// A task without hidden Go tests is graded as before: no proof is run, recorded or noted.
func TestOnceRunsNoProofForOtherTasks(t *testing.T) {
	f := newModuleOnce(t, "svc", "decoy", "printf 'new\\n' > svc/value.txt")
	rec, log := gradeGo(t, f)
	if !*rec.Passed || rec.Proof != nil || notRun(rec) || strings.Contains(log, "go test -json") {
		t.Errorf("passed %v, proof %+v, notes %v\n%s", *rec.Passed, rec.Proof, rec.Notes, log)
	}
	if _, err := os.Stat(filepath.Join(rec.RecordsDir, task.ProofEvents)); err == nil {
		t.Error("a proof's events for a task without Go tests")
	}
}
