package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// versioned wraps a fake Claude Code so that --version prints version, as the real CLI does.
func versioned(t *testing.T, cli, version string) string {
	t.Helper()
	wrapper := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n[ \"$1\" = --version ] && { echo '" + version + " (Claude Code)'; exit 0; }\nexec '" + cli + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func TestExperimentNewPlanListAndRemove(t *testing.T) {
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	ctx := context.Background()
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nKeep it short.\n")
	expect(t, f.run(ctx, "context", "snapshot", "lean", "--working-tree"), ExitOK)
	gitIn(t, f.repo, "checkout", "--", "CLAUDE.md")
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")

	// Imported tasks need a review for solution leaks, then a validation in both contexts.
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean"), ExitError, "no task can be in this experiment yet",
		"value: its instruction needs a review for solution leaks (then: agentium task edit value --reviewed)")
	expect(t, f.run(ctx, "task", "edit", "value", "--reviewed"), ExitOK)
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value"), ExitError,
		"task value cannot be in this experiment: not validated (agentium task validate value --snapshot lean)")
	expect(t, f.run(ctx, "task", "validate", "value"), ExitOK)
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean"), ExitError, "value: not validated in context lean")
	expect(t, f.run(ctx, "task", "validate", "value", "--snapshot", "lean"), ExitOK)

	// The default: the Quick tier's sample of what is eligible, and a budget a quarter above the estimate (6 runs at
	// the default profile's $1.612 on claude-sonnet-5, rounded up).
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--seed", "7"), ExitOK,
		"Created experiment lean-ab: context A/B, A = base, B = lean, 1 task(s) × 3 run(s) per arm = 6 runs, budget $13.00",
		"note: the Quick tier asks for 12 tasks; only 1 can be in it", "agentium experiment plan lean-ab")
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean"), ExitError, "already exists")

	notReady := f.run(ctx, "experiment", "plan", "lean-ab")
	expect(t, notReady, ExitOK, "Experiment lean-ab: context A/B, A = base, B = lean", "tasks (1, seed 7): value",
		"ok       Claude Code 2.1.281", "MISSING  context base is not calibrated: agentium run calibrate --model claude-sonnet-5",
		"MISSING  context lean is not calibrated: agentium run calibrate --model claude-sonnet-5 --snapshot lean",
		"Not ready to run", "Quick", "Confident", "This experiment", "$9.67", "$18.00", "cost, success", "40–56%", "100+ pp", "94–100+ pp",
		"* only 1 task(s) can be in this experiment", "a default task run's tokens at claude-sonnet-5's list prices of 2026-09-29",
		"τ = 0.10–0.25")

	expect(t, f.run(ctx, "run", "calibrate", "--snapshot", "lean"), ExitOK)
	ready := f.run(ctx, "experiment", "plan", "lean-ab")
	expect(t, ready, ExitOK, "ok       context base calibrated", "ok       context lean calibrated", "first request 25000 tokens",
		"ok       1 task(s), each valid in every arm's context")
	if strings.Contains(ready.stdout, "MISSING") || strings.Contains(ready.stdout, "Not ready") {
		t.Errorf("a calibrated experiment is ready:\n%s", ready.stdout)
	}

	// Calibrations are per Claude Code version and model.
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.300")
	expect(t, f.run(ctx, "experiment", "plan", "lean-ab"), ExitOK, "ok       Claude Code 2.1.300",
		"context base was calibrated on Claude Code 2.1.281, not 2.1.300", "Not ready to run")
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
	expect(t, f.run(ctx, "experiment", "new", "opus", "--b", "lean", "--model", "claude-opus-5-5"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "opus"), ExitOK, "context base was calibrated with claude-sonnet-5, not claude-opus-5-5")

	// A/A: one context in both arms, checked once.
	aa := f.run(ctx, "experiment", "plan", "noise")
	expect(t, aa, ExitError, `experiment "noise": not found`)
	expect(t, f.run(ctx, "experiment", "new", "noise", "--template", "aa", "--a", "lean", "--task", "value", "--repeats", "2", "--budget", "5"), ExitOK,
		"A/A calibration of context lean, 1 task(s) × 2 run(s) per arm = 4 runs, budget $5.00")
	aa = f.run(ctx, "experiment", "plan", "noise")
	expect(t, aa, ExitOK, "arm A: context lean", "arm B: context lean",
		"WARNING  the budget $5.00 is below the estimated $6.45: expect it to stop the experiment early")
	if n := strings.Count(aa.stdout, "context lean calibrated"); n != 1 {
		t.Errorf("the shared context is checked %d times:\n%s", n, aa.stdout)
	}

	// Earlier fair task runs on the model replace the default profile (the fake agent reports $0.02 a run); an unfair
	// run (another tool set than the calibration's) and calibration runs are not counted.
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Edit","Read","Monitor"`, `"review"`, 25000, "")
	expect(t, f.run(ctx, "run", "once", "value"), ExitOK, "outcome      unfair")
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
	for range 3 {
		expect(t, f.run(ctx, "run", "once", "value"), ExitOK, "outcome      ok")
	}
	expect(t, f.run(ctx, "experiment", "plan", "lean-ab"), ExitOK, "the median of this project's 3 earlier task runs on claude-sonnet-5", "$0.12")

	// An alias has no list price: a budget must be given.
	expect(t, f.run(ctx, "experiment", "new", "alias", "--b", "lean", "--model", "sonnet"), ExitError, "set --budget")
	expect(t, f.run(ctx, "experiment", "new", "alias", "--b", "lean", "--model", "sonnet", "--budget", "20"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "alias"), ExitOK, "unknown", "sonnet has no list price")

	list := f.run(ctx, "experiment", "list")
	expect(t, list, ExitOK, "lean-ab", "context-ab", "base / lean", "1 × 3", "$13.00", "noise", "aa", "lean / lean", "1 × 2")
	expect(t, f.run(ctx, "experiment", "rm", "alias"), ExitOK, "Removed experiment alias")
	if list := f.run(ctx, "experiment", "list"); strings.Contains(list.stdout, "alias") {
		t.Errorf("removed experiment still listed:\n%s", list.stdout)
	}

	// Later changes to the tasks show up before running.
	expect(t, f.run(ctx, "task", "validate", "value"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "lean-ab"), ExitOK, "MISSING  task value: not validated in context lean", "Not ready")
	expect(t, f.run(ctx, "task", "rm", "value"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "lean-ab"), ExitOK, "task(s) removed since the experiment was made: value")
}

