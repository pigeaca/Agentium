package cli

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
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
// graded runs: the task list's thresholds (tellsMinRunsToRate) and its rule for a pass (experiment.Success), counted
// alone, so a task that has become flaky or unreviewed since the experiment locked is still named. Only a --goal
// better design has any. The cautions are advice: a store that cannot be read gives none, never a failure.
func planCautionsOf(ctx context.Context, w *workspace, d experiment.Design) planCautions {
	if d.Goal != experiment.GoalBetter {
		return planCautions{}
	}
	all, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return planCautions{}
	}
	grades, err := w.db.TaskGrades(ctx, w.project.ID)
	if err != nil {
		return planCautions{}
	}
	graded, passed := map[int64]int{}, map[int64]int{}
	for _, g := range grades {
		if !experiment.Fair(g.Outcome) {
			continue
		}
		var changed []string
		if g.ConfigChanged {
			changed = []string{"config"}
		}
		graded[g.TaskID]++
		if experiment.Success(g.Outcome, &g.Passed, changed) {
			passed[g.TaskID]++
		}
	}
	c := planCautions{Applies: true}
	for _, t := range all {
		if !slices.Contains(d.Tasks, t.Name) {
			continue
		}
		n, k := graded[t.ID], passed[t.ID]
		switch {
		case k == 0 && n >= 2:
			c.NeverPassed = append(c.NeverPassed, t.Name)
		case n >= tellsMinRunsToRate && k == n:
			c.TooEasy = append(c.TooEasy, t.Name)
		}
	}
	slices.Sort(c.TooEasy)
	slices.Sort(c.NeverPassed)
	return c
}

// lines are the caution lines of "before it runs": the count, as many names as fit on two lines, then how many more
// there are and the command that lists them all.
func (c planCautions) lines(sh term.Shapes, m marks, width int) []string {
	var out []string
	mark := sh.Style.Paint(term.OutcomeInfra, m.warn)
	room := min(width, term.MaxContentWidth) - 4 - 2
	if n := len(c.TooEasy); n > 0 {
		head := fmt.Sprintf("%s passed every time, may not separate: ", taskCount(n))
		out = append(out, cautionList(mark, head, c.TooEasy, sh, room)...)
	}
	if n := len(c.NeverPassed); n > 0 {
		head := fmt.Sprintf("%s never passed, check %s text: ", taskCount(n), map[bool]string{true: "its", false: "their"}[n == 1])
		out = append(out, cautionList(mark, head, c.NeverPassed, sh, room)...)
	}
	return out
}

// cautionList is head and then the most names that keep it to two lines; the rest are counted, with the command that
// lists them.
func cautionList(mark, head string, names []string, sh term.Shapes, room int) []string {
	for k := len(names); ; k-- {
		text := head + strings.Join(names[:k], ", ")
		switch {
		case k == 0:
			text = head + "agentium task list"
		case k < len(names):
			text += fmt.Sprintf(" + %d more: agentium task list", len(names)-k)
		}
		if len(term.Wrap(term.Sanitize(text), room)) <= 2 || k == 0 {
			return checkLines(mark, text, sh, room+4+2)
		}
	}
}

// pctWords is a share as a whole percent, or with a decimal below 1%: "25%", "4%", "0.4%", "under 0.1%".
func pctWords(share float64) string {
	share = math.Abs(share) * 100
	if share < 0.05 {
		return "under 0.1%"
	}
	if share >= 0.95 {
		return fmt.Sprintf("%.0f%%", share)
	}
	return fmt.Sprintf("%.1f%%", share)
}

// maxRunsWords is the most runs the panel puts a number on: seeing a smaller change takes more than that.
const maxRunsWords = 10000

