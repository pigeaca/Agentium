package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/stats"
)

const (
	startKeys           = "calibration_estimate_usd,calibration_runs_needed,command,context_a,context_b,experiment,log,north_star,nothing_was_run,project,readiness,ready,run,run_command,schema,status,tasks_awaiting_review,tasks_ready"
	startExperimentKeys = "budget_usd,looks,method,model,name,repeats_per_arm,runs,spend,tasks,template"
	experimentKeys      = "arms,budget_usd,concurrency,goal,grader,judge,judge_pairs,method,name,repeats_per_arm,run_budget_usd,runs,tasks,template"
	expArmKeys          = "context,effort,model,name"
	lookKeys            = "analysed,conditional_power,decision,interval,level,look,note,tasks_counted,tasks_planned,verdict"
	intervalKeys        = "estimate,high,low"
	spendKeys           = "estimate_basis,expected_tasks,expected_usd,if_cut_usd,known,max_usd,worst_case_usd"
	estimateBasisKeys   = "arm,basis,history_runs,model,per_run_usd"
	planKeys            = "calibration_estimate_usd,calibration_runs_needed,command,eligible_tasks,experiment,ineligible_tasks,looks,readiness,ready,schema,sizes,spend,usage"
	usageKeys           = "latest,limit,models,runs,windows"
	usageModelKeys      = "measured_runs,model,per_run,runs"
	usageLatestKeys     = "age_seconds,current,fits,read_at,resets_at,used"
	lookPlanKeys        = "efficacy_level,equivalence_level,estimated_usd,look,runs,tasks,worst_case_usd"
	runResultKeys       = "budget_usd,ended_by,judge_paused,looks,method,next_command,north_star,note,resume_at,runs,spent_usd,status,stopped_at_look,verdict"
	verdictKeys         = "decisive,metrics,summary"
	metricKeys          = "a,b,decisive,interval,level,metric,note,role,tasks,verdict"
	progressKeys        = "arms,budget_usd,calibration_usd,ended_by,judge_usd,looks,pair_judge_usd,settled,slots,spent_usd,uncompared_pairs,unjudged_runs"
	armProgKeys         = "cancelled,context,cost_usd,fair,infra,name,settled,successes,unfair"
	lockKeys            = "budget_changes,claude_code,local_binding,locked_at,method,price_table,sign_in"
)

