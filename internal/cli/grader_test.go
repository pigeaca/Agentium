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
	"github.com/pigeaca/agentium/internal/run"
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

// container-v1 is named but not graded in until the containers plan's step 4: --grader refuses it at every entry
// point as it refuses an unknown mode (same words, same exit code), and a lock that names it is refused on resume,
// never graded on the host or in the sandbox instead.
func TestContainerGraderIsRefusedAsUnknown(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	for _, flag := range []string{"container", task.GraderContainer} {
		want := `--grader "` + flag + `": use host or sandbox`
		expect(t, f.run(ctx, "experiment", "new", "c-"+flag, "--b", "lean", "--task", "value", "--grader", flag), ExitUsage, want)
		expect(t, f.run(ctx, "run", "once", "value", "--grader", flag), ExitUsage, want)
		expect(t, f.run(ctx, "task", "validate", "value", "--grader", flag), ExitUsage, want)
	}

	expect(t, f.run(ctx, "experiment", "new", "locked", "--b", "lean", "--task", "value", "--budget", "10", "--grader", "host"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "locked"), ExitOK)
	var lock map[string]any
	if err := json.Unmarshal(storedLock(t, f, "locked"), &lock); err != nil {
		t.Fatal(err)
	}
	lock["grader"] = task.GraderContainer
	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	amendLock(t, f, "locked", encoded)
	before := len(records(t, experimentRuns(t, f, "locked")))
	expect(t, f.run(ctx, "experiment", "run", "locked"), ExitError, "graded in container-v1, which this Agentium does not grade in")
	if after := len(records(t, experimentRuns(t, f, "locked"))); after != before {
		t.Errorf("a container lock resumed: %d runs, then %d", before, after)
	}
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

// storedTask reads task name from the fixture's database.
func storedTask(t *testing.T, f runFixture, name string) store.Task {
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
	return tk
}

// harmlessTask makes the experiment fixture's task "value" call the security server in its verification (a Mach lookup
// the grading sandbox denies and flags) and stores its earlier validation again, as one made in the sandbox whose
// reference logged harmless while passing.
func harmlessTask(t *testing.T, f runFixture, harmless []task.DenialKey) {
	t.Helper()
	ctx := context.Background()
	v := task.ValidationOf(storedTask(t, f, "value"))
	expect(t, f.run(ctx, "task", "edit", "value", "--verify",
		"/usr/bin/security find-generic-password -s agentium-made-up-service >/dev/null 2>&1; sh run_tests.sh"), ExitOK)
	v.Grader, v.Harmless = task.GraderSandbox, harmless
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	tk := storedTask(t, f, "value")
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ok, err := db.SetTaskValidation(ctx, tk.ID, tk.Verify, tk.Setup, encoded, time.Now()); err != nil || !ok {
		t.Fatalf("store the validation: %v, %v", ok, err)
	}
}

// skipUnreadDenials skips when a run's denials could not be read (the log lagged): what the test asserts of them is
// unknown then.
func skipUnreadDenials(t *testing.T, recs ...run.Record) {
	t.Helper()
	for _, r := range recs {
		if r.Sandbox != nil && r.Sandbox.Unread != "" {
			t.Skipf("LOG-BLIND: the unified log did not show the sandbox's denials here (in time): %s", r.Sandbox.Unread)
		}
	}
}

// The task's harmless denials reach its grades: through the experiment's lock to each run of a sandbox experiment, and
// from the task's validation to run once. An agent that fails the hidden test, whose verification makes the lookup
// the reference made too, fails: the lookup is harmless, not a reason to leave the run out.
func TestHarmlessDenialsReachTheRuns(t *testing.T) {
	needGradingSandbox(t)
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	*f.grader = task.GraderSandbox
	harmlessTask(t, f, []task.DenialKey{{Operation: "mach-lookup", Target: "com.apple.SecurityServer"}})
	writeFile(t, ctrl, "no-change", "")

	once := jsonRun(t, f, ExitOK, "run", "once", "value", "--grader", "sandbox")
	id, _ := once.get("run").(map[string]any)["id"].(string)
	var rec run.Record
	for _, r := range records(t, runsOf(t, f)) {
		if r.ID == id {
			rec = r
		}
	}
	skipUnreadDenials(t, rec)
	if rec.Outcome != "ok" || rec.Passed == nil || *rec.Passed || rec.Sandbox == nil || rec.Sandbox.Harmless == 0 || rec.Sandbox.FlaggedCount != 0 {
		t.Errorf("run once: %s, passed %v, sandbox %+v", rec.Outcome, rec.Passed, rec.Sandbox)
	}

	expect(t, f.run(ctx, "experiment", "new", "boxed", "--b", "lean", "--task", "value", "--budget", "10"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "boxed"), ExitOK)
	var lock experiment.Lock
	if err := json.Unmarshal(storedLock(t, f, "boxed"), &lock); err != nil {
		t.Fatal(err)
	}
	if got := lock.Harmless["value"]; len(got) != 1 || got[0].Target != "com.apple.SecurityServer" {
		t.Errorf("the lock's harmless denials: %v", lock.Harmless)
	}
	runs := records(t, experimentRuns(t, f, "boxed"))
	skipUnreadDenials(t, runs...)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	for _, r := range runs {
		if r.Outcome != "ok" || r.Passed == nil || *r.Passed || r.Sandbox == nil || r.Sandbox.Harmless == 0 || r.Sandbox.FlaggedCount != 0 {
			t.Errorf("run %s: %s, passed %v, sandbox %+v", r.ID, r.Outcome, r.Passed, r.Sandbox)
		}
	}
}

// runsOf is every stored run of the fixture's project.
func runsOf(t *testing.T, f runFixture) []store.Run {
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
	runs, err := db.Runs(ctx, projects[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// Runs left out for flagged sandbox denials have their own count, in experiment show and its JSON, apart from
// infrastructure failures; each was run once.
func TestLeftOutRunsShowInProgress(t *testing.T) {
	needGradingSandbox(t)
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	*f.grader = task.GraderSandbox
	harmlessTask(t, f, nil)
	writeFile(t, ctrl, "no-change", "")
	expect(t, f.run(ctx, "experiment", "new", "boxed", "--b", "lean", "--task", "value", "--budget", "10"), ExitOK)
	f.run(ctx, "experiment", "run", "boxed")
	runs := records(t, experimentRuns(t, f, "boxed"))
	skipUnreadDenials(t, runs...)
	if len(runs) != 2 {
		t.Fatalf("%d runs, want one per slot", len(runs))
	}
	for _, r := range runs {
		if r.Outcome != "infra-sandbox" {
			t.Errorf("run %s: %s, sandbox %+v", r.ID, r.Outcome, r.Sandbox)
		}
	}
	expect(t, f.run(ctx, "experiment", "show", "boxed"), ExitOK, "arm A: 1 run(s) left out for sandbox denials", "arm B: 1 run(s) left out for sandbox denials")
	show := jsonRun(t, f, ExitOK, "experiment", "show", "boxed")
	arms, _ := show.get("progress").(map[string]any)["arms"].([]any)
	for _, a := range arms {
		if m := a.(map[string]any); m["left_out_sandbox"] != float64(1) || m["infra"] != float64(0) {
			t.Errorf("arm %v", m)
		}
	}
}