// aboutPct is "about 4%", or "under 0.1%" for a change too small to print.
func aboutPct(share float64) string {
	if math.Abs(share) < 0.0005 {
		return "under 0.1%"
	}
	return "about " + pctWords(share)
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
		// The threshold is a reduction; the same distance on the log scale is a larger rise (24.7% less, 32.8% more).
		seen = fmt.Sprintf("%s less cost, or %s more", pctWords(*a.Smallest), pctWords(*a.Smallest/(1-*a.Smallest)))
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
		expected = fmt.Sprintf("%s less: the context is %s tokens smaller", aboutPct(e.Share), groupDigits(diff))
	default:
		expected = fmt.Sprintf("%s more: the context is %s tokens larger", aboutPct(e.Share), groupDigits(-diff))
	}
	p.Lines = append(p.Lines, row("expected from size", expected, term.Default))
	var note []string
	if a.NotSure {
		p.Lines = append(p.Lines, row("likely result", "not sure", term.OutcomeInfra))
		if a.RunsToSee > 0 {
			runs := fmt.Sprintf("about %s runs", groupDigits(int64(a.RunsToSee)))
			if a.RunsToSee > maxRunsWords {
				runs = fmt.Sprintf("more than %s runs", groupDigits(maxRunsWords))
			}
			note = append(note, fmt.Sprintf("seeing %s would take %s", pctWords(e.Share), runs))
		}
	}
	note = append(note, "size is not everything: a context that changes what the agent does can move cost more")
	p.Lines = append(p.Lines, st.Paint(term.Muted, m.words(strings.Join(note, " · "))))
	return p
}

// usagePanel is the plan's five-hour limit: the share used as a bar, then in words where runs pause (also marked on the
// bar, see limitBar), how many runs fit now and how many limits the experiment needs. The caller draws it only for a current
// reading of a subscription's window.
func usagePanel(u experiment.UsagePreview, now time.Time, sh term.Shapes, m marks, width int) term.Panel {
	st := sh.Style
	l := u.Latest
	p := term.Panel{Title: "your plan", Overflow: term.WrapText}
	inner := min(width, term.MaxContentWidth) - 4
	value := fmt.Sprintf("%.0f%% used", 100*l.Used)
	// The reading is the last run's: other sessions may have used the plan since, so the words say when it was taken.
	read := ""
	if age, ok := l.Age(now); ok {
		read = "read " + ageWords(age) + " ago " + m.sep + " "
	}
	p.Lines = []string{limitBar(sh, "five-hour limit", value, l.Used, u.Limit, term.Level(l.Used, u.Limit), inner)}
	limits := fmt.Sprintf("%.1f", u.Windows)
	needs := fmt.Sprintf("this experiment needs about %s %s", limits, map[bool]string{true: "limit", false: "limits"}[limits == "1.0"])
	var words string
	switch {
	case l.Fits >= u.Runs:
		words = fmt.Sprintf("%sby that reading every run fits %s %s", read, m.sep, needs)
	case l.Fits == 0:
		words = fmt.Sprintf("%sruns pause at %.0f%% %s by that reading no run fits %s %s: it pauses and goes on after the reset (add --wait)", read, 100*u.Limit, m.sep, m.sep, needs)
	default:
		words = fmt.Sprintf("%sruns pause at %.0f%% %s by that reading about %d more %s fit %s %s: it pauses and goes on after the reset (add --wait)", read, 100*u.Limit, m.sep,
			l.Fits, plural(l.Fits, "run", "runs"), m.sep, needs)
	}
	p.Lines = append(p.Lines, st.Paint(term.Muted, words))
	return p
}

// limitBar is a label, a bar of the share used with a mark where runs pause, and a value, inner cells wide. term's bar
// has no mark: the bar's cells are drawn by the shapes, the cell at the pause share becomes "|" (also in ASCII) and the
// parts are painted again, the fill in role and the track muted, as sideLines does for a report's bars.
func limitBar(sh term.Shapes, label, value string, used, pause float64, role term.Role, inner int) string {
	const labelWidth = 16
	valueWidth := max(9, term.Width(value))
	n := max(inner-labelWidth-valueWidth-2, 4)
	cells := []rune(term.Plain(sh.Bar(term.Bar{Fraction: used}, n)))
	if len(cells) == 0 {
		return ""
	}
	mark := min(max(int(math.Round(pause*float64(len(cells)-1))), 0), len(cells)-1)
	var b strings.Builder
	b.WriteString(term.Pad(label, labelWidth) + " ")
	for i := 0; i < len(cells); {
		kind := func(j int) term.Role {
			switch {
			case j == mark:
				return term.Default
			case cells[j] == '░' || cells[j] == '.':
				return term.Muted
			}
			return role
		}
		j, text := i, ""
		for ; j < len(cells) && kind(j) == kind(i); j++ {
			text += string(cells[j])
		}
		switch kind(i) {
		case term.Default:
			b.WriteString("|")
		default:
			b.WriteString(sh.Style.Paint(kind(i), text))
		}
		i = j
	}
	return b.String() + " " + term.PadLeft(value, valueWidth)
}

// ageWords is how long ago a reading was taken: "under a minute", "12 min", "2h", "2h 05m".
func ageWords(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case int(d.Minutes())%60 == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}
