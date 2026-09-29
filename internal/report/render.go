package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/stats"
)

// JSON writes the report as indented JSON.
func (r Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Markdown writes the report for a pull request or a review.
func (r Report) Markdown(w io.Writer) error {
	var b strings.Builder
	d, l := r.Lock.Design, r.Lock
	fmt.Fprintf(&b, "# Experiment %s\n\n", r.Experiment)
	if r.Template == experiment.TemplateAA {
		fmt.Fprintf(&b, "A/A calibration of context `%s` (both arms).\n\n", r.Arms[0].Context)
	} else {
		fmt.Fprintf(&b, "Context A/B: A = `%s`, B = `%s`. Goal: %s.\n\n", r.Arms[0].Context, r.Arms[1].Context,
			map[string]string{experiment.GoalCheaper: "cheaper, without losing success", experiment.GoalBetter: "more successful"}[d.Goal])
	}
	for _, res := range r.Analysis.Results {
		if res.Role != experiment.RoleSecondary {
			fmt.Fprintf(&b, "- %s\n", headline(res, d))
		}
	}
	fmt.Fprintf(&b, "\n%d of %d runs settled (%s); spent $%.2f of $%.2f. %d task(s) × %d run(s) per arm; %s, effort %s, Claude Code %s, sign-in %s. Locked %s (method %s).\n",
		r.Settled, r.Slots, r.Status, r.SpentUSD, d.BudgetUSD, len(l.Tasks), d.Repeats, d.Model, orDefault(d.Effort), l.ClaudeCode, l.SignIn,
		l.LockedAt.Format("2006-01-02 15:04 UTC"), l.Method)

	b.WriteString("\n## Metrics\n\nA and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.\n\n")
	b.WriteString("| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |\n|---|---|---|---|---|---|---|---|\n")
	for _, res := range r.Analysis.Results {
		verdict := res.Verdict
		if res.Warning != "" {
			verdict += " (" + res.Warning + ")"
		}
		if res.Note != "" {
			verdict += " (" + res.Note + ")"
		}
		if res.TasksToResolve > 0 {
			verdict += fmt.Sprintf("; about %d tasks would resolve it", res.TasksToResolve)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", title(res.Metric), res.Role, level(res, res.A), level(res, res.B),
			effect(res, res.Boot95.Estimate), span(res, res.Boot95), span(res, res.T95), verdict)
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

	b.WriteString("\n## Context and cost per arm\n\nMeans over counted runs. The first request is what Claude Code sent first: the context overhead.\n\n| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |\n|---|---|---|---|---|---|---|\n")
	for i, a := range r.Arms {
		first := fmt.Sprintf("%.0f", a.FirstRequest)
		if i > 0 && r.Arms[0].FirstRequest > 0 {
			first += fmt.Sprintf(" (%+.0f)", a.FirstRequest-r.Arms[0].FirstRequest)
		}
		fmt.Fprintf(&b, "| %s | `%s` | %d | %s | $%.3f | $%.3f | %s |\n", a.Name, a.Context, a.Counted, first, a.CostUSD, a.ColdCostUSD, pct(a.CacheReadShare))
	}

	b.WriteString("\n## Behavior\n\nRuns counted in each arm, unless a total.\n\n| | A | B |\n|---|---|---|\n")
	behaviorRows := []struct {
		label string
		value func(Behavior) string
	}{
		{"changed a test file", func(x Behavior) string { return fmt.Sprint(x.TestsChanged) }},
		{"test files removed (total)", func(x Behavior) string { return fmt.Sprint(x.TestsRemoved) }},
		{"ran tests", func(x Behavior) string { return fmt.Sprint(x.RanTests) }},
		{"ran the task's checks", func(x Behavior) string { return fmt.Sprint(x.RanChecks) }},
		{"committed", func(x Behavior) string { return fmt.Sprint(x.Committed) }},
		{"changed the checks", func(x Behavior) string { return fmt.Sprint(x.ChecksChanged) }},
		{"permission denials (total)", func(x Behavior) string { return fmt.Sprint(x.Denials) }},
		{"reads outside the checkout (total)", func(x Behavior) string { return fmt.Sprint(x.OutsideReads) }},
		{"files changed (mean)", func(x Behavior) string { return fmt.Sprintf("%.1f", x.FilesChanged) }},
		{"lines changed (mean)", func(x Behavior) string { return fmt.Sprintf("%.1f", x.LinesChanged) }},
		{"shell commands (mean)", func(x Behavior) string { return fmt.Sprintf("%.1f", x.BashCommands) }},
	}
	for _, row := range behaviorRows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", row.label, row.value(r.Arms[0].Behavior), row.value(r.Arms[1].Behavior))
	}

	b.WriteString("\n## Per task\n\n● success, ○ failure, × not counted; cost is the mean of counted runs.\n\n| Task | A | B | Cost A → B |\n|---|---|---|---|\n")
	for _, t := range r.Tasks {
		ca, cb := t.Arms[r.Arms[0].Name], t.Arms[r.Arms[1].Name]
		fmt.Fprintf(&b, "| %s | %s %d/%d | %s %d/%d | $%.3f → $%.3f |\n", t.Task, orDash(ca.Marks), ca.Successes, ca.Counted, orDash(cb.Marks),
			cb.Successes, cb.Counted, ca.CostUSD, cb.CostUSD)
	}

	b.WriteString("\n## Notes\n\n")
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// headline says a primary or guard metric's result in words.
func headline(res experiment.MetricResult, d experiment.Design) string {
	if res.Tasks < 2 {
		return fmt.Sprintf("**%s**: no result (%s).", title(res.Metric), res.Note)
	}
	i := widest95(res)
	verdict := res.Verdict
	switch verdict {
	case stats.Regressed:
		if res.Role == experiment.RoleGuard {
			verdict += " (a loss the intervals can tell from none"
			if -100*i.Low <= 100*d.SuccessMargin {
				verdict += fmt.Sprintf(", though within the %.0f pp margin", 100*d.SuccessMargin)
			}
			verdict += ")"
		}
	case stats.NoLoss:
		verdict = fmt.Sprintf("no loss beyond %.0f pp", 100*d.SuccessMargin)
	case stats.Equivalent:
		if res.Ratio {
			verdict = fmt.Sprintf("equivalent within ±%.0f%% (a ratio within [%.2f, %.2f])", 100*d.CostMargin, 1-d.CostMargin, 1+d.CostMargin)
		} else {
			verdict = fmt.Sprintf("equivalent within ±%.0f pp", 100*d.SuccessMargin)
		}
	case stats.Inconclusive:
		if res.TasksToResolve > 0 {
			verdict += fmt.Sprintf(" (about %d tasks would resolve it; this experiment has %d)", res.TasksToResolve, res.Tasks)
		}
	case stats.Exploratory:
		verdict = "exploratory: too few tasks or runs for a verdict"
		if res.Warning != "" {
			verdict += "; warning: " + res.Warning
		}
	}
	if res.Ratio {
		return fmt.Sprintf("**%s %s** [%s, %s]: %s.", title(res.Metric), change(i.Estimate), change(i.Low), change(i.High), verdict)
	}
	return fmt.Sprintf("**%s %s → %s**, Δ %+.0f pp [%+.0f, %+.0f]: %s.", title(res.Metric), pctOf(res.A), pctOf(res.B), 100*i.Estimate, 100*i.Low,
		100*i.High, verdict)
}

func change(ratio float64) string { return fmt.Sprintf("%+.0f%%", 100*(ratio-1)) }

func pct(x float64) string {
	if math.IsNaN(x) {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*x)
}

func level(res experiment.MetricResult, p *float64) string {
	if p == nil {
		return "-"
	}
	switch x := *p; res.Metric {
	case experiment.MetricSuccess:
		return pct(x)
	case experiment.MetricCost:
		return fmt.Sprintf("$%.3f", x)
	case experiment.MetricTime:
		return fmt.Sprintf("%.0f s", x)
	default:
		return fmt.Sprintf("%.0f", x)
	}
}

func pctOf(p *float64) string {
	if p == nil {
		return "-"
	}
	return pct(*p)
}

func effect(res experiment.MetricResult, x float64) string {
	if res.Tasks < 2 {
		return "-"
	}
	if res.Ratio {
		return change(x)
	}
	return fmt.Sprintf("%+.0f pp", 100*x)
}

func span(res experiment.MetricResult, i stats.Interval) string {
	if res.Tasks < 2 {
		return "-"
	}
	if res.Ratio {
		return fmt.Sprintf("[%s, %s]", change(i.Low), change(i.High))
	}
	return fmt.Sprintf("[%+.0f, %+.0f] pp", 100*i.Low, 100*i.High)
}

func orDefault(effort string) string {
	if effort == "" {
		return "default"
	}
	return effort
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
