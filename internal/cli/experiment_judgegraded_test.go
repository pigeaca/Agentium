package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/task"
)

// addJudgedTask adds the judge-graded task "judged" to an experiment fixture: its base is the task "value"'s, and its
// solution sets the value without tests, so the judge grades it. It is validated and reviewed.
func addJudgedTask(t *testing.T, f runFixture) {
	t.Helper()
	ctx := context.Background()
	base := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "HEAD~1"))
	gitIn(t, f.repo, "checkout", "-q", "-b", "untested", base)
	writeFile(t, f.repo, "value.txt", "new\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "Make the value new, untested")
	solution := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "HEAD"))
	gitIn(t, f.repo, "checkout", "-q", "main")
	expect(t, f.run(ctx, "task", "add", "judged", "--base", base, "--solution", solution, "--instruction", "Make the value new.", "--judge-graded",
		"--verify", "true"), ExitOK, "judge-graded (no hidden tests)")
	expect(t, f.run(ctx, "task", "validate", "judged"), ExitOK, "Result: valid")
}

// An experiment that mixes a test-graded and a judge-graded task: the judge's majority of 5 grades the judge-graded
// task's runs (unvalidated, labelled wherever a pass shows), the tests the other's; the preview and the budget hold the
// grading's calls at their cap; the report keeps the two successes apart.
func TestExperimentMixesJudgeGradedTasks(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	created := f.run(ctx, "experiment", "new", "mixed", "--b", "lean", "--task", "value", "--task", "judged", "--goal", "better", "--repeats", "1", "--seed", "5")
	expect(t, created, ExitOK, "2 task(s) × 1 run(s) per arm = 4 runs", "1 task(s) are judge-graded (judged)", "a majority of 5 calls",
		"each run's grading up to $5.00", "no verdict rests on them",
		"warning: only 1 test-graded task(s), below success's floor of 20: success, the primary metric, cannot reach a verdict")
	d := storedDesign(t, f, "mixed")
	if d.Version != 5 || len(d.JudgeGraded) != 1 || d.JudgeGraded[0] != "judged" || d.JudgeGrading == nil || *d.JudgeGrading != llmjudge.GradingSettings() {
		t.Fatalf("stored design: version %d, judge-graded %v, grading %+v", d.Version, d.JudgeGraded, d.JudgeGrading)
	}
	// The default budget holds the grading's calls: a judge-graded pair may reach 2 × ($3 + overshoot + $5).
	if d.BudgetUSD < d.MaxPairCapUSD() || d.MaxPairCapUSD() < 16 {
		t.Errorf("budget $%.2f, a judge-graded pair up to $%.2f", d.BudgetUSD, d.MaxPairCapUSD())
	}
	plan := f.run(ctx, "experiment", "plan", "mixed")
	expect(t, plan, ExitOK, "judge-graded: 1 task(s) are judge-graded (judged)", "Grading by the judge: about $0.65 for the 2 run(s) of judge-graded tasks × 5 call(s)",
		"its grading may reach $5.00 (5 calls at $0.50, each asked twice at most)", "WARNING  1 task(s) are judge-graded")

	first := f.run(ctx, "experiment", "run", "mixed")
	expect(t, first, ExitOK, "(a judge-graded task's run: its grading up to $5.00)", "judged, arm A, repeat 1: ok, $0.30; graded by the judge (unvalidated): fixed (5 of 5), $0.25",
		"arm A: the judge says 1 of 1 judge-graded run(s) fixed (unvalidated; not in SUCCESSES)", "Experiment mixed: done")
	runs := experimentRuns(t, f, "mixed")
	for i, rec := range records(t, runs) {
		switch rec.Task {
		case "judged":
			v := rec.Judge
			if rec.GradedBy != task.GradingJudge || rec.Outcome != agent.OutcomeOK || rec.Passed == nil || !*rec.Passed || v == nil || v.Requested != 5 ||
				len(v.Answers) != 5 || v.CostUSD < 0.2499 || v.CostUSD > 0.2501 || runs[i].CostUSD != 0.30 || len(rec.Verify) != 0 {
				t.Errorf("a judge-graded run: graded by %q, %s, passed %v, verdict %+v, cost column %v, verify %v", rec.GradedBy, rec.Outcome, rec.Passed, v,
					runs[i].CostUSD, rec.Verify)
			}
		default:
			if rec.GradedBy != "" || rec.Judge != nil || rec.Passed == nil || !*rec.Passed {
				t.Errorf("a test-graded run: graded by %q, verdict %+v, passed %v", rec.GradedBy, rec.Judge, rec.Passed)
			}
		}
	}
	// The judge read the reference's change and the run's, never a test: the task has none.
	prompts := judgeFiles(t, ctrl, "prompt")
	if len(prompts) != 10 {
		t.Fatalf("%d judge calls, want 2 judge-graded runs × 5", len(prompts))
	}
	for _, p := range prompts {
		if strings.Count(p, "diff --git a/value.txt b/value.txt") != 2 || strings.Contains(p, "value_test") || !strings.Contains(p, "Make the value new.") {
			t.Errorf("the judge's prompt:\n%s", p)
		}
	}
	emptyWorkspaces(t, f)

	// Off a terminal the report is its Markdown (the terminal's plain-words view has its own golden: reportview_test).
	report := f.run(ctx, "experiment", "report", "mixed")
	expect(t, report, ExitOK, "**The judge says fixed 100% → 100%** (judge-graded tasks, unvalidated): exploratory, never a verdict",
		"Success (passed the tests, test-graded tasks only)", "The judge says fixed (judge-graded tasks, unvalidated): A 1 of 1 runs (100%), B 1 of 1 (100%)",
		"| judged (judge) | ● 1/1 | ● 1/1 |", "| The judge says fixed | secondary | 100% | 100% |", "1 task(s) are judge-graded (judged)", "grading cost $0.50")
	js := f.run(ctx, "experiment", "report", "mixed", "--json")
	var doc struct {
		Runs []struct {
			Task     string `json:"task"`
			GradedBy string `json:"graded_by"`
		} `json:"runs"`
		JudgeGrading *struct {
			Tasks []string `json:"tasks"`
		} `json:"judge_grading"`
	}
	if err := json.Unmarshal([]byte(js.stdout), &doc); err != nil || doc.JudgeGrading == nil || len(doc.JudgeGrading.Tasks) != 1 {
		t.Fatalf("the report's JSON: %v\n%.1500s", err, js.stdout)
	}
	for _, r := range doc.Runs {
		if (r.Task == "judged") != (r.GradedBy == "judge") {
			t.Errorf("the report's JSON: run of %s graded by %q", r.Task, r.GradedBy)
		}
	}
	shown := jsonRun(t, f, ExitOK, "experiment", "show", "mixed")
	arms := shown.get("progress").(map[string]any)["arms"].([]any)
	if a := arms[0].(map[string]any); a["judge_graded"] != 1.0 || a["judge_fixed"] != 1.0 || a["successes"] != 1.0 {
		t.Errorf("experiment show's arm: %v", a)
	}

	// The judge says "no": the run fails, labelled the judge's; the test-graded task's runs are untouched.
	writeFile(t, ctrl, "judge-answer", "no")
	expect(t, f.run(ctx, "experiment", "new", "refused", "--b", "lean", "--task", "value", "--task", "judged", "--goal", "better", "--repeats", "1"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "refused"), ExitOK, "graded by the judge (unvalidated): not fixed (5 of 5 said partly or no)")
	for _, rec := range records(t, experimentRuns(t, f, "refused")) {
		if rec.Task == "judged" && (rec.Outcome != agent.OutcomeOK || rec.Passed == nil || *rec.Passed) {
			t.Errorf("a run the judge called not fixed: %s, passed %v", rec.Outcome, rec.Passed)
		}
	}
	expect(t, f.run(ctx, "experiment", "report", "refused", "--markdown"), ExitOK, "| judged (judge) | ○ 0/1 | ○ 0/1 |")
}

