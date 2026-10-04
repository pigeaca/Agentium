package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// calibrationsLog is the models the fake agent was asked to calibrate on, one per calibration run, in order.
func calibrationsLog(t *testing.T, ctrl string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ctrl, "calibrations"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// openFixtureDB opens the fixture's database and its project.
func openFixtureDB(t *testing.T, f runFixture) (*store.Store, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	return db, projects[0].ID
}

// storedCalibrations is the models of the calibrations stored for a context, newest first.
func storedCalibrations(t *testing.T, f runFixture, contextName, snapshot string) []string {
	t.Helper()
	db, id := openFixtureDB(t, f)
	all, err := db.Calibrations(context.Background(), id, contextName, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, c := range all {
		var cal run.Calibration
		if err := json.Unmarshal(c.Result, &cal); err != nil {
			t.Fatal(err)
		}
		models = append(models, cal.RequestedModel)
	}
	return models
}

// An arm whose context has no calibration on the experiment's model is calibrated before the first pair, as run
// calibrate would have, and its cost counts in the spend; a calibrated arm, and a resume, are not calibrated again.
func TestExperimentCalibratesWhatItLacksBeforeItsFirstPair(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t) // both contexts are calibrated on Sonnet, not on Opus
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "ctx", "--b", "lean", "--model", opus, "--task", "value", "--repeats", "1", "--seed", "5",
		"--budget", "40", "--concurrency", "1"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "ctx"), ExitOK, "context base on "+opus+" is not calibrated: calibrated when the experiment runs, about $",
		"context lean on "+opus+" is not calibrated: calibrated when the experiment runs, about $", "Calibration: 2 context calibration(s)")

	got := f.run(ctx, "experiment", "run", "ctx")
	expect(t, got, ExitOK, "Calibrating 2 context(s) on "+opus, "Locked: Claude Code 2.1.281", "calibration $0.04 of it")
	if strings.Index(got.stdout, "Calibrating") > strings.Index(got.stdout, "Locked:") || strings.Index(got.stdout, "Locked:") > strings.Index(got.stdout, "[1/2]") {
		t.Errorf("the calibrations come before the lock and the first run:\n%s", got.stdout)
	}
	if log := calibrationsLog(t, ctrl); len(log) != 2 || log[0] != opus || log[1] != opus {
		t.Errorf("calibration runs on %v, want two on %s", log, opus)
	}
	// Recorded as run calibrate records them: one per context, on the model asked for, with the run's own record.
	if models := storedCalibrations(t, f, "base", ""); len(models) != 2 || !slices.Contains(models, opus) || !slices.Contains(models, sonnet) {
		t.Errorf("calibrations of the base context: %v, want one on Sonnet (earlier) and one on Opus", models)
	}
	// Their runs are stored runs: in the project's runs as calibrations, in the experiment's spend, not among its slots.
	db, id := openFixtureDB(t, f)
	e, err := db.ExperimentByName(ctx, id, "ctx")
	if err != nil {
		t.Fatal(err)
	}
	calRuns, err := db.ExperimentCalibrationRuns(ctx, e.ID)
	if err != nil || len(calRuns) != 2 {
		t.Fatalf("calibration runs of the experiment: %d, %v", len(calRuns), err)
	}
	if slots := experimentRuns(t, f, "ctx"); len(slots) != 2 {
		t.Errorf("%d slot runs, want 2 (1 task × 1 run × 2 arms)", len(slots))
	}
	expect(t, f.run(ctx, "experiment", "show", "ctx"), ExitOK, "spent $0.64 of $40.00 (calibration $0.04 of it, not in the arms' costs)")
	expect(t, f.run(ctx, "run", "list"), ExitOK, "(calibration)")

	// A resume (the experiment is locked) and a second experiment on the same model calibrate nothing.
	expect(t, f.run(ctx, "experiment", "run", "ctx"), ExitOK)
	expect(t, f.run(ctx, "experiment", "new", "again", "--b", "lean", "--model", opus, "--task", "value", "--repeats", "1", "--budget", "40"), ExitOK)
	plan := f.run(ctx, "experiment", "plan", "again")
	expect(t, plan, ExitOK, "ok       context base calibrated", "ok       context lean calibrated")
	if strings.Contains(plan.stdout, "Calibration:") {
		t.Errorf("a calibrated experiment still plans a calibration:\n%s", plan.stdout)
	}
	expect(t, f.run(ctx, "experiment", "run", "again"), ExitOK)
	if log := calibrationsLog(t, ctrl); len(log) != 2 {
		t.Errorf("calibration runs on %v: a calibrated arm was calibrated again", log)
	}
}

