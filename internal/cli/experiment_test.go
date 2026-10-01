package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
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
	t.Parallel()
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
	expect(t, f.run(ctx, "task", "validate", "value", "--repeat", "3"), ExitOK)
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean"), ExitError, "value: not validated in context lean")
	expect(t, f.run(ctx, "task", "validate", "value", "--snapshot", "lean", "--repeat", "3"), ExitOK)

	// The default: the Quick tier's sample of what is eligible, and a budget a quarter above the estimate (6 runs at
	// the default profile's $1.612 on claude-sonnet-5) plus 3 caps of $3 held for runs in flight, rounded up.
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--seed", "7"), ExitOK,
		"Created experiment lean-ab: context A/B, A = base, B = lean, 1 task(s) × 3 run(s) per arm = 6 runs, budget $22.00",
		"note: the Quick tier asks for 12 tasks; only 1 can be in it", "agentium experiment plan lean-ab")
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean"), ExitError, "already exists")

	notReady := f.run(ctx, "experiment", "plan", "lean-ab")
	expect(t, notReady, ExitOK, "Experiment lean-ab: context A/B, A = base, B = lean", "tasks (1, seed 7): value",
		"ok       Claude Code 2.1.281", "MISSING  context base is not calibrated: agentium run calibrate --model claude-sonnet-5",
		"MISSING  context lean is not calibrated: agentium run calibrate --model claude-sonnet-5 --snapshot lean",
		"Not ready to run", "Quick", "Confident", "This experiment", "$9.67", "$18.00", "cost, success", "40–56%", "100+ pp", "94–100+ pp",
		"* only 1 task(s) can be in this experiment", "Estimated cost per run on claude-sonnet-5:",
		"value, without runs of their own: $1.61, a default task run's tokens at claude-sonnet-5's list prices of 2026-09-29",
		"τ = 0.10–0.25", "the study assumed 0.05 for success",
		"note: at this size the no-loss guard certifies only about 94–100+ pp, wider than the 15 pp success margin")
	if strings.Contains(notReady.stdout, "WARNING") {
		t.Errorf("the default budget covers the estimate and the reserve:\n%s", notReady.stdout)
	}

	expect(t, f.run(ctx, "run", "calibrate", "--snapshot", "lean"), ExitOK)
	ready := f.run(ctx, "experiment", "plan", "lean-ab")
	expect(t, ready, ExitOK, "ok       context base calibrated", "ok       context lean calibrated", "first request 25000 tokens",
		"ok       1 task(s), each valid in every arm's context",
		"Floors (method phase1-v2): verdicts on cost need 8 tasks with 1 or more runs per arm, and on success 20 tasks with 3 or more;")
	if strings.Contains(ready.stdout, "MISSING") || strings.Contains(ready.stdout, "Not ready") {
		t.Errorf("a calibrated experiment is ready:\n%s", ready.stdout)
	}

	// A validation that ran each stage fewer than 3 times only warns, and names the command that fixes it.
	expect(t, f.run(ctx, "task", "validate", "value", "--snapshot", "lean"), ExitOK)
	few := f.run(ctx, "experiment", "plan", "lean-ab")
	expect(t, few, ExitOK, "WARNING  validated with fewer than 3 repeats", "value (agentium task validate NAME --repeat 3 --snapshot lean)",
		"ok       1 task(s), each valid in every arm's context")
	if strings.Contains(few.stdout, "MISSING") || strings.Contains(few.stdout, "Not ready") {
		t.Errorf("few repeats is a warning, not a gate:\n%s", few.stdout)
	}
	expect(t, f.run(ctx, "task", "validate", "value", "--snapshot", "lean", "--repeat", "3"), ExitOK)
	if again := f.run(ctx, "experiment", "plan", "lean-ab"); strings.Contains(again.stdout, "fewer than 3 repeats") {
		t.Errorf("3 repeats should not warn:\n%s", again.stdout)
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
	expect(t, f.run(ctx, "experiment", "new", "noise", "--template", "aa", "--a", "lean", "--task", "value", "--repeats", "2", "--budget", "5"), ExitUsage,
		"the budget $5.00 is below one pair of runs at their caps ($6.00)")
	expect(t, f.run(ctx, "experiment", "new", "noise", "--template", "aa", "--a", "lean", "--task", "value", "--repeats", "2", "--budget", "8"), ExitOK,
		"A/A calibration of context lean, 1 task(s) × 2 run(s) per arm = 4 runs, budget $8.00")
	aa = f.run(ctx, "experiment", "plan", "noise")
	expect(t, aa, ExitOK, "arm A: context lean", "arm B: context lean",
		"WARNING  the budget $8.00 is below the estimated $6.45 plus $9.00 held for runs in flight: expect it to stop the experiment early")
	if n := strings.Count(aa.stdout, "context lean calibrated"); n != 1 {
		t.Errorf("the shared context is checked %d times:\n%s", n, aa.stdout)
	}

	// The calibration's sign-in must be the runs'.
	f.vars["ANTHROPIC_API_KEY"] = "sk-test-not-real" // secret-scan: allow
	expect(t, f.run(ctx, "experiment", "plan", "lean-ab"), ExitOK, "context base was calibrated with sign-in login, and runs would now use api-key")
	delete(f.vars, "ANTHROPIC_API_KEY")

	// The task's own earlier fair runs on the model replace the default profile (the fake agent reports $0.02 a run);
	// an unfair run (another tool set than the calibration's), a run stopped before its result, a run without a cost and
	// calibration runs are not counted.
	f.vars["AGENTIUM_CLAUDE"] = calibratingAgent(t, `"Bash","Edit","Read","Monitor"`, `"review"`, 25000, "")
	expect(t, f.run(ctx, "run", "once", "value"), ExitOK, "outcome      unfair")
	saveRuns(t, f,
		store.Run{TaskName: "value", Outcome: "timeout", CostUSD: 5, Record: []byte(`{"model":"claude-sonnet-5","metrics":{"saw_result":false}}`)},
		store.Run{TaskName: "value", Outcome: "ok", CostUSD: 0, Record: sonnetRecord})
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
	for range 3 {
		expect(t, f.run(ctx, "run", "once", "value"), ExitOK, "outcome      ok")
	}
	estimated := f.run(ctx, "experiment", "plan", "lean-ab")
	expect(t, estimated, ExitOK, "from each task's own earlier runs (their median): value $0.02 (3 run(s))", "$0.12")
	if strings.Contains(estimated.stdout, "without runs of their own") {
		t.Errorf("every task has runs of its own:\n%s", estimated.stdout)
	}

	// An alias has no list price: a budget must be given.
	expect(t, f.run(ctx, "experiment", "new", "alias", "--b", "lean", "--model", "sonnet"), ExitError, "set --budget")
	expect(t, f.run(ctx, "experiment", "new", "alias", "--b", "lean", "--model", "sonnet", "--budget", "20"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "alias"), ExitOK, "unknown", "sonnet has no list price")

	list := f.run(ctx, "experiment", "list")
	expect(t, list, ExitOK, "lean-ab", "context-ab", "base / lean", "1 × 3", "$22.00", "noise", "aa", "lean / lean", "1 × 2")
	expect(t, f.run(ctx, "experiment", "rm", "alias"), ExitOK, "Removed experiment alias")
	if list := f.run(ctx, "experiment", "list"); strings.Contains(list.stdout, "alias") {
		t.Errorf("removed experiment still listed:\n%s", list.stdout)
	}

	// Later changes to the tasks show up before running.
	expect(t, f.run(ctx, "task", "validate", "value"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "lean-ab"), ExitOK, "MISSING  task value: not validated in context lean", "Not ready",
		"Quick               0*         3     0", "-  no tasks")
	expect(t, f.run(ctx, "task", "rm", "value"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "lean-ab"), ExitOK, "task(s) removed since the experiment was made: value")
}