// agentRuns counts the experiment fixture's agent runs (each leaves its process ID in ctrl), never the judge's calls.
func agentRuns(t *testing.T, ctrl string) int {
	t.Helper()
	pids, err := filepath.Glob(filepath.Join(ctrl, "pid-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(pids)
}

// The judge at a usage limit leaves a judge-graded run's grade pending: the run is the agent's (ok, never failed, never
// infrastructure), its slot settled. The experiment pauses; once the limit lifts, a resume grades the run from its
// stored change, without running the agent again, its first try's spend counted.
func TestExperimentJudgeGradeAtALimit(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "limit", "--b", "lean", "--task", "judged", "--goal", "better", "--repeats", "1", "--concurrency", "1"), ExitOK)
	writeFile(t, ctrl, "judge-limit", "")
	paused := f.run(ctx, "experiment", "run", "limit")
	expect(t, paused, ExitOK, "ok, $0.30; graded by the judge (unvalidated): grade pending (the judge answered 0 of 5 times",
		"Paused: the judge hit a usage limit or a sign-in failure", "1 grade(s) pending, graded again from the run's change")
	recs := records(t, experimentRuns(t, f, "limit"))
	if len(recs) != 1 || recs[0].Outcome != agent.OutcomeOK || recs[0].Passed != nil || recs[0].Judge == nil || recs[0].Judge.Stopped != llmjudge.StoppedLimit ||
		!strings.Contains(strings.Join(recs[0].Notes, "; "), "not graded yet: the judge answered 0 of 5 times") || recs[0].GradeErrors != 0 {
		t.Fatalf("the pending run: %+v", recs)
	}
	// Still at the limit: the resume grades the pending run first, hits the limit, and pauses before any new agent run.
	expect(t, f.run(ctx, "experiment", "run", "limit"), ExitOK, "Graded run "+recs[0].ID+" again", "grade pending (", "Paused: the judge hit a usage limit")
	if n := agentRuns(t, ctrl); n != 1 {
		t.Fatalf("%d agent runs while the judge was still at its limit, want 1", n)
	}
	os.Remove(filepath.Join(ctrl, "judge-limit"))
	resumed := f.run(ctx, "experiment", "run", "limit")
	expect(t, resumed, ExitOK, "Graded run "+recs[0].ID+" again (task judged, arm "+recs[0].Arm+"; the agent did not run again): fixed (5 of 5), $0.25",
		"graded by the judge (unvalidated): fixed (5 of 5)", "Experiment limit: done", "2 of 2 runs settled")
	if strings.Contains(resumed.stdout, "attempt 2") {
		t.Errorf("a slot ran again:\n%s", resumed.stdout)
	}
	runs := experimentRuns(t, f, "limit")
	if len(runs) != 2 || agentRuns(t, ctrl) != 2 {
		t.Fatalf("%d runs stored, %d agent runs: want 2 and 2 (one per slot)", len(runs), agentRuns(t, ctrl))
	}
	for i, rec := range records(t, runs) {
		if rec.Outcome != agent.OutcomeOK || rec.Passed == nil || !*rec.Passed || runs[i].Passed == nil || !*runs[i].Passed {
			t.Errorf("run %s: %s, passed %v (column %v)", rec.ID, rec.Outcome, rec.Passed, runs[i].Passed)
		}
	}
	// Two agent runs ($0.30 each), two gradings ($0.25 each) and the two calls that hit the limit ($0.01 each).
	expect(t, f.run(ctx, "experiment", "show", "limit"), ExitOK, "spent $1.12 of")
}