// A calibration that fails its checks stops the experiment before any task run, with the reasons run calibrate prints;
// nothing is locked, and a later run with a healthy agent calibrates and goes on.
func TestFailedCalibrationStopsTheExperimentBeforeAnyTaskRun(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "ctx", "--b", "lean", "--model", opus, "--task", "value", "--repeats", "1", "--seed", "5",
		"--budget", "40", "--concurrency", "1"), ExitOK)
	if err := os.WriteFile(filepath.Join(ctrl, "calibration-fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := f.run(ctx, "experiment", "run", "ctx")
	expect(t, got, ExitError, "Calibrating 2 context(s) on "+opus, "base  ok       FAILED", "a calibration on "+opus+" failed its checks", "the experiment did not start",
		"failed arms were not saved")
	if strings.Contains(got.stdout, "Locked:") || strings.Contains(got.stdout, "[1/2]") {
		t.Errorf("the experiment went on after a failed calibration:\n%s", got.stdout)
	}
	if _, started := paidRuns(t, f, ctrl); started != 0 || len(experimentRuns(t, f, "ctx")) != 0 {
		t.Errorf("a task run started (%d marks, %d slot runs)", started, len(experimentRuns(t, f, "ctx")))
	}
	expect(t, f.run(ctx, "experiment", "show", "ctx"), ExitOK, "not run yet")
	if models := storedCalibrations(t, f, "base", ""); slices.Contains(models, opus) {
		t.Errorf("a failed calibration was saved: %v", models)
	}
	// Its runs cost money, so they are stored; the experiment's budget will count them.
	db, id := openFixtureDB(t, f)
	e, _ := db.ExperimentByName(ctx, id, "ctx")
	if calRuns, err := db.ExperimentCalibrationRuns(ctx, e.ID); err != nil || len(calRuns) != 2 {
		t.Errorf("stored calibration runs: %d, %v; want the 2 that failed", len(calRuns), err)
	}

	if err := os.Remove(filepath.Join(ctrl, "calibration-fail")); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "run", "ctx"), ExitOK, "Calibrating 2 context(s)", "Locked:", "[2/2]")
	if models := storedCalibrations(t, f, "base", ""); !slices.Contains(models, opus) {
		t.Errorf("calibrations %v after the healthy retry", models)
	}
}

