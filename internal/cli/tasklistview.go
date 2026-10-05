package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// What a task tells: the one tag the task list gives each task, from its stored validation, its review and how its
// graded runs went. The values are also the JSON `tells` of task list.
const (
	tellsRetired       = "retired"
	tellsInvalid       = "invalid"
	tellsFlaky         = "flaky"
	tellsUnchecked     = "unchecked"
	tellsNotValidated  = "not-validated"
	tellsUnstated      = "unstated-requirements"
	tellsNotReviewed   = "not-reviewed"
	tellsNotRun        = "not-run"
	tellsNeverPassed   = "never-passed"
	tellsFewRuns       = "few-runs"
	tellsTooEasy       = "too-easy"
	tellsSeparates     = "separates"
	tellsMinRunsToRate = 4 // below this many graded runs a task is not rated, only counted
	maxRunMarks        = 8 // the marks of a task's last runs the list draws
)

// tellOf is the tag for t, whose n graded runs passed k times; hasGaps says whether its hidden tests need something
// nothing states. The first rule that fits wins. Rules 1 to 7 are what blocks the task from an experiment
// (experiment.Ineligible has the same reasons); hasGaps counts only while the task awaits a review, since a reviewed
// task's gaps are accepted ones (task edit --reviewed --accept-gaps stores no mark).
func tellOf(t store.Task, hasGaps bool, n, k int) string {
	switch {
	case t.Retired():
		return tellsRetired
	case t.Grading == task.GradingJudge && t.Validation != nil && task.ValidationOf(t).Judge == nil:
		return tellsNotValidated // a judge-graded task needs the judge's check, as experiment.Ineligible says
	case t.Validation != nil && !slices.Contains([]string{task.StatusValid, task.StatusFlaky, task.StatusUnchecked}, task.StatusOf(t)):
		return tellsInvalid // invalid, or unreadable
	case t.Validation != nil && task.StatusOf(t) == task.StatusFlaky:
		return tellsFlaky
	case t.Validation != nil && task.StatusOf(t) == task.StatusUnchecked:
		return tellsUnchecked
	case t.Validation == nil:
		return tellsNotValidated
	case t.NeedsReview && hasGaps:
		return tellsUnstated
	case t.NeedsReview:
		return tellsNotReviewed
	case n == 0:
		return tellsNotRun
	case k == 0 && n >= 2:
		return tellsNeverPassed
	case n < tellsMinRunsToRate:
		return tellsFewRuns
	case k == n:
		return tellsTooEasy
	}
	return tellsSeparates
}

// tellRank orders the list: the tasks that tell something first, the blocked ones after (the nearest to ready first),
// retired last.
var tellRank = map[string]int{tellsSeparates: 0, tellsTooEasy: 1, tellsNeverPassed: 2, tellsFewRuns: 3, tellsNotRun: 4,
	tellsNotReviewed: 5, tellsUnstated: 6, tellsNotValidated: 7, tellsUnchecked: 8, tellsFlaky: 9, tellsInvalid: 10, tellsRetired: 11}

// blocked reports whether the tag is one of rules 1 to 7: the task cannot be in an experiment.
func blocked(tell string) bool { return tellRank[tell] >= tellRank[tellsNotReviewed] }

// tellWords is the tag in words; n and k are the graded runs and the passed ones, for "passed k of n".
func tellWords(tell string, n, k int) string {
	switch tell {
	case tellsRetired:
		return "retired"
	case tellsInvalid:
		return "invalid"
	case tellsFlaky:
		return "flaky"
	case tellsUnchecked:
		return "no solution to check with"
	case tellsNotValidated:
		return "not validated"
	case tellsUnstated:
		return "unstated requirements"
	case tellsNotReviewed:
		return "not reviewed"
	case tellsNotRun:
		return "not run yet"
	case tellsNeverPassed:
		return "never passed: check the task text"
	case tellsFewRuns:
		return fmt.Sprintf("passed %d of %d", k, n)
	case tellsTooEasy:
		return "always passes: too easy"
	}
	return "separates"
}

// tellRole is the tag's color: good for a task that separates, a caution for one to check or one that blocks, a failure
// for an invalid one, muted for the rest.
func tellRole(tell string) term.Role {
	switch tell {
	case tellsSeparates:
		return term.OutcomeOK
	case tellsInvalid:
		return term.OutcomeFailed
	case tellsNeverPassed, tellsFlaky, tellsUnchecked, tellsNotValidated, tellsUnstated, tellsNotReviewed:
		return term.LevelCaution
	}
	return term.Muted
}