// A judge error that is not a limit (calls that bring no answer) leaves the grade pending too: the experiment grades it
// again after its runs, from the stored change, and the agent runs once per slot. A refusal is final: the run is left
// ungraded, never failed, never tried again, and counted per arm.
func TestExperimentJudgeGradeErrorsAndRefusals(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "errs", "--b", "lean", "--task", "judged", "--goal", "better", "--repeats", "1", "--concurrency", "1"), ExitOK)
	writeFile(t, ctrl, "judge-broken-calls", "5") // the first run's grading: every call brings no answer
	done := f.run(ctx, "experiment", "run", "errs")
	expect(t, done, ExitOK, "graded by the judge (unvalidated): grade pending (the judge answered 0 of 5 times", "; the agent did not run again): fixed (5 of 5)",
		"Experiment errs: done", "2 of 2 runs settled")
	runs := experimentRuns(t, f, "errs")
	if len(runs) != 2 || agentRuns(t, ctrl) != 2 {
		t.Fatalf("%d runs stored, %d agent runs: want 2 and 2", len(runs), agentRuns(t, ctrl))
	}
	for _, rec := range records(t, runs) {
		if rec.Outcome != agent.OutcomeOK || rec.Passed == nil || !*rec.Passed {
			t.Errorf("run %s: %s, passed %v, notes %v", rec.ID, rec.Outcome, rec.Passed, rec.Notes)
		}
	}
	if n := len(judgeFiles(t, ctrl, "prompt")); n != 15 {
		t.Errorf("%d judge calls, want 5 broken, 5 and 5", n)
	}

	os.Remove(filepath.Join(ctrl, "judge-broken-calls"))
	writeFile(t, ctrl, "judge-answer", "refuse") // no valid verdict, even when asked again
	before := agentRuns(t, ctrl)
	expect(t, f.run(ctx, "experiment", "new", "refusals", "--b", "lean", "--task", "judged", "--goal", "better", "--repeats", "1"), ExitOK)
	refused := f.run(ctx, "experiment", "run", "refusals")
	expect(t, refused, ExitOK, "graded by the judge (unvalidated): not graded (the judge answered 0 of 5 times (0 fixed, 0 not; 5 refused or malformed)",
		"1 left without a grade (not counted, not tried again)", "Experiment refusals: done", "2 of 2 runs settled")
	if agentRuns(t, ctrl)-before != 2 || strings.Contains(refused.stdout, "again (task") {
		t.Errorf("refused runs were run or graded again:\n%s", refused.stdout)
	}
	expect(t, f.run(ctx, "run", "list"), ExitOK, "ungraded (judge)")
	for _, rec := range records(t, experimentRuns(t, f, "refusals")) {
		if rec.Outcome != agent.OutcomeOK || rec.Passed != nil || rec.Ungraded == "" {
			t.Errorf("a refused run: %s, passed %v, ungraded %q", rec.Outcome, rec.Passed, rec.Ungraded)
		}
	}
	expect(t, f.run(ctx, "experiment", "report", "refusals", "--markdown"), ExitOK, "2 judge-graded, left without a grade (not tried again)",
		"Runs the judge left without a grade (not counted, not tried again): A 1, B 1")
}