// Every experiment command's document has the keys its schema fixes, nested objects included, in the states a script
// meets: before the first run, after a seq-v1 experiment stopped early at its first look, and for a fixed design.
func TestJSONExperimentCommandKeys(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	control(t, ctrl, map[string]string{"cost-lean": "0.15", "cost-jitter": ""})

	empty := jsonRun(t, f, ExitOK, "experiment", "list")
	assertKeys(t, empty.doc, "command,experiments,schema")
	if got, ok := empty.get("experiments").([]any); !ok || len(got) != 0 {
		t.Errorf("experiment list with none: %s", empty.stdout)
	}

	created := jsonRun(t, f, ExitOK, "experiment", "new", "lean-seq", "--b", "lean", "--seed", "5")
	assertKeys(t, created.doc, "command,eligible_tasks,experiment,notes,plan_command,schema")
	assertKeys(t, created.get("experiment"), experimentKeys)
	assertKeys(t, created.get("experiment", "arms").([]any)[0], expArmKeys)
	if created.get("experiment", "method") != "seq-v1" || created.get("experiment", "runs") != float64(32) || created.get("plan_command") != "agentium experiment plan lean-seq" {
		t.Errorf("experiment new: %s", created.stdout)
	}

	// A task waiting for review is not eligible: the plan says so, with its reason.
	expect(t, f.run(context.Background(), "task", "import", "--commit", "HEAD", "--name", "by-hand", "--verify", "true"), ExitOK)
	plan := jsonRun(t, f, ExitOK, "experiment", "plan", "lean-seq")
	if ineligible, _ := plan.get("ineligible_tasks").([]any); len(ineligible) == 0 {
		t.Errorf("an unreviewed task is eligible: %s", plan.stdout)
	} else {
		assertKeys(t, ineligible[0], "reason,task")
	}
	assertKeys(t, plan.doc, planKeys)
	assertKeys(t, plan.get("spend"), spendKeys)
	assertKeys(t, plan.get("spend", "estimate_basis").([]any)[0], estimateBasisKeys)
	assertKeys(t, plan.get("usage"), usageKeys)
	assertKeys(t, plan.get("usage", "models").([]any)[0], usageModelKeys)
	looks := plan.get("looks").([]any)
	assertKeys(t, looks[0], lookPlanKeys)
	if len(looks) != 3 || len(plan.get("sizes").([]any)) != 0 || plan.get("spend", "max_usd") == nil || math.Abs(plan.get("spend", "worst_case_usd").(float64)-16*2*3.3) > 1e-9 || // 16 pairs at $3 caps and their $0.30 overshoot
		plan.get("spend", "expected_usd").(float64) >= plan.get("spend", "max_usd").(float64) {
		t.Errorf("experiment plan, a seq-v1 design: %s", plan.stdout)
	}
	assertKeys(t, plan.get("readiness").([]any)[0], "status,text")

	before := jsonRun(t, f, ExitOK, "experiment", "show", "lean-seq")
	assertKeys(t, before.doc, "calibration_usd,command,experiment,lock,locked,progress,schema,status,status_note")
	if before.get("locked") != false || before.get("lock") != nil || before.get("progress") != nil || before.get("status") != "draft" {
		t.Errorf("experiment show before the run: %s", before.stdout)
	}

	// Without --yes, a paid run is refused, and nothing is opened, locked or spent.
	refused := jsonRun(t, f, ExitError, "experiment", "run", "lean-seq")
	assertKeys(t, refused.doc, "command,experiment,run,schema")
	assertKeys(t, refused.get("run"), runResultKeys)
	if refused.get("run", "status") != "refused" || refused.get("run", "verdict") != nil || len(refused.get("run", "looks").([]any)) != 0 {
		t.Errorf("experiment run without --yes: %s", refused.stdout)
	}
	if n := len(experimentRuns(t, f, "lean-seq")); n != 0 || len(storedLock(t, f, "lean-seq")) != 0 {
		t.Errorf("a refused run changed the experiment: %d run(s)", n)
	}

	run := jsonRun(t, f, ExitOK, "experiment", "run", "lean-seq", "--yes")
	t.Log("experiment run --json, stopped at look 1:\n" + run.stdout)
	assertKeys(t, run.doc, "command,experiment,run,schema")
	res := run.get("run")
	assertKeys(t, res, runResultKeys)
	assertKeys(t, run.get("run", "looks").([]any)[0], lookKeys)
	assertKeys(t, run.get("run", "looks").([]any)[0].(map[string]any)["interval"], intervalKeys)
	assertKeys(t, run.get("run", "verdict"), verdictKeys)
	assertKeys(t, run.get("run", "verdict", "metrics").([]any)[0], metricKeys)
	for _, m := range run.get("run", "verdict", "metrics").([]any) {
		if m.(map[string]any)["interval"] != nil {
			assertKeys(t, m.(map[string]any)["interval"], intervalKeys)
		}
	}
	assertKeys(t, run.get("run", "runs"), "failed,pending,settled,skipped,total")
	assertKeys(t, run.get("run", "north_star"), "decisive,experiment,metric,seconds,spent_usd,verdict")
	if run.get("run", "status") != "done" || run.get("run", "ended_by") != "stop" || run.get("run", "stopped_at_look") != float64(1) ||
		run.get("run", "method") != "seq-v1" || run.get("run", "runs", "settled") != float64(16) || run.get("run", "runs", "pending") != float64(0) || run.get("run", "runs", "skipped") != float64(16) ||
		run.get("run", "runs", "total") != float64(32) || run.get("run", "verdict", "decisive") != true || run.get("run", "north_star", "decisive") != true ||
		run.get("run", "next_command") != "agentium experiment report lean-seq" || run.get("run", "resume_at") != nil {
		t.Errorf("experiment run: %s", run.stdout)
	}
	look := run.get("run", "looks").([]any)[0].(map[string]any)
	if look["look"] != float64(1) || look["tasks_counted"] != float64(8) || look["verdict"] != "improved" || look["decision"] != "stop" || look["level"].(float64) < 0.998 {
		t.Errorf("look 1: %v", look)
	}
	var cost map[string]any
	for _, m := range run.get("run", "verdict", "metrics").([]any) {
		if m.(map[string]any)["metric"] == "cost" {
			cost = m.(map[string]any)
		}
	}
	if cost == nil || cost["role"] != "primary" || cost["verdict"] != "improved" || cost["decisive"] != true || cost["interval"].(map[string]any)["high"].(float64) >= 1 {
		t.Errorf("the cost verdict: %v", cost)
	}
	if strings.Contains(run.stdout, "[1/32]") || strings.Contains(run.stdout, "started") {
		t.Errorf("progress lines in the document:\n%s", run.stdout)
	}
	if n := len(experimentRuns(t, f, "lean-seq")); n != 16 {
		t.Errorf("%d runs stored, want stage 1's 16", n)
	}

	after := jsonRun(t, f, ExitOK, "experiment", "show", "lean-seq")
	assertKeys(t, after.doc, "calibration_usd,command,experiment,lock,locked,progress,schema,status,status_note")
	assertKeys(t, after.get("lock"), lockKeys)
	assertKeys(t, after.get("progress"), progressKeys)
	assertKeys(t, after.get("progress", "arms").([]any)[0], armProgKeys)
	assertKeys(t, after.get("progress", "looks").([]any)[0], lookKeys)
	if after.get("status") != "done" || after.get("locked") != true || after.get("progress", "settled") != float64(16) || after.get("progress", "ended_by") != "stop" ||
		after.get("lock", "claude_code") != "2.1.281" {
		t.Errorf("experiment show after the run: %s", after.stdout)
	}

	// Resuming a finished experiment runs nothing and says the same.
	again := jsonRun(t, f, ExitOK, "experiment", "run", "lean-seq", "--yes")
	if again.get("run", "status") != "done" || again.get("run", "runs", "settled") != float64(16) || len(experimentRuns(t, f, "lean-seq")) != 16 {
		t.Errorf("a resumed finished experiment: %s", again.stdout)
	}

	// A fixed design (a success experiment) has sizes, not looks.
	tasks := storedTasks(t, f.data)
	jsonRun(t, f, ExitOK, "experiment", "new", "fixed", "--b", "lean", "--goal", "better", "--task", tasks[0].Name, "--task", tasks[1].Name)
	fixed := jsonRun(t, f, ExitOK, "experiment", "plan", "fixed")
	assertKeys(t, fixed.doc, planKeys)
	assertKeys(t, fixed.get("sizes").([]any)[0], "cost_usd,judge_usd,name,repeats_per_arm,runs,short,tasks,worst_case_usd")
	if len(fixed.get("looks").([]any)) != 0 || fixed.get("spend", "expected_tasks") != nil || fixed.get("experiment", "method") != "phase1-v2" {
		t.Errorf("experiment plan, a fixed design: %s", fixed.stdout)
	}

	list := jsonRun(t, f, ExitOK, "experiment", "list")
	assertKeys(t, list.get("experiments").([]any)[0], "arms,budget_usd,created,goal,method,model,name,repeats_per_arm,status,tasks,template")
	assertKeys(t, list.get("experiments").([]any)[0].(map[string]any)["arms"].([]any)[0], "context,name")
	if got := list.get("experiments").([]any); len(got) != 2 {
		t.Errorf("experiment list: %s", list.stdout)
	}
	removed := jsonRun(t, f, ExitOK, "experiment", "rm", "fixed")
	assertKeys(t, removed.doc, "command,removed,schema")
	jsonRun(t, f, ExitError, "experiment", "rm", "lean-seq") // it has run: it stays
}

