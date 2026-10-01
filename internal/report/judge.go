package report

import (
	"fmt"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
)

// Judge is the LLM judge's second opinion on an experiment's runs: present only when the experiment's lock has judge
// settings. It decides nothing (.agents/decisions/2026-10-01-llm-judge-alongside-tests.md): pass, fail and every
// verdict of the analysis stay the tests'. Counts cover counted (fair) runs; costs cover every run, as the spend does.
type Judge struct {
	Model   string     `json:"model"`
	Effort  string     `json:"effort"`
	Repeats int        `json:"repeats"`
	Arms    []JudgeArm `json:"arms"`
	// Agreement is how many of the judged runs with two or more answers got the same answer from every repeat.
	Agreement Proportion `json:"agreement"`
	CostUSD   float64    `json:"cost_usd"` // in the spend; never in the arms' costs
	// Pending counts counted, graded runs that still need the judge (run.NeedsJudging): never judged, or stopped early.
	Pending int `json:"pending"`
	// Flagged are the passing runs the judge did not call fixed, in schedule order, every one of them; the terminal and
	// Markdown show the first MaxFlagged.
	Flagged []Flagged `json:"flagged"`
}

// MaxFlagged is how many flagged runs the terminal and Markdown list; the JSON has them all.
const MaxFlagged = 10

// JudgeArm is one arm's verdicts, split by the tests' result (experiment.Success), and what its judge cost.
type JudgeArm struct {
	Name      string      `json:"name"`
	Passing   JudgeCounts `json:"passing"`
	Failing   JudgeCounts `json:"failing"`
	NotJudged NotJudged   `json:"not_judged"`
	CostUSD   float64     `json:"cost_usd"`
}

// JudgeCounts are the verdicts of an arm's counted runs with one test result. Judged runs got an answer; each share is
// of them, with its 95% Wilson interval.
type JudgeCounts struct {
	Judged int        `json:"judged"`
	Fixed  Proportion `json:"fixed"`
	Partly Proportion `json:"partly"`
	No     Proportion `json:"no"`
}

// Proportion is Count of Of, with its share and 95% Wilson interval. Share is nil when Of is zero (the interval is
// then 0 to 1).
type Proportion struct {
	Count int      `json:"count"`
	Of    int      `json:"of"`
	Share *float64 `json:"share"`
	Low   float64  `json:"low"`
	High  float64  `json:"high"`
}

func proportion(count, of int) Proportion {
	p := Proportion{Count: count, Of: of}
	p.Low, p.High = stats.Wilson(count, of)
	if of > 0 {
		share := float64(count) / float64(of)
		p.Share = &share
	}
	return p
}

// NotJudged counts an arm's counted runs without a judge's answer, by why.
type NotJudged struct {
	Empty       int `json:"empty"`        // the run changed no code: the judge was not asked
	NoAnswer    int `json:"no_answer"`    // no repeat answered (final)
	Stopped     int `json:"stopped"`      // the judgement stopped early (a usage limit, an interrupt): a resume judges it
	Waiting     int `json:"waiting"`      // graded but never judged yet: a resume judges it
	NoReference int `json:"no_reference"` // the task has no reference solution in code to judge against
	NotGraded   int `json:"not_graded"`   // no test result, so nothing to judge beside
}

func (n NotJudged) total() int {
	return n.Empty + n.NoAnswer + n.Stopped + n.Waiting + n.NoReference + n.NotGraded
}

// Flagged is a passing run the judge did not call fixed.
type Flagged struct {
	Run     string `json:"run"`
	Task    string `json:"task"`
	Arm     string `json:"arm"`
	Verdict string `json:"verdict"` // judge.Partly or judge.No
	// Detail is the verdict in words (run.Describe): how many repeats agreed.
	Detail string `json:"detail"`
	// Reason is the judge's one-line reason, scrubbed; empty when the repeats had no majority.
	Reason string `json:"reason"`
}

