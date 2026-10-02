package experiment

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
)

func judged(s judge.Settings) Design {
	d := validDesign()
	d.Judge = &s
	return d
}

// The judge's settings are validated, and a design without them encodes as designs did before the judge.
func TestDesignJudgeValidationAndEncoding(t *testing.T) {
	if err := judged(judge.Settings{}.WithDefaults()).Validate(); err != nil {
		t.Fatalf("the judge's defaults: %v", err)
	}
	for name, c := range map[string]struct {
		s    judge.Settings
		want string
	}{
		"no repeats":   {judge.Settings{Model: "m", Effort: "high"}, "the judge's repeats must be 1 to 9"},
		"many repeats": {judge.Settings{Model: "m", Effort: "high", Repeats: 10}, "the judge's repeats must be 1 to 9"},
		"no model":     {judge.Settings{Effort: "high", Repeats: 3}, "the judge needs a model and an effort"},
		"no effort":    {judge.Settings{Model: "m", Repeats: 3}, "the judge needs a model and an effort"},
	} {
		if err := judged(c.s).Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	off, err := json.Marshal(validDesign())
	if err != nil || strings.Contains(string(off), "judge") {
		t.Errorf("a design without the judge encodes %s, %v", off, err)
	}
	on, err := json.Marshal(judged(judge.Settings{Model: "claude-opus-5-5", Effort: "high", Repeats: 3}))
	if err != nil || !strings.Contains(string(on), `"judge":{"model":"claude-opus-5-5","effort":"high","repeats":3}`) {
		t.Errorf("a design with the judge encodes %s, %v", on, err)
	}
	var back Design
	if err := json.Unmarshal(on, &back); err != nil || back.Judge == nil || *back.Judge != (judge.Settings{Model: "claude-opus-5-5", Effort: "high", Repeats: 3}) {
		t.Errorf("decoded %+v, %v", back.Judge, err)
	}
}

// Each run's cap gains repeats × 2 call caps (a malformed reply is asked again), and so do the reserve, the default
// budget and the worst case; the estimate is runs × repeats × the pilot's call.
func TestJudgeCapsAndEstimates(t *testing.T) {
	plain := validDesign() // 1 task × 3 repeats × 2 arms = 6 runs, $3 a run, concurrency 2
	if plain.JudgeCapUSD() != 0 || plain.RunCapUSD() != 3 || plain.JudgeEstimateUSD() != 0 || Reserve(plain) != 9 {
		t.Errorf("without the judge: cap %v, run cap %v, estimate %v, reserve %v", plain.JudgeCapUSD(), plain.RunCapUSD(), plain.JudgeEstimateUSD(), Reserve(plain))
	}
	d := judged(judge.Settings{Model: "m", Effort: "high", Repeats: 3})
	if d.JudgeCapUSD() != 3 || d.RunCapUSD() != 6 || Reserve(d) != 18 {
		t.Errorf("with 3 repeats: judge cap %v (want 3 × 2 × $0.50), run cap %v, reserve %v (want 3 run caps of $6)", d.JudgeCapUSD(), d.RunCapUSD(), Reserve(d))
	}
	if want := 6 * 3 * judge.EstimateUSD; math.Abs(d.JudgeEstimateUSD()-want) > 1e-9 {
		t.Errorf("estimate %v, want %v", d.JudgeEstimateUSD(), want)
	}
	est := Estimate{PerRunUSD: 1, Known: true}
	// 1.25 × ($6 for the agent + $1.17 for the judge) + $18, rounded up.
	if got := DefaultBudget(d, est); got != math.Ceil(1.25*(6+6*3*judge.EstimateUSD)+18) {
		t.Errorf("default budget %v", got)
	}
	d.RunBudgetUSD, d.BudgetUSD = 3, 11
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "below one pair of runs at their caps ($12.00)") {
		t.Errorf("a budget below a pair with their judgements: %v", err)
	}
	rows := Preview(d, []string{"t1"}, est)
	own := rows[len(rows)-1]
	if own.CostUSD != 6 || math.Abs(own.JudgeUSD-6*3*judge.EstimateUSD) > 1e-9 || own.WorstUSD != 36 {
		t.Errorf("this experiment's row = %+v: the agent's $6 apart from the judge's, worst 6 × $6", own)
	}
	if quick := rows[0]; math.Abs(quick.JudgeUSD-float64(quick.Runs*3)*judge.EstimateUSD) > 1e-9 {
		t.Errorf("the quick tier's judge estimate = %v for %d runs", quick.JudgeUSD, quick.Runs)
	}
}

// A run whose judge hit a usage limit pauses the execution like a usage pause: runs in flight finish, no new one
// starts, and the spend (the judge's included) counts.
func TestExecutePausesWhenTheJudgeHitsALimit(t *testing.T) {
	slots := scheduleOf(t, 4, 1) // 8 runs
	f := &fake{outcome: func(s Slot, _ int) Result {
		r := Result{Outcome: claude.OutcomeOK, CostUSD: 1.2, JudgeUSD: 0.2}
		if s.Position == 2 {
			r.Pause = "the judge hit a usage limit"
		}
		return r
	}}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 7, BudgetUSD: 100, MaxAttempts: 3}, f.run)
	if err != nil || sum.Status != StatusUsage || sum.Note != "the judge hit a usage limit" || sum.Settled != 3 || sum.Pending != 5 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if math.Abs(sum.SpentUSD-3.6) > 1e-9 {
		t.Errorf("spent %v, want 3 runs at $1.20, judgements included", sum.SpentUSD)
	}
}

// The reserve holds each run's judgement too: with judging, fewer runs fit, and spending never passes the budget.
func TestExecuteReservesTheJudgement(t *testing.T) {
	slots := scheduleOf(t, 4, 1)
	f := &fake{outcome: func(Slot, int) Result { return Result{Outcome: claude.OutcomeOK, CostUSD: 3, JudgeUSD: 2} }} // agent $1, judge $2
	d := judged(judge.Settings{Model: "m", Effort: "high", Repeats: 1})
	d.RunBudgetUSD = 1 // a run's cap: $1 + 1 × 2 × $0.50
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: d.RunCapUSD(), BudgetUSD: 12, MaxAttempts: 3}, f.run)
	// Pairs start while spend + both caps fit: pairs of $4 each fit until $12 + $3 does not.
	if err != nil || sum.Status != StatusBudget || sum.Settled != 4 || sum.SpentUSD > 12 || !strings.Contains(sum.Note, "$2.00 per run at most") {
		t.Fatalf("summary %+v, %v", sum, err)
	}
}