// Agentium killed while the judge grades a run loses neither the run nor the judge's spend: the resume recovers the run
// with its grade pending and grades it from its stored change, never by running the agent again.
func TestExperimentJudgeGradeSurvivesAKill(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "crash", "--b", "lean", "--task", "judged", "--goal", "better", "--repeats", "1", "--concurrency", "1"), ExitOK)
	writeFile(t, ctrl, "judge-block", "")
	var environ []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ANTHROPIC_") && !strings.HasPrefix(kv, "AGENTIUM_") && !strings.HasPrefix(kv, "CLAUDE_") && !strings.HasPrefix(kv, "HOME=") {
			environ = append(environ, kv)
		}
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestExperimentHelperProcess$", "-test.count=1")
	helper.Dir = f.repo
	helper.Env = append(environ, "AGENTIUM_TEST_HELPER=1", "AGENTIUM_TEST_ARGS=experiment run crash", "AGENTIUM_HOME="+f.data,
		"HOME="+f.home, "AGENTIUM_CLAUDE="+f.vars["AGENTIUM_CLAUDE"])
	var out bytes.Buffer
	helper.Stdout, helper.Stderr = &out, &out
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first grading call", func() bool { return len(judgeFiles(t, ctrl, "prompt")) > 0 })
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	helper.Wait()
	os.Remove(filepath.Join(ctrl, "judge-block")) // the orphaned call ends
	if n := len(experimentRuns(t, f, "crash")); n != 0 || agentRuns(t, ctrl) != 1 {
		t.Fatalf("before the resume: %d runs stored, %d agent runs\n%s", n, agentRuns(t, ctrl), out.String())
	}
	resumed := f.run(ctx, "experiment", "run", "crash")
	expect(t, resumed, ExitOK, "Recovered run ", "left behind by a stopped Agentium: ok, $0.30 (its grade is pending: graded again from its change)", "Graded run ",
		"; the agent did not run again): fixed (5 of 5)", "Experiment crash: done", "2 of 2 runs settled")
	runs := experimentRuns(t, f, "crash")
	if len(runs) != 2 || agentRuns(t, ctrl) != 2 {
		t.Fatalf("%d runs stored, %d agent runs: want 2 and 2\n%s", len(runs), agentRuns(t, ctrl), resumed.stdout)
	}
	for _, rec := range records(t, runs) {
		if rec.Outcome != agent.OutcomeOK || rec.Passed == nil || !*rec.Passed {
			t.Errorf("run %s: %s, passed %v, recovered %q", rec.ID, rec.Outcome, rec.Passed, rec.Recovered)
		}
	}
	emptyWorkspaces(t, f)
}

