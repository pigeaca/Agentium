package experiment

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// pairLock is a locked experiment of tasks t1 (whose reference changes code) and docs (whose reference is a document),
// repeats runs per arm, with the pair judge.
func pairLock(t *testing.T, repeats int) Lock {
	t.Helper()
	d := validDesign()
	d.Tasks, d.Repeats, d.Seed = []string{"docs", "t1"}, repeats, 7
	d.JudgePairs = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 1}
	withSolution := func(reference ...string) task.Spec {
		s := taskSpec("b", "true")
		s.Solution, s.Reference = "s", reference
		return s
	}
	return Lock{Design: d, Schedule: Schedule(d), Tasks: []LockedTask{
		NewLockedTask("t1", "Do it.", withSolution("value.txt", "tests/value_test.sh")),
		NewLockedTask("docs", "Write it.", withSolution("README.md")),
	}}
}

// storedRun is a stored run of slot with outcome, passed or not, and an optional comparison.
func storedRun(t *testing.T, id string, slot Slot, outcome string, passed bool, pj *run.PairJudgement) store.Run {
	t.Helper()
	rec := run.Record{ID: id, Task: slot.Task, Arm: slot.Arm, Outcome: outcome, Passed: &passed, PairJudge: pj}
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return store.Run{ID: id, TaskName: slot.Task, Arm: slot.Arm, Outcome: outcome, Passed: &passed, Record: encoded, Slot: slot.Position}
}

// slotOf finds the slot of task, arm and repeat.
func slotOf(t *testing.T, l Lock, task, arm string, repeat int) Slot {
	t.Helper()
	for _, s := range l.Schedule {
		if s.Task == task && s.Arm == arm && s.Repeat == repeat {
			return s
		}
	}
	t.Fatalf("no slot for %s, arm %s, repeat %d", task, arm, repeat)
	return Slot{}
}

// Pairs are the schedule's: a task's run in each arm with the same repeat index, each arm's settled run, whatever the
// order the arms ran in or the attempts before.
func TestPairsOfPairsTheSchedulesSlots(t *testing.T) {
	l := pairLock(t, 2)
	at := func(task, arm string, repeat int) Slot { return slotOf(t, l, task, arm, repeat) }
	done := &run.PairJudgement{RunA: "a1", Verdict: judge.PairVerdict{Prefer: judge.PreferB}}
	stopped := &run.PairJudgement{RunA: "a2", Verdict: judge.PairVerdict{Stopped: judge.StoppedLimit}}
	runs := []store.Run{
		storedRun(t, "b1-infra", at("t1", "B", 1), claude.OutcomeInfra, false, nil), // retried: not the slot's run
		storedRun(t, "b1", at("t1", "B", 1), claude.OutcomeOK, true, done),
		storedRun(t, "a1", at("t1", "A", 1), claude.OutcomeOK, true, nil),
		storedRun(t, "a2", at("t1", "A", 2), claude.OutcomeOK, true, nil),
		storedRun(t, "b2", at("t1", "B", 2), claude.OutcomeTimeout, true, stopped),
		storedRun(t, "da1", at("docs", "A", 1), claude.OutcomeOK, true, nil),
		storedRun(t, "db1", at("docs", "B", 1), claude.OutcomeOK, true, nil),
		storedRun(t, "da2", at("docs", "A", 2), claude.OutcomeOK, false, nil),
	}
	pairs, err := PairsOf(l, runs)
	if err != nil || len(pairs) != 4 {
		t.Fatalf("%d pairs, %v", len(pairs), err)
	}
	byKey := map[string]PairRuns{}
	for i, p := range pairs {
		if p.Pair != l.Schedule[2*i].Pair {
			t.Errorf("pair %d out of the schedule's order", p.Pair)
		}
		byKey[p.Task+string(rune('0'+p.Repeat))] = p
	}
	for key, want := range map[string][2]string{"t11": {"a1", "b1"}, "t12": {"a2", "b2"}, "docs1": {"da1", "db1"}, "docs2": {"da2", ""}} {
		p := byKey[key]
		got := [2]string{}
		if p.A != nil {
			got[0] = p.A.ID
		}
		if p.B != nil {
			got[1] = p.B.ID
		}
		if got != want {
			t.Errorf("%s pairs %v, want %v", key, got, want)
		}
	}
	for key, want := range map[string][2]bool{ // comparable, needs comparing
		"t11":   {true, false},  // compared: final
		"t12":   {true, true},   // stopped at a limit: compared again
		"docs1": {false, false}, // no reference in code
		"docs2": {false, false}, // half a pair
	} {
		p := byKey[key]
		if got := [2]bool{p.Comparable(l), p.NeedsComparing(l)}; got != want {
			t.Errorf("%s: comparable, needs comparing = %v, want %v", key, got, want)
		}
	}
	if n, err := uncompared(l, runs); n != 1 || err != nil {
		t.Errorf("uncompared = %d, %v", n, err)
	}
	off := l
	off.Design.JudgePairs = nil
	if n, _ := uncompared(off, runs); n != 0 || byKey["t12"].NeedsComparing(off) {
		t.Errorf("without the pair judge, %d pairs wait", n)
	}
}

