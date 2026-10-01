package experiment

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
)

// meter is a subscription's five-hour window as the fake runs see it: every finished run uses step of it.
type meter struct {
	mu     sync.Mutex
	used   float64
	resets time.Time
	step   float64
}

func (m *meter) run(f *fake) Executor {
	return func(ctx context.Context, slot Slot, attempt int, overlap []int) (Result, error) {
		res, err := f.run(ctx, slot, attempt, overlap)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.used += m.step
		res.Usage = &claude.UsageReading{FiveHour: m.used, FiveHourResets: m.resets}
		return res, err
	}
}

func wholePairs(t *testing.T, slots []Slot, ran []int) {
	t.Helper()
	count := map[int]int{}
	for _, pos := range ran {
		count[slots[pos].Pair]++
	}
	for pair, n := range count {
		if n != 2 {
			t.Errorf("pair %d ran %d of its 2 runs", pair, n)
		}
	}
}

// The gate stops starting pairs before the limit and lets the runs in flight finish: nothing is cancelled, every
// pair that started is whole, and the window never passes the limit.
func TestExecutePausesBeforeTheUsageLimit(t *testing.T) {
	slots := scheduleOf(t, 5, 2) // 20 runs
	resets := time.Now().Add(time.Hour).Truncate(time.Second)
	m := &meter{used: 0.30, resets: resets, step: 0.06}
	f := &fake{}
	gate := &UsageGate{Limit: 0.85, PerRun: 0.06, Latest: claude.UsageReading{FiveHour: 0.30, FiveHourResets: resets}}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3, Usage: gate}, m.run(f))
	if err != nil || sum.Status != StatusUsage || !sum.ResumeAt.Equal(resets) || sum.Pending == 0 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if m.used > 0.85+1e-9 {
		t.Errorf("the window reached %.2f, past the 0.85 limit", m.used)
	}
	if len(f.ran) != 8 { // 0.30 + 8 × 0.06 = 0.78; one more pair would reach 0.90
		t.Errorf("%d runs, want 8", len(f.ran))
	}
	wholePairs(t, slots, f.ran)
	if !strings.Contains(sum.Note, "at 78%") || !strings.Contains(sum.Note, "would pass the 85% limit") {
		t.Errorf("note %q", sum.Note)
	}
	if gate.Latest.FiveHour != 0.30 {
		t.Error("the caller's gate was changed; Execute must keep its own copy")
	}
}

// With Wait, the execution waits for the window to reset and carries on in the new one.
func TestExecuteWaitsForTheUsageWindow(t *testing.T) {
	slots := scheduleOf(t, 5, 2)
	first := time.Now().Add(time.Hour).Truncate(time.Second)
	m := &meter{used: 0.75, resets: first, step: 0.03} // one pair fits this window; the other 18 runs fit the next
	f := &fake{}
	var waits []time.Time
	var events []Event
	var mu sync.Mutex
	gate := &UsageGate{Limit: 0.85, PerRun: 0.03, Latest: claude.UsageReading{FiveHour: 0.75, FiveHourResets: first},
		Wait: func(ctx context.Context, until time.Time) error {
			waits = append(waits, until)
			m.mu.Lock()
			m.used, m.resets = 0, until.Add(5*time.Hour) // the next window
			m.mu.Unlock()
			return nil
		}}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3, Usage: gate,
		Progress: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() }}, m.run(f))
	if err != nil || sum.Status != StatusDone || sum.Settled != 20 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if len(waits) != 1 || !waits[0].Equal(first) {
		t.Errorf("waits = %v, want one until %v", waits, first)
	}
	var waited bool
	for _, e := range events {
		if e.Kind == "wait" && e.Until.Equal(first) && e.Usage > 0.8 {
			waited = true
		}
	}
	if !waited {
		t.Error("no wait event with the reset time and the window's share")
	}
	wholePairs(t, slots, f.ran)
}