// Run once takes a judge-graded task: it names the grading's cost before it starts, the judge grades the run, and run
// show and the JSON say the grade is the judge's.
func TestRunOnceJudgeGraded(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	once := f.run(ctx, "run", "once", "judged")
	expect(t, once, ExitOK, "graded by the judge: 5 calls on claude-opus-5-5, unvalidated): it may cost up to $3.00, and its grading up to $5.00",
		"judge (grading): fixed (5 of 5), $0.25", "graded by the judge (unvalidated): passed", "outcome      ok; judge: fixed (5 of 5)",
		"grading      by the judge's majority", "judge        claude-opus-5-5 at effort high, answers yes, yes, yes, yes, yes")
	if strings.Contains(once.stdout, "verification passed") {
		t.Errorf("a judge-graded run says its verification passed:\n%s", once.stdout)
	}
	if n := len(judgeFiles(t, ctrl, "prompt")); n != 5 {
		t.Errorf("%d judge calls, want 5", n)
	}
	id := ""
	for _, r := range jsonRun(t, f, ExitOK, "run", "list").get("runs").([]any) {
		if entry := r.(map[string]any); entry["task"] == "judged" {
			id = entry["id"].(string)
		}
	}
	shown := jsonRun(t, f, ExitOK, "run", "show", id)
	r := shown.get("run").(map[string]any)
	g, _ := r["judge_grade"].(map[string]any)
	if r["graded_by"] != "judge" || r["passed"] != true || g == nil || g["fixed"] != "yes" || g["requested"] != 5.0 || g["unvalidated"] != true {
		t.Errorf("run show's JSON: %v", r)
	}
	var keys []string
	for k := range g {
		keys = append(keys, k)
	}
	if b, _ := json.Marshal(keys); len(keys) != 7 {
		t.Errorf("judge_grade keys %s", b)
	}
	expect(t, f.run(ctx, "run", "show", id), ExitOK, "outcome      ok; judge: fixed (5 of 5)")
	// run list marks the judge's pass, and its JSON names who graded the run (absent for the tests).
	listed := f.run(ctx, "run", "list")
	for _, line := range strings.Split(listed.stdout, "\n") {
		if strings.Contains(line, id) && !strings.Contains(line, "yes (judge)") {
			t.Errorf("run list's line of a judge-graded run: %q", line)
		}
	}
	for _, r := range jsonRun(t, f, ExitOK, "run", "list").get("runs").([]any) {
		entry := r.(map[string]any)
		if g, has := entry["graded_by"]; (entry["id"] == id) != (has && g == "judge") {
			t.Errorf("run list's JSON row: %v", entry)
		}
	}

	// A judge that brings no answer leaves the grade pending (the run the agent's), never failed; run once makes one
	// grading attempt, within the cost it named.
	writeFile(t, ctrl, "judge-broken", "")
	broken := f.run(ctx, "run", "once", "judged")
	expect(t, broken, ExitOK, "outcome      ok; judge: grade pending (the judge answered 0 of 5 times", "not graded yet: the judge answered 0 of 5 times",
		"a run outside an experiment stays ungraded")
	expect(t, f.run(ctx, "run", "list"), ExitOK, "pending (judge)")
}

// The help's figures for judge grading are the code's: its calls and its cap.
func TestHelpStatesJudgeGrading(t *testing.T) {
	calls := fmt.Sprintf("majority of %d calls", llmjudge.GradeRepeats)
	capUSD := fmt.Sprintf("$%.2f", llmjudge.CapUSD(llmjudge.GradingSettings()))
	for name, help := range map[string]string{"experiment": experimentUsage, "run": runUsage} {
		if !strings.Contains(strings.Join(strings.Fields(help), " "), calls) || !strings.Contains(help, capUSD) {
			t.Errorf("%s's help does not say %q and %s", name, calls, capUSD)
		}
	}
}

// A task whose grading changed after experiment new (removed and added again under its name, graded another way)
// refuses the lock: the design fixed which tasks the judge grades, and with them its version, caps and budget.
func TestExperimentRefusesATaskWhoseGradingChanged(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "changed", "--b", "lean", "--task", "judged", "--goal", "better", "--repeats", "1"), ExitOK)
	base := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "main~1"))
	expect(t, f.run(ctx, "task", "rm", "judged"), ExitOK)
	expect(t, f.run(ctx, "task", "add", "judged", "--base", base, "--solution", "main", "--instruction", "Make the value new.", "--verify", "sh run_tests.sh"), ExitOK)
	expect(t, f.run(ctx, "task", "edit", "judged", "--reviewed"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "judged", "--snapshot", "lean"), ExitOK)
	got := f.run(ctx, "experiment", "run", "changed")
	expect(t, got, ExitError, "task judged is graded by its tests now, but the experiment was made when it was not", "agentium experiment new")
	if n := len(experimentRuns(t, f, "changed")); n != 0 {
		t.Errorf("%d run(s) started", n)
	}
}

