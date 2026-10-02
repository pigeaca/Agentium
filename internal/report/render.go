package report

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
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
	} else if d.PerArmProfiles() {
		fmt.Fprintf(&b, "%s\n\n", modelABLine(r, d, "`"))
	} else {
		fmt.Fprintf(&b, "Context A/B: A = `%s`, B = `%s`. Goal: %s.\n\n", r.Arms[0].Context, r.Arms[1].Context,
			map[string]string{experiment.GoalCheaper: "cheaper, without losing success", experiment.GoalBetter: "more successful"}[d.Goal])
	}
	if r.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", r.Summary)
	}
	for _, res := range r.Analysis.Results {
		if res.Role != experiment.RoleSecondary {
			fmt.Fprintf(&b, "- %s\n", r.headline(res))
		}
	}
	model, effort := modelEffort(d)
	fmt.Fprintf(&b, "\n%d of %d runs settled (%s); spent $%.2f of $%.2f%s. %d task(s) × %d run(s) per arm; %s, effort %s, Claude Code %s, sign-in %s. Locked %s (method %s).\n",
		r.Settled, r.Slots, r.Status, r.SpentUSD, d.BudgetUSD, r.calibrationNote(), len(l.Tasks), d.Repeats, model, effort, l.ClaudeCode, l.SignIn,
		l.LockedAt.Format("2006-01-02 15:04 UTC"), l.Method)
	if r.NorthStar != nil {
		fmt.Fprintf(&b, "\n%s.\n", r.NorthStar.Line())
	}
	if l.LocalBinding {
		if r.NorthStar != nil {
			b.WriteString("\n") // a blank line keeps the two notes apart
		}
		b.WriteString(localBindingNote + "\n")
	}

	b.WriteString("\n## Metrics\n\nA and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.\n\n")
	fmt.Fprintf(&b, "| Metric | Role | %s | %s | B vs A | 95%% bootstrap | 95%% t | Verdict |\n|---|---|---|---|---|---|---|---|\n", r.Arms[0].label(), r.Arms[1].label())
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
	fmt.Fprintf(&b, "\nSuccess: pass@1 %s (%s) and %s (%s); every run of a task passed (pass^k) in %s and %s of tasks.\n",
		rate(r.Analysis.PassAt1, r.Arms[0].Name), r.Arms[0].tag(), rate(r.Analysis.PassAt1, r.Arms[1].Name), r.Arms[1].tag(), rate(r.Analysis.PassAll, r.Arms[0].Name),
		rate(r.Analysis.PassAll, r.Arms[1].Name))

	writeNoise(&b, r)

	b.WriteString("\n## Context and cost per arm\n\nMeans over counted runs. The first request is what Claude Code sent first: the context overhead.\n\n| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |\n|---|---|---|---|---|---|---|\n")
	for i, a := range r.Arms {
		first := num(a.FirstRequest, "%.0f")
		if base := r.Arms[0].FirstRequest; i > 0 && a.FirstRequest != nil && base != nil {
			first += fmt.Sprintf(" (%+.0f)", *a.FirstRequest-*base)
		}
		fmt.Fprintf(&b, "| %s | `%s` | %d | %s | %s | %s | %s |\n", a.label(), a.Context, a.Counted, first, num(a.CostUSD, "$%.3f"),
			num(a.ColdCostUSD, "$%.3f"), pctOf(a.CacheReadShare))
	}

	if desc, rows, ok := contextUse(r); ok {
		fmt.Fprintf(&b, "\n## Context use\n\n%s\n\n|", desc)
		for _, a := range r.Arms {
			fmt.Fprintf(&b, " | %s", a.label())
		}
		b.WriteString(" |\n|---" + strings.Repeat("|---", len(r.Arms)) + "|\n")
		for _, row := range rows {
			label := row.label
			if row.name != "" {
				label += "`" + row.name + "`"
			}
			fmt.Fprintf(&b, "| %s | %s |\n", label, strings.Join(row.cells, " | "))
		}
		b.WriteString("\nLoaded at start: ")
		for i, a := range r.Arms {
			files := "none"
			if len(a.ContextUse.Start) > 0 {
				files = "`" + strings.Join(a.ContextUse.Start, "`, `") + "`"
			}
			sep := map[bool]string{true: "; ", false: ""}[i > 0]
			fmt.Fprintf(&b, "%s%s, %s", sep, a.label(), files)
		}
		b.WriteString(".\n")
	}

	fmt.Fprintf(&b, "\n## Behavior\n\nRuns counted in each arm, unless a total.\n\n| | %s | %s |\n|---|---|---|\n", r.Arms[0].label(), r.Arms[1].label())
	for _, row := range behaviorRows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", row.label, row.value(r.Arms[0].Behavior), row.value(r.Arms[1].Behavior))
	}

	fmt.Fprintf(&b, "\n## Per task\n\n● success, ○ failure, × not counted; cost is the mean of counted runs.\n\n| Task | %s | %s | Cost A → B |\n|---|---|---|---|\n", r.Arms[0].label(), r.Arms[1].label())
	for _, t := range r.Tasks {
		ca, cb := t.Arms[r.Arms[0].Name], t.Arms[r.Arms[1].Name]
		fmt.Fprintf(&b, "| %s | %s %d/%d | %s %d/%d | %s → %s |\n", t.Task, orDash(ca.Marks), ca.Successes, ca.Counted, orDash(cb.Marks),
			cb.Successes, cb.Counted, num(ca.CostUSD, "$%.3f"), num(cb.CostUSD, "$%.3f"))
	}

	if r.Judge != nil {
		v := r.judgeView()
		fmt.Fprintf(&b, "\n## Judge\n\n%s\n\n| %s |\n|---%s|\n", v.intro, strings.Join(judgeColumns, " | "), strings.Repeat("|---", len(judgeColumns)-1))
		for _, row := range v.rows {
			fmt.Fprintf(&b, "| %s |\n", strings.Join(row, " | "))
		}
		b.WriteString("\n")
		for _, line := range v.lines {
			fmt.Fprintf(&b, "%s\n", line)
		}
		fmt.Fprintf(&b, "\n%s\n", v.flaggedTitle)
		if len(v.flagged) > 0 {
			b.WriteString("\n")
		}
		for _, f := range v.flagged {
			fmt.Fprintf(&b, "- %s: %s. %s\n", f[0], f[1], sentence(f[2]))
		}
		if v.more > 0 {
			fmt.Fprintf(&b, "- and %d more (the JSON report lists them all).\n", v.more)
		}
	}

	b.WriteString("\n## Notes\n\n")
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// useRow is a row of the context-use table: a label, the item's name (empty for the first row) and a cell per arm.
type useRow struct {
	label, name string
	cells       []string
}

