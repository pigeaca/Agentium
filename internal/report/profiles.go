package report

import (
	"fmt"

	"github.com/pigeaca/agentium/internal/experiment"
)

// modelEffort is the model and effort a report's summary line names: the design's, or each arm's in a model-ab
// experiment (whose design model is arm A's alone, and would label arm B wrongly).
func modelEffort(d experiment.Design) (model, effort string) {
	if !d.PerArmProfiles() || len(d.Arms) != 2 {
		return d.Model, orDefault(d.Effort)
	}
	a, b := d.Arms[0], d.Arms[1]
	return "A " + a.Model + ", B " + b.Model, "A " + orDefault(a.Effort) + ", B " + orDefault(b.Effort)
}

// modelABLine introduces a model-ab report, quoting the names with quote (a backtick for Markdown). The reports'
// tables, headlines and notes still read as for contexts until the reports name arms by profile.
func modelABLine(r Report, d experiment.Design, quote string) string {
	a, b := d.Arms[0], d.Arms[1]
	goal := map[string]string{experiment.GoalCheaper: "cheaper, without losing success", experiment.GoalBetter: "more successful"}[d.Goal]
	return fmt.Sprintf("Model A/B on context %s%s%s: A = %s%s%s, B = %s%s%s. Goal: %s.", quote, r.Arms[0].Context, quote,
		quote, experiment.Profile(a.Model, a.Effort), quote, quote, experiment.Profile(b.Model, b.Effort), quote, goal)
}