// Only pairs whose two runs both count as passing are compared: a failing run, a run that changed the test runner's
// configuration, an unfair or unsettled one leave the pair alone.
func TestComparableNeedsTwoPassingRuns(t *testing.T) {
	l := pairLock(t, 1)
	yes, no := true, false
	pass := run.Record{Outcome: claude.OutcomeOK, Passed: &yes}
	for name, c := range map[string]struct {
		a, b run.Record
		want bool
	}{
		"both pass":      {pass, pass, true},
		"capped passes":  {run.Record{Outcome: claude.OutcomeCapped, Passed: &yes}, pass, true},
		"one fails":      {pass, run.Record{Outcome: claude.OutcomeOK, Passed: &no}, false},
		"not graded":     {run.Record{Outcome: claude.OutcomeOK}, pass, false},
		"unfair":         {pass, run.Record{Outcome: claude.OutcomeUnfair, Passed: &yes}, false},
		"config changed": {run.Record{Outcome: claude.OutcomeOK, Passed: &yes, Behavior: run.Behavior{ConfigChanged: []string{"jest.config.js"}}}, pass, false},
		"both fail":      {run.Record{Outcome: claude.OutcomeOK, Passed: &no}, run.Record{Outcome: claude.OutcomeOK, Passed: &no}, false},
	} {
		p := PairRuns{Task: "t1", Repeat: 1, A: &PairRun{ID: "a", Rec: c.a}, B: &PairRun{ID: "b", Rec: c.b}}
		if got := p.Comparable(l); got != c.want {
			t.Errorf("%s: comparable = %v", name, got)
		}
	}
	if (PairRuns{Task: "t1", A: &PairRun{Rec: pass}}).Comparable(l) {
		t.Error("half a pair is comparable")
	}
}

// With more than one run per arm, the preference counts tasks, not pairs: a task's pairs are one vote. Pair-level counts
// (complete, flips, incomplete, empty) stay per pair. With one run per arm the two agree.
func TestPairPreferenceClustersByTask(t *testing.T) {
	v := func(prefer string, flip bool) judge.PairVerdict { return judge.PairVerdict{Prefer: prefer, Flip: flip} }
	var pairs []PairRuns
	add := func(task string, verdicts ...judge.PairVerdict) {
		for i, verdict := range verdicts {
			pairs = append(pairs, PairRuns{Task: task, Repeat: i + 1, B: &PairRun{Rec: run.Record{PairJudge: &run.PairJudgement{Verdict: verdict}}}})
		}
	}
	add("b-twice", v(judge.PreferB, false), v(judge.PreferB, false), v(judge.PreferA, false)) // B 2:1, one vote for B
	add("a-once", v(judge.PreferA, false), v(judge.PreferTie, true))                          // a flip prefers neither: A
	add("even", v(judge.PreferA, false), v(judge.PreferB, false))                             // a tie
	add("open", judge.PairVerdict{Stopped: judge.StoppedLimit}, judge.PairVerdict{Empty: true})
	add("empty", judge.PairVerdict{Empty: true})
	pairs = append(pairs, PairRuns{Task: "uncompared", B: &PairRun{}}) // both failed, say: no comparison
	got := PairPreferenceOf(pairs)
	if p := got.Pairs; p.Complete != 7 || p.A != 3 || p.B != 3 || p.Ties != 1 || p.Flips != 1 || p.Incomplete != 1 || p.Empty != 2 {
		t.Errorf("pairs %+v", p)
	}
	if p := got.Tasks; p.Complete != 3 || p.A != 1 || p.B != 1 || p.Ties != 1 || p.Flips != 0 || p.Incomplete != 1 || p.Empty != 1 || p.Enough ||
		p.BShare != 0.5 || p.P != 1 {
		t.Errorf("tasks %+v", p)
	}

	// One run per arm: every task one pair, and the summaries agree on the preference.
	pairs = nil
	for i, prefer := range []string{judge.PreferB, judge.PreferB, judge.PreferB, judge.PreferA, judge.PreferB, judge.PreferB, judge.PreferTie} {
		add(string(rune('a'+i)), v(prefer, false))
	}
	got = PairPreferenceOf(pairs)
	if got.Pairs != got.Tasks || !got.Tasks.Enough || got.Tasks.B != 5 || got.Tasks.A != 1 || math.Abs(got.Tasks.BShare-5.0/6) > 1e-12 {
		t.Errorf("one run per arm: pairs %+v, tasks %+v", got.Pairs, got.Tasks)
	}
}

