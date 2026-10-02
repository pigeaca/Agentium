package claude

import (
	"math"
	"testing"
)

// The allowance's floor follows the model's output price, measured on claude-sonnet-5 ($10 per million): a model
// without a list price gets the dearest in the table; the share of the cap takes over for large caps.
func TestCapOvershootScalesWithTheModel(t *testing.T) {
	t.Parallel()
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	for model, floor := range map[string]float64{
		"claude-sonnet-5": 0.15, "claude-sonnet-5-5": 0.15, "claude-opus-5-5": 0.30, "claude-opus-5": 0.375, "claude-opus-4-8": 0.375,
		"claude-haiku-4-5": 0.075, "claude-fable-5-1": 0.75, "no-such-model": 0.75, "sonnet": 0.75,
	} {
		if got := CapOvershootFloorUSD(model); !near(got, floor) {
			t.Errorf("%s: floor $%.3f, want $%.3f", model, got, floor)
		}
		if got := CapOvershootUSD(0.5, model); !near(got, max(0.05, floor)) {
			t.Errorf("%s: allowance at a $0.50 cap $%.3f", model, got)
		}
	}
	if !near(CapOvershootUSD(10, "claude-opus-5-5"), 1) || CapOvershootUSD(0, "claude-opus-5-5") != 0 {
		t.Error("a large cap's allowance is its share; no cap has none")
	}
}

// Only a run stopped at its cost cap gets an Overshoot, which says whether it passed the allowance.
func TestCapOvershootRecord(t *testing.T) {
	t.Parallel()
	o := CapOvershoot(Metrics{Result: "error_max_budget_usd"}, 0.507, 0.5, "claude-sonnet-5-5")
	if o == nil || math.Abs(o.OverUSD-0.007) > 1e-9 || o.AllowanceUSD != 0.15 || o.Exceeded() {
		t.Errorf("the smoke check's run: %+v", o)
	}
	if o := CapOvershoot(Metrics{Result: "error_max_budget_usd"}, 0.9, 0.5, "claude-sonnet-5-5"); o == nil || !o.Exceeded() {
		t.Errorf("$0.40 past a $0.50 cap: %+v", o)
	}
	for _, m := range []Metrics{{Result: "success"}, {Result: "error_max_turns"}} {
		if CapOvershoot(m, 0.9, 0.5, "claude-sonnet-5-5") != nil {
			t.Errorf("%s has no overshoot", m.Result)
		}
	}
}
