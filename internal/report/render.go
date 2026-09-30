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
	enc.SetEscapeHTML(false) // for people reading it: "<agentium data>", not "\u003cagentium data\u003e"
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

	writeNoise(&b, r)

	b.WriteString("\n## Context and cost per arm\n\nMeans over counted runs. The first request is what Claude Code sent first: the context overhead.\n\n| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |\n|---|---|---|---|---|---|---|\n")
	for i, a := range r.Arms {
		first := num(a.FirstRequest, "%.0f")
		if base := r.Arms[0].FirstRequest; i > 0 && a.FirstRequest != nil && base != nil {
			first += fmt.Sprintf(" (%+.0f)", *a.FirstRequest-*base)
		}
		fmt.Fprintf(&b, "| %s | `%s` | %d | %s | %s | %s | %s |\n", a.Name, a.Context, a.Counted, first, num(a.CostUSD, "$%.3f"),
			num(a.ColdCostUSD, "$%.3f"), pctOf(a.CacheReadShare))
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
		{"passed with changed runner configuration (a failure here)", func(x Behavior) string { return fmt.Sprint(x.ConfigPasses) }},
		{"permission denials (total)", func(x Behavior) string { return fmt.Sprint(x.Denials) }},
		{"files changed (mean)", func(x Behavior) string { return num(x.FilesChanged, "%.1f") }},
		{"lines changed (mean)", func(x Behavior) string { return num(x.LinesChanged, "%.1f") }},
		{"shell commands (mean)", func(x Behavior) string { return num(x.BashCommands, "%.1f") }},
	}
	for _, row := range behaviorRows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", row.label, row.value(r.Arms[0].Behavior), row.value(r.Arms[1].Behavior))
	}

	b.WriteString("\n## Per task\n\n● success, ○ failure, × not counted; cost is the mean of counted runs.\n\n| Task | A | B | Cost A → B |\n|---|---|---|---|\n")
	for _, t := range r.Tasks {
		ca, cb := t.Arms[r.Arms[0].Name], t.Arms[r.Arms[1].Name]
		fmt.Fprintf(&b, "| %s | %s %d/%d | %s %d/%d | %s → %s |\n", t.Task, orDash(ca.Marks), ca.Successes, ca.Counted, orDash(cb.Marks),
			cb.Successes, cb.Counted, num(ca.CostUSD, "$%.3f"), num(cb.CostUSD, "$%.3f"))
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
	i, level := verdictInterval(res)
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
		return fmt.Sprintf("**%s %s** (%s: %s to %s): %s.", title(res.Metric), change(i.Estimate), level, change(i.Low), change(i.High), verdict)
	}
	return fmt.Sprintf("**%s %s → %s**, Δ %+.0f pp (%s: %+.0f to %+.0f): %s.", title(res.Metric), pctOf(res.A), pctOf(res.B), 100*i.Estimate, level,
		100*i.Low, 100*i.High, verdict)
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

// num formats a value that may be missing.
func num(p *float64, format string) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf(format, *p)
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

// writeNoise writes the noise section: each component with its range, next to the planner's default, and how it was
// estimated. Without two tasks with cost in both arms there is nothing to estimate.
func writeNoise(b *strings.Builder, r Report) {
	n := r.Analysis.Noise
	if n == nil {
		return
	}
	aa := r.Template == experiment.TemplateAA
	fmt.Fprintf(b, "\n## Noise\n\nWhat the runs show, for planning later experiments: %d task(s), %.1f run(s) per task and arm on average; 95%% ranges.", n.Tasks, n.Repeats)
	if aa {
		b.WriteString(" Both arms use the same context, so this is the noise itself; compare it with the planner's defaults, which size every preview until a calibration replaces them.")
	}
	b.WriteString(" Cost ranges assume normal noise in log cost; with few tasks every range is wide.\n\n")
	b.WriteString("| Component | Estimate | 95% range | Planner's default | How it was estimated |\n|---|---|---|---|---|\n")
	tauDefault := fmt.Sprintf("%.2f–%.2f", experiment.TauLow, experiment.TauHigh)
	row := func(label string, c *experiment.Component, low, high float64, def, missing string) {
		if c == nil {
			fmt.Fprintf(b, "| %s | - | - | %s | %s |\n", label, def, missing)
			return
		}
		fmt.Fprintf(b, "| %s | %.2f | %.2f–%.2f | %s: %s | %s |\n", label, c.Estimate, c.Low, c.High, def, compare(low, high, c), c.Basis)
	}
	row("σ, per-run spread of log cost", n.Sigma, experiment.SigmaLogCost, experiment.SigmaLogCost, fmt.Sprintf("%.2f", experiment.SigmaLogCost),
		"not separable from τ with one run per arm in an A/B: the paired differences' variance is 2σ² + τ²; the τ below takes the default σ")
	if !aa {
		row("τ, spread of the cost effect across tasks", n.Tau, experiment.TauLow, experiment.TauHigh, tauDefault, "")
	}
	if !n.SuccessVaries {
		b.WriteString("\nSuccess did not vary (every counted run passed, or every one failed), so there is no w.\n")
		return
	}
	row("w, per-run variance of success", n.W, experiment.WSuccess, experiment.WSuccess, fmt.Sprintf("%.2f", experiment.WSuccess),
		"not separable from τ with one run per arm in an A/B")
	if !aa && n.TauSuccess != nil {
		row("τ, spread of the success effect across tasks", n.TauSuccess, experiment.TauLow, experiment.TauHigh, tauDefault, "")
	}
}

// compare places a default (a value, or a range from low to high) against a measured range: a default outside it
// sizes plans for noise the runs did not show. A range truncated to zero measured no spread, not a spread of zero, so
// nothing is compared with it; a bootstrap range runs narrow with few tasks, so a default outside it is only a hint.
func compare(low, high float64, c *experiment.Component) string {
	if c.High == 0 {
		return "not compared: no spread detected (range truncated at zero)"
	}
	hint := ""
	if c.Bootstrap {
		hint = " (a bootstrap range, narrow with few tasks: a hint, not a finding)"
	}
	switch {
	case high < c.Low:
		return "below the range, so plans may understate this noise" + hint
	case low > c.High:
		return "above the range, so plans may overstate this noise" + hint
	case low == high:
		return "within the range"
	}
	return "overlaps the range"
}