// contextUse lays out the context-use section: its description and rows. ok is false when no counted run in any arm
// has its context use recorded, and the section is left out.
func contextUse(r Report) (desc string, rows []useRow, ok bool) {
	var missing []string
	for _, a := range r.Arms {
		ok = ok || a.ContextUse.Recorded > 0
		if a.ContextUse.Recorded < a.Counted {
			missing = append(missing, fmt.Sprintf("%d of %s's %d counted runs", a.Counted-a.ContextUse.Recorded, a.Name, a.Counted))
		}
	}
	if !ok {
		return "", nil, false
	}
	desc = "What the counted runs used of their context beyond what loads at start: path-scoped rules and folder instructions " +
		"that loaded for the files the agent worked with; context files and linked documents the agent or its subagents read " +
		"(with the Read tool, or given to cat, sed, grep and the like); the project's skills and commands they invoked; and the " +
		"subagents they started (the project's and Claude Code's by name, any other only counted)."
	if len(missing) > 0 {
		desc += " Not recorded or not recoverable for " + strings.Join(missing, " and ") + "."
	}
	first := useRow{label: "files loaded at start"}
	for _, a := range r.Arms {
		first.cells = append(first.cells, fmt.Sprint(len(a.ContextUse.Start)))
	}
	rows = append(rows, first)
	kinds := []struct {
		label string
		of    func(ArmContextUse) map[string]int
	}{
		{"", func(u ArmContextUse) map[string]int { return u.Files }},
		{"skill ", func(u ArmContextUse) map[string]int { return u.Skills }},
		{"subagent ", func(u ArmContextUse) map[string]int { return u.Subagents }},
	}
	for _, k := range kinds {
		names := map[string]bool{}
		for _, a := range r.Arms {
			for n := range k.of(a.ContextUse) {
				names[n] = true
			}
		}
		for _, n := range slices.Sorted(maps.Keys(names)) {
			row := useRow{label: k.label, name: n}
			for _, a := range r.Arms {
				cell := "-"
				if a.ContextUse.Recorded > 0 {
					cell = fmt.Sprintf("%d of %d", k.of(a.ContextUse)[n], a.ContextUse.Recorded)
				}
				row.cells = append(row.cells, cell)
			}
			rows = append(rows, row)
		}
	}
	if slices.ContainsFunc(r.Arms, func(a Arm) bool { return a.ContextUse.OtherSubagents > 0 }) {
		row := useRow{label: "other subagents"}
		for _, a := range r.Arms {
			cell := "-"
			if a.ContextUse.Recorded > 0 {
				cell = fmt.Sprintf("%d of %d", a.ContextUse.OtherSubagents, a.ContextUse.Recorded)
			}
			row.cells = append(row.cells, cell)
		}
		rows = append(rows, row)
	}
	if len(rows) == 1 {
		desc += " No counted run used such a file, invoked a project skill or started a subagent."
	}
	return desc, rows, true
}

