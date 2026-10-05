package cli

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// planCautions are the tasks of a --goal better experiment that may not tell its versions apart: the task list's tags
// (tellsTooEasy, tellsNeverPassed), so the list and the preview agree. They are cautions: nothing blocks a run on
// them, and which tasks run does not change. Applies is false for another goal, where they are not drawn.
type planCautions struct {
	Applies     bool
	TooEasy     []string // passed every time, with at least tellsMinRunsToRate graded runs
	NeverPassed []string // never passed, with at least two graded runs
}

// planCautionsOf names, among the design's tasks, those that passed every time and those that never passed, from their
// graded runs (the same query and rule as the task list). Only a --goal better design has any.
func planCautionsOf(ctx context.Context, w *workspace, d experiment.Design) (planCautions, error) {
	if d.Goal != experiment.GoalBetter {
		return planCautions{}, nil
	}
	all, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return planCautions{}, err
	}
	var own []store.Task
	for _, t := range all {
		if slices.Contains(d.Tasks, t.Name) {
			own = append(own, t)
		}
	}
	tells, err := tellsOf(ctx, w.db, w.project.ID, own, func(store.Task) bool { return false })
	if err != nil {
		return planCautions{}, err
	}
	return cautionsFrom(tells), nil
}

// cautionsFrom picks the tasks that cannot separate from rows, sorted by name.
func cautionsFrom(rows []taskTell) planCautions {
	c := planCautions{Applies: true}
	for _, r := range rows {
		switch r.Tell {
		case tellsTooEasy:
			c.TooEasy = append(c.TooEasy, r.Task.Name)
		case tellsNeverPassed:
			c.NeverPassed = append(c.NeverPassed, r.Task.Name)
		}
	}
	slices.Sort(c.TooEasy)
	slices.Sort(c.NeverPassed)
	return c
}

// lines are the caution lines of "before it runs", each cut as checkLines cuts a long list.
func (c planCautions) lines(sh term.Shapes, m marks, width int) []string {
	var out []string
	mark := sh.Style.Paint(term.OutcomeInfra, m.warn)
	if n := len(c.TooEasy); n > 0 {
		text := fmt.Sprintf("%s passed every time so far, so %s may not tell the versions apart: %s", taskCount(n),
			map[bool]string{true: "it", false: "they"}[n == 1], strings.Join(c.TooEasy, ", "))
		out = append(out, checkLines(mark, text, sh, width)...)
	}
	if n := len(c.NeverPassed); n > 0 {
		out = append(out, checkLines(mark, fmt.Sprintf("%s never passed: check %s text (agentium task show NAME): %s", taskCount(n),
			map[bool]string{true: "its", false: "their"}[n == 1], strings.Join(c.NeverPassed, ", ")), sh, width)...)
	}
	return out
}

// pctWords is a share as a whole percent, or with a decimal below 1%: "25%", "4%", "0.4%".
func pctWords(share float64) string {
	share = math.Abs(share) * 100
	if share >= 0.95 {
		return fmt.Sprintf("%.0f%%", share)
	}
	return fmt.Sprintf("%.1f%%", share)
}

// groupDigits writes n with commas between thousands: 4575 is "4,575".
func groupDigits(n int64) string {
	s := fmt.Sprint(max(n, -n))
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if n < 0 {
		s = "-" + s
	}
	return s
}

