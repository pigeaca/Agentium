package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/sandbox"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// needGradingSandbox skips unless sandbox-exec can apply a profile here (macOS, not nested in another sandbox).
func needGradingSandbox(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("starts sandboxed processes")
	}
	if err := sandbox.Usable(context.Background()); err != nil {
		t.Skipf("the grading sandbox: %v", err)
	}
}

// setValidationGrader rewrites the mode the task name's stored validation names.
func setValidationGrader(t *testing.T, f runFixture, name, grader string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	tk, err := db.TaskByName(ctx, projects[0].ID, name)
	if err != nil {
		t.Fatal(err)
	}
	v := task.ValidationOf(tk)
	v.Grader = grader
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.SetTaskValidation(ctx, tk.ID, tk.Verify, tk.Setup, encoded, time.Now()); err != nil || !ok {
		t.Fatalf("store the validation: %v, %v", ok, err)
	}
}

// The default mode is the platform's: the sandbox on macOS, the host elsewhere; a test can pin it.
func TestDefaultGrader(t *testing.T) {
	want := task.GraderHost
	if runtime.GOOS == "darwin" {
		want = task.GraderSandbox
	}
	if got := defaultGrader(Env{}); got != want {
		t.Errorf("defaultGrader = %q, want %q", got, want)
	}
	if got := defaultGrader(Env{DefaultGrader: task.GraderHost}); got != task.GraderHost {
		t.Errorf("pinned: %q", got)
	}
}

// One experiment never mixes modes: a host experiment refuses a task validated in the sandbox (and says how to
// validate it on the host), and --grader takes only host or sandbox.
func TestHostExperimentRefusesASandboxValidatedTask(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	setValidationGrader(t, f, "value", task.GraderSandbox)
	expect(t, f.run(ctx, "experiment", "new", "mixed", "--b", "lean", "--task", "value", "--budget", "10", "--grader", "host"), ExitError,
		"validated in the sandbox (sandbox-v1), but this experiment grades on the host", "agentium task validate value --snapshot lean --grader host")
	expect(t, f.run(ctx, "experiment", "new", "bad", "--b", "lean", "--task", "value", "--grader", "docker"), ExitUsage, `--grader "docker": use host or sandbox`)
	expect(t, f.run(ctx, "task", "validate", "value", "--grader", "docker"), ExitUsage, `--grader "docker"`)
	expect(t, f.run(ctx, "run", "once", "value", "--grader", "docker"), ExitUsage, `--grader "docker"`)
}

// An experiment locked before grader modes (its lock names none) resumes as it ran, on the host, whatever the default
// is now; a new lock names its mode.
func TestOldLockResumesOnTheHost(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "old", "--b", "lean", "--task", "value", "--budget", "10", "--grader", "host"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "old"), ExitOK, "graded on the host")
	var lock map[string]any
	if err := json.Unmarshal(storedLock(t, f, "old"), &lock); err != nil {
		t.Fatal(err)
	}
	if lock["grader"] != task.GraderHost {
		t.Errorf("the lock's grader %v", lock["grader"])
	}
	delete(lock, "grader") // as locks were before modes
	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	amendLock(t, f, "old", encoded)
	*f.grader = task.GraderSandbox // the default now, which an old lock must not take
	expect(t, f.run(ctx, "experiment", "run", "old"), ExitOK, "Resuming experiment old")
	for _, r := range records(t, experimentRuns(t, f, "old")) {
		if r.Grader != task.GraderHost || r.Sandbox != nil {
			t.Errorf("run %s graded %q, sandbox %+v", r.ID, r.Grader, r.Sandbox)
		}
	}
	if report := f.run(ctx, "experiment", "report", "old"); strings.Contains(report.stdout, "Graded") {
		t.Errorf("an old lock's report names a mode:\n%s", report.stdout)
	}
}

// amendLock replaces experiment name's stored lock.
func amendLock(t *testing.T, f runFixture, name string, lock []byte) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	e, err := db.ExperimentByName(ctx, projects[0].ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AmendLock(ctx, e.ID, lock); err != nil {
		t.Fatal(err)
	}
}

// A sandbox experiment takes a task validated on the host, validates it again in the sandbox before it locks (the
// isolation plan's decision 5), locks the mode, grades every run in the sandbox (a solving agent's hidden-test pass
// stays a pass), and its report says so.
func TestSandboxExperimentRevalidatesAndGradesInTheSandbox(t *testing.T) {
	needGradingSandbox(t)
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	*f.grader = task.GraderSandbox
	expect(t, f.run(ctx, "experiment", "new", "boxed", "--b", "lean", "--task", "value", "--budget", "10"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "boxed"), ExitOK,
		"1 task(s) validated in another mode are validated again in the sandbox (sandbox-v1) when the experiment runs")
	run := f.run(ctx, "experiment", "run", "boxed")
	expect(t, run, ExitOK, "Validating 1 task(s) again in the sandbox (sandbox-v1)", "graded in the sandbox (sandbox-v1)")
	var lock experiment.Lock
	if err := json.Unmarshal(storedLock(t, f, "boxed"), &lock); err != nil {
		t.Fatal(err)
	}
	if lock.Grader != task.GraderSandbox || lock.Design.Grader != task.GraderSandbox || lock.Design.Version != experiment.DesignVersionSandbox {
		t.Errorf("the lock: grader %q, design grader %q, version %d", lock.Grader, lock.Design.Grader, lock.Design.Version)
	}
	runs := records(t, experimentRuns(t, f, "boxed"))
	if len(runs) == 0 {
		t.Fatalf("no runs:\n%s", run.stdout)
	}
	for _, r := range runs {
		if r.Grader != task.GraderSandbox || r.Sandbox == nil || r.Sandbox.Canary != task.CanaryPassed || r.Passed == nil || !*r.Passed {
			t.Errorf("run %s: grader %q, sandbox %+v, passed %v, notes %v", r.ID, r.Grader, r.Sandbox, r.Passed, r.Notes)
		}
	}
	if doc := jsonRun(t, f, ExitOK, "task", "validate", "value", "--snapshot", "lean"); doc.get("grader") != task.GraderSandbox {
		t.Errorf("task validate graded %v by default", doc.get("grader"))
	}
	expect(t, f.run(ctx, "experiment", "report", "boxed", "--markdown"), ExitOK, "Graded in Agentium's grading sandbox (sandbox-v1)")
}

// run once grades in the sandbox when asked (and by default on macOS): the hidden-test pass stays a pass, and the run
// says where it was graded and what the sandbox reported.
func TestRunOnceInTheSandbox(t *testing.T) {
	needGradingSandbox(t)
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt", false)
	expect(t, f.run(context.Background(), "run", "once", "value", "--grader", "sandbox"), ExitOK, "graded in the sandbox (sandbox-v1)",
		"outcome      ok; verification passed", "grading      in the sandbox (sandbox-v1), canary passed")
	doc := jsonRun(t, f, ExitOK, "run", "once", "value", "--grader", "sandbox")
	r, _ := doc.get("run").(map[string]any)
	if r["grader"] != task.GraderSandbox || r["sandbox"] == nil {
		t.Errorf("run once --json: %v", r)
	}
	assertKeys(t, r["sandbox"], "canary,denials,flagged")
}