// sonnetRecord is a stored run's record as cost estimates read it: a fair run on claude-sonnet-5 that reported its cost.
var sonnetRecord = []byte(`{"model":"claude-sonnet-5","metrics":{"saw_result":true}}`)

// taskIDs maps the fixture's tasks to their IDs, which link a stored run to its task.
func taskIDs(t *testing.T, f runFixture) map[string]int64 {
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
	tasks, err := db.Tasks(ctx, projects[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, task := range tasks {
		ids[task.Name] = task.ID
	}
	return ids
}

// saveRuns stores earlier runs in the fixture's project, as estimates find them: an arm, IDs and times are filled in.
func saveRuns(t *testing.T, f runFixture, runs ...store.Run) {
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
	stored, err := db.Runs(ctx, projects[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, r := range runs {
		r.ID, r.ProjectID, r.Started, r.Finished = fmt.Sprintf("20260929T000000Z-%06d", len(stored)+i), projects[0].ID, now, now
		if r.Arm == "" {
			r.Arm = "base"
		}
		if err := db.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

// Each task is estimated from its own earlier runs on the model when it has some, and the others from the project's
// median: the 16-run A/B's new tasks cost twice that median. The default budget and the warning follow the tasks'
// estimates, and the preview says which basis each task used.
func TestExperimentEstimatesEachTaskFromItsOwnRuns(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "task", "import", "--commit", "HEAD", "--name", "costly", "--verify", "sh run_tests.sh"), ExitOK)
	expect(t, f.run(ctx, "task", "edit", "costly", "--reviewed"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "costly"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "costly", "--snapshot", "lean"), ExitOK)
	ids := taskIDs(t, f)
	var runs []store.Run
	for _, r := range []struct {
		task string
		id   int64
		cost float64
	}{
		{"costly", ids["costly"], 1.6}, {"costly", ids["costly"], 2.0},
		{"gone", 0, 0.2}, {"gone", 0, 0.3}, {"gone", 0, 0.4}, // a removed task's runs
		{"value", 0, 0.4}, // a removed task's run under value's name: not value's own
	} { // the project's median: $0.40
		runs = append(runs, store.Run{TaskID: r.id, TaskName: r.task, Outcome: "ok", CostUSD: r.cost, Record: sonnetRecord})
	}
	// value's own run on another model: no use on claude-sonnet-5.
	runs = append(runs, store.Run{TaskID: ids["value"], TaskName: "value", Outcome: "ok", CostUSD: 5,
		Record: []byte(`{"model":"claude-opus-5-5","metrics":{"saw_result":true}}`)})
	saveRuns(t, f, runs...)

	// value $0.40 and costly $1.80 a run, 2 runs each: $4.40, so 1.25 × $4.40 + 3 caps of $3 held for runs in flight,
	// rounded up. The project's median alone gave $1.60 and a budget of $11.
	expect(t, f.run(ctx, "experiment", "new", "mixed", "--b", "lean", "--task", "value", "--task", "costly", "--repeats", "1"), ExitOK,
		"2 task(s) × 1 run(s) per arm = 4 runs, budget $15.00")
	plan := f.run(ctx, "experiment", "plan", "mixed")
	expect(t, plan, ExitOK, "Estimated cost per run on claude-sonnet-5:",
		"from each task's own earlier runs (their median): costly $1.80 (2 run(s))",
		"value, without runs of their own: $0.40, the median of this project's 6 earlier task runs on claude-sonnet-5",
		"The tiers' estimates average the 2 eligible task(s), each estimated the same way: $1.10 a run.")
	// The experiment sums its tasks' estimates; Quick would draw 2 tasks × 3 runs per arm at their average, $1.10 a run.
	if row := armRow(plan.stdout, "Quick"); len(row) < 6 || row[4] != "$13.20" || row[5] != "$36.00" {
		t.Errorf("Quick row = %v, want $13.20 and the worst case $36.00:\n%s", row, plan.stdout)
	}
	if !strings.Contains(plan.stdout, "This experiment     2          1     4      $4.40      $12.00") {
		t.Errorf("this experiment's row, want $4.40 and the worst case $12.00:\n%s", plan.stdout)
	}
	if strings.Contains(plan.stdout, "the budget $") { // the budget's own warning; validation warnings may appear
		t.Errorf("the default budget covers the estimate and the reserve:\n%s", plan.stdout)
	}
	// The tiers draw from every eligible task, also those outside the experiment: the note shows the average they use.
	expect(t, f.run(ctx, "experiment", "new", "solo", "--b", "lean", "--task", "value", "--repeats", "1"), ExitOK)
	solo := f.run(ctx, "experiment", "plan", "solo")
	expect(t, solo, ExitOK, "value, without runs of their own: $0.40", "average the 2 eligible task(s), each estimated the same way: $1.10 a run.")
	if strings.Contains(solo.stdout, "costly $1.80") {
		t.Errorf("only the experiment's own tasks are listed:\n%s", solo.stdout)
	}
	// A budget set by hand below the tasks' estimates plus the reserve is flagged.
	expect(t, f.run(ctx, "experiment", "new", "tight", "--b", "lean", "--task", "value", "--task", "costly", "--repeats", "1", "--budget", "12"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "tight"), ExitOK,
		"WARNING  the budget $12.00 is below the estimated $4.40 plus $9.00 held for runs in flight")
}

func TestExperimentNewUsage(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "list"), ExitOK, "No experiments yet")
	expect(t, f.run(ctx, "experiment", "rm", "x"), ExitError, "not found")
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
	expect(t, f.run(ctx, "context", "snapshot", "base"), ExitUsage, `"base" names each task's own context`)
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nKeep it short.\n")
	expect(t, f.run(ctx, "context", "snapshot", "lean", "--working-tree"), ExitOK)
	expect(t, f.run(ctx, "task", "edit", "value", "--reviewed"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "value", "--snapshot", "lean"), ExitOK)
	expect(t, f.run(ctx, "experiment", "new", "x", "--b", "lean", "--task", "value", "--task", "value", "--budget", "20"), ExitUsage, "listed twice")
	expect(t, f.run(ctx, "experiment", "new", "x", "--b", "lean", "--task", "nope", "--budget", "20"), ExitError, `task "nope": not found`)
	expect(t, f.run(ctx, "experiment", "new", "x", "--b", "lean", "--concurrency", "99"), ExitUsage, "concurrency must be 1 to 8")

	// Readiness when Claude Code cannot tell its version, and when a snapshot commit is gone.
	broken := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.vars["AGENTIUM_CLAUDE"] = broken
	expect(t, f.run(ctx, "experiment", "new", "x", "--b", "lean"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "x"), ExitOK, "MISSING  Claude Code at "+broken+": its version could not be read", "Not ready")
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	design := `{"version":1,"template":"context-ab","arms":[{"name":"A","context":"base"},{"name":"B","context":"lean","snapshot":"` +
		strings.Repeat("ab", 20) + `"}],"tasks":["value"],"repeats":3,"model":"claude-sonnet-5","goal":"cheaper","cost_margin":0.1,` +
		`"success_margin":0.15,"run_budget_usd":3,"budget_usd":30,"timeout":60000000000,"verify_timeout":60000000000,"concurrency":2,"seed":1}`
	if _, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: projects[0].ID, Name: "gone", Template: "context-ab", Design: []byte(design), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	expect(t, f.run(ctx, "experiment", "plan", "gone"), ExitOK, "context lean: its snapshot commit abababababab is gone from Agentium's repository")
	expect(t, f.run(ctx, "experiment", "new", "x", "--template", "ab", "--b", "base"), ExitUsage, `unknown template "ab"`)
	expect(t, f.run(ctx, "experiment"), ExitUsage, "agentium experiment new NAME")
	expect(t, f.run(ctx, "experiment", "bogus"), ExitUsage, `unknown subcommand "bogus"`)
}
