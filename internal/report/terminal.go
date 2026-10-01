package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/term"
)

// Terminal writes the report for a person reading it in a terminal: the content and order of Markdown, without
// markup. Headings are bold, tables are aligned columns with numbers to the right, verdicts are colored (green for
// improved and no loss, red for regressed, yellow for exploratory and inconclusive) and explanations and notes are
// dim. With an uncolored style it is plain text. It is rendered from the report's data, not from the Markdown.
func (r Report) Terminal(w io.Writer, st term.Style) error {
	var b strings.Builder
	d, l := r.Lock.Design, r.Lock
	section := func(name, intro string) {
		b.WriteString("\n" + st.Heading(name) + "\n\n")
		if intro != "" {
			b.WriteString(st.Note(intro) + "\n\n")
		}
	}
	table := func(cols ...term.Column) *term.Table { return term.NewTable(st, cols...) }

	b.WriteString(st.Heading("Experiment "+r.Experiment) + "\n\n")
	if r.Template == experiment.TemplateAA {
		fmt.Fprintf(&b, "A/A calibration of context %s (both arms).\n\n", r.Arms[0].Context)
	} else {
		fmt.Fprintf(&b, "Context A/B: A = %s, B = %s. Goal: %s.\n\n", r.Arms[0].Context, r.Arms[1].Context,
			map[string]string{experiment.GoalCheaper: "cheaper, without losing success", experiment.GoalBetter: "more successful"}[d.Goal])
	}
	for _, res := range r.Analysis.Results {
		if res.Role == experiment.RoleSecondary {
			continue
		}
		bold, mid, verdict := headlineParts(res, d)
		if res.Tasks < 2 {
			verdict = st.Warn(verdict)
		} else {
			verdict = verdictStyle(st, res.Verdict, verdict)
		}
		fmt.Fprintf(&b, "- %s%s%s.\n", st.Heading(bold), mid, verdict)
	}
	fmt.Fprintf(&b, "\n%d of %d runs settled (%s); spent $%.2f of $%.2f. %d task(s) × %d run(s) per arm; %s, effort %s, Claude Code %s, sign-in %s. Locked %s (method %s).\n",
		r.Settled, r.Slots, st.Status(r.Status), r.SpentUSD, d.BudgetUSD, len(l.Tasks), d.Repeats, d.Model, orDefault(d.Effort), l.ClaudeCode, l.SignIn,
		l.LockedAt.Format("2006-01-02 15:04 UTC"), l.Method)
	if r.NorthStar != nil {
		fmt.Fprintf(&b, "\n%s.\n", r.NorthStar.Line())
	}

	section("Metrics", "A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.")
	t := table(term.Left("Metric"), term.Left("Role"), term.Right("A"), term.Right("B"), term.Right("B vs A"), term.Right("95% bootstrap"), term.Right("95% t"), term.Left("Verdict"))
	for _, res := range r.Analysis.Results {
		verdict := verdictStyle(st, res.Verdict, res.Verdict)
		if res.Warning != "" {
			verdict += " (" + res.Warning + ")"
		}
		if res.Note != "" {
			verdict += " (" + res.Note + ")"
		}
		if res.TasksToResolve > 0 {
			verdict += fmt.Sprintf("; about %d tasks would resolve it", res.TasksToResolve)
		}
		t.Row(title(res.Metric), res.Role, level(res, res.A), level(res, res.B), effect(res, res.Boot95.Estimate), span(res, res.Boot95), span(res, res.T95), verdict)
	}
	if err := t.Write(&b); err != nil {
		return err
	}
	rate := func(m map[string]float64, arm string) string {
		if v, ok := m[arm]; ok {
			return pct(v)
		}
		return "-"
	}
	fmt.Fprintf(&b, "\nSuccess: pass@1 %s (A) and %s (B); every run of a task passed (pass^k) in %s and %s of tasks.\n",
		rate(r.Analysis.PassAt1, r.Arms[0].Name), rate(r.Analysis.PassAt1, r.Arms[1].Name), rate(r.Analysis.PassAll, r.Arms[0].Name),
		rate(r.Analysis.PassAll, r.Arms[1].Name))

	if n, ok := noiseOf(r); ok {
		section("Noise", n.intro)
		// How each component was estimated is a sentence: it goes on its own line under the row, so the table stays
		// narrow enough for a terminal.
		cols := []term.Column{term.Left(noiseColumns[0]), term.Right(noiseColumns[1]), term.Right(noiseColumns[2]), term.Left(noiseColumns[3])}
		t := table(cols...)
		for _, row := range n.rows {
			t.Row(row[:4]...)
			if len(row) > 4 && row[4] != "" {
				t.Line("  " + st.Note(noiseColumns[4]+": "+row[4]))
			}
		}
		if err := t.Write(&b); err != nil {
			return err
		}
		if n.noSuccessSpread {
			b.WriteString("\nSuccess did not vary (every counted run passed, or every one failed), so there is no w.\n")
		}
	}

	section("Context and cost per arm", "Means over counted runs. The first request is what Claude Code sent first: the context overhead.")
	t = table(term.Left("Arm"), term.Left("Context"), term.Right("Runs counted"), term.Right("First request (tokens)"), term.Right("Cost per run"),
		term.Right("Cold-cache cost"), term.Right("Cache-read share"))
	for i, a := range r.Arms {
		first := num(a.FirstRequest, "%.0f")
		if base := r.Arms[0].FirstRequest; i > 0 && a.FirstRequest != nil && base != nil {
			first += fmt.Sprintf(" (%+.0f)", *a.FirstRequest-*base)
		}
		t.Row(a.Name, a.Context, fmt.Sprint(a.Counted), first, num(a.CostUSD, "$%.3f"), num(a.ColdCostUSD, "$%.3f"), pctOf(a.CacheReadShare))
	}
	if err := t.Write(&b); err != nil {
		return err
	}

	if desc, rows, ok := contextUse(r); ok {
		section("Context use", desc)
		cols := []term.Column{term.Left("")}
		for _, a := range r.Arms {
			cols = append(cols, term.Right(a.Name))
		}
		t = table(cols...)
		for _, row := range rows {
			t.Row(append([]string{row.label + row.name}, row.cells...)...)
		}
		if err := t.Write(&b); err != nil {
			return err
		}
		for _, a := range r.Arms {
			files := "none"
			if len(a.ContextUse.Start) > 0 {
				files = strings.Join(a.ContextUse.Start, ", ")
			}
			fmt.Fprintf(&b, "Loaded at start in %s: %s\n", a.Name, files)
		}
	}

	section("Behavior", "Runs counted in each arm, unless a total.")
	t = table(term.Left(""), term.Right("A"), term.Right("B"))
	for _, row := range behaviorRows {
		t.Row(row.label, row.value(r.Arms[0].Behavior), row.value(r.Arms[1].Behavior))
	}
	if err := t.Write(&b); err != nil {
		return err
	}

	section("Per task", "● success, ○ failure, × not counted; cost is the mean of counted runs.")
	// Marks and counts are separate columns, so the counts line up whatever the number of marks.
	t = table(term.Left("Task"), term.Left("A"), term.Right(""), term.Left("B"), term.Right(""), term.Right("Cost A → B"))
	for _, tr := range r.Tasks {
		ca, cb := tr.Arms[r.Arms[0].Name], tr.Arms[r.Arms[1].Name]
		t.Row(tr.Task, orDash(ca.Marks), fmt.Sprintf("%d/%d", ca.Successes, ca.Counted), orDash(cb.Marks), fmt.Sprintf("%d/%d", cb.Successes, cb.Counted),
			num(ca.CostUSD, "$%.3f")+" → "+num(cb.CostUSD, "$%.3f"))
	}
	if err := t.Write(&b); err != nil {
		return err
	}

	if r.Judge != nil {
		v := r.judgeView()
		section("Judge", v.intro)
		t = table(term.Left(judgeColumns[0]), term.Left(judgeColumns[1]), term.Right(judgeColumns[2]), term.Right(judgeColumns[3]),
			term.Right(judgeColumns[4]), term.Right(judgeColumns[5]))
		for _, row := range v.rows {
			t.Row(row...)
		}
		if err := t.Write(&b); err != nil {
			return err
		}
		b.WriteString("\n")
		for _, line := range v.lines {
			b.WriteString(line + "\n")
		}
		b.WriteString("\n" + v.flaggedTitle + "\n")
		for _, f := range v.flagged {
			fmt.Fprintf(&b, "- %s: %s\n  %s\n", f[0], st.Warn(f[1]), st.Note(sentence(f[2])))
		}
		if v.more > 0 {
			fmt.Fprintf(&b, "- and %d more (agentium experiment report %s --json lists them all)\n", v.more, r.Experiment)
		}
	}

	section("Notes", "")
	for _, n := range r.Notes {
		b.WriteString(st.Note("- "+n) + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// verdictStyle colors text by the verdict it states: green for a gain or no loss, red for a loss, yellow when the
// runs cannot tell.
func verdictStyle(st term.Style, verdict, text string) string {
	switch verdict {
	case stats.Improved, stats.ImprovedSmall, stats.NoLoss, stats.Equivalent:
		return st.Good(text)
	case stats.Regressed:
		return st.Bad(text)
	case stats.Inconclusive, stats.Exploratory:
		return st.Warn(text)
	}
	return text
}
