package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
)

const (
	sonnet = "claude-sonnet-5"
	opus   = "claude-opus-5-5"
)

// modelCalibrator is calibratingAgent as a model reports itself: the calibration records what Claude Code said.
func modelCalibrator(t *testing.T, model string) string {
	t.Helper()
	script, err := os.ReadFile(calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""))
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte(strings.ReplaceAll(string(script), `"model":"`+sonnet+`"`, `"model":"`+model+`"`)), 0o755); err != nil {
		t.Fatal(err)
	}
	return versioned(t, cli, "2.1.281")
}

// calibrateOn calibrates the base context on model, then puts experimentAgent back as Claude Code.
func calibrateOn(t *testing.T, f runFixture, ctrl, model string) {
	t.Helper()
	f.vars["AGENTIUM_CLAUDE"] = modelCalibrator(t, model)
	expect(t, f.run(context.Background(), "run", "calibrate", "--model", model), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, ctrl)
}

func TestModelABDesignIsValidated(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--a", sonnet}, "needs --a MODEL[:EFFORT] and --b MODEL[:EFFORT]"},
		{[]string{"--b", opus}, "needs --a MODEL[:EFFORT] and --b MODEL[:EFFORT]"},
		{[]string{"--a", sonnet, "--b", sonnet}, "both arms run claude-sonnet-5"},
		{[]string{"--a", sonnet + ":high", "--b", sonnet + ":high"}, "both arms run claude-sonnet-5:high"},
		{[]string{"--a", sonnet, "--b", opus + ":huge"}, `--b "claude-opus-5-5:huge": unknown effort "huge" (use low, medium, high, xhigh, max)`},
		{[]string{"--a", ":high", "--b", opus}, "names no model"},
		{[]string{"--a", sonnet + ":", "--b", opus}, "names no effort after the colon"},
		{[]string{"--a", sonnet, "--b", opus, "--model", opus}, "not --model or --effort"},
		{[]string{"--a", sonnet, "--b", opus, "--effort", "high"}, "not --model or --effort"},
		{[]string{"--a", sonnet, "--b", opus, "--run-budget-a", "-1"}, "must be positive"},
		{[]string{"--a", sonnet, "--b", opus, "--context", "nope"}, `snapshot "nope": not found`},
	} {
		code := ExitUsage
		if strings.Contains(c.want, "not found") {
			code = ExitError
		}
		expect(t, f.run(ctx, append([]string{"experiment", "new", "m", "--template", "model-ab"}, c.args...)...), code, c.want)
	}
	// Context templates take no per-arm models, and --context belongs to model-ab alone.
	expect(t, f.run(ctx, "experiment", "new", "m", "--b", "lean", "--run-budget-a", "2"), ExitUsage, "belong to the model-ab template")
	expect(t, f.run(ctx, "experiment", "new", "m", "--template", "aa", "--context", "lean"), ExitUsage, "--context belongs to the model-ab template")
	expect(t, f.run(ctx, "experiment", "new", "m", "--template", "model-ab", "--a", sonnet, "--b", opus, "--b", opus, "--task", "value", "--context", "base",
		"--budget", "5"), ExitUsage, "below one pair of runs at their caps ($6.00)")

	// A valid one is stored with each arm's profile, on one context (the base's, or a snapshot).
	expect(t, f.run(ctx, "experiment", "new", "m", "--template", "model-ab", "--a", sonnet, "--b", opus+":high", "--task", "value", "--repeats", "2"),
		ExitOK, "Created experiment m: model A/B on context base, A = claude-sonnet-5, B = claude-opus-5-5:high, 1 task(s) × 2 run(s) per arm = 4 runs")
	expect(t, f.run(ctx, "experiment", "new", "lean-m", "--template", "model-ab", "--a", opus+":medium", "--b", opus+":high", "--context", "lean",
		"--task", "value", "--repeats", "1"), ExitOK, "model A/B on context lean, A = claude-opus-5-5:medium, B = claude-opus-5-5:high")
	expect(t, f.run(ctx, "experiment", "list"), ExitOK, "model-ab", "base / base", "claude-sonnet-5 / claude-opus-5-5:high", "lean / lean")
	d := loadDesign(t, f, "m")
	if d.Arms[0].Model != sonnet || d.Arms[0].Effort != "" || d.Arms[1].Model != opus || d.Arms[1].Effort != "high" || d.Model != sonnet || d.Version != 2 {
		t.Errorf("arms %+v, model %q", d.Arms, d.Model)
	}
}

