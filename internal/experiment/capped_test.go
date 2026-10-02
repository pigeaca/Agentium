package experiment

import (
	"context"
	"sync"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
)

// The seq-v1 smoke check's preview: claude-sonnet-5's default task run is $1.61 at list prices, and each run stopped at
// $0.50, so "est. cost by then" ($25.79) stood above the worst case ($8). Claude Code stops a run at its cap, so no run
// is expected to cost more: every estimate the preview, the plan and the budget read is limited to the arm's cap.
func TestEstimatesNeverExceedTheRunCap(t *testing.T) {
	t.Parallel()
	d := seqDesign(16)
	d.RunBudgetUSD = 0.5
	profile := EstimateRun("claude-sonnet-5", nil)
	if profile.PerRunUSD < 1.6 {
		t.Fatalf("the default profile is $%.2f a run; the test needs one above the cap", profile.PerRunUSD)
	}
	capped := profile
	capped.CapUSD = d.RunBudgetUSD
	capped.Tasks = map[string]TaskCost{d.Tasks[0]: {PerRunUSD: 2, Runs: 3}, d.Tasks[1]: {PerRunUSD: 0.3, Runs: 1}}
	if v, ok := capped.TaskUSD(d.Tasks[0]); !ok || v != 0.5 {
		t.Errorf("a task whose own runs cost $2 is estimated at $%.2f, want the $0.50 cap", v)
	}
	if v, _ := capped.TaskUSD(d.Tasks[1]); v != 0.3 {
		t.Errorf("a task below the cap keeps its own estimate: $%.2f", v)
	}
	if v, _ := capped.TaskUSD("other"); v != 0.5 || !capped.Capped(capped.PerRunUSD) {
		t.Errorf("a task without runs is estimated at $%.2f, want the cap", v)
	}
	if v, _ := capped.MeanUSD(nil); v != 0.5 {
		t.Errorf("the mean without tasks is $%.2f, want the cap", v)
	}

	p, err := PreviewSequential(d, Same(capped))
	if err != nil {
		t.Fatal(err)
	}
	for k, l := range p.Looks {
		if l.CostUSD > l.WorstUSD {
			t.Errorf("look %d: estimated $%.2f by then, above its worst case $%.2f", k+1, l.CostUSD, l.WorstUSD)
		}
	}
	if want := 2*0.3 + 30*0.5; !near(p.MaxUSD, want) || p.MaxUSD > p.WorstUSD || p.NoneUSD > p.MaxUSD {
		t.Errorf("maximum $%.2f (want $%.2f), worst case $%.2f, expected $%.2f", p.MaxUSD, want, p.WorstUSD, p.NoneUSD)
	}

	// A fixed design and its tiers too, and the default budget from them.
	fixed := validDesign()
	fixed.RunBudgetUSD, fixed.Tasks, fixed.Repeats = 0.5, []string{"a", "b"}, 3
	capped.Tasks = nil
	for _, row := range Preview(fixed, []string{"a", "b", "c"}, capped) {
		if !row.CostKnown || row.CostUSD > row.WorstUSD || !near(row.CostUSD, float64(row.Runs)*0.5) {
			t.Errorf("%s: estimated $%.2f for %d runs, worst case $%.2f", row.Name, row.CostUSD, row.Runs, row.WorstUSD)
		}
	}
	if got, want := DefaultBudget(fixed, capped), 1.25*12*0.5+Reserve(fixed); got < want || got > want+1 {
		t.Errorf("default budget $%.2f, want $%.2f rounded up: from the capped estimate", got, want)
	}
	// Without a cap (an estimate made outside a design) nothing changes.
	if v, _ := profile.TaskUSD("other"); v != profile.PerRunUSD || profile.Capped(profile.PerRunUSD) {
		t.Error("an estimate without a cap is not limited")
	}
}

// A capped run stops after the turn that crosses its cap, so it spends a little more (the smoke check: $0.507 against
// $0.50). The reserve, the budget check and the worst case hold each run's overshoot allowance too, so runs that each
// pass their cap by the full allowance still never take the total past the budget; reserving the bare cap would.
func TestExecuteOvershootStaysWithinTheBudget(t *testing.T) {
	t.Parallel()
	d := validDesign()
	d.RunBudgetUSD, d.Concurrency = 0.5, 2
	over := d.RunBudgetUSD + claude.CapOvershootUSD(d.RunBudgetUSD, d.Model)
	if !near(over, 0.65) || !near(d.RunCapUSD(), over) || !near(d.PairCapUSD(), 2*over) || !near(Reserve(d), 3*over) {
		t.Fatalf("a $0.50 cap holds $%.2f a run (pair $%.2f, reserve $%.2f), want $0.65 with the $0.15 floor", d.RunCapUSD(), d.PairCapUSD(), Reserve(d))
	}
	if !near(claude.CapOvershootUSD(3, d.Model), 0.3) || claude.CapOvershootUSD(0, d.Model) != 0 {
		t.Errorf("the allowance is %.0f%% of a cap, at least $%.2f: $%.2f at $3", 100*claude.CapOvershootShare, claude.CapOvershootMinUSD, claude.CapOvershootUSD(3, d.Model))
	}
	const budget = 3.6
	run := func(runCap float64) (Summary, float64) {
		slots := scheduleOf(t, 16, 1)
		f := &fake{outcome: func(Slot, int) Result { return Result{Outcome: claude.OutcomeCapped, CostUSD: over} }}
		var mu sync.Mutex
		running := map[int]bool{}
		committed := 0.0
		sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: d.Concurrency, RunCapUSD: runCap, BudgetUSD: budget, MaxAttempts: 3,
			Progress: func(e Event) {
				mu.Lock()
				defer mu.Unlock()
				switch e.Kind {
				case "start":
					running[e.Slot.Position] = true
				case "finish":
					delete(running, e.Slot.Position)
				}
				committed = max(committed, e.SpentUSD+float64(len(running))*over)
			}}, f.run)
		if err != nil {
			t.Fatal(err)
		}
		return sum, committed
	}
	sum, committed := run(d.RunCapUSD())
	if sum.Status != StatusBudget || sum.SpentUSD > budget+1e-9 || committed > budget+1e-9 {
		t.Errorf("with the allowance: %+v; spend plus runs in flight reached $%.2f, over the $%.2f budget", sum, committed, budget)
	}
	if old, _ := run(d.RunBudgetUSD); old.SpentUSD <= budget {
		t.Errorf("reserving the bare $0.50 cap spent $%.2f: the test no longer shows the overshoot it guards against", old.SpentUSD)
	}
}