// Failures are error documents with the exit code; usage mistakes exit 2.
func TestJSONExperimentErrors(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, t.TempDir()+"/data")
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"experiment", "show", "nope"}, ExitError},
		{[]string{"experiment", "plan"}, ExitUsage},
		{[]string{"experiment", "new", "x", "--template", "aa"}, ExitUsage}, // a removed flag
		{[]string{"experiment", "list", "extra"}, ExitUsage},
		{[]string{"experiment", "run", "x", "--usage-limit", "0"}, ExitUsage},
	} {
		got := jsonRun(t, f, c.code, c.args...)
		assertKeys(t, got.get("error"), "code,message")
		if got.get("error", "code") != float64(c.code) || got.get("error", "message") == "" {
			t.Errorf("%v: %s", c.args, got.stdout)
		}
	}
}

// experiment run --json never asks or reads stdin, even at a terminal, and without --yes it opens nothing.
func TestJSONExperimentRunNeverAsks(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t, t.TempDir()+"/data")
	stop := make(chan struct{})
	defer close(stop)
	done := make(chan cliResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{DefaultGrader: "host", Args: []string{"experiment", "run", "x", "--json"}, Stdin: blockingReader{stop}, StdinTerminal: true, Terminal: true,
			Stdout: &stdout, Stderr: &stderr, Dir: f.repo, Getenv: func(k string) string { return f.vars[k] },
			Environ:  func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home} },
			LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now})
		done <- cliResult{code, stdout.String(), stderr.String()}
	}()
	select {
	case res := <-done:
		got := checkJSON(t, f, res, ExitError, []string{"experiment", "run", "x"})
		if got.get("run", "status") != "refused" || strings.Contains(res.stdout, "[y/N]") {
			t.Errorf("experiment run --json at a terminal: %s", res.stdout)
		}
	case <-time.After(time.Minute):
		t.Fatal("experiment run --json blocked")
	}
}

