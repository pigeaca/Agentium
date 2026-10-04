package report

import (
	"fmt"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/task"
)

// JudgeGrading is how the judge graded an experiment's judge-graded tasks: present only when the lock has such tasks,
// so a report of test-graded tasks reads as it always did. Their grades are unvalidated: they count toward the judge's
// own success (experiment.MetricJudgeSuccess), never toward the tests' success, and no verdict rests on them.
type JudgeGrading struct {
	Model   string            `json:"model"`
	Effort  string            `json:"effort"`
	Repeats int               `json:"repeats"`
	Tasks   []string          `json:"tasks"` // the judge-graded tasks, in the lock's order
	Arms    []JudgeGradingArm `json:"arms"`
	CostUSD float64           `json:"cost_usd"` // grading's spend, in the total; never in the arms' costs
}

// JudgeGradingArm is one arm's judge-graded runs: the counted ones the judge graded and how many it called fixed (with
// the 95% Wilson interval), the fair runs it left without a grade for good (a tie, refusals or malformed replies, or
// errors on every attempt: not counted, not tried again), those whose grade is still pending, and what grading cost.
type JudgeGradingArm struct {
	Name     string     `json:"name"`
	Profile  string     `json:"profile,omitempty"` // in a model-ab experiment
	Graded   int        `json:"graded"`
	Fixed    Proportion `json:"fixed"`
	Ungraded int        `json:"ungraded"`
	Pending  int        `json:"pending,omitempty"`
	CostUSD  float64    `json:"cost_usd"`
}

// judgeGraded reports whether the run was graded by the judge.
func judgeGraded(rec run.Record) bool { return rec.GradedBy == task.GradingJudge }

// counted reports whether the analysis counts the run: a fair one, and for a judge-graded run one the judge graded (a
// grade pending or left out for good counts nowhere, cost included). For a test-graded run it is experiment.Fair.
func counted(rec run.Record) bool {
	return experiment.Fair(rec.Outcome) && (!judgeGraded(rec) || rec.Passed != nil)
}

// judgeGradingSummary sums up the judge's grading; nil without judge-graded tasks.
func judgeGradingSummary(in Input) *JudgeGrading {
	l := in.Lock
	if !l.JudgeGraded() {
		return nil
	}
	s := judge.GradingSettings()
	if g := l.Design.JudgeGrading; g != nil {
		s = g.WithDefaults()
	}
	out := &JudgeGrading{Model: s.Model, Effort: s.Effort, Repeats: s.Repeats, Tasks: []string{}}
	for _, t := range l.Tasks {
		if t.JudgeGraded() {
			out.Tasks = append(out.Tasks, t.Name)
		}
	}
	for _, a := range l.Arms {
		arm := JudgeGradingArm{Name: a.Name, Profile: armProfile(l.Design, a)}
		fixed := 0
		for _, r := range in.Runs {
			rec := r.Record
			if rec.Arm != a.Name || !judgeGraded(rec) {
				continue
			}
			arm.CostUSD += rec.Spend().JudgeUSD
			switch {
			case !experiment.Fair(rec.Outcome):
			case rec.Passed != nil:
				arm.Graded++
				if experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged) {
					fixed++
				}
			case run.NeedsGrading(rec):
				arm.Pending++
			default:
				arm.Ungraded++
			}
		}
		arm.Fixed = proportion(fixed, arm.Graded)
		out.CostUSD += arm.CostUSD
		out.Arms = append(out.Arms, arm)
	}
	return out
}

// judgeSuccessResult is the analysis' judge-success metric; nil without it.
func (r Report) judgeSuccessResult() *experiment.MetricResult {
	for i, res := range r.Analysis.Results {
		if res.Metric == experiment.MetricJudgeSuccess {
			return &r.Analysis.Results[i]
		}
	}
	return nil
}

// judgeHeadline is the headline of the judge's success, beside the verdicts but never one: "The judge says fixed 67% →
// 100% (judge-graded tasks, unvalidated): exploratory, never a verdict"; parts as headlineParts gives them. ok is false
// without judge-graded tasks.
func (r Report) judgeHeadline() (bold, mid, verdict string, ok bool) {
	res := r.judgeSuccessResult()
	if res == nil {
		return "", "", "", false
	}
	return fmt.Sprintf("%s %s → %s", title(res.Metric), pctOf(res.A), pctOf(res.B)), " (judge-graded tasks, unvalidated): ",
		"exploratory, never a verdict", true
}

