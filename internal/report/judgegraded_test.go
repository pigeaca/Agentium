package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/report/reporttest"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/term"
)

// An experiment that mixes test-graded and judge-graded tasks: success is two metrics over separate tasks, each against
// its own floor, and the judge's never has a verdict; cost counts every graded run of both kinds; every judge grade is
// labelled in the Markdown, the terminal and the JSON; the judge's texts are scrubbed.
func TestReportJudgeGraded(t *testing.T) {
	rep, err := Build(inputOf(reporttest.JudgeGraded()))
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]experiment.MetricResult{}
	for _, r := range rep.Analysis.Results {
		results[r.Metric] = r
	}
	success, judged, cost := results[experiment.MetricSuccess], results[experiment.MetricJudgeSuccess], results[experiment.MetricCost]
	if success.Tasks != 6 || judged.Tasks != 3 || cost.Tasks != 9 {
		t.Errorf("tasks: success %d (want the 6 test-graded), the judge's %d (want the 3 judge-graded with a grade in both arms), cost %d (want the 9 graded in both)",
			success.Tasks, judged.Tasks, cost.Tasks)
	}
	// Arm B's run left without a grade, and none in arm A: the arms differ, so the cost verdict is demoted, with the
	// sensitivity check (that run counted) beside it; the tests' success never counts a judge-graded run.
	u := rep.Analysis.Ungraded
	if u == nil || u.Ungraded["A"] != 0 || u.Ungraded["B"] != 1 || !u.Imbalanced || u.AsCounted[experiment.MetricCost] == "" {
		t.Fatalf("the ungraded check: %+v", u)
	}
	if _, ok := u.AsCounted[experiment.MetricSuccess]; ok || cost.Verdict != stats.Inconclusive || !strings.Contains(cost.Note, "the judge left without a grade differ (A 0, B 1)") {
		t.Errorf("cost: %+v; as counted %v", cost, u.AsCounted)
	}
	if judged.Role != experiment.RoleSecondary || judged.Verdict != stats.Exploratory || judged.Note != experiment.NoteJudgeSuccess ||
		judged.FloorTasks != experiment.MinTasksSuccess || judged.FloorRepeats != experiment.MinRepeats || judged.FullTasks != 0 {
		t.Errorf("the judge's success: %+v", judged)
	}
	if success.Repeats != 1 || success.FloorTasks != experiment.MinTasksSuccess || success.Verdict != stats.Exploratory {
		t.Errorf("success counts only test-graded tasks toward its floor: %+v", success)
	}
	if rep.Analysis.Excluded[experiment.OutcomeUngraded] != 1 || len(rep.Analysis.Excluded) != 1 || rep.Analysis.JudgePassAt1["B"] != 2.0/3 || rep.Analysis.JudgePassAt1["A"] != 1 {
		t.Errorf("excluded %v, the judge's pass@1 %v", rep.Analysis.Excluded, rep.Analysis.JudgePassAt1)
	}
	g := rep.JudgeGrading
	if g == nil || len(g.Tasks) != 4 || g.Repeats != 5 || g.Arms[0].Graded != 4 || g.Arms[1].Graded != 3 || g.Arms[1].Fixed.Count != 2 || g.Arms[1].Ungraded != 1 ||
		g.Arms[0].Ungraded != 0 {
		t.Fatalf("judge grading %+v", g)
	}
	if rep.Judge != nil {
		t.Errorf("no second-opinion judge was asked for, yet the report has one: %+v", rep.Judge)
	}

	var md, plain, js bytes.Buffer
	if err := rep.Markdown(&md); err != nil {
		t.Fatal(err)
	}
	if err := rep.Terminal(&plain, term.Style{}); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&js); err != nil {
		t.Fatal(err)
	}
	golden(t, "lean-ab-judge-graded.md", md.Bytes())
	golden(t, "lean-ab-judge-graded.txt", plain.Bytes())
	goldenJSON(t, "lean-ab-judge-graded.json", js.Bytes())
	for _, out := range []string{md.String(), plain.String()} {
		for _, want := range []string{"The judge says fixed 100% → 67%", "(judge-graded tasks, unvalidated): exploratory, never a verdict",
			"Success (passed the tests, test-graded tasks only): pass@1", "The judge says fixed (judge-graded tasks, unvalidated): A 4 of 4 runs (100%), B 2 of 3 (67%)",
			"task-6 (judge)", "(judge) marks a judge-graded task", "4 task(s) are judge-graded (task-6, task-7, task-8, task-9)", "never toward success",
			"Runs the judge left without a grade (not counted, not tried again): A 0, B 1; the arms differ, so the verdicts they would feed (cost, time, output tokens) are demoted to inconclusive",
			"1 judge-graded, left without a grade (not tried again)", "the north star leaves out an experiment with judge-graded tasks", "against success's floor of 20 tasks, counted over judge-graded tasks alone"} {
			if !strings.Contains(out, want) {
				t.Errorf("the report lacks %q:\n%s", want, out)
			}
		}
	}
	if !strings.Contains(md.String(), "| The judge says fixed | secondary | 100% | 67% |") {
		t.Errorf("the metrics table lacks the judge's row:\n%s", md.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["judge_grading"]; !ok || !strings.Contains(js.String(), `"graded_by": "judge"`) || !strings.Contains(js.String(), `"judged": true`) ||
		!strings.Contains(js.String(), `"judge_pass_at_1"`) {
		t.Errorf("the JSON lacks the judge grading's fields")
	}
	for _, out := range []string{md.String(), plain.String(), js.String()} {
		if strings.Contains(out, "sk-ant-api03-jjj") || strings.Contains(out, "/home/someone") {
			t.Errorf("a judge's text leaked a key or a path")
		}
	}
}

// A report of test-graded tasks has none of the judge grading's additions.
func TestReportWithoutJudgeGradedTasksHasNoneOfIt(t *testing.T) {
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	var md, js bytes.Buffer
	if err := rep.Markdown(&md); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&js); err != nil {
		t.Fatal(err)
	}
	for _, not := range []string{"judge-graded", "The judge says fixed", "(judge)", "passed the tests"} {
		if strings.Contains(md.String(), not) {
			t.Errorf("the Markdown says %q", not)
		}
	}
	for _, not := range []string{"judge_grading", "graded_by", `"judged"`, "judge_pass_at_1", experiment.MetricJudgeSuccess} {
		if strings.Contains(js.String(), not) {
			t.Errorf("the JSON has %q", not)
		}
	}
}

// Beside the second-opinion judge (--judge), a judge-graded run's verdict is its grade, never a second opinion: the
// judge section counts, flags and costs the test-graded runs alone.
func TestSecondOpinionLeavesOutJudgeGradedRuns(t *testing.T) {
	in := inputOf(reporttest.JudgeGraded())
	s := judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 3}
	in.Lock.Design.Judge = &s
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	j := rep.Judge
	if j == nil {
		t.Fatal("no second-opinion section")
	}
	judged := 0
	for _, a := range j.Arms {
		judged += a.Passing.Judged + a.Failing.Judged
		if a.CostUSD != 0 {
			t.Errorf("arm %s: the grading's spend counted as the second opinion's: $%.2f", a.Name, a.CostUSD)
		}
	}
	if judged != 0 || j.CostUSD != 0 || len(j.Flagged) != 0 {
		t.Errorf("the second opinion counted judge-graded runs: %d judged, $%.2f, flagged %+v", judged, j.CostUSD, j.Flagged)
	}
	if rep.JudgeGrading == nil || rep.JudgeGrading.CostUSD <= 0 {
		t.Errorf("the grading's spend: %+v", rep.JudgeGrading)
	}
}
