package report

import (
	"fmt"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/term"
)

// PairJudge is the pair judge's comparisons of an experiment's passing pairs: present only when the experiment was
// made with --judge-pairs. It is unvalidated and exploratory (.agents/plans/archive/2026-10-01-judge-pairs.md): it decides
// nothing, and every verdict stays the tests'.
type PairJudge struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
	// Tasks is the preference clustered by task (experiment.PairPreferenceOf): one vote per task, the version its
	// comparisons preferred more often. The share, its 95% Wilson interval, the exact binomial p and the floor of
	// judge.MinPreferences (Enough) count these votes. A vote is never a flip, so Tasks.Flips is always 0: the flips
	// are in Pairs and in each task's row.
	Tasks judge.PreferenceSummary `json:"tasks"`
	// Pairs counts the comparisons one by one: complete, incomplete, empty, ties and flips.
	Pairs judge.PreferenceSummary `json:"pairs"`
	// Uncompared counts the pairs still to compare (experiment.PairRuns.NeedsComparing): a resume compares them.
	Uncompared int     `json:"uncompared"`
	CostUSD    float64 `json:"cost_usd"` // in the spend; never in the arms' costs
	// PerTask is each task with a comparison, in the lock's order.
	PerTask []PairTask `json:"per_task"`
}

// PairTask is one task's comparisons as one vote.
type PairTask struct {
	Task string `json:"task"`
	// Prefer is the task's vote: "A", "B" or "tie"; "" when no comparison is complete, and "empty" when every one
	// was not asked (a change had no code).
	Prefer   string `json:"prefer"`
	Compared int    `json:"compared"` // complete comparisons
	Flips    int    `json:"flips"`    // complete comparisons whose two orders disagreed (each a tie)
	// Reason is the judge's one-line reason, scrubbed, from a comparison that preferred the vote's version, in the
	// order with arm A's change first (Change 1 is A's); empty for a tie or without one.
	Reason string `json:"reason,omitempty"`
}

// pairJudgeSummary builds the pair judge's summary; nil without the pair judge.
func pairJudgeSummary(in Input) (*PairJudge, error) {
	settings := in.Lock.Design.JudgePairs
	if settings == nil {
		return nil, nil
	}
	s := settings.WithDefaults()
	var records []experiment.PairRun
	out := &PairJudge{Model: s.Model, Effort: s.Effort, PerTask: []PairTask{}}
	for _, r := range in.Runs {
		records = append(records, experiment.PairRun{ID: r.ID, Slot: r.Slot, Rec: r.Record})
		out.CostUSD += r.Record.Spend().PairJudgeUSD
	}
	pairs, err := experiment.PairRecords(in.Lock, records)
	if err != nil {
		return nil, err
	}
	preference := experiment.PairPreferenceOf(pairs)
	out.Tasks, out.Pairs = preference.Tasks, preference.Pairs
	byTask := map[string][]judge.PairVerdict{}
	for _, p := range pairs {
		if p.NeedsComparing(in.Lock) {
			out.Uncompared++
		}
		if p.B != nil && p.B.Rec.PairJudge != nil {
			byTask[p.Task] = append(byTask[p.Task], p.B.Rec.PairJudge.Verdict)
		}
	}
	for _, t := range in.Lock.Tasks {
		verdicts := byTask[t.Name]
		if len(verdicts) == 0 {
			continue
		}
		vote := experiment.TaskVote(verdicts)
		row := PairTask{Task: t.Name, Prefer: vote.Prefer}
		if vote.Empty {
			row.Prefer = "empty"
		}
		for _, v := range verdicts {
			if !v.Complete() {
				continue
			}
			row.Compared++
			if v.Flip {
				row.Flips++
			}
			if row.Reason == "" && (vote.Prefer == judge.PreferA || vote.Prefer == judge.PreferB) && v.Prefer == vote.Prefer {
				row.Reason = oneLine(term.Sanitize(in.scrub(v.AB.Reason))) // no escape codes, no line breaks
			}
		}
		out.PerTask = append(out.PerTask, row)
	}
	return out, nil
}