// A budget stop is a result, not a failure (exit 0, as in human mode): the document says where it stopped, the look it
// has, and how to go on; a higher budget finishes it; a lower one is a usage error.
func TestJSONExperimentRunBudgetStop(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	control(t, ctrl, map[string]string{"cost-jitter": ""})
	jsonRun(t, f, ExitOK, "experiment", "new", "short", "--b", "lean", "--no-futility", "--run-budget", "1", "--budget", "8", "--seed", "3")
	stopped := jsonRun(t, f, ExitOK, "experiment", "run", "short", "--yes")
	assertKeys(t, stopped.get("run"), runResultKeys)
	pending, settled := stopped.get("run", "runs", "pending").(float64), stopped.get("run", "runs", "settled").(float64)
	if stopped.get("run", "status") != "budget" || stopped.get("run", "ended_by") != nil || stopped.get("run", "stopped_at_look") != nil ||
		len(stopped.get("run", "looks").([]any)) != 1 || pending <= 0 || settled <= 16 || settled+pending != 32 ||
		stopped.get("run", "budget_usd") != float64(8) || stopped.get("run", "spent_usd").(float64) > 8 ||
		stopped.get("run", "next_command") != "agentium experiment run short --budget USD" || stopped.get("run", "verdict", "decisive") != false {
		t.Errorf("a budget stop: %s", stopped.stdout)
	}
	t.Log("experiment run --json, stopped at the budget:\n" + stopped.stdout)
	lower := jsonRun(t, f, ExitUsage, "experiment", "run", "short", "--yes", "--budget", "2")
	if !strings.Contains(lower.get("error", "message").(string), "can only be raised") {
		t.Errorf("a lower budget: %s", lower.stdout)
	}
	refusedBudget := jsonRun(t, f, ExitError, "experiment", "run", "short", "--budget", "12")
	if refusedBudget.get("run", "next_command") != "agentium experiment run short --yes --json --budget 12" || refusedBudget.get("run", "spent_usd") != nil ||
		refusedBudget.get("run", "budget_usd") != nil || refusedBudget.get("run", "runs") != nil || refusedBudget.get("run", "method") != nil {
		t.Errorf("a refused run: unknown fields are null and the flags go into next_command: %s", refusedBudget.stdout)
	}
	resumed := jsonRun(t, f, ExitOK, "experiment", "run", "short", "--yes", "--budget", "30")
	if resumed.get("run", "status") != "done" || resumed.get("run", "ended_by") != "final" || resumed.get("run", "stopped_at_look") != nil ||
		resumed.get("run", "runs", "pending") != float64(0) || resumed.get("run", "runs", "skipped") != float64(0) || len(resumed.get("run", "looks").([]any)) != 3 || resumed.get("run", "budget_usd") != float64(30) {
		t.Errorf("the resumed run: %s", resumed.stdout)
	}
	shown := jsonRun(t, f, ExitOK, "experiment", "show", "short")
	changes, _ := shown.get("lock", "budget_changes").([]any)
	if len(changes) != 1 {
		t.Fatalf("the budget raise is not in the lock: %s", shown.stdout)
	}
	assertKeys(t, changes[0], "at,from_usd,to_usd")
}