// The pair judge's settings are validated; it encodes only when on; its cap is its 4 calls', with the overshoot
// allowance of a judge that was never measured, and the pair cap, reserve, default budget and worst case hold it.
func TestDesignPairJudgeCapsAndValidation(t *testing.T) {
	d := validDesign() // 1 task × 3 repeats, $3 a run (and $0.30 overshoot), concurrency 2
	if d.PairJudgeCapUSD() != 0 || d.PairJudgeEstimateUSD() != 0 || !near(d.PairCapUSD(), 6.6) {
		t.Errorf("without the pair judge: cap %v, estimate %v, pair cap %v", d.PairJudgeCapUSD(), d.PairJudgeEstimateUSD(), d.PairCapUSD())
	}
	off, err := json.Marshal(d)
	if err != nil || strings.Contains(string(off), "judge_pairs") {
		t.Errorf("a design without the pair judge encodes %s, %v", off, err)
	}
	d.JudgePairs = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 1}
	if err := d.Validate(); err != nil {
		t.Fatalf("the pair judge's defaults: %v", err)
	}
	if d.PairJudgeCapUSD() != 2 || !near(d.PairCapUSD(), 8.6) || !near(d.RunCapUSD(), 3.3) || !near(Reserve(d), 3*(3.3+2)) {
		t.Errorf("pair judge cap %v (want 4 × $0.50), pair cap %v, run cap %v (a run's own), reserve %v", d.PairJudgeCapUSD(), d.PairCapUSD(), d.RunCapUSD(), Reserve(d))
	}
	if want := 3 * 2 * judge.PairCallEstimateUSD; math.Abs(d.PairJudgeEstimateUSD()-want) > 1e-12 || math.Abs(d.JudgingEstimateUSD()-want) > 1e-12 {
		t.Errorf("estimate %v, want 3 pairs × 2 calls × $%.3f", d.PairJudgeEstimateUSD(), judge.PairCallEstimateUSD)
	}
	est := Estimate{PerRunUSD: 1, Known: true}
	if got := DefaultBudget(d, est); got != math.Ceil(1.25*(6+3*judge.PairEstimateUSD)+3*(3.3+2)) {
		t.Errorf("default budget %v", got)
	}
	if own := Preview(d, []string{"t1"}, est)[2]; !near(own.WorstUSD, 3*8.6) || math.Abs(own.JudgeUSD-3*judge.PairEstimateUSD) > 1e-12 {
		t.Errorf("this experiment's row %+v: worst 3 × $8.60, the pairs' estimate apart", own)
	}
	both := d
	both.Judge = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 1}
	if !near(both.PairCapUSD(), 2*(3.3+1)+2) || math.Abs(both.JudgingEstimateUSD()-(6*judge.EstimateUSD+3*judge.PairEstimateUSD)) > 1e-12 {
		t.Errorf("both judges: pair cap %v, estimate %v", both.PairCapUSD(), both.JudgingEstimateUSD())
	}
	other := d
	other.JudgePairs = &judge.Settings{Model: judge.DefaultModel, Effort: "max", Repeats: 1}
	if got := other.PairJudgeCapUSD(); !near(got, 4*(0.5+0.30)) {
		t.Errorf("a pair judge at another effort: cap %v, want 4 × ($0.50 + its $0.30 allowance)", got)
	}
	d.BudgetUSD = 8.5
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "below one pair of runs at their caps ($8.60)") {
		t.Errorf("a budget below a pair and its comparison: %v", err)
	}
	for name, c := range map[string]struct {
		s    judge.Settings
		want string
	}{
		"repeats":   {judge.Settings{Model: "m", Effort: "high", Repeats: 3}, "the pair judge asks each order once (repeats 1)"},
		"no model":  {judge.Settings{Effort: "high", Repeats: 1}, "the pair judge needs a model and an effort"},
		"no effort": {judge.Settings{Model: "m", Repeats: 1}, "the pair judge needs a model and an effort"},
	} {
		bad := validDesign()
		bad.JudgePairs = &c.s
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	on, err := json.Marshal(d)
	var back Design
	if err != nil || !strings.Contains(string(on), `"judge_pairs":{"model":"claude-opus-5-5","effort":"high","repeats":1}`) ||
		json.Unmarshal(on, &back) != nil || back.JudgePairs == nil || *back.JudgePairs != *d.JudgePairs {
		t.Errorf("a design with the pair judge encodes %s and decodes %+v (%v)", on, back.JudgePairs, err)
	}
}

