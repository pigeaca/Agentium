package report

import (
	"fmt"
	"strings"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/stats"
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

// modelABLine introduces a model-ab report, quoting the names with quote (a backtick for Markdown).
func modelABLine(r Report, d experiment.Design, quote string) string {
	a, b := d.Arms[0], d.Arms[1]
	goal := map[string]string{experiment.GoalCheaper: "cheaper, without losing success", experiment.GoalBetter: "more successful"}[d.Goal]
	return fmt.Sprintf("Model A/B on context %s%s%s: A = %s%s%s, B = %s%s%s. Goal: %s.", quote, r.Arms[0].Context, quote,
		quote, experiment.Profile(a.Model, a.Effort), quote, quote, experiment.Profile(b.Model, b.Effort), quote, goal)
}

// label names an arm for a table heading: "A", or in a model-ab experiment "A (claude-opus-5-5:high)", its profile.
func (a Arm) label() string {
	if a.Profile == "" {
		return a.Name
	}
	return a.Name + " (" + a.Profile + ")"
}

// tag is the arm's name in a sentence's parentheses: "A", or "A: claude-opus-5-5:high" in a model-ab experiment.
func (a Arm) tag() string {
	if a.Profile == "" {
		return a.Name
	}
	return a.Name + ": " + a.Profile
}

// armProfile is a locked arm's profile in a model-ab experiment, and empty in a context experiment (whose reports are
// unchanged). It reads the arm's requested model, not the one the calibration saw.
func armProfile(d experiment.Design, a experiment.LockedArm) string {
	if !d.PerArmProfiles() {
		return ""
	}
	return experiment.Profile(a.Arm.Model, a.Arm.Effort)
}

// headlineParts is the headline of a primary or guard metric. In a model-ab experiment the subject names the arms by
// profile ("Cost of B (claude-sonnet-5-5) vs A (claude-opus-5-5) -48%"); a context experiment's reads as ever.
func (r Report) headlineParts(res experiment.MetricResult) (bold, mid, verdict string) {
	d := r.Lock.Design
	bold, mid, verdict = headlineParts(res, d)
	if !d.PerArmProfiles() || len(r.Arms) != 2 || res.Tasks < 2 {
		return bold, mid, verdict
	}
	a, b := r.Arms[0], r.Arms[1]
	if res.Ratio {
		return strings.Replace(bold, title(res.Metric), fmt.Sprintf("%s of %s vs %s", title(res.Metric), b.label(), a.label()), 1), mid, verdict
	}
	return fmt.Sprintf("%s: %s %s → %s %s", title(res.Metric), a.label(), pctOf(res.A), b.label(), pctOf(res.B)), mid, verdict
}

// headline is headlineParts as a Markdown sentence.
func (r Report) headline(res experiment.MetricResult) string {
	bold, mid, verdict := r.headlineParts(res)
	return "**" + bold + "**" + mid + verdict + "."
}

// summarize says a model-ab experiment's verdicts in one sentence, for example "B (claude-sonnet-5-5) costs 48% less;
// success: exploratory." A ratio metric with a verdict states its direction and size; any other result gives its
// verdict word.
func summarize(r Report) string {
	if !r.Lock.Design.PerArmProfiles() || len(r.Arms) != 2 {
		return ""
	}
	var phrases, verdicts []string
	for _, res := range r.Analysis.Results {
		if res.Role == experiment.RoleSecondary {
			continue
		}
		if part, phrase := summaryPart(res); phrase {
			phrases = append(phrases, part)
		} else {
			verdicts = append(verdicts, part)
		}
	}
	subject := "B (" + r.Arms[1].Profile + ")"
	switch {
	case len(phrases) > 0:
		return strings.Join(append([]string{subject + " " + strings.Join(phrases, " and ")}, verdicts...), "; ") + "."
	case len(verdicts) > 0:
		return subject + ": " + strings.Join(verdicts, "; ") + "."
	}
	return ""
}

// summaryPart is one metric's part of the summary; phrase is true when it is a verb phrase ("costs 48% less") that
// follows a subject, and false for a labelled verdict ("success: exploratory").
func summaryPart(res experiment.MetricResult) (text string, phrase bool) {
	name := strings.ToLower(title(res.Metric))
	if res.Tasks < 2 {
		return name + ": no result", false
	}
	verb := map[string]string{experiment.MetricCost: "costs", experiment.MetricTime: "takes", experiment.MetricOutput: "writes"}[res.Metric]
	switch res.Verdict {
	case stats.Improved, stats.ImprovedSmall, stats.Regressed:
		if !res.Ratio {
			break
		}
		i, _ := verdictInterval(res)
		size := fmt.Sprintf("%.0f%% less", 100*(1-i.Estimate))
		if i.Estimate >= 1 {
			size = fmt.Sprintf("%.0f%% more", 100*(i.Estimate-1))
			if i.Estimate >= 2 {
				size = fmt.Sprintf("%.1f× more", i.Estimate)
			}
		}
		return fmt.Sprintf("%s %s", verb, size) + map[string]string{experiment.MetricTime: " time", experiment.MetricOutput: " output"}[res.Metric], true
	case stats.Equivalent:
		if res.Ratio {
			return verb + " about the same", true
		}
	}
	return name + ": " + res.Verdict, false
}

// noiseScope says on which models the noise was measured: in a model-ab experiment, on both profiles together.
func (r Report) noiseScope() string {
	if !r.Lock.Design.PerArmProfiles() || len(r.Arms) != 2 {
		return ""
	}
	return fmt.Sprintf(" Measured on %s and %s together, so it pools two models' noise; each model's own may differ.", r.Arms[0].label(), r.Arms[1].label())
}

// calibrationNote says what part of the spend went to the calibrations the experiment made; empty when none did.
func (r Report) calibrationNote() string {
	if r.CalibrationUSD == 0 {
		return ""
	}
	return fmt.Sprintf(" (calibration $%.2f of it)", r.CalibrationUSD)
}