// start --json --yes runs the experiment and answers with start's document and the run's result; without --yes it
// stays a preview that spends nothing, and asked to run while not ready it fails with the readiness in the document.
func TestJSONStartYesRunsTheExperiment(t *testing.T) {
	t.Parallel()
	f, ctrl := readyFixture(t)
	preview := jsonRun(t, f, ExitOK, "start")
	assertKeys(t, preview.doc, startKeys)
	assertKeys(t, preview.get("experiment"), startExperimentKeys)
	assertKeys(t, preview.get("experiment", "spend"), spendKeys)
	assertKeys(t, preview.get("experiment", "looks").([]any)[0], lookPlanKeys)
	if preview.get("status") != "preview" || preview.get("run") != nil || preview.get("nothing_was_run") != true || preview.get("experiment", "method") != "seq-v1" {
		t.Errorf("start preview: %s", preview.stdout)
	}
	if _, started := paidRuns(t, f, ctrl); started != 0 {
		t.Fatalf("the preview started %d run(s)", started)
	}

	ran := jsonRun(t, f, ExitOK, "start", "--yes")
	t.Log("start --json --yes:\n" + ran.stdout)
	assertKeys(t, ran.doc, startKeys)
	assertKeys(t, ran.get("run"), runResultKeys)
	assertKeys(t, ran.get("run", "verdict"), verdictKeys)
	assertKeys(t, ran.get("project"), "id,name")
	if ran.get("status") != "ran" || ran.get("nothing_was_run") != false || ran.get("run", "status") != "done" || ran.get("experiment", "name") != "quick-aa-baseline" ||
		ran.get("run", "runs", "settled") != float64(18) || ran.get("run", "runs", "pending") != float64(0) || ran.get("run", "ended_by") != "final" {
		t.Errorf("start --yes: %s", ran.stdout)
	}
	if stored, _ := paidRuns(t, f, ctrl); stored < 18 {
		t.Errorf("%d runs stored, want at least the 18 slots", stored)
	}
	if strings.Contains(ran.stdout, "[1/18]") || strings.Contains(ran.stdout, "Running up to") {
		t.Errorf("progress text leaked into the document:\n%s", ran.stdout)
	}
	// The experiment is done: a second start --yes finds it finished and runs nothing more.
	_, before := paidRuns(t, f, ctrl)
	again := jsonRun(t, f, ExitOK, "start", "--yes")
	if _, after := paidRuns(t, f, ctrl); again.get("status") != "finished" || after != before {
		t.Errorf("a second start --yes: status %v, %d -> %d runs started", again.get("status"), before, after)
	}
}

func TestJSONStartYesWhileNotReadyFails(t *testing.T) {
	t.Parallel()
	f, ctrl := startFixture(t, 9)
	f.vars["AGENTIUM_CLAUDE"] = "/nonexistent/claude"
	got := jsonRun(t, f, ExitError, "start", "--accept-mined", "--yes")
	if got.get("status") != "not_ready" || got.get("ready") != false || got.get("run") != nil || got.get("nothing_was_run") != true {
		t.Errorf("start --yes, not ready: %s", got.stdout)
	}
	if stored, started := paidRuns(t, f, ctrl); stored != 0 || started != 0 {
		t.Errorf("a start that was not ready ran the agent: %d stored, %d started", stored, started)
	}
	if again := jsonRun(t, f, ExitOK, "start"); again.get("status") != "not_ready" {
		t.Errorf("without --yes, not ready is a preview with exit 0: %s", again.stdout)
	}
}