// A model A/B calibrates each model once, however many of its arms there are.
func TestModelABCalibratesBothModels(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	const sonnet5 = "claude-sonnet-5" // the fixture's calibrations are on the default model, not this one
	expect(t, f.run(ctx, "experiment", "new", "m", "--a", sonnet5, "--b", opus, "--task", "value", "--repeats", "1", "--seed", "5",
		"--budget", "40", "--concurrency", "1"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "m"), ExitOK, "context base on "+sonnet5+" is not calibrated: calibrated when the experiment runs",
		"context base on "+opus+" is not calibrated: calibrated when the experiment runs", "Calibration: 2 context calibration(s)")

	got := f.run(ctx, "experiment", "run", "m")
	expect(t, got, ExitOK, "Calibrating 1 context(s) on "+sonnet5, "Calibrating 1 context(s) on "+opus, "Locked:", "[2/2]")
	if log := calibrationsLog(t, ctrl); len(log) != 2 || log[0] != sonnet5 || log[1] != opus {
		t.Errorf("calibration runs on %v, want %s then %s", log, sonnet5, opus)
	}
	for _, model := range []string{sonnet5, opus} {
		if models := storedCalibrations(t, f, "base", ""); !slices.Contains(models, model) {
			t.Errorf("no calibration on %s: %v", model, models)
		}
	}
	// Both models' runs came out as their calibrations saw them, so no run was unfair or stopped the experiment.
	if runs := experimentRuns(t, f, "m"); len(runs) != 2 {
		t.Errorf("%d runs, want 2", len(runs))
	}
	// The report names each arm by profile in all three formats.
	label := "A (" + sonnet5 + ")"
	expect(t, f.run(ctx, "experiment", "report", "m"), ExitOK, label, "B ("+opus+")", "(calibration $0.04 of it)") // piped: Markdown
	expect(t, f.run(ctx, "experiment", "report", "m", "--markdown"), ExitOK, label, "B ("+opus+")")
	expect(t, f.run(ctx, "experiment", "report", "m", "--json"), ExitOK, `"profile": "`+opus+`"`, `"calibration_usd": 0.04`)
	*f.terminal = true
	expect(t, f.run(ctx, "experiment", "report", "m", "--details"), ExitOK, label, "B ("+opus+")") // the headlines and the noise note need two tasks: the report tests cover them
	*f.terminal = false
}

// run once checks a run against the calibration on its own model, not the newest of any.
func TestRunOnceUsesItsOwnModelsCalibration(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	ctx := context.Background()
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, "")
	expect(t, f.run(ctx, "run", "calibrate"), ExitOK) // Sonnet: three tools
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Edit","Read","Monitor"`, `"review"`, 25000, "")
	expect(t, f.run(ctx, "run", "calibrate", "--model", opus), ExitOK) // Opus, calibrated last: four tools

	// The agent has four tools. On Opus that is what its calibration found; on Sonnet it is a difference, which the
	// newest calibration (Opus's) would have hidden.
	own := f.run(ctx, "run", "once", "value", "--model", opus)
	expect(t, own, ExitOK, "Checking the environment against the calibration of base", "outcome      ok")
	if strings.Contains(own.stdout, "the calibration used") {
		t.Errorf("its own model's calibration was reported as another's:\n%s", own.stdout)
	}
	expect(t, f.run(ctx, "run", "once", "value", "--model", sonnet), ExitOK, "outcome      unfair", "tools differ (added Monitor; missing none)")
	// Without a calibration on its model, the newest of any is used with the note that says so, as before.
	expect(t, f.run(ctx, "run", "once", "value", "--model", "claude-haiku-5"), ExitOK, "note: the calibration used "+opus+", this run claude-haiku-5")
}

// A budget that cannot hold the calibrations and one pair at its caps is refused before anything is spent, naming the
// remedies; calibrations that failed earlier count, so a retried calibration is paid from the same budget.
func TestExperimentRefusesABudgetBelowItsCalibrations(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	// The pair's caps are $6.60 with their overshoot; two Opus calibrations may cost up to $1.60 more ($0.50 caps and
	// Opus 5.5's $0.30 overshoot floor).
	expect(t, f.run(ctx, "experiment", "new", "ctx", "--b", "lean", "--model", opus, "--task", "value", "--repeats", "1", "--budget", "7",
		"--concurrency", "1"), ExitOK)
	got := f.run(ctx, "experiment", "run", "ctx")
	expect(t, got, ExitUsage, "the budget $7.00 cannot hold the calibrations this experiment needs (up to $1.60", "one pair of runs at their caps ($6.60)",
		"--budget", "agentium run calibrate")
	if log := calibrationsLog(t, ctrl); len(log) != 0 {
		t.Errorf("a refused experiment spent on calibrations: %v", log)
	}
	expect(t, f.run(ctx, "experiment", "show", "ctx"), ExitOK, "not run yet")

	// Raised, it goes on: a calibration that fails costs money, and the retry counts the failed runs too.
	if err := os.WriteFile(filepath.Join(ctrl, "calibration-fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "run", "ctx", "--budget", "9"), ExitError, "failed its checks")
	show := f.run(ctx, "experiment", "show", "ctx")
	expect(t, show, ExitOK, "Calibration runs so far: $0.04 (in its budget)")
	if err := os.Remove(filepath.Join(ctrl, "calibration-fail")); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "run", "ctx", "--budget", "9"), ExitOK, "Calibrating 2 context(s)", "calibration $0.08 of it")
	// The failed and the healthy calibration runs all count in the budget.
	db, id := openFixtureDB(t, f)
	e, _ := db.ExperimentByName(ctx, id, "ctx")
	if calRuns, err := db.ExperimentCalibrationRuns(ctx, e.ID); err != nil || len(calRuns) != 4 {
		t.Errorf("calibration runs: %d, %v; want 2 failed and 2 healthy", len(calRuns), err)
	}
	run := f.run(ctx, "run", "list")
	var calID string
	for _, line := range strings.Split(run.stdout, "\n") {
		if strings.Contains(line, "(calibration)") {
			calID = strings.Fields(line)[0]
		}
	}
	expect(t, f.run(ctx, "run", "show", calID), ExitOK, "experiment   ctx (a calibration before its first pair)")
}