// A cancelled wait ends the execution as interrupted; runs without readings (an API key) never pause; a pair that
// cannot fit even an empty window pauses at once, and says so.
func TestExecuteUsageEdges(t *testing.T) {
	slots := scheduleOf(t, 3, 1)
	resets := time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	gate := &UsageGate{Limit: 0.85, PerRun: 0.06, Latest: claude.UsageReading{FiveHour: 0.84, FiveHourResets: resets},
		Wait: func(ctx context.Context, until time.Time) error { cancel(); return ctx.Err() }}
	sum, err := Execute(ctx, Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3, Usage: gate}, (&fake{}).run)
	if err != nil || sum.Status != StatusStopped || sum.Note != "interrupted" || sum.Settled != 0 {
		t.Errorf("a cancelled wait: %+v, %v", sum, err)
	}

	f := &fake{}
	sum, err = Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3,
		Usage: &UsageGate{Limit: 0.85, PerRun: 0.06}}, f.run)
	if err != nil || sum.Status != StatusDone || sum.Settled != 6 {
		t.Errorf("without readings: %+v, %v", sum, err)
	}

	sum, err = Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3,
		Usage: &UsageGate{Limit: 0.10, PerRun: 0.06, Latest: claude.UsageReading{FiveHour: 0.05, FiveHourResets: resets},
			Wait: func(context.Context, time.Time) error { t.Error("waited for a window no pair fits"); return nil }}}, (&fake{}).run)
	if err != nil || sum.Status != StatusUsage || !strings.Contains(sum.Note, "a pair needs about 12%") {
		t.Errorf("a pair above the limit: %+v, %v", sum, err)
	}
}