// An interrupted run that spent money still gets its document: status stopped, exit 1, the spend and counts, and the
// reason in note; a resume finishes it. (A run error after some runs, outcome.Err, takes the same path.)
func TestJSONExperimentRunInterrupted(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	expect(t, f.run(context.Background(), "experiment", "new", "stop", "--b", "lean", "--task", "value", "--repeats", "1", "--concurrency", "1"), ExitOK)
	writeFile(t, ctrl, "hang", "s1-t1\n")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, "the second run's agent", func() bool {
			_, err := os.Stat(filepath.Join(ctrl, "hanging-e1-s1-t1"))
			return err == nil
		})
		cancel()
	}()
	res := checkJSON(t, f, f.run(ctx, "experiment", "run", "stop", "--yes", "--json"), ExitError, []string{"experiment", "run"})
	assertKeys(t, res.get("run"), runResultKeys)
	if res.get("run", "status") != "stopped" || res.get("run", "runs", "settled") != float64(1) || res.get("run", "runs", "pending") != float64(1) ||
		res.get("run", "spent_usd") == nil || res.get("run", "spent_usd").(float64) <= 0 || res.get("run", "note") != "interrupted" ||
		res.get("error") != nil {
		t.Errorf("an interrupted run: %s", res.stdout)
	}
	os.Remove(filepath.Join(ctrl, "hang"))
	done := jsonRun(t, f, ExitOK, "experiment", "run", "stop", "--yes")
	if done.get("run", "status") != "done" || done.get("run", "runs", "settled") != float64(2) {
		t.Errorf("the resume: %s", done.stdout)
	}
}

// start --json --yes whose run is interrupted still answers with start's document and the run's stopped result.
func TestJSONStartYesInterrupted(t *testing.T) {
	t.Parallel()
	f, ctrl := readyFixture(t)
	writeFile(t, ctrl, "hang", "s1-t1\ns2-t1\n")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, "a hanging agent", func() bool {
			m, _ := filepath.Glob(filepath.Join(ctrl, "hanging-*"))
			return len(m) > 0
		})
		cancel()
	}()
	res := checkJSON(t, f, f.run(ctx, "start", "--yes", "--json"), ExitError, []string{"start", "--yes"})
	assertKeys(t, res.doc, startKeys)
	if res.get("status") != "ran" || res.get("run", "status") != "stopped" || res.get("nothing_was_run") != false || res.get("run", "spent_usd") == nil {
		t.Errorf("start --yes, interrupted: %s", res.stdout)
	}
}

// A pause before the usage limit is a result (exit 0): resume_at says when the window resets, and the looks so far stay.
func TestJSONExperimentRunUsagePause(t *testing.T) {
	t.Parallel()
	f, ctrl := seqFixture(t)
	resets := time.Now().Add(time.Hour).Truncate(time.Second)
	control(t, ctrl, map[string]string{"cost-jitter": "", "usage": "0.00 0.045 " + strconv.FormatInt(resets.Unix(), 10) + "\n"})
	jsonRun(t, f, ExitOK, "experiment", "new", "window", "--b", "lean", "--no-futility", "--concurrency", "1", "--seed", "3")
	paused := jsonRun(t, f, ExitOK, "experiment", "run", "window", "--yes")
	assertKeys(t, paused.get("run"), runResultKeys)
	at, _ := time.Parse(time.RFC3339, paused.get("run", "resume_at").(string))
	if paused.get("run", "status") != "usage" || !at.Equal(resets) || paused.get("run", "judge_paused") != false || len(paused.get("run", "looks").([]any)) != 1 ||
		paused.get("run", "runs", "pending").(float64) <= 0 || paused.get("run", "runs", "skipped") != float64(0) ||
		paused.get("run", "next_command") != "agentium experiment run window" {
		t.Errorf("a usage pause: %s", paused.stdout)
	}
	// The plan reads the window as the runs left it: a current reading with its time, and the model's own rate.
	plan := jsonRun(t, f, ExitOK, "experiment", "plan", "window")
	assertKeys(t, plan.get("usage"), usageKeys)
	assertKeys(t, plan.get("usage", "latest"), usageLatestKeys)
	model := plan.get("usage", "models").([]any)[0].(map[string]any)
	if plan.get("usage", "latest", "current") != true || plan.get("usage", "latest", "used") == nil || plan.get("usage", "latest", "read_at") == nil ||
		plan.get("usage", "latest", "age_seconds") == nil || model["model"] != "claude-sonnet-5-5" || model["runs"] != float64(32) {
		t.Errorf("the plan's usage after a pause: %s", plan.stdout)
	}
}