// Execute holds each pair's comparison from its first run's start, once per pair, and counts what work outside the
// schedule spends and holds: no run starts unless all of it fits.
func TestExecuteHoldsThePairsComparison(t *testing.T) {
	slots := scheduleOf(t, 3, 1) // 6 runs, 3 pairs
	f := &fake{outcome: func(Slot, int) Result { return Result{Outcome: claude.OutcomeOK, CostUSD: 1} }}
	// A pair needs its runs' $1 caps and its $2 comparison: $4 of a $3.90 budget is too much, though both runs fit.
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, PairHoldUSD: 2, BudgetUSD: 3.9, MaxAttempts: 3}, f.run)
	if err != nil || sum.Status != StatusBudget || sum.Settled != 0 || len(f.ran) != 0 {
		t.Fatalf("summary %+v (%d ran), %v", sum, len(f.ran), err)
	}
	// $4 fits one pair at a time, its two runs together: the hold is the pair's, not each run's.
	f = &fake{outcome: func(Slot, int) Result { return Result{Outcome: claude.OutcomeOK} }}
	sum, err = Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, PairHoldUSD: 2, BudgetUSD: 4, MaxAttempts: 3}, f.run)
	if err != nil || sum.Status != StatusDone || sum.Settled != 6 || f.peak != 2 {
		t.Fatalf("summary %+v (peak %d), %v", sum, f.peak, err)
	}
	for _, o := range f.overlaps {
		if slots[o[0]].Pair != slots[o[1]].Pair {
			t.Errorf("slots %d and %d of different pairs ran together, holding two comparisons in a $4 budget", o[0], o[1])
		}
	}

	// Work outside the schedule: what it spent counts like spend, what it holds like a reserve.
	var mu sync.Mutex
	outSpent, outHeld := 0.0, 0.0
	var seen []float64
	f = &fake{outcome: func(s Slot, _ int) Result {
		mu.Lock()
		defer mu.Unlock()
		outSpent, outHeld = outSpent+0.25, outHeld-0.25 // a comparison's call stored while the runs go on, within its hold
		return Result{Outcome: claude.OutcomeOK, CostUSD: 1}
	}}
	outside := func() (float64, float64) { mu.Lock(); defer mu.Unlock(); return outSpent, outHeld }
	outHeld = 1.5
	sum, err = Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 6, MaxAttempts: 3, Outside: outside,
		Progress: func(e Event) {
			if e.Kind == "finish" {
				seen = append(seen, e.SpentUSD)
			}
		}}, f.run)
	// Pair 1: $2 + $1.50 held fits $6; then $2 spent and $1.50 outside, and $2 more fit: pair 2 runs; pair 3 ($4 spent,
	// $1.50 outside and $2) does not.
	if err != nil || sum.Status != StatusBudget || sum.Settled != 4 || sum.SpentUSD != 4 || !strings.Contains(sum.Note, "($5.00 spent") {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if len(seen) != 4 || seen[0] != 1.25 || seen[3] != 5 {
		t.Errorf("progress spent %v: each finish counts the outside spend", seen)
	}
}