// behaviorRows are the behavior table's rows: a label and how to read it from an arm.
var behaviorRows = []struct {
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

// headline says a primary or guard metric's result in words.
func headline(res experiment.MetricResult, d experiment.Design) string {
	bold, mid, verdict := headlineParts(res, d)
	return "**" + bold + "**" + mid + verdict + "."
}

// headlineParts splits a headline into its bold subject, the text up to the verdict, and the verdict (without the
// final full stop), so the terminal rendering can style the parts.
func headlineParts(res experiment.MetricResult, d experiment.Design) (bold, mid, verdict string) {
	if res.Tasks < 2 {
		return title(res.Metric), ": ", fmt.Sprintf("no result (%s)", res.Note)
	}
	i, level := verdictInterval(res)
	verdict = res.Verdict
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
		return title(res.Metric) + " " + change(i.Estimate), fmt.Sprintf(" (%s: %s to %s): ", level, change(i.Low), change(i.High)), verdict
	}
	return fmt.Sprintf("%s %s → %s", title(res.Metric), pctOf(res.A), pctOf(res.B)),
		fmt.Sprintf(", Δ %+.0f pp (%s: %+.0f to %+.0f): ", 100*i.Estimate, level, 100*i.Low, 100*i.High), verdict
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
	n, ok := noiseOf(r)
	if !ok {
		return
	}
	fmt.Fprintf(b, "\n## Noise\n\n%s\n\n", n.intro)
	b.WriteString("| Component | Estimate | 95% range | Planner's default | How it was estimated |\n|---|---|---|---|---|\n")
	for _, row := range n.rows {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", row[0], row[1], row[2], row[3], row[4])
	}
	if n.noSuccessSpread {
		b.WriteString("\nSuccess did not vary (every counted run passed, or every one failed), so there is no w.\n")
	}
}

// noise is the noise section's content, shared by the Markdown and terminal renderings.
type noise struct {
	intro string
	// rows are the table's cells: component, estimate, 95% range, planner's default, how it was estimated.
	rows            [][]string
	noSuccessSpread bool // success did not vary, so w has no row
}

var noiseColumns = []string{"Component", "Estimate", "95% range", "Planner's default", "How it was estimated"}

// noiseOf collects the noise section; ok is false when there is no estimate.
func noiseOf(r Report) (noise, bool) {
	n := r.Analysis.Noise
	if n == nil {
		return noise{}, false
	}
	aa := r.Template == experiment.TemplateAA
	out := noise{}
	out.intro = fmt.Sprintf("What the runs show, for planning later experiments: %d task(s), %.1f run(s) per task and arm on average; 95%% ranges.", n.Tasks, n.Repeats)
	if aa {
		out.intro += " Both arms use the same context, so this is the noise itself; compare it with the planner's defaults, which size every preview until a calibration replaces them."
	}
	out.intro += " Cost ranges assume normal noise in log cost; with few tasks every range is wide." + r.noiseScope()
	tauDefault := fmt.Sprintf("%.2f–%.2f", experiment.TauLow, experiment.TauHigh)
	row := func(label string, c *experiment.Component, low, high float64, def, missing string) {
		if c == nil {
			out.rows = append(out.rows, []string{label, "-", "-", def, missing})
			return
		}
		out.rows = append(out.rows, []string{label, fmt.Sprintf("%.2f", c.Estimate), fmt.Sprintf("%.2f–%.2f", c.Low, c.High), def + ": " + compare(low, high, c), c.Basis})
	}
	row("σ, per-run spread of log cost", n.Sigma, experiment.SigmaLogCost, experiment.SigmaLogCost, fmt.Sprintf("%.2f", experiment.SigmaLogCost),
		"not separable from τ with one run per arm in an A/B: the paired differences' variance is 2σ² + τ²; the τ below takes the default σ")
	if !aa {
		row("τ, spread of the cost effect across tasks", n.Tau, experiment.TauLow, experiment.TauHigh, tauDefault, "")
	}
	if !n.SuccessVaries {
		out.noSuccessSpread = true
		return out, true
	}
	row("w, per-run variance of success", n.W, experiment.WSuccess, experiment.WSuccess, fmt.Sprintf("%.2f", experiment.WSuccess),
		"not separable from τ with one run per arm in an A/B")
	if !aa && n.TauSuccess != nil {
		row("τ, spread of the success effect across tasks", n.TauSuccess, experiment.TauLow, experiment.TauHigh, tauDefault, "")
	}
	return out, true
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

// sentence ends text with a full stop unless it already ends a sentence.
func sentence(text string) string {
	if text == "" || strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") || strings.HasSuffix(text, "?") {
		return text
	}
	return text + "."
}