// storedLock reads a stored experiment's lock.
func storedLock(t *testing.T, f runFixture, name string) []byte {
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
	return e.Lock
}

// loadDesign reads a stored experiment's design.
func loadDesign(t *testing.T, f runFixture, name string) experiment.Design {
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
	var d experiment.Design
	if err := json.Unmarshal(e.Design, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Each arm is estimated from the earlier runs on its own model, the caps and the budget hold both arms, and an arm's
// model without a calibration is named with the command that makes it.
func TestModelABPreviewEstimatesEachArmOnItsModel(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	id := taskIDs(t, f)["value"]
	var runs []store.Run
	for range 3 {
		runs = append(runs,
			store.Run{TaskID: id, TaskName: "value", Outcome: "ok", CostUSD: 0.4, Record: sonnetRecord},
			store.Run{TaskID: id, TaskName: "value", Outcome: "ok", CostUSD: 2, Record: []byte(`{"model":"` + opus + `","metrics":{"saw_result":true}}`)})
	}
	saveRuns(t, f, runs...)

	// Per run $0.40 on Sonnet and $2.00 on Opus: 2 repeats cost $4.80, and the default budget is 1.25 × that plus 3 caps
	// of the larger cap, $5, and a quarter more of Opus's calibration ($0.18): $22. The arms' caps are $3 and $5, so a pair's worst case is $8.
	expect(t, f.run(ctx, "experiment", "new", "m", "--template", "model-ab", "--a", sonnet, "--b", opus+":high", "--task", "value", "--repeats", "2",
		"--run-budget-b", "5"), ExitOK, "= 4 runs, budget $22.00")
	plan := f.run(ctx, "experiment", "plan", "m")
	expect(t, plan, ExitOK, "arm A: model claude-sonnet-5, effort the CLI's default, context base; each run up to $3.00",
		"arm B: model claude-opus-5-5, effort high, context base; each run up to $5.00",
		"Arm A: Estimated cost per run on claude-sonnet-5:", "value $0.40 (3 run(s))",
		"Arm B: Estimated cost per run on claude-opus-5-5 at effort high:", "value $2.00 (3 run(s))",
		"Worst case: every run reaches its cap (arm A $3.00, arm B $5.00)",
		"ok       context base on claude-opus-5-5 is not calibrated: calibrated when the experiment runs, about $",
		"ok       context base calibrated on claude-sonnet-5", "Calibration: 1 context calibration(s)")
	if !strings.Contains(plan.stdout, "This experiment     1          2     4      $4.80      $16.00") {
		t.Errorf("this experiment's row, want $4.80 and the worst case 2 × ($3 + $5) = $16.00:\n%s", plan.stdout)
	}
	if strings.Contains(plan.stdout, "WARNING  the budget") {
		t.Errorf("the default budget covers both arms' estimates and the reserve:\n%s", plan.stdout)
	}
	// A budget that covers one arm's estimate and the reserve but not both arms is flagged.
	expect(t, f.run(ctx, "experiment", "new", "tight", "--template", "model-ab", "--a", sonnet, "--b", opus, "--task", "value", "--repeats", "2", "--budget", "12"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "tight"), ExitOK,
		"WARNING  the budget $12.00 is below the estimated $4.80 plus $0.18 of calibration plus $9.00 held for runs in flight")

	// A calibration on the other model is none for this one; once there, the arm is ready, and the base's other
	// calibration is untouched.
	calibrateOn(t, f, ctrl, opus)
	// The newest calibration is now on Opus, but a Sonnet context A/B still finds its own.
	expect(t, f.run(ctx, "experiment", "new", "ctx", "--b", "lean", "--task", "value"), ExitOK)
	if ctxPlan := f.run(ctx, "experiment", "plan", "ctx"); strings.Contains(ctxPlan.stdout, "MISSING") || !strings.Contains(ctxPlan.stdout, "ok       context base calibrated") {
		t.Errorf("a Sonnet experiment must keep its calibration:\n%s", ctxPlan.stdout)
	}
	ready := f.run(ctx, "experiment", "plan", "m")
	expect(t, ready, ExitOK, "ok       context base calibrated on claude-opus-5-5", "ok       context base calibrated on claude-sonnet-5")
	if strings.Contains(ready.stdout, "MISSING") {
		t.Errorf("both models are calibrated:\n%s", ready.stdout)
	}

	// A model without a list price is flagged, here with no earlier runs to estimate from.
	expect(t, f.run(ctx, "experiment", "new", "alias", "--template", "model-ab", "--a", sonnet, "--b", "opus", "--task", "value", "--budget", "30"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "alias"), ExitOK, "WARNING  model opus has no list price in Agentium's table",
		"Arm B: Estimated cost per run on opus:", "opus has no list price in Agentium's table and fewer than 3 earlier task runs", "unknown")
	expect(t, f.run(ctx, "experiment", "new", "alias2", "--template", "model-ab", "--a", sonnet, "--b", "opus", "--task", "value"), ExitError,
		"set --budget", "arm B: opus has no list price")
}

// Every run uses its arm's model and effort, the lock records them, and a calibration per arm is what drift is checked
// against.
func TestModelABRunsEachArmOnItsOwnProfile(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	calibrateOn(t, f, ctrl, opus)
	expect(t, f.run(ctx, "experiment", "new", "m", "--template", "model-ab", "--a", sonnet+":low", "--b", opus+":high", "--task", "value", "--repeats", "3",
		"--seed", "5", "--budget", "40"), ExitOK)
	out := f.run(ctx, "experiment", "run", "m")
	expect(t, out, ExitOK, "Locked: Claude Code 2.1.281, A = claude-sonnet-5:low, B = claude-opus-5-5:high, sign-in login, 6 runs", "6 of 6 runs settled")
	runs := experimentRuns(t, f, "m")
	if len(runs) != 6 {
		t.Fatalf("%d runs", len(runs))
	}
	for _, r := range runs {
		got, err := os.ReadFile(filepath.Join(ctrl, "args-e1-s"+strconv.Itoa(r.Slot)+"-t1"))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"A": sonnet + " low", "B": opus + " high"}[r.Arm]
		if strings.TrimSpace(string(got)) != want {
			t.Errorf("slot %d (arm %s) ran with %q, want %q", r.Slot, r.Arm, got, want)
		}
		if r.Outcome != "ok" {
			t.Errorf("slot %d: outcome %s", r.Slot, r.Outcome)
		}
	}
	// The lock keeps each arm's model and effort, and the calibration of that model.
	show := f.run(ctx, "experiment", "show", "m")
	expect(t, show, ExitOK, "model A/B on context base", "A = claude-sonnet-5:low, B = claude-opus-5-5:high")
	var lock experiment.Lock
	stored := storedLock(t, f, "m")
	if err := json.Unmarshal(stored, &lock); err != nil {
		t.Fatal(err)
	}
	a, b := lock.Arms[0], lock.Arms[1]
	if a.Arm.Model != sonnet || a.Arm.Effort != "low" || b.Arm.Model != opus || b.Arm.Effort != "high" || a.Model != sonnet || b.Model != opus || a.Calibration == b.Calibration {
		t.Errorf("locked arms %+v / %+v", a, b)
	}
	// The report names the arms by profile where it would otherwise label both with the design's model.
	report := f.run(ctx, "experiment", "report", "m")
	expect(t, report, ExitOK, "Model A/B on context `base`: A = `claude-sonnet-5:low`, B = `claude-opus-5-5:high`.",
		"A claude-sonnet-5, B claude-opus-5-5, effort A low, B high")
}

func TestModelABChecksDriftPerArm(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	calibrateOn(t, f, ctrl, opus)
	expect(t, f.run(ctx, "experiment", "new", "m", "--template", "model-ab", "--a", sonnet, "--b", opus, "--task", "value", "--repeats", "2", "--seed", "5",
		"--budget", "40", "--concurrency", "1"), ExitOK)
	// Every run reports Sonnet: arm B's calibration saw Opus, so its run does not compare, and the experiment stops.
	if err := os.WriteFile(filepath.Join(ctrl, "report-model"), []byte(sonnet), 0o644); err != nil {
		t.Fatal(err)
	}
	out := f.run(ctx, "experiment", "run", "m")
	expect(t, out, ExitError, "Claude Code reported model claude-sonnet-5, but arm B's calibration saw claude-opus-5-5: later runs would not compare")
	if strings.Contains(out.stdout, "arm A's calibration") {
		t.Errorf("arm A's runs report its own model:\n%s", out.stdout)
	}
}

// Arm caps that are equal but below --run-budget run to completion within the budget.
func TestModelABEqualArmCapsRun(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	calibrateOn(t, f, ctrl, opus)
	expect(t, f.run(ctx, "experiment", "new", "m", "--template", "model-ab", "--a", sonnet, "--b", opus, "--task", "value", "--repeats", "2", "--seed", "5",
		"--run-budget-a", "1", "--run-budget-b", "1", "--budget", "4"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "m"), ExitOK, "each run up to $1.00 and", "Experiment m: done", "4 of 4 runs settled")
}

// An effort-only comparison gets one estimate per effort once earlier runs record theirs; runs without an effort
// (made before it was recorded) still estimate by model.
func TestModelABEstimatesByEffort(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	id := taskIDs(t, f)["value"]
	var runs []store.Run
	for range 3 {
		for effort, cost := range map[string]float64{"medium": 1, "high": 3} {
			runs = append(runs, store.Run{TaskID: id, TaskName: "value", Outcome: "ok", CostUSD: cost,
				Record: []byte(`{"model":"` + opus + `","effort":"` + effort + `","effort_recorded":true,"metrics":{"saw_result":true}}`)})
		}
	}
	saveRuns(t, f, runs...)
	expect(t, f.run(ctx, "experiment", "new", "e", "--template", "model-ab", "--a", opus+":medium", "--b", opus+":high", "--task", "value", "--repeats", "1",
		"--budget", "40"), ExitOK)
	expect(t, f.run(ctx, "experiment", "plan", "e"), ExitOK, "value $1.00 (3 run(s))", "value $3.00 (3 run(s))", "$4.00",
		"not split by model or effort")
}

// Runs from before efforts were recorded have an unknown effort: they fill in for an arm only while fewer than 3 runs
// match its effort, and the basis says so. Runs at another recorded effort never count.
func TestModelABUnrecordedEffortRunsFillIn(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	var runs []store.Run
	for range 20 { // $2 each, made before runs recorded their effort
		runs = append(runs, store.Run{TaskName: "gone", Outcome: "ok", CostUSD: 2, Record: []byte(`{"model":"` + opus + `","metrics":{"saw_result":true}}`)})
	}
	runs = append(runs, store.Run{TaskName: "gone", Outcome: "ok", CostUSD: 10,
		Record: []byte(`{"model":"` + opus + `","effort":"high","effort_recorded":true,"metrics":{"saw_result":true}}`)})
	saveRuns(t, f, runs...)
	expect(t, f.run(ctx, "experiment", "new", "e", "--template", "model-ab", "--a", opus+":medium", "--b", opus+":high", "--task", "value", "--repeats", "1",
		"--budget", "40"), ExitOK)
	plan := f.run(ctx, "experiment", "plan", "e")
	expect(t, plan, ExitOK, "the median of this project's 20 earlier task runs on claude-opus-5-5 at effort medium (20 of them from before runs recorded their effort",
		"the median of this project's 21 earlier task runs on claude-opus-5-5 at effort high (20 of them from before runs recorded their effort")
}

// A design whose version does not match its template is not one this Agentium reads: a model-ab design at version 1
// would run both arms on arm A's model in an older Agentium, and a context design at version 2 is not ours.
func TestLoadRefusesMismatchedDesignVersions(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	base := func(version, template, arms string) string {
		return `{"version":` + version + `,"template":"` + template + `","arms":` + arms + `,"tasks":["value"],"repeats":1,"model":"claude-sonnet-5","goal":"cheaper",` +
			`"cost_margin":0.1,"success_margin":0.15,"run_budget_usd":3,"budget_usd":30,"timeout":60000000000,"verify_timeout":60000000000,"concurrency":2,"seed":1}`
	}
	modelArms := `[{"name":"A","context":"base","requested_model":"claude-sonnet-5"},{"name":"B","context":"base","requested_model":"claude-opus-5-5"}]`
	contextArms := `[{"name":"A","context":"base"},{"name":"B","context":"base"}]`
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	for name, c := range map[string]struct{ template, design string }{
		"old-model": {"model-ab", base("1", "model-ab", modelArms)},
		"new-ctx":   {"aa", base("2", "aa", contextArms)},
		"good":      {"model-ab", base("2", "model-ab", modelArms)},
	} {
		if _, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: projects[0].ID, Name: name, Template: c.template, Design: []byte(c.design), CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	expect(t, f.run(ctx, "experiment", "plan", "old-model"), ExitError, "its design (version 1) is not one this Agentium reads")
	expect(t, f.run(ctx, "experiment", "plan", "new-ctx"), ExitError, "its design (version 2) is not one this Agentium reads")
	expect(t, f.run(ctx, "experiment", "plan", "good"), ExitOK, "model A/B")
}
