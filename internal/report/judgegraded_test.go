package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/experiment"
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
	if success.Tasks != 6 || judged.Tasks != 4 || cost.Tasks != 10 {
		t.Errorf("tasks: success %d (want the 6 test-graded), the judge's %d (want the 4 judge-graded), cost %d (want all 10)", success.Tasks, judged.Tasks, cost.Tasks)
	}
	if judged.Role != experiment.RoleSecondary || judged.Verdict != stats.Exploratory || judged.Note != experiment.NoteJudgeSuccess ||
		judged.FloorTasks != experiment.MinTasksSuccess || judged.FloorRepeats != experiment.MinRepeats || judged.FullTasks != 0 {
		t.Errorf("the judge's success: %+v", judged)
	}
	if success.Repeats != 1 || success.FloorTasks != experiment.MinTasksSuccess || success.Verdict != stats.Exploratory {
		t.Errorf("success counts only test-graded tasks toward its floor: %+v", success)
	}
	if rep.Analysis.Excluded["infra"] != 1 || rep.Analysis.JudgePassAt1["B"] != 0.75 || rep.Analysis.JudgePassAt1["A"] != 1 {
		t.Errorf("excluded %v, the judge's pass@1 %v", rep.Analysis.Excluded, rep.Analysis.JudgePassAt1)
	}
	g := rep.JudgeGrading
	if g == nil || len(g.Tasks) != 4 || g.Repeats != 5 || g.Arms[0].Graded != 4 || g.Arms[1].Graded != 4 || g.Arms[1].Fixed.Count != 3 || g.Arms[1].Ungraded != 1 {
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
		for _, want := range []string{"The judge says fixed 100% → 75%", "(judge-graded tasks, unvalidated): exploratory, never a verdict",
			"Success (passed the tests, test-graded tasks only): pass@1", "The judge says fixed (judge-graded tasks, unvalidated): A 4 of 4 runs (100%), B 3 of 4 (75%)",
			"task-6 (judge)", "(judge) marks a judge-graded task", "4 task(s) are judge-graded (task-6, task-7, task-8, task-9)", "never toward success",
			"1 run(s) the judge could not grade", "against success's floor of 20 tasks, counted over judge-graded tasks alone"} {
			if !strings.Contains(out, want) {
				t.Errorf("the report lacks %q:\n%s", want, out)
			}
		}
	}
	if !strings.Contains(md.String(), "| The judge says fixed | secondary | 100% | 75% |") {
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
