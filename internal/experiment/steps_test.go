package experiment

import (
	"context"
	"slices"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/run"
)

// A slot's run reports each step it begins as a "step" event with its slot and attempt, for a live display that asks
// for them, and its grade comes back in the result; otherwise the run gets no step callback at all.
func TestSlotReportsItsSteps(t *testing.T) {
	x, _ := pairExecution(t)
	x.tries, x.storedTries, x.seenSubagents = map[int]int{}, map[int]int{}, map[string]map[string][]string{}
	for _, a := range x.lock.Design.Arms {
		x.lock.Arms = append(x.lock.Arms, LockedArm{Arm: a})
	}
	var events []Event
	x.event = func(e Event) { events = append(events, e) }
	x.r.Observer.Event, x.r.Observer.Steps = func(Event) {}, true
	stepped := false
	passed := true
	x.r.ExecuteRun = func(_ context.Context, e run.Env, _ RunMeta, _ run.Spec) (run.Record, error) {
		if e.Step != nil {
			stepped = true
			for _, s := range []string{run.StepPreparing, run.StepAgent, run.StepGrading} {
				e.Step(s)
			}
		}
		return run.Record{Outcome: claude.OutcomeOK, Passed: &passed}, nil
	}
	slot := x.lock.Schedule[1]
	res, err := x.slot(context.Background(), slot, 2, nil)
	if err != nil || res.Passed == nil || !*res.Passed {
		t.Fatalf("result %+v, %v", res, err)
	}
	var steps []string
	for _, e := range events {
		if e.Kind != "step" || e.Slot != slot || e.Attempt != 2 || e.SpentUSD != 0 {
			t.Errorf("event %+v", e)
		}
		steps = append(steps, e.Step)
	}
	if !slices.Equal(steps, []string{run.StepPreparing, run.StepAgent, run.StepGrading}) {
		t.Errorf("steps %v", steps)
	}

	// An observer that takes events but not steps (the plain lines, JSON) gets none: a stalled terminal there must not
	// hold a run at a step boundary. Nor does an execution without an observer of events.
	for _, o := range []Observer{{Event: func(Event) {}}, {Steps: true}} {
		events, stepped = nil, false
		x.r.Observer = o
		if _, err := x.slot(context.Background(), slot, 3, nil); err != nil || stepped || len(events) != 0 {
			t.Errorf("observer with events %v, steps %v: stepped %v, %d event(s), %v", o.Event != nil, o.Steps, stepped, len(events), err)
		}
	}
}