// taskTell is one task with what its runs and its state tell.
type taskTell struct {
	Task  store.Task
	Tell  string
	N, K  int    // the graded runs and the passed ones
	Marks []bool // every graded run, oldest first: passed or not
}

// tellsOf gives each task its tag, in the store's order. Graded runs are the stored task runs with a fair outcome and
// a recorded grade, of every version together. gaps says whether a task's hidden tests need what nothing states
// (called only for a task awaiting a review).
func tellsOf(ctx context.Context, db *store.Store, projectID int64, tasks []store.Task, gaps func(store.Task) bool) ([]taskTell, error) {
	grades, err := db.TaskGrades(ctx, projectID)
	if err != nil {
		return nil, err
	}
	marks := map[int64][]bool{}
	for _, g := range grades {
		if experiment.Fair(g.Outcome) {
			// A pass counts as experiments count it: not when the agent changed the test runner's configuration.
			var changed []string
			if g.ConfigChanged {
				changed = []string{"config"}
			}
			marks[g.TaskID] = append(marks[g.TaskID], experiment.Success(g.Outcome, &g.Passed, changed))
		}
	}
	out := make([]taskTell, len(tasks))
	for i, t := range tasks {
		tt := taskTell{Task: t, Marks: marks[t.ID], N: len(marks[t.ID])}
		for _, p := range tt.Marks {
			if p {
				tt.K++
			}
		}
		hasGaps := gapsTold(t) && gaps(t)
		tt.Tell = tellOf(t, hasGaps, tt.N, tt.K)
		out[i] = tt
	}
	return out, nil
}

// gapsTold reports whether t's tag depends on its unstated requirements: only while it awaits a review, and is not
// retired. The designed list checks the gaps of these tasks alone.
func gapsTold(t store.Task) bool { return t.NeedsReview && !t.Retired() }

// gapsFunc answers whether a task has unstated requirements; a check that fails counts as no gaps.
func gapsFunc(ctx context.Context, fair *task.Fairness) func(store.Task) bool {
	return func(t store.Task) bool {
		gaps, err := task.Gaps(ctx, fair, t)
		return err == nil && len(gaps) > 0
	}
}

// tellCounts counts the tags for the line under the list and under pool status.
type tellCounts struct {
	Separate, TooEasy, ToCheck, FewRuns, NotRun, NotReady, Retired int
}

func countTells(rows []taskTell) tellCounts {
	var c tellCounts
	for _, r := range rows {
		switch r.Tell {
		case tellsSeparates:
			c.Separate++
		case tellsTooEasy:
			c.TooEasy++
		case tellsNeverPassed:
			c.ToCheck++
		case tellsFewRuns:
			c.FewRuns++
		case tellsNotRun:
			c.NotRun++
		case tellsRetired:
			c.Retired++
		default:
			c.NotReady++
		}
	}
	return c
}

// line is the counts in words, only those that are not zero; "" when there are no tasks. A count's own spaces are
// non-breaking, so wrapped keeps each whole; it turns them into spaces.
func (c tellCounts) line(m marks) string {
	var parts []string
	for _, p := range []struct {
		n     int
		words string
	}{{c.Separate, "separate"}, {c.TooEasy, "too easy"}, {c.ToCheck, "to check"}, {c.FewRuns, "with few runs"}, {c.NotRun, "not run yet"},
		{c.NotReady, "not ready"}, {c.Retired, "retired"}} {
		if p.n > 0 {
			parts = append(parts, strings.ReplaceAll(fmt.Sprintf("%d %s", p.n, p.words), " ", nbsp)) // a count wraps whole
		}
	}
	return m.words(strings.Join(parts, " · "))
}

// designedList reports whether task list shows the designed list: a terminal with color and room, not --json, not
// --details; everything else prints the table.
func designedList(env Env, details bool) (term.Capabilities, bool) {
	caps := term.DetectCapabilities(env.Terminal, env.Getenv, envSize(env))
	return caps, !details && !env.JSON && !env.Plain && caps.Designed()
}

// writeTaskList prints the designed list.
func writeTaskList(env Env, rows []taskTell, caps term.Capabilities) error {
	lines := taskListView(rows, caps.Shapes(), caps.Width, env.style().Command("agentium task show NAME"))
	_, err := io.WriteString(env.Stdout, strings.Join(lines, "\n")+"\n")
	return err
}

const (
	minListName   = 12
	maxListModule = 16
	minListModule = 6
	minListTag    = 12
)