// successLabel is the Success line's lead: "Success", or, beside judge-graded tasks, "Success (passed the tests,
// test-graded tasks only)", so the line names its kind.
func (r Report) successLabel() string {
	if r.JudgeGrading == nil {
		return "Success"
	}
	return "Success (passed the tests, test-graded tasks only)"
}

// judgeGradingLine is the judge's success in a sentence, under the tests' Success line: "The judge says fixed
// (judge-graded tasks, unvalidated): A 2 of 3 runs (67%), B 3 of 3 (100%); a majority of 5 calls on claude-opus-5-5 at
// effort high grades each run; grading cost $0.65." Empty without judge-graded tasks.
func (r Report) judgeGradingLine() string {
	g := r.JudgeGrading
	if g == nil {
		return ""
	}
	var parts []string
	for i, a := range g.Arms {
		runs := " runs"
		if i > 0 {
			runs = ""
		}
		share := "-"
		if a.Fixed.Share != nil {
			share = pct(*a.Fixed.Share)
		}
		label := a.Name
		if i < len(r.Arms) {
			label = r.Arms[i].tag()
		}
		parts = append(parts, fmt.Sprintf("%s %d of %d%s (%s)", label, a.Fixed.Count, a.Graded, runs, share))
	}
	return fmt.Sprintf("The judge says fixed (judge-graded tasks, unvalidated): %s; a majority of %d calls on %s at effort %s grades each run; grading cost $%.2f.",
		strings.Join(parts, ", "), g.Repeats, g.Model, g.Effort, g.CostUSD)
}

// judgeGradingNote is the honesty note on judge-graded tasks: what grades them, why their grades stand apart, what
// counts them, and the judge's success against its own floor; "" without them.
func (r Report) judgeGradingNote() string {
	g := r.JudgeGrading
	if g == nil {
		return ""
	}
	ungraded, pending := 0, 0
	var perArm, pendingArms []string
	for i, a := range g.Arms {
		ungraded += a.Ungraded
		pending += a.Pending
		label := a.Name
		if i < len(r.Arms) {
			label = r.Arms[i].tag()
		}
		perArm = append(perArm, fmt.Sprintf("%s %d", label, a.Ungraded))
		pendingArms = append(pendingArms, fmt.Sprintf("%s %d", label, a.Pending))
	}
	note := fmt.Sprintf("%d task(s) are judge-graded (%s): no tests ran; the judge compared each run's change with the reference solution, and a majority of its %d calls "+
		"passed or failed the run. A judge error leaves the grade pending, graded again later from the run's change, never by running the agent again; a tie, "+
		"a refusal or a reply still malformed when asked again leaves the run without a grade: not counted, never a fail, not tried again. These grades are "+
		"unvalidated (the judge pilot's grading without tests was inconclusive), so they count toward \"the judge says fixed\" alone, never toward success, "+
		"which is the tests'; no verdict rests on them, and the north star leaves out an experiment with judge-graded tasks. Cost, time and output tokens count "+
		"every graded run of both kinds.",
		len(g.Tasks), strings.Join(g.Tasks, ", "), g.Repeats)
	if ungraded > 0 {
		note += fmt.Sprintf(" Runs the judge left without a grade (not counted, not tried again): %s", strings.Join(perArm, ", "))
		if u := r.Analysis.Ungraded; u != nil {
			switch {
			case u.Imbalanced:
				note += "; the arms differ, so the verdicts they would feed (cost, time, output tokens) are demoted to inconclusive"
			case len(u.Disagrees) > 0:
				note += "; counting them changes the " + strings.Join(u.Disagrees, " and ") + " verdict, so it is demoted to inconclusive"
			}
		}
		note += "."
	}
	if pending > 0 {
		note += fmt.Sprintf(" Grades still pending (not counted until graded): %s.", strings.Join(pendingArms, ", "))
	}
	if res := r.judgeSuccessResult(); res != nil {
		note += fmt.Sprintf(" The judge says fixed: %d of %d task(s) have %d or more graded %s in both arms, against success's floor of %d tasks, counted over judge-graded tasks alone.",
			res.FullTasks, res.Tasks, res.FloorRepeats, plural(res.FloorRepeats, "run"), res.FloorTasks)
	}
	return note
}

// judgedTaskName is a task's name in the per-task table, marked when the judge grades it.
func (r Report) judgedTaskName(t TaskRow) string {
	if t.Judged {
		return t.Task + " (judge)"
	}
	return t.Task
}

// judgedLegend is the per-task legend's addition beside judge-graded tasks; "" without them.
func (r Report) judgedLegend() string {
	if r.JudgeGrading == nil {
		return ""
	}
	return " (judge) marks a judge-graded task: ● the judge says fixed, ○ not fixed (unvalidated)."
}
