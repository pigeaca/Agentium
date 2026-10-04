package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
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
		"each run's grading up to $5.00", "no verdict rests on them")
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
			if rec.GradedBy != task.GradingJudge || rec.Outcome != claude.OutcomeOK || rec.Passed == nil || !*rec.Passed || v == nil || v.Requested != 5 ||
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
		if rec.Task == "judged" && (rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed) {
			t.Errorf("a run the judge called not fixed: %s, passed %v", rec.Outcome, rec.Passed)
		}
	}
	expect(t, f.run(ctx, "experiment", "report", "refused", "--markdown"), ExitOK, "| judged (judge) | ○ 0/1 | ○ 0/1 |")
}

// The judge at a usage limit leaves a judge-graded run without a grade: infrastructure, never failed. The experiment
// pauses; once the limit lifts, a resume tries the run again, its first try's spend counted.
func TestExperimentJudgeGradeAtALimit(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	addJudgedTask(t, f)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "limit", "--b", "lean", "--task", "judged", "--goal", "better", "--repeats", "1", "--concurrency", "1"), ExitOK)
	writeFile(t, ctrl, "judge-limit", "")
	paused := f.run(ctx, "experiment", "run", "limit")
	expect(t, paused, ExitOK, "infra, $0.30; graded by the judge (unvalidated): not graded (the judge answered 0 of 5 times",
		"Paused: the judge hit a usage limit or a sign-in failure")
	recs := records(t, experimentRuns(t, f, "limit"))
	if len(recs) != 1 || recs[0].Outcome != claude.OutcomeInfra || recs[0].Passed != nil || recs[0].Judge == nil || recs[0].Judge.Stopped != llmjudge.StoppedLimit ||
		!strings.Contains(strings.Join(recs[0].Notes, "; "), "not graded: the judge answered 0 of 5 times") {
		t.Fatalf("the ungraded run: %+v", recs)
	}
	os.Remove(filepath.Join(ctrl, "judge-limit"))
	resumed := f.run(ctx, "experiment", "run", "limit")
	expect(t, resumed, ExitOK, "(attempt 2 of 3)", "graded by the judge (unvalidated): fixed (5 of 5)", "Experiment limit: done")
	runs := experimentRuns(t, f, "limit")
	graded, infra := 0, 0
	for _, rec := range records(t, runs) {
		switch rec.Outcome {
		case claude.OutcomeOK:
			graded++
		case claude.OutcomeInfra:
			infra++
		}
	}
	if graded != 2 || infra != 1 {
		t.Errorf("%d graded and %d infrastructure runs, want 2 and 1", graded, infra)
	}
	// Three agent runs ($0.30 each), two gradings ($0.25 each) and the call that hit the limit ($0.01).
	expect(t, f.run(ctx, "experiment", "show", "limit"), ExitOK, "spent $1.41 of")
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

	// A judge that brings no answer leaves the run ungraded (infrastructure), never failed.
	writeFile(t, ctrl, "judge-broken", "")
	broken := f.run(ctx, "run", "once", "judged")
	expect(t, broken, ExitOK, "outcome      infra; judge: not graded (the judge answered 0 of 5 times", "not graded: the judge answered 0 of 5 times")
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