// canAnswerPanel says how large a change this size can see, and for a context experiment what its contexts' size alone
// is expected to change. Below the metric's floor it says that, in place of a number.
func canAnswerPanel(a experiment.CanAnswer, sh term.Shapes, m marks) term.Panel {
	st := sh.Style
	p := term.Panel{Title: "can it answer?", Overflow: term.WrapText}
	const labelWidth = 21
	row := func(label, text string, role term.Role) string {
		if role != term.Default {
			text = st.Paint(role, text)
		}
		return term.Pad(label, labelWidth) + text
	}
	metric := map[string]string{experiment.MetricCost: "cost", experiment.MetricSuccess: "passes"}[a.Metric]
	switch {
	case !a.FloorMet:
		p.Border = term.LevelCaution
		p.Lines = []string{st.Paint(term.OutcomeInfra, fmt.Sprintf("%s no answer on %s at this size: it needs %s of %d %s each", m.warn, metric,
			taskCount(a.FloorTasks), a.FloorRepeats, plural(a.FloorRepeats, "run", "runs")))}
		return p
	case a.Smallest == nil:
		p.Lines = []string{st.Paint(term.Muted, "no tasks yet: nothing to size")}
		return p
	}
	var seen string
	switch {
	case a.Metric == experiment.MetricSuccess && *a.Smallest >= 1:
		seen = "no change of the pass rate"
	case a.Metric == experiment.MetricSuccess:
		seen = fmt.Sprintf("a pass-rate change of %.0f points or more", 100**a.Smallest)
	default:
		seen = fmt.Sprintf("a cost change of %s or more", pctWords(*a.Smallest))
	}
	p.Lines = []string{row("this size can see", seen, term.Default)}
	e := a.Expected
	if e == nil {
		return p
	}
	var expected string
	switch diff := e.TokensA - e.TokensB; {
	case diff == 0:
		expected = "no change: the contexts are the same size"
	case diff > 0:
		expected = fmt.Sprintf("about %s less: the context is %s tokens smaller", pctWords(e.Share), groupDigits(diff))
	default:
		expected = fmt.Sprintf("about %s more: the context is %s tokens larger", pctWords(e.Share), groupDigits(-diff))
	}
	p.Lines = append(p.Lines, row("expected from size", expected, term.Default))
	var note []string
	if a.NotSure {
		p.Lines = append(p.Lines, row("likely result", "not sure", term.OutcomeInfra))
		if a.RunsToSee > 0 {
			note = append(note, fmt.Sprintf("seeing %s would take about %d runs", pctWords(e.Share), a.RunsToSee))
		}
	}
	note = append(note, "size is not everything: a context that changes what the agent does can move cost more")
	p.Lines = append(p.Lines, st.Paint(term.Muted, m.words(strings.Join(note, " · "))))
	return p
}

// usagePanel is the plan's five-hour limit: the share used as a bar, then in words where runs pause (the mark; the bar
// carries none), how many runs fit now and how many limits the experiment needs. The caller draws it only for a current
// reading of a subscription's window.
func usagePanel(u experiment.UsagePreview, sh term.Shapes, m marks, width int) term.Panel {
	st := sh.Style
	l := u.Latest
	p := term.Panel{Title: "your plan", Overflow: term.WrapText}
	inner := min(width, term.MaxContentWidth) - 4
	p.Lines = []string{sh.Bar(term.Bar{Label: "five-hour limit", LabelWidth: 16, Fraction: l.Used, Value: fmt.Sprintf("%.0f%% used", 100*l.Used), ValueWidth: 9,
		Role: term.Level(l.Used, u.Limit)}, inner)}
	limits := fmt.Sprintf("%.1f", u.Windows)
	needs := fmt.Sprintf("this experiment needs about %s %s", limits, map[bool]string{true: "limit", false: "limits"}[limits == "1.0"])
	var words string
	switch {
	case l.Fits >= u.Runs:
		words = fmt.Sprintf("every run fits now %s %s", m.sep, needs)
	case l.Fits == 0:
		words = fmt.Sprintf("runs pause at %.0f%% %s no run fits now %s %s: it pauses and goes on after the reset (add --wait)", 100*u.Limit, m.sep, m.sep, needs)
	default:
		words = fmt.Sprintf("runs pause at %.0f%% %s about %d more %s fit now %s %s: it pauses and goes on after the reset (add --wait)", 100*u.Limit, m.sep,
			l.Fits, plural(l.Fits, "run", "runs"), m.sep, needs)
	}
	p.Lines = append(p.Lines, st.Paint(term.Muted, words))
	return p
}