// A refusal that needs no calibration (here a Gradle project without the opt-in) comes before any calibration is paid for.
func TestRefusedExperimentSpendsNothingOnCalibrations(t *testing.T) {
	f, ctrl := experimentFixture(t) // not parallel: PATH is the test process's own
	ctx := context.Background()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gradle"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeFile(t, f.repo, "build.gradle", "\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "build file")
	writeFile(t, f.repo, "tests/tool_test.sh", "grep -q second value.txt\n")
	writeFile(t, f.repo, "value.txt", "new second\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "Make the value second")
	expect(t, f.run(ctx, "task", "import", "--commit", "HEAD", "--name", "tool", "--verify", "sh run_tests.sh"), ExitOK)
	expect(t, f.run(ctx, "task", "edit", "tool", "--reviewed"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "tool", "--snapshot", "lean"), ExitOK, "valid")
	expect(t, f.run(ctx, "experiment", "new", "g", "--b", "lean", "--model", opus, "--task", "tool", "--repeats", "1", "--budget", "40"), ExitOK)
	before, _ := paidRuns(t, f, ctrl)
	expect(t, f.run(ctx, "experiment", "run", "g"), ExitError, "agentium init --allow-local-binding")
	if log := calibrationsLog(t, ctrl); len(log) != 0 {
		t.Errorf("a refused experiment calibrated: %v", log)
	}
	if n, _ := paidRuns(t, f, ctrl); n != before {
		t.Errorf("%d run(s) stored by a refused experiment", n-before)
	}
}

// A calibration run that cannot finish (an error from the calibrator, not an unhealthy result) also stops the experiment
// before it locks; the experiment is not left half-run, and a later run calibrates and goes on.
func TestCalibrationErrorStopsTheExperimentBeforeTheLock(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "ctx", "--b", "lean", "--model", opus, "--task", "value", "--repeats", "1", "--seed", "5",
		"--budget", "40", "--concurrency", "1"), ExitOK)
	if err := os.WriteFile(filepath.Join(ctrl, "calibration-crash"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := f.run(ctx, "experiment", "run", "ctx")
	if got.code != ExitError || strings.Contains(got.stdout, "Locked:") || strings.Contains(got.stdout, "[1/2]") {
		t.Errorf("exit %d after a calibration that could not finish:\n%s\n%s", got.code, got.stdout, got.stderr)
	}
	expect(t, f.run(ctx, "experiment", "show", "ctx"), ExitOK, "not run yet")
	if models := storedCalibrations(t, f, "base", ""); slices.Contains(models, opus) || len(experimentRuns(t, f, "ctx")) != 0 {
		t.Errorf("a calibration was saved or a task ran after the error: %v", models)
	}
	if err := os.Remove(filepath.Join(ctrl, "calibration-crash")); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "run", "ctx"), ExitOK, "Calibrating 2 context(s)", "Locked:")
}