// judgeSummary builds the Judge section; nil without the judge.
func judgeSummary(in Input) *Judge {
	settings := in.Lock.Design.Judge
	if settings == nil {
		return nil
	}
	s := settings.WithDefaults()
	out := &Judge{Model: s.Model, Effort: s.Effort, Repeats: s.Repeats, Flagged: []Flagged{}}
	byArm := map[string]*JudgeArm{}
	for _, a := range in.Lock.Arms {
		out.Arms = append(out.Arms, JudgeArm{Name: a.Name})
	}
	for i := range out.Arms {
		byArm[out.Arms[i].Name] = &out.Arms[i]
	}
	type tally struct{ fixed, partly, no, judged int }
	counts := map[string]*[2]tally{} // per arm: passing, failing
	agreed, multi := 0, 0
	for _, r := range bySlot(in.Runs) {
		rec := r.Record
		arm := byArm[rec.Arm]
		if arm == nil {
			continue
		}
		arm.CostUSD += rec.JudgeCostUSD()
		out.CostUSD += rec.JudgeCostUSD()
		if !experiment.Fair(rec.Outcome) {
			continue
		}
		v, nj := rec.Judge, &arm.NotJudged
		t, known := in.Lock.Task(rec.Task)
		switch {
		case rec.Passed == nil:
			nj.NotGraded++
			continue
		case known && run.NeedsJudging(rec, t.Spec()):
			if v != nil {
				nj.Stopped++
			} else {
				nj.Waiting++
			}
			out.Pending++
			continue
		case v == nil:
			nj.NoReference++ // a known task is waiting above; a run of a task the lock lacks cannot be judged either
			continue
		case v.Empty:
			nj.Empty++
			continue
		case v.Fixed == "":
			nj.NoAnswer++
			continue
		}
		success := experiment.Success(rec.Outcome, rec.Passed, rec.Behavior.ConfigChanged)
		if counts[rec.Arm] == nil {
			counts[rec.Arm] = &[2]tally{}
		}
		c := &counts[rec.Arm][map[bool]int{true: 0, false: 1}[success]]
		c.judged++
		switch v.Fixed {
		case judge.Yes:
			c.fixed++
		case judge.Partly:
			c.partly++
		case judge.No:
			c.no++
		}
		if len(v.Answers) >= 2 {
			multi++
			if allSame(v.Answers) {
				agreed++
			}
		}
		if success && v.Fixed != judge.Yes {
			out.Flagged = append(out.Flagged, Flagged{Run: r.ID, Task: rec.Task, Arm: rec.Arm, Verdict: v.Fixed, Detail: run.Describe(*v),
				Reason: oneLine(in.scrub(v.Reason))})
		}
	}
	for i := range out.Arms {
		a := &out.Arms[i]
		c := counts[a.Name]
		if c == nil {
			c = &[2]tally{}
		}
		for j, dst := range []*JudgeCounts{&a.Passing, &a.Failing} {
			x := c[j]
			*dst = JudgeCounts{Judged: x.judged, Fixed: proportion(x.fixed, x.judged), Partly: proportion(x.partly, x.judged), No: proportion(x.no, x.judged)}
		}
	}
	out.Agreement = proportion(agreed, multi)
	return out
}

func allSame(answers []string) bool {
	for _, a := range answers {
		if a != answers[0] {
			return false
		}
	}
	return true
}

// oneLine joins a text's lines and runs of spaces into one line.
func oneLine(text string) string { return strings.Join(strings.Fields(text), " ") }

// shareVerdict is a run's verdict as a report shares it: its texts scrubbed (Claude Code's, which may name paths).
func (in Input) shareVerdict(v *judge.Verdict) *judge.Verdict {
	if v == nil {
		return nil
	}
	out := *v
	out.Reason = in.scrub(v.Reason)
	out.Reasons = in.scrubAll(v.Reasons)
	out.Errors = in.scrubAll(v.Errors)
	if out.Reasons == nil {
		out.Reasons = []string{}
	}
	return &out
}

// judgeView is the Judge section's text, shared by the terminal and Markdown renderings.
type judgeView struct {
	intro string
	rows  [][]string // arm, tests, judged, fixed, partly, no
	lines []string   // not judged, agreement, cost, pending
	// flaggedTitle heads the list of flagged runs; flagged are its first MaxFlagged entries (arm and task first, then the
	// verdict, then the reason), and more how many it leaves out.
	flaggedTitle string
	flagged      [][3]string
	more         int
}

var judgeColumns = []string{"Arm", "Tests", "Judged", "Fixed", "Partly", "No"}