// pairView is the pair judge's section, shared by the terminal (--details) and Markdown renderings.
type pairView struct {
	intro string
	lines []string   // the preference, the comparisons, the cost and what is left to compare
	rows  [][]string // task, prefers, compared, why
}

var pairColumns = []string{"Task", "Prefers", "Compared", "Why (Change 1 is A's)"}

func (r Report) pairView() pairView {
	p := r.PairJudge
	v := pairView{intro: fmt.Sprintf("Which passing fix the judge preferred, when both arms passed a task's paired runs: %s at effort %s compared "+
		"the two changes beside the task's instruction and its reference solution, in both orders; when the orders disagree it counts as a tie (a flip). "+
		"It is unvalidated and exploratory, and decides nothing. Each task gets one vote, the arm its comparisons preferred more often, so repeats do "+
		"not count twice; the share is of the tasks with a preference, with a 95%% Wilson interval and an exact binomial test against an even split.",
		p.Model, p.Effort)}
	t := p.Tasks
	n := t.A + t.B
	if !t.Enough {
		v.lines = append(v.lines, fmt.Sprintf("Judge prefers: too few to say (%d %s with a preference, %d needed).", n, plural(n, "task"), judge.MinPreferences))
	} else {
		lead, other, ln, on := r.Arms[1].label(), r.Arms[0].label(), t.B, t.A
		share, low, high := t.BShare, t.Low, t.High
		if t.A > t.B {
			lead, other, ln, on = other, lead, on, ln
			share, low, high = 1-share, 1-t.High, 1-t.Low
		}
		chance := "unlikely to be chance alone"
		if t.P >= 0.05 {
			chance = "could be chance"
		}
		v.lines = append(v.lines, fmt.Sprintf("Judge prefers: %s in %d of %d tasks with a preference (%s; 95%%: %.0f–%.0f%%; p = %.2f), %s in %d: %s.",
			lead, ln, n, pct(share), 100*low, 100*high, t.P, other, on, chance))
	}
	c := p.Pairs
	v.lines = append(v.lines, fmt.Sprintf("Comparisons: %d complete (%d %s, %d %s), %d incomplete, %d not asked (a change had no code); tasks: %d %s.",
		c.Complete, c.Ties, plural(c.Ties, "tie"), c.Flips, plural(c.Flips, "flip"), c.Incomplete, c.Empty, t.Ties, plural(t.Ties, "tie")))
	v.lines = append(v.lines, fmt.Sprintf("Pair judge cost: $%.2f, in the spend and not in the arms' costs.", p.CostUSD))
	if p.Uncompared > 0 {
		v.lines = append(v.lines, fmt.Sprintf("%d pair(s) still to compare: agentium experiment run %s compares them.", p.Uncompared, r.Experiment))
	}
	for _, row := range p.PerTask {
		prefers := map[string]string{judge.PreferA: r.Arms[0].label(), judge.PreferB: r.Arms[1].label(), judge.PreferTie: "tie", "empty": "not asked", "": "-"}[row.Prefer]
		compared := fmt.Sprint(row.Compared)
		if row.Flips > 0 {
			compared += fmt.Sprintf(" (%d %s)", row.Flips, plural(row.Flips, "flip"))
		}
		v.rows = append(v.rows, []string{row.Task, prefers, compared, orDash(sentence(row.Reason))})
	}
	return v
}

// markdownPairs writes the pair judge's section; nothing without it.
func (r Report) markdownPairs(b *strings.Builder) {
	if r.PairJudge == nil {
		return
	}
	v := r.pairView()
	fmt.Fprintf(b, "\n## Judge pairs\n\n%s\n\n", v.intro)
	for _, line := range v.lines {
		fmt.Fprintf(b, "%s\n", line)
	}
	if len(v.rows) == 0 {
		return
	}
	fmt.Fprintf(b, "\n| %s |\n|---%s|\n", strings.Join(pairColumns, " | "), strings.Repeat("|---", len(pairColumns)-1))
	for _, row := range v.rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = strings.ReplaceAll(c, "|", `\|`) // a reason's bar must not split the table
		}
		fmt.Fprintf(b, "| %s |\n", strings.Join(cells, " | "))
	}
}