func TestUsagePerRunAndLatest(t *testing.T) {
	w1, w2 := time.Date(2026, 9, 29, 20, 20, 0, 0, time.UTC), time.Date(2026, 9, 30, 1, 20, 0, 0, time.UTC)
	sample := func(first, last float64, firstResets, lastResets time.Time) UsageSample {
		return UsageSample{First: claude.UsageReading{FiveHour: first, FiveHourResets: firstResets},
			Last: claude.UsageReading{FiveHour: last, FiveHourResets: lastResets}}
	}
	if per, runs := UsagePerRun(nil); per != DefaultUsagePerRun || runs != 0 {
		t.Errorf("no runs: %v, %d", per, runs)
	}
	earlier := []UsageSample{sample(0.07, 0.15, w1, w1), sample(0.10, 0.22, w1, w1), sample(0.20, 0.31, w1, w1), sample(0.25, 0.37, w1, w1)}
	if per, runs := UsagePerRun(earlier); runs != 4 || abs(per-0.075) > 1e-9 { // (0.37 − 0.07) / 4
		t.Errorf("one window: %v, %d", per, runs)
	}
	later := append(earlier, sample(0.05, 0.11, w2, w2), sample(0.08, 0.16, w2, w2), sample(0.12, 0.23, w2, w2),
		sample(0.80, 0.02, w1, w2), // crossed a reset: left out
		UsageSample{})              // no readings: left out
	if per, runs := UsagePerRun(later); runs != 3 || abs(per-0.06) > 1e-9 { // the latest window: (0.23 − 0.05) / 3
		t.Errorf("latest window: %v, %d", per, runs)
	}
	if per, runs := UsagePerRun(later[:6]); runs != 4 || abs(per-0.075) > 1e-9 { // w2 has only 2 runs: w1 still counts
		t.Errorf("too few in the latest window: %v, %d", per, runs)
	}
	latest, ok := LatestUsage(later)
	if !ok || latest.FiveHour != 0.23 || !latest.FiveHourResets.Equal(w2) {
		t.Errorf("latest = %+v, %v", latest, ok)
	}

	// Calibration runs are a few short turns: a window of them alone measures nothing, so an older window of task runs
	// counts, or the default. Their readings are still the latest.
	w3 := w2.Add(5 * time.Hour)
	calibration := func(first, last float64, resets time.Time) UsageSample {
		s := sample(first, last, resets, resets)
		s.Calibration = true
		return s
	}
	calibrations := []UsageSample{calibration(0.10, 0.11, w3), calibration(0.11, 0.12, w3), calibration(0.12, 0.13, w3), calibration(0.13, 0.14, w3)}
	if per, runs := UsagePerRun(calibrations); per != DefaultUsagePerRun || runs != 0 {
		t.Errorf("only calibration runs: %v, %d; want the default", per, runs)
	}
	if per, runs := UsagePerRun(append(later[:7:7], calibrations...)); runs != 3 || abs(per-0.06) > 1e-9 {
		t.Errorf("a later window of calibration runs: %v, %d; want w2's task runs", per, runs)
	}
	if latest, ok := LatestUsage(append(later[:7:7], calibrations...)); !ok || latest.FiveHour != 0.14 || !latest.FiveHourResets.Equal(w3) {
		t.Errorf("latest with calibration runs = %+v, %v", latest, ok)
	}
	// Calibrations in a window with task runs neither count as runs nor widen the rise past the task runs' readings.
	mixed := append(calibrations[:2:2], sample(0.20, 0.26, w3, w3), sample(0.26, 0.32, w3, w3), sample(0.32, 0.38, w3, w3), calibration(0.38, 0.39, w3))
	if per, runs := UsagePerRun(mixed); runs != 3 || abs(per-0.06) > 1e-9 { // (0.38 − 0.20) / 3
		t.Errorf("task and calibration runs in one window: %v, %d", per, runs)
	}
	if _, ok := LatestUsage([]UsageSample{{}}); ok {
		t.Error("no readings, no latest")
	}
	if got := UsageWindows(24, 0.06, 0.85); abs(got-24*0.06/0.85) > 1e-9 {
		t.Errorf("windows = %v", got)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// A pair's second run is never held back by the gate: not when it retries after an infrastructure failure (the retry
// that is due runs before any pause), and not when an earlier execution left the pair half done.
func TestUsageGateKeepsPairsWhole(t *testing.T) {
	slots := scheduleOf(t, 3, 1)
	partnerOf := func(pos int) int {
		for q, s := range slots {
			if q != pos && s.Pair == slots[pos].Pair {
				return q
			}
		}
		return -1
	}
	resets := time.Now().Add(time.Hour)
	second := partnerOf(0)
	m := &meter{used: 0.72, resets: resets, step: 0.04}
	f := &fake{outcome: func(slot Slot, attempt int) Result {
		if slot.Position == second && attempt == 1 {
			return Result{Outcome: claude.OutcomeInfra}
		}
		return Result{Outcome: claude.OutcomeOK, CostUSD: 0.5}
	}}
	gate := &UsageGate{Limit: 0.85, PerRun: 0.04, Latest: claude.UsageReading{FiveHour: 0.72, FiveHourResets: resets}}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3, Usage: gate,
		Backoff: func(int) time.Duration { return 20 * time.Millisecond }}, m.run(f))
	if err != nil || sum.Status != StatusUsage || sum.Settled != 2 {
		t.Fatalf("a retried second run: %+v, %v (ran %v)", sum, err, f.ran)
	}

	// Resumed with the pair's first run stored and the window nearly full: the second still runs, then it pauses.
	f = &fake{}
	gate = &UsageGate{Limit: 0.85, PerRun: 0.04, Latest: claude.UsageReading{FiveHour: 0.84, FiveHourResets: resets}}
	sum, err = Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3, Usage: gate,
		Prior: []Attempt{{Slot: 0, Outcome: claude.OutcomeOK, CostUSD: 0.5}}}, f.run)
	if err != nil || sum.Status != StatusUsage || len(f.ran) != 1 || f.ran[0] != second {
		t.Errorf("a half pair on resume: %+v, %v (ran %v, want [%d])", sum, err, f.ran, second)
	}
}