// taskListView draws the tasks, one row each: the name, the module (when some task has one), the last runs as marks
// and what the task tells, then the counts and a pointer to task show (show is that command, painted). Names come
// from outside Agentium, so they are sanitized first. No line is wider than width (or term.MaxContentWidth); a cut
// name or tag ends in the ellipsis, the marks are never cut.
func taskListView(rows []taskTell, sh term.Shapes, width int, show string) []string {
	st, m := sh.Style, marksFor(sh)
	w := min(width, term.MaxContentWidth)
	slices.SortStableFunc(rows, func(a, b taskTell) int { return tellRank[a.Tell] - tellRank[b.Tell] })
	hasModule := slices.ContainsFunc(rows, func(r taskTell) bool { return r.Task.Module != "" })

	type cells struct{ name, module, runs, tag string }
	body := make([]cells, len(rows))
	title := fmt.Sprintf("tasks %s %d", m.sep, len(rows))
	nameW, moduleW, runsW, tagW := term.Width(title), term.Width("module"), term.Width("runs"), 0
	for i, r := range rows {
		role := func(p bool) term.Role {
			switch {
			case blocked(r.Tell) || r.Tell == tellsRetired:
				return term.Muted
			case p:
				return term.OutcomeOK
			}
			return term.OutcomeFailed
		}
		shown := r.Marks[max(len(r.Marks)-maxRunMarks, 0):]
		var runs strings.Builder
		if older := len(r.Marks) - len(shown); older > 0 {
			count := fmt.Sprintf("+%d ", older)
			if sh.ASCII {
				count = fmt.Sprintf("%d older ", older) // "+" is a passed run's mark in ASCII
			}
			runs.WriteString(st.Paint(term.Muted, count))
		}
		for _, p := range shown {
			mark := m.fail
			if p {
				mark = m.ok
			}
			runs.WriteString(st.Paint(role(p), mark))
		}
		body[i] = cells{name: term.Sanitize(r.Task.Name), module: term.Sanitize(r.Task.Module), runs: runs.String(), tag: tellWords(r.Tell, r.N, r.K)}
		nameW, moduleW = max(nameW, term.Width(body[i].name)), max(moduleW, term.Width(body[i].module))
		runsW, tagW = max(runsW, term.Width(body[i].runs)), max(tagW, term.Width(body[i].tag))
	}
	// Two spaces lead and separate the columns. Too wide: the names give way first, then the tags, then the modules.
	const lead, gap = 2, 2
	moduleCols := 0
	if hasModule {
		moduleCols = 1
	}
	moduleW = min(moduleW, maxListModule) // a name is as wide as the longest unless the width runs out: its hash tells tasks apart
	over := func() int { return lead + nameW + gap + (moduleW+gap)*moduleCols + runsW + gap + tagW - w }
	nameW = max(nameW-max(over(), 0), min(nameW, minListName))
	tagW = max(tagW-max(over(), 0), min(tagW, minListTag))
	if hasModule {
		moduleW = max(moduleW-max(over(), 0), min(moduleW, minListModule))
	}
	nameW = max(nameW-max(over(), 0), 1) // a very narrow width: the name takes what is left

	pad := strings.Repeat(" ", lead)
	head := pad + term.Pad(title, nameW+gap)
	if hasModule {
		head += st.Paint(term.Muted, term.Pad("module", moduleW+gap))
	}
	head += st.Paint(term.Muted, term.Pad("runs", runsW+gap)+"what it tells you")
	out := []string{head}
	for i, r := range rows {
		c := body[i]
		line := pad + term.Pad(sh.Fit(c.name, nameW), nameW+gap)
		if hasModule {
			line += st.Paint(term.Muted, term.Pad(sh.Fit(c.module, moduleW), moduleW+gap))
		}
		line += term.Pad(c.runs, runsW+gap) + st.Paint(tellRole(r.Tell), sh.Fit(c.tag, tagW))
		out = append(out, line)
	}
	if line := countTells(rows).line(m); line != "" {
		out = append(out, wrapped(st, pad, line, term.Muted, w)...)
	}
	const what = "a task's text, its tests and what blocks it"
	if one := pad + show + st.Paint(term.Muted, ": "+what); term.Width(one) <= w {
		out = append(out, one)
	} else { // a narrow terminal: the command, then its words on the next line
		out = append(out, pad+show+st.Paint(term.Muted, ":"))
		out = append(out, wrapped(st, pad+"  ", what, term.Muted, w)...)
	}
	for i, line := range out {
		out[i] = term.Truncate(strings.TrimRight(line, " "), w, sh.Ellipsis())
	}
	return out
}