func TestExperimentNewUsage(t *testing.T) {
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	ctx := context.Background()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"lean-ab"}, "a context A/B needs --b SNAPSHOT"},
		{[]string{"noise", "--template", "aa", "--b", "lean"}, "drop --b"},
		{[]string{"x", "--b", "lean", "--tier", "huge"}, `unknown tier "huge"`},
		{[]string{"x", "--b", "lean", "--tier", "quick", "--task", "value"}, "use one"},
		{[]string{"bad name", "--b", "lean"}, "cannot name an experiment"},
		{[]string{"x", "--b", "lean", "--repeats", "-1"}, "--repeats must be positive"},
	} {
		expect(t, f.run(ctx, append([]string{"experiment", "new"}, c.args...)...), ExitUsage, c.want)
	}
	expect(t, f.run(ctx, "experiment", "new", "x", "--b", "nope"), ExitError, `snapshot "nope": not found`)
	expect(t, f.run(ctx, "experiment", "new", "x", "--template", "ab", "--b", "base"), ExitError)
	expect(t, f.run(ctx, "experiment", "list"), ExitOK, "No experiments yet")
	expect(t, f.run(ctx, "experiment", "rm", "x"), ExitError, "not found")
	expect(t, f.run(ctx, "experiment"), ExitUsage, "agentium experiment new NAME")
	expect(t, f.run(ctx, "experiment", "bogus"), ExitUsage, `unknown subcommand "bogus"`)
}
