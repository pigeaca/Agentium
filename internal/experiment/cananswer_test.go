package experiment

import (
	"fmt"
	"math"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/store"
)

func within(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.5f, want %.5f (±%g)", what, got, want, tol)
	}
}

func tasksNamed(n int) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf("t%02d", i))
	}
	return out
}

// The plan's check: 4,575 tokens fewer, 38 requests a run and $1.23 a run on claude-sonnet-5. By hand: the first
// request writes 4,575 tokens at $4 per million, the 37 later ones read them at $0.20: 4,575 × (4 + 37 × 0.2) / 1e6 =
// $0.052155, which over $1.23 is 4.24%.
func TestExpectedContextChangePlanCheck(t *testing.T) {
	e, ok := ExpectedContextChange("claude-sonnet-5", 20000, 15425, 38, 1.23)
	if !ok {
		t.Fatal("not known")
	}
	within(t, "share", e.Share, 0.052155/1.23, 1e-9)
	within(t, "share, in words", e.Share, 0.0424, 0.0001)

	// A larger context B costs more: the share is negative, by the same arithmetic.
	more, _ := ExpectedContextChange("claude-sonnet-5", 15425, 20000, 38, 1.23)
	within(t, "larger B", more.Share, -0.052155/1.23, 1e-9)
	same, _ := ExpectedContextChange("claude-sonnet-5", 9000, 9000, 38, 1.23)
	if same.Share != 0 {
		t.Errorf("equal contexts: share %v", same.Share)
	}
	// One request has no later read: only the write counts. 1000 × 4 / 1e6 = $0.004 over $0.50 is 0.8%.
	one, _ := ExpectedContextChange("claude-sonnet-5", 6000, 5000, 1, 0.50)
	within(t, "one request", one.Share, 0.008, 1e-9)
	for name, args := range map[string]struct {
		model          string
		a, b           int64
		requests, cost float64
	}{
		"no list price": {"some-model", 20000, 15000, 38, 1.23},
		"no requests":   {"claude-sonnet-5", 20000, 15000, 0, 1.23},
		"no cost":       {"claude-sonnet-5", 20000, 15000, 38, 0},
		"no first":      {"claude-sonnet-5", 0, 15000, 38, 1.23},
	} {
		if _, ok := ExpectedContextChange(args.model, args.a, args.b, args.requests, args.cost); ok {
			t.Errorf("%s: want unknown", name)
		}
	}
}

// By hand: with σ = 0.19, τ = 0.10 and one run, a task's difference has variance 0.01 + 2 × 0.0361 = 0.0822; 8 tasks
// give 2.80158 × √(0.0822 / 8) = 0.28398 in log cost, and 1 − e^−0.28398 = 24.7%: "25% or more".
func TestSmallestCostChangeFixed(t *testing.T) {
	d := Design{Method: MethodV2, Tasks: tasksNamed(8), Repeats: 1}
	got, ok := SmallestCostChange(d)
	if !ok {
		t.Fatal("not known")
	}
	within(t, "smallest", got, 0.24735, 0.0002)
	if got != Detect(8, 1).Cost[0] {
		t.Errorf("a fixed design is Detect's low end: %v vs %v", got, Detect(8, 1).Cost[0])
	}
	if _, ok := SmallestCostChange(Design{Method: MethodV2, Repeats: 1}); ok {
		t.Error("no tasks: want unknown")
	}
}

// A design that checks early spends error at its interim looks, so its last bound is above 1.96 and it needs a larger
// change than a fixed design of the same size. The bound's own value is
// stats.SequentialLooks'; here the figure follows from it: 1 − exp(−(bound + 0.84162) × √(0.0822 / 16)).
func TestSmallestCostChangeEarlyChecksNeedMore(t *testing.T) {
	seq := Design{Method: MethodSeq, Tasks: tasksNamed(16), Repeats: 1}
	fixed := Design{Method: MethodV2, Tasks: tasksNamed(16), Repeats: 1}
	s, ok := SmallestCostChange(seq)
	if !ok {
		t.Fatal("not known")
	}
	f, _ := SmallestCostChange(fixed)
	if s <= f {
		t.Errorf("early checks see %.4f, a fixed design %.4f: the early one needs the larger change", s, f)
	}
	planned, err := mustSeq(t, 16).planned()
	if err != nil {
		t.Fatal(err)
	}
	bound := planned[len(planned)-1].EffBound
	if bound <= 1.96 {
		t.Fatalf("last bound %.4f is not above 1.96", bound)
	}
	within(t, "seq smallest", s, 1-math.Exp(-(bound+0.8416212)*math.Sqrt(0.0822/16)), 1e-4)
	one, _ := SmallestCostChange(Design{Method: MethodSeq, Tasks: tasksNamed(8), Repeats: 1})
	eight, _ := SmallestCostChange(Design{Method: MethodV2, Tasks: tasksNamed(8), Repeats: 1})
	if one < eight {
		t.Errorf("a single look sees %.4f, below the fixed design's %.4f", one, eight)
	}
}

