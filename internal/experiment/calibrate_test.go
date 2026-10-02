package experiment

import "testing"

func TestCalibrationEstimateIsBoundedByItsCap(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"claude-sonnet-5", "claude-opus-5-5", "no-such-model"} {
		if got := CalibrationEstimateUSD(model); got <= 0 || got > CalibrationBudgetUSD {
			t.Errorf("estimate on %s = %v, want within (0, %v]", model, got, CalibrationBudgetUSD)
		}
	}
	if CalibrationEstimateUSD("claude-opus-5-5") <= CalibrationEstimateUSD("claude-sonnet-5") {
		t.Error("a larger model's calibration should cost more")
	}
	estimate, capUSD := CalibrationCosts([]CalibrationNeed{{Model: "no-such-model"}, {Model: "no-such-model"}})
	if estimate != 2*fallbackCalibrationUSD || !near(capUSD, 2*(CalibrationBudgetUSD+0.75)) { // each cap with its overshoot, at the dearest output price
		t.Errorf("costs = %v, %v", estimate, capUSD)
	}
}

// A calibration covers a context on a model: the two arms of a model A/B need two, and those of an A/A one.
func TestSameCalibrationIsPerContextAndModel(t *testing.T) {
	t.Parallel()
	ctxAB := Design{Template: TemplateContextAB, Model: "m"}
	a, b := Arm{Name: "A", Context: "base"}, Arm{Name: "B", Context: "lean", Snapshot: "abc"}
	if sameCalibration(ctxAB, a, b) || !sameCalibration(ctxAB, a, Arm{Name: "B", Context: "base"}) {
		t.Error("a context A/B calibrates each context, and an A/A's one context once")
	}
	modelAB := Design{Template: TemplateModelAB, Model: "x"}
	x, y := Arm{Name: "A", Context: "base", Model: "x"}, Arm{Name: "B", Context: "base", Model: "y", Effort: "high"}
	if sameCalibration(modelAB, x, y) || !sameCalibration(modelAB, x, Arm{Name: "B", Context: "base", Model: "x", Effort: "high"}) {
		t.Error("a model A/B calibrates each model once, whatever the effort")
	}
}