// A person's own run needs no --yes, and --yes changes nothing there: human text, no JSON.
func TestExperimentRunYesIsANoOpWithoutJSON(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	for _, name := range []string{"plain", "yes"} {
		expect(t, f.run(ctx, "experiment", "new", name, "--b", "lean", "--task", "value", "--repeats", "1"), ExitOK)
	}
	plain := f.run(ctx, "experiment", "run", "plain")
	withYes := f.run(ctx, "experiment", "run", "yes", "--yes")
	expect(t, withYes, ExitOK, "Experiment yes: done", "2 of 2 runs settled", "The report: agentium experiment report yes")
	if plain.code != withYes.code || strings.HasPrefix(strings.TrimSpace(withYes.stdout), "{") || withYes.stderr != plain.stderr ||
		strings.Count(withYes.stdout, "\n") != strings.Count(plain.stdout, "\n") {
		t.Errorf("--yes changed human mode:\n%s\nvs\n%s", plain.stdout, withYes.stdout)
	}
}

// A number JSON cannot hold (NaN, Inf) becomes null: the document is written after the money was spent.
func TestRunDocumentSurvivesNonFiniteNumbers(t *testing.T) {
	t.Parallel()
	nan, inf := math.NaN(), math.Inf(1)
	env := Env{Getenv: func(string) string { return "" }}
	analysis := experiment.Analysis{Results: []experiment.MetricResult{{Metric: "cost", Role: experiment.RolePrimary, Tasks: 3, A: &nan, B: &inf, Level: math.NaN(),
		Boot95: stats.Interval{Estimate: nan, Low: 1, High: inf}, T95: stats.Interval{Estimate: 1, Low: 0, High: 2}, Verdict: stats.Inconclusive}},
		Sequential: &experiment.SeqStatus{Planned: []int{1, 2}, Reported: 1, Looks: []experiment.Look{{Look: 1, Analysed: true, EffLevel: nan, Verdict: stats.Inconclusive,
			Interval: &stats.Interval{Estimate: 1, Low: nan, High: 2}, ConditionalPower: &nan, Decision: experiment.LookContinue}}}}
	doc := runResultDoc{Status: "done", Looks: looksOf(analysis.Sequential), Verdict: verdictOf(env, analysis), SpentUSD: finiteOf(nan)}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("a non-finite number failed the document: %v", err)
	}
	var back runResultDoc
	if err := json.Unmarshal(data, &back); err != nil || back.SpentUSD != nil || back.Verdict.Metrics[0].A != nil || back.Verdict.Metrics[0].Interval != nil ||
		back.Looks[0].Level != nil || back.Looks[0].Interval != nil || back.Looks[0].ConditionalPower != nil {
		t.Errorf("non-finite numbers are not null: %v\n%s", err, data)
	}
}

// Not ready: the error does not point at checks a JSON run never shows.
func TestJSONExperimentRunNotReadyMessage(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "unready", "--b", "lean", "--task", "value", "--repeats", "1"), ExitOK)
	expect(t, f.run(ctx, "task", "edit", "value", "--setup", "true"), ExitOK) // a changed task waits for its review again
	human := f.run(ctx, "experiment", "run", "unready")
	expect(t, human, ExitError, "not ready to run: see above")
	got := jsonRun(t, f, ExitError, "experiment", "run", "unready", "--yes")
	if msg, _ := got.get("error", "message").(string); !strings.Contains(msg, "not ready to run (agentium experiment plan unready)") || strings.Contains(msg, "see above") {
		t.Errorf("experiment run --json, not ready: %s", got.stdout)
	}
}

// A slot out of attempts is failed, not pending: a resume does not retry it. total = settled + pending + skipped + failed,
// in a result that stopped at the budget.
func TestJSONExperimentRunCountsFailedSlots(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	control(t, ctrl, map[string]string{"infra": "s0-t1\ns0-t2\ns0-t3\n"}) // slot 0 fails all 3 of its attempts
	jsonRun(t, f, ExitOK, "experiment", "new", "flaky", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--run-budget", "1", "--budget", "2.5", "--concurrency", "1")
	got := jsonRun(t, f, ExitOK, "experiment", "run", "flaky", "--yes")
	n := func(k string) int { return int(got.get("run", "runs", k).(float64)) }
	t.Log("runs:", got.get("run", "runs"))
	if got.get("run", "status") != "budget" || n("failed") != 1 || n("pending") <= 0 || n("skipped") != 0 || n("total") != 6 ||
		n("settled")+n("pending")+n("skipped")+n("failed") != n("total") {
		t.Errorf("a failed slot in a budget stop: %s", got.stdout)
	}
}