// Grading again is held to the budget: a pending grade whose attempt (up to the grading's cap) the budget cannot hold
// stays pending, and the experiment stops at the budget, saying so, rather than spend past it.
func TestExperimentJudgeGradeHeldToTheBudget(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "probe", "--b", "lean", "--task", "judged", "--repeats", "1"), ExitOK)
	maxPair := storedDesign(t, f, "probe").MaxPairCapUSD()
	budget := math.Ceil(maxPair*100+50) / 100 // a judge-graded pair at its caps, and $0.50 more
	expect(t, f.run(ctx, "experiment", "new", "tight", "--b", "lean", "--task", "judged", "--goal", "better", "--repeats", "1", "--concurrency", "1",
		"--budget", fmt.Sprintf("%.2f", budget)), ExitOK)
	// Each agent run costs enough that, after both, a grading attempt ($5.00) no longer fits; the first run's grading
	// brings no answer, so its grade is pending.
	writeFile(t, ctrl, "cost", fmt.Sprintf("%.2f", (budget-5)/2+0.1))
	writeFile(t, ctrl, "judge-broken-calls", "5")
	got := f.run(ctx, "experiment", "run", "tight")
	expect(t, got, ExitOK, "1 run(s) still wait for the judge's grade, but the budget leaves no room for one ($5.00)", "Stopped at the budget.")
	if strings.Contains(got.stdout, "Graded run ") || agentRuns(t, ctrl) != 2 {
		t.Errorf("graded again past the budget, or ran the agent again (%d runs):\n%s", agentRuns(t, ctrl), got.stdout)
	}
	pending := 0
	for _, rec := range records(t, experimentRuns(t, f, "tight")) {
		if run.NeedsGrading(rec) {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("%d pending grade(s), want 1", pending)
	}
}

// task validate warns, without failing, about an instruction longer than the grading judge reads.
func TestValidateWarnsAboutALongInstruction(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	long := filepath.Join(t.TempDir(), "long.md")
	if err := os.WriteFile(long, []byte("Make the value new. "+strings.Repeat("x", llmjudge.MaxInstructionChars)), 0o600); err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "untested~1"))
	expect(t, f.run(ctx, "task", "add", "long", "--base", base, "--solution", "untested", "--instruction", "@"+long, "--judge-graded", "--verify", "true"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "long"), ExitOK, "The instruction has 20020 characters; the grading judge reads the first 20000", "Result: valid")
	v := jsonRun(t, f, ExitOK, "task", "validate", "long")
	if j, _ := v.get("judge").(map[string]any); j == nil || j["instruction_truncated"] != true {
		t.Errorf("task validate's JSON: %v", v.get("judge"))
	}
	if j, _ := jsonRun(t, f, ExitOK, "task", "validate", "judged").get("judge").(map[string]any); j == nil || j["instruction_truncated"] != nil {
		t.Errorf("a short instruction's JSON: %v", j)
	}
}

// A seq-v1 experiment's look waits for its stage's grades: a grade the judge's errors left pending is graded again from
// the run's change before the look, and the agent runs once per slot.
func TestSeqExperimentWaitsForPendingGrades(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "seq", "--b", "lean", "--task", "value", "--task", "judged", "--goal", "cheaper", "--concurrency", "1", "--seed", "5"),
		ExitOK, "seq-v1")
	writeFile(t, ctrl, "judge-broken-calls", "5") // the first judge-graded run's grading brings no answer
	got := f.run(ctx, "experiment", "run", "seq")
	expect(t, got, ExitOK, "grade pending (the judge answered 0 of 5 times", "; the agent did not run again): fixed (5 of 5)", "Look 1 of 1", "Experiment seq: done")
	if i, j := strings.Index(got.stdout, "Graded run "), strings.Index(got.stdout, "Look 1 of 1"); i < 0 || j < i {
		t.Errorf("the look came before the pending grade was graded:\n%s", got.stdout)
	}
	if n := agentRuns(t, ctrl); n != 4 || len(experimentRuns(t, f, "seq")) != 4 {
		t.Errorf("%d agent runs, %d stored: want 4 and 4", n, len(experimentRuns(t, f, "seq")))
	}
}