func mustSeq(t *testing.T, tasks int) Sequential {
	t.Helper()
	s, err := NewSequential(tasks, true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// By hand: 4.24% is a log change of −ln(0.957598) = 0.043326; one task of one run per version sees 2.80158 × √0.0822 =
// 0.80323, so (0.80323 / 0.043326)² = 343.7 tasks, 687 runs, 690 to two digits.
func TestRunsToSeeCostChange(t *testing.T) {
	if got := RunsToSeeCostChange(0.0424); got != 690 {
		t.Errorf("4.24%% takes %d runs, want 690", got)
	}
	// 4.24% more is a log change of ln(1.0424) = 0.04153: 2 × (0.80323 / 0.04153)² = 748 runs, 750.
	if got := RunsToSeeCostChange(-0.0424); got != 750 {
		t.Errorf("4.24%% more takes %d runs", got)
	}
	// 25%: (0.80323 / 0.28768)² = 7.80 tasks, 15.6 runs: 16.
	if got := RunsToSeeCostChange(0.25); got != 16 {
		t.Errorf("25%% takes %d runs, want 16", got)
	}
	if RunsToSeeCostChange(0) != 0 {
		t.Error("no change takes no estimate")
	}
	for v, want := range map[float64]int{688.4: 690, 1234: 1200, 99.4: 99, 15.6: 16, 12400: 12000} {
		if got := roundTwoDigits(v); got != want {
			t.Errorf("roundTwoDigits(%v) = %d, want %d", v, got, want)
		}
	}
}

func TestCanAnswerFloors(t *testing.T) {
	// --goal better at the default size, 12 tasks of 3 runs: below the passes floor of 20 tasks of 3 runs.
	d := Design{Method: MethodV2, Goal: GoalBetter, Template: TemplateContextAB, Tasks: tasksNamed(12), Repeats: 3, Model: "claude-sonnet-5"}
	c := Review{Design: d}.CanAnswer()
	if c.Metric != MetricSuccess || c.FloorMet || c.Smallest != nil || c.FloorTasks != MinTasksSuccess || c.FloorRepeats != MinRepeats {
		t.Errorf("below the floor: %+v", c)
	}
	// At the floor the smallest change is Detect's low end: 20 tasks of 3 runs, 2.80158 × √((0.01 + 2 × 0.2 / 3) / 20) = 0.2372.
	d.Tasks = tasksNamed(20)
	c = Review{Design: d}.CanAnswer()
	if !c.FloorMet || c.Smallest == nil {
		t.Fatalf("at the floor: %+v", c)
	}
	within(t, "success", *c.Smallest, 2.801585*math.Sqrt((0.01+2*0.2/3)/20), 1e-5)
	// Cost: 8 tasks of 1 run meets seq-v1's floor; 5 does not.
	d = Design{Method: MethodSeq, Goal: GoalCheaper, Template: TemplateContextAB, Tasks: tasksNamed(5), Repeats: 1}
	if c := (Review{Design: d}).CanAnswer(); c.Metric != MetricCost || c.FloorMet || c.FloorTasks != MinTasksCost || c.FloorRepeats != 1 {
		t.Errorf("cost below its floor: %+v", c)
	}
}

func contextReview(first [2]int64, requests float64, pastRuns int) Review {
	d := Design{Method: MethodV2, Goal: GoalCheaper, Template: TemplateContextAB, Tasks: tasksNamed(8), Repeats: 1, Model: "claude-sonnet-5"}
	est := Same(Estimate{PerRunUSD: 1.23, Known: true, Runs: 6})
	return Review{Design: d, Estimates: est, Size: ContextSize{FirstRequest: first, Requests: requests, PastRuns: pastRuns}}
}

func TestCanAnswerExpectedChange(t *testing.T) {
	// The plan's check: 4.24% expected, 8 tasks of 1 run see 25% or more: not sure, and 690 runs would see it.
	c := contextReview([2]int64{20000, 15425}, 38, 6).CanAnswer()
	if c.Expected == nil || !c.NotSure || c.RunsToSee != 690 {
		t.Fatalf("plan's check: %+v", c)
	}
	within(t, "expected", c.Expected.Share, 0.0424, 0.0001)
	within(t, "smallest", *c.Smallest, 0.2474, 0.0002)

	// A big context change is above what the size sees: no likely result is promised. 150,000 tokens × 11.4e-6 = $1.71
	// is more than the $1.23 run: 139%, which is above 24.7%.
	big := contextReview([2]int64{160000, 10000}, 38, 6).CanAnswer()
	if big.Expected == nil || big.NotSure {
		t.Errorf("a large change: %+v", big)
	}
	for name, r := range map[string]Review{
		"no calibration for B": contextReview([2]int64{20000, 0}, 38, 6),
		"too few past runs":    contextReview([2]int64{20000, 15425}, 0, 2),
	} {
		if c := r.CanAnswer(); c.Expected != nil || c.NotSure || c.RunsToSee != 0 {
			t.Errorf("%s: an expected change: %+v", name, c)
		}
	}
	// A model experiment and an A/A never show one, whatever the review holds.
	for _, template := range []string{TemplateModelAB, TemplateAA} {
		r := contextReview([2]int64{20000, 15425}, 38, 6)
		r.Design.Template = template
		if c := r.CanAnswer(); c.Expected != nil || c.Smallest == nil {
			t.Errorf("%s: %+v", template, c)
		}
	}
	// --goal better never has an expected cost change.
	r := contextReview([2]int64{20000, 15425}, 38, 6)
	r.Design.Goal, r.Design.Tasks = GoalBetter, tasksNamed(20)
	r.Design.Repeats = 3
	if c := r.CanAnswer(); c.Expected != nil {
		t.Errorf("better: %+v", c)
	}
}

func turnsRun(model string, turns int, outcome string) store.Run {
	return store.Run{Kind: "task", Outcome: outcome,
		Record: []byte(fmt.Sprintf(`{"model":%q,"effort_recorded":true,"metrics":{"saw_result":true,"turns":%d}}`, model, turns))}
}

func TestMedianRequests(t *testing.T) {
	runs := []store.Run{
		turnsRun("claude-sonnet-5", 30, agent.OutcomeOK), turnsRun("claude-sonnet-5", 46, agent.OutcomeCapped), turnsRun("claude-sonnet-5", 38, agent.OutcomeOK),
		turnsRun("claude-sonnet-5", 500, agent.OutcomeInfra), // not a fair run
		turnsRun("claude-haiku-4-5", 900, agent.OutcomeOK),   // another model
		{Kind: "calibration", Outcome: agent.OutcomeOK, Record: []byte(`{"model":"claude-sonnet-5","effort_recorded":true,"metrics":{"saw_result":true,"turns":99}}`)},
		{Kind: "task", Outcome: agent.OutcomeOK, Record: []byte(`{"model":"claude-sonnet-5","effort_recorded":true,"metrics":{"saw_result":false,"turns":77}}`)},
	}
	if m, n := MedianRequests(runs, "claude-sonnet-5", ""); m != 38 || n != 3 {
		t.Errorf("median %v of %d, want 38 of 3", m, n)
	}
	runs = append(runs, turnsRun("claude-sonnet-5", 40, agent.OutcomeOK)) // four: the middle two, 38 and 40
	if m, _ := MedianRequests(runs, "claude-sonnet-5", ""); m != 39 {
		t.Errorf("median of four = %v, want 39", m)
	}
	if m, n := MedianRequests(runs[:2], "claude-sonnet-5", ""); m != 0 || n != 2 {
		t.Errorf("fewer than %d runs: %v of %d", MinPastRuns, m, n)
	}
}

// A rise is seen from exp(mde)-1, not from the smallest reduction: at 8 tasks of 1 run a reduction of 24.7% is seen from
// 24.7%, a rise from 32.8%. By hand, 30,210 more tokens × $11.4e-6 over a $1.23 run is a 28% rise: ln 1.28 = 0.247 is
// below the 0.284 an 8-task design sees, though 28% is above 24.7%.
func TestCanAnswerRiseIsComparedOnTheLogScale(t *testing.T) {
	c := contextReview([2]int64{20000, 50210}, 38, 6).CanAnswer()
	if c.Expected == nil {
		t.Fatal("no expected change")
	}
	within(t, "share", c.Expected.Share, -0.28, 1e-4)
	if !c.NotSure {
		t.Errorf("a 28%% rise at 8 tasks of 1 run is not seen: %+v", c)
	}
	if c := contextReview([2]int64{20000, 80000}, 38, 6).CanAnswer(); c.NotSure { // a 56% rise is
		t.Errorf("a 56%% rise: %+v", c)
	}
}

// The cost of a run and the requests of a run must come from earlier runs: an estimate resting on the default profile
// or the cap shows no expected change.
func TestCanAnswerNeedsTheEstimateToRestOnRuns(t *testing.T) {
	r := contextReview([2]int64{20000, 15425}, 38, 6)
	r.Estimates = Same(Estimate{PerRunUSD: 1.23, Known: true, Runs: 1})
	if c := r.CanAnswer(); c.Expected != nil {
		t.Errorf("an estimate from the default profile: %+v", c)
	}
	r.Estimates = Same(Estimate{PerRunUSD: 1.23, Known: true, Runs: 6, CapUSD: 1})
	if c := r.CanAnswer(); c.Expected != nil {
		t.Errorf("an estimate at the cap: %+v", c)
	}
}

func TestMedianRequestsUsesTheEffortOfTheEstimate(t *testing.T) {
	var runs []store.Run
	for range 3 {
		runs = append(runs, store.Run{Kind: "task", Outcome: agent.OutcomeOK,
			Record: []byte(`{"model":"claude-sonnet-5","effort":"high","effort_recorded":true,"metrics":{"saw_result":true,"turns":50}}`)})
	}
	if m, n := MedianRequests(runs, "claude-sonnet-5", ""); m != 0 || n != 0 {
		t.Errorf("runs at another effort: %v of %d", m, n)
	}
	if m, n := MedianRequests(runs, "claude-sonnet-5", "high"); m != 50 || n != 3 {
		t.Errorf("runs at the design's effort: %v of %d", m, n)
	}
}