func (r Report) judgeView() judgeView {
	j := r.Judge
	v := judgeView{intro: fmt.Sprintf("A second opinion beside the tests, which decides nothing: pass, fail and the verdicts above are the tests'. "+
		"The judge, %s at effort %s with %d %s per run (the majority answer), read each counted run's code change beside the task's instruction "+
		"and its reference solution. Its accuracy is unmeasured. Tests passed or failed as the success metric counts them; shares are of the runs it judged, with 95%% Wilson intervals.",
		j.Model, j.Effort, j.Repeats, plural(j.Repeats, "repeat"))}
	cell := func(p Proportion) string {
		if p.Share == nil {
			return "-"
		}
		return fmt.Sprintf("%d (%s; %.0f–%.0f%%)", p.Count, pct(*p.Share), 100*p.Low, 100*p.High)
	}
	for _, a := range j.Arms {
		for _, side := range []struct {
			label string
			c     JudgeCounts
		}{{"passed", a.Passing}, {"failed", a.Failing}} {
			v.rows = append(v.rows, []string{a.Name, side.label, fmt.Sprint(side.c.Judged), cell(side.c.Fixed), cell(side.c.Partly), cell(side.c.No)})
		}
	}
	var missing []string
	for _, a := range j.Arms {
		n := a.NotJudged
		if n.total() == 0 {
			continue
		}
		var why []string
		for _, w := range []struct {
			n    int
			text string
		}{{n.Empty, "changed no code"}, {n.NoAnswer, "got no answer"}, {n.Stopped, "stopped early"}, {n.Waiting, "not judged yet"},
			{n.NoReference, "have no reference in code"}, {n.NotGraded, "have no test result"}} {
			if w.n > 0 {
				why = append(why, fmt.Sprintf("%d %s", w.n, w.text))
			}
		}
		missing = append(missing, fmt.Sprintf("%s %d (%s)", a.Name, n.total(), strings.Join(why, ", ")))
	}
	if len(missing) == 0 {
		v.lines = append(v.lines, "Not judged: none; the judge answered for every counted run.")
	} else {
		v.lines = append(v.lines, "Not judged: "+strings.Join(missing, "; ")+".")
	}
	if g := j.Agreement; g.Share != nil {
		v.lines = append(v.lines, fmt.Sprintf("Repeat agreement: every repeat gave the same answer in %d of %d runs with two or more answers (%s; %.0f–%.0f%%).",
			g.Count, g.Of, pct(*g.Share), 100*g.Low, 100*g.High))
	} else {
		v.lines = append(v.lines, "Repeat agreement: no judged run has two or more answers to compare.")
	}
	var costs []string
	for _, a := range j.Arms {
		costs = append(costs, fmt.Sprintf("%s $%.2f", a.Name, a.CostUSD))
	}
	v.lines = append(v.lines, fmt.Sprintf("Judge cost: %s; $%.2f in total, in the spend and not in the arms' costs.", strings.Join(costs, ", "), j.CostUSD))
	if j.Pending > 0 {
		v.lines = append(v.lines, fmt.Sprintf("%d run(s) still need the judge: agentium experiment run %s judges them.", j.Pending, r.Experiment))
	}
	if len(j.Flagged) == 0 {
		v.flaggedTitle = "The judge called every passing run it judged fixed."
		return v
	}
	v.flaggedTitle = fmt.Sprintf("Passing runs the judge did not call fixed (%d), with its reasons, to check by hand:", len(j.Flagged))
	for i, f := range j.Flagged {
		if i == MaxFlagged {
			v.more = len(j.Flagged) - MaxFlagged
			break
		}
		reason := f.Reason
		if reason == "" {
			reason = "The repeats had no majority, so there is no single reason."
		}
		v.flagged = append(v.flagged, [3]string{fmt.Sprintf("%s, arm %s", f.Task, f.Arm), f.Detail, reason})
	}
	return v
}

// pendingNote replaces the "not finished" note when every slot settled and only judge verdicts are missing: the
// success and cost verdicts are complete then. ok is false otherwise.
func pendingNote(rep Report) (string, bool) {
	if rep.Judge == nil || rep.Judge.Pending == 0 || rep.Settled < rep.Slots {
		return "", false
	}
	note := fmt.Sprintf("Every run settled, so the success and cost verdicts are complete; %d run(s) lack a judge verdict", rep.Judge.Pending)
	switch rep.Status {
	case experiment.StatusBudget:
		note += fmt.Sprintf(", which the budget leaves no room for: agentium experiment run %s --budget USD judges them.", rep.Experiment)
	case experiment.StatusUsage:
		note += fmt.Sprintf(" (the judge was paused at a usage limit): agentium experiment run %s judges them once it resets.", rep.Experiment)
	default:
		note += fmt.Sprintf(": agentium experiment run %s judges them.", rep.Experiment)
	}
	return note, true
}
