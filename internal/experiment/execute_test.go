package experiment

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/task"
)

func scheduleOf(t *testing.T, tasks, repeats int) []Slot {
	t.Helper()
	d := validDesign()
	d.Tasks = nil
	for i := range tasks {
		d.Tasks = append(d.Tasks, string(rune('a'+i)))
	}
	d.Repeats, d.Seed = repeats, 42
	return Schedule(d)
}

func TestScheduleInterleavesPairsRepeatByRepeat(t *testing.T) {
	d := validDesign()
	d.Tasks, d.Repeats, d.Seed = []string{"a", "b", "c", "d"}, 3, 9
	slots := Schedule(d)
	if len(slots) != 24 || !slices.Equal(Schedule(d), slots) {
		t.Fatalf("Schedule = %d slots, or not reproducible", len(slots))
	}
	firstArms := map[string]int{}
	for i := 0; i < len(slots); i += 2 {
		a, b := slots[i], slots[i+1]
		if a.Pair != b.Pair || a.Task != b.Task || a.Repeat != b.Repeat || a.Arm == b.Arm || a.Position != i {
			t.Fatalf("slots %d and %d are not a pair: %+v %+v", i, i+1, a, b)
		}
		if want := i/8 + 1; a.Repeat != want {
			t.Errorf("slot %d is repeat %d, want %d: every task's repeat r comes before any repeat r+1", i, a.Repeat, want)
		}
		firstArms[a.Arm]++
	}
	if firstArms["A"] == 0 || firstArms["B"] == 0 {
		t.Errorf("arm order within pairs never varies: %v", firstArms)
	}
	var orders [][]string
	for r := range 3 {
		var order []string
		for i := r * 8; i < r*8+8; i += 2 {
			order = append(order, slots[i].Task)
		}
		orders = append(orders, order)
	}
	if slices.Equal(orders[0], orders[1]) && slices.Equal(orders[1], orders[2]) {
		t.Errorf("task order is the same in every repeat: %v", orders)
	}
	d.Seed = 10
	if slices.Equal(Schedule(d), slots) {
		t.Error("another seed gives the same schedule")
	}
}

func TestOverlap(t *testing.T) {
	if got := Overlap(5, 2, 20); !slices.Equal(got, []int{2, 3, 4, 6, 7, 8}) {
		t.Errorf("Overlap(5, 2, 20) = %v", got)
	}
	if got := Overlap(0, 1, 3); !slices.Equal(got, []int{1}) {
		t.Errorf("Overlap(0, 1, 3) = %v", got)
	}
}

// fake records what the scheduler did: which slots ran, at what concurrency, and what overlapped.
type fake struct {
	mu       sync.Mutex
	inFlight map[int]bool
	peak     int
	overlaps [][2]int // positions in flight together
	ran      []int
	outcome  func(slot Slot, attempt int) Result
	given    map[int][]int // the overlap each run was told about
}

func (f *fake) run(ctx context.Context, slot Slot, attempt int, overlap []int) (Result, error) {
	f.mu.Lock()
	if f.inFlight == nil {
		f.inFlight, f.given = map[int]bool{}, map[int][]int{}
	}
	for q := range f.inFlight {
		f.overlaps = append(f.overlaps, [2]int{q, slot.Position})
	}
	f.inFlight[slot.Position] = true
	f.peak = max(f.peak, len(f.inFlight))
	f.ran = append(f.ran, slot.Position)
	f.given[slot.Position] = overlap
	f.mu.Unlock()
	time.Sleep(time.Duration(5+slot.Position%3) * time.Millisecond)
	f.mu.Lock()
	delete(f.inFlight, slot.Position)
	f.mu.Unlock()
	if f.outcome != nil {
		return f.outcome(slot, attempt), nil
	}
	return Result{Outcome: claude.OutcomeOK, CostUSD: 0.5}, nil
}

func TestExecuteRunsEverySlotWithinTheWindow(t *testing.T) {
	slots := scheduleOf(t, 6, 3)
	f := &fake{}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 3, RunCapUSD: 1, BudgetUSD: 1000, MaxAttempts: 3}, f.run)
	if err != nil || sum.Status != StatusDone || sum.Settled != 36 || sum.Pending != 0 || sum.SpentUSD != 18 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if f.peak > 3 || f.peak < 2 {
		t.Errorf("peak concurrency %d, want 2 or 3 of 3", f.peak)
	}
	for _, o := range f.overlaps {
		if d := o[0] - o[1]; d >= Window(3) || d <= -Window(3) {
			t.Errorf("slots %d and %d ran together, more than the window apart", o[0], o[1])
		}
		if !slices.Contains(f.given[o[1]], o[0]) || !slices.Contains(f.given[o[0]], o[1]) {
			t.Errorf("slots %d and %d overlapped, but were not told so", o[0], o[1])
		}
	}
}

func TestExecuteNeverPassesTheBudget(t *testing.T) {
	slots := scheduleOf(t, 5, 2)                                                                             // 20 runs
	f := &fake{outcome: func(Slot, int) Result { return Result{Outcome: claude.OutcomeCapped, CostUSD: 1} }} // every run at its cap
	var mu sync.Mutex
	maxCommitted := 0.0
	running := map[int]bool{}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 7.5, MaxAttempts: 3,
		Progress: func(e Event) {
			mu.Lock()
			defer mu.Unlock()
			switch e.Kind {
			case "start":
				running[e.Slot.Position] = true
			case "finish":
				delete(running, e.Slot.Position)
			}
			maxCommitted = max(maxCommitted, e.SpentUSD+float64(len(running)))
		}}, f.run)
	if err != nil || sum.Status != StatusBudget || sum.SpentUSD > 7.5 || sum.Settled+sum.Pending != 20 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if maxCommitted > 7.5 {
		t.Errorf("spend plus runs in flight reached $%.2f, over the $7.50 budget", maxCommitted)
	}
	if !strings.Contains(sum.Note, "would not fit the $7.50 budget") {
		t.Errorf("note %q", sum.Note)
	}
	// Whole pairs: a pair's second run is never left without room once its first started.
	settled := map[int]int{}
	for _, pos := range f.ran {
		settled[slots[pos].Pair]++
	}
	for pair, n := range settled {
		if n != 2 {
			t.Errorf("pair %d ran %d of its 2 runs", pair, n)
		}
	}
}

func TestExecuteRetriesInfrastructureFailures(t *testing.T) {
	slots := scheduleOf(t, 3, 1) // 6 runs
	var mu sync.Mutex
	attempts := map[int][]int{}
	f := &fake{outcome: func(s Slot, attempt int) Result {
		mu.Lock()
		attempts[s.Position] = append(attempts[s.Position], attempt)
		mu.Unlock()
		switch {
		case s.Position == 1 && attempt < 3: // recovers on the last attempt
			return Result{Outcome: claude.OutcomeInfra, CostUSD: 0.1}
		case s.Position == 4: // never recovers
			return Result{Outcome: claude.OutcomeInfra}
		}
		return Result{Outcome: claude.OutcomeOK, CostUSD: 1}
	}}
	var retries []time.Duration
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 2, BudgetUSD: 100, MaxAttempts: 3,
		Backoff: func(n int) time.Duration { return time.Duration(n) * time.Millisecond },
		Progress: func(e Event) {
			if e.Kind == "retry" {
				retries = append(retries, e.RetryIn)
			}
		}}, f.run)
	if err != nil || sum.Status != StatusDone || sum.Settled != 5 || sum.Failed != 1 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if !slices.Equal(attempts[1], []int{1, 2, 3}) || !slices.Equal(attempts[4], []int{1, 2, 3}) || !slices.Equal(attempts[0], []int{1}) {
		t.Errorf("attempts %v", attempts)
	}
	if len(retries) != 4 || retries[0] != time.Millisecond || retries[1] != 2*time.Millisecond {
		t.Errorf("retries waited %v", retries)
	}
	if want := 5 + 0.2; sum.SpentUSD < want-1e-9 || sum.SpentUSD > want+1e-9 { // failed attempts' spend counts too
		t.Errorf("spent %v, want %v", sum.SpentUSD, want)
	}
}

func TestExecuteStopsOnAnOutage(t *testing.T) {
	slots := scheduleOf(t, 4, 1)
	f := &fake{outcome: func(s Slot, _ int) Result {
		if s.Position == 0 {
			return Result{Outcome: claude.OutcomeOK}
		}
		return Result{Outcome: claude.OutcomeInfra}
	}}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return 0 }}, f.run)
	// Slot 1 uses its three attempts (one slot failing alone does not stop anything); slot 2's failure right after is the
	// fourth in a row, on a second slot.
	if err != nil || sum.Status != StatusStopped || !strings.Contains(sum.Note, "4 runs in a row failed") || !slices.Equal(f.ran, []int{0, 1, 1, 1, 2}) {
		t.Fatalf("summary %+v, %v; ran %v", sum, err, f.ran)
	}
}

func TestExecuteStopsWhenARunSaysSo(t *testing.T) {
	slots := scheduleOf(t, 4, 1)
	f := &fake{outcome: func(s Slot, _ int) Result {
		r := Result{Outcome: claude.OutcomeOK}
		if s.Position == 2 {
			r.Outcome, r.Stop = claude.OutcomeUnfair, "Claude Code changed from 2.1.281 to 2.1.300"
		}
		return r
	}}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3}, f.run)
	if err != nil || sum.Status != StatusStopped || sum.Note != "Claude Code changed from 2.1.281 to 2.1.300" || sum.Settled != 3 || sum.Pending != 5 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
}

func TestExecuteResumesFromStoredRuns(t *testing.T) {
	slots := scheduleOf(t, 2, 1) // 4 runs
	prior := []Attempt{
		{Slot: 0, Outcome: claude.OutcomeOK, CostUSD: 1},
		{Slot: 1, Outcome: claude.OutcomeInfra, CostUSD: 0.2},
		{Slot: 1, Outcome: claude.OutcomeCancelled, CostUSD: 0.3}, // interrupted: spent, but not an attempt
		{Slot: 2, Outcome: claude.OutcomeInfra}, {Slot: 2, Outcome: claude.OutcomeInfra}, {Slot: 2, Outcome: claude.OutcomeInfra},
	}
	var started []Event
	f := &fake{}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3, Prior: prior,
		Progress: func(e Event) {
			if e.Kind == "start" {
				started = append(started, e)
			}
		}}, f.run)
	if err != nil || sum.Status != StatusDone || sum.Settled != 3 || sum.Failed != 1 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if !slices.Equal(f.ran, []int{1, 3}) || started[0].Attempt != 2 || started[1].Attempt != 1 {
		t.Errorf("ran %v, starts %+v: settled and failed slots are skipped, attempts continue", f.ran, started)
	}
	if want := 1.5 + 1.0; sum.SpentUSD != want {
		t.Errorf("spent %v, want %v (stored spend counts)", sum.SpentUSD, want)
	}
	if _, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3,
		Prior: []Attempt{{Slot: 9}}}, f.run); err == nil {
		t.Error("a stored run outside the schedule is an error")
	}
}

func TestExecuteInterrupted(t *testing.T) {
	slots := scheduleOf(t, 4, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	calls := 0
	run := func(ctx context.Context, s Slot, attempt int, _ []int) (Result, error) {
		mu.Lock()
		calls++
		if calls == 2 {
			cancel()
		}
		mu.Unlock()
		<-ctx.Done()
		return Result{Outcome: claude.OutcomeCancelled, CostUSD: 0.4}, ctx.Err()
	}
	sum, err := Execute(ctx, Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3}, run)
	if err != nil || sum.Status != StatusStopped || sum.Note != "interrupted" || sum.Pending != 8 || sum.SpentUSD != 0.8 || calls != 2 {
		t.Fatalf("summary %+v, %v, %d calls", sum, err, calls)
	}
}

func TestExecuteReturnsAgentiumsOwnErrors(t *testing.T) {
	slots := scheduleOf(t, 2, 1)
	calls := 0
	run := func(context.Context, Slot, int, []int) (Result, error) {
		calls++
		return Result{Outcome: claude.OutcomeInfra}, errors.New("disk full")
	}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3}, run)
	if err == nil || !strings.Contains(err.Error(), "disk full") || sum.Status != StatusStopped || calls != 1 {
		t.Fatalf("summary %+v, %v, %d calls: nothing more starts after Agentium's own failure", sum, err, calls)
	}
}

func TestCounting(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		outcome string
		passed  *bool
		config  []string
		fair    bool
		success bool
	}{
		{claude.OutcomeOK, &yes, nil, true, true},
		{claude.OutcomeCapped, &yes, nil, true, true},
		{claude.OutcomeTimeout, &no, nil, true, false},
		{claude.OutcomeOK, nil, nil, true, false},                     // grading did not finish
		{claude.OutcomeOK, &yes, []string{"pytest.ini"}, true, false}, // passed with a changed test runner
		{claude.OutcomeUnfair, &yes, nil, false, false},
		{claude.OutcomeInfra, nil, nil, false, false},
		{claude.OutcomeCancelled, nil, nil, false, false},
	} {
		if Fair(c.outcome) != c.fair || Success(c.outcome, c.passed, c.config) != c.success {
			t.Errorf("%s passed=%v config=%v: fair %v success %v", c.outcome, c.passed, c.config, Fair(c.outcome), Success(c.outcome, c.passed, c.config))
		}
	}
	if !Settles(claude.OutcomeUnfair) || Settles(claude.OutcomeInfra) || Settles(claude.OutcomeCancelled) {
		t.Error("unfair runs settle their slot; infrastructure failures and cancelled runs do not")
	}
}

func TestLockCheckAndTaskDigest(t *testing.T) {
	l := Lock{Method: MethodVersion, ClaudeCode: "2.1.281", SignIn: "login"}
	if err := l.Check("2.1.281", "login"); err != nil {
		t.Errorf("same machine: %v", err)
	}
	for _, c := range []struct{ version, signIn, want string }{
		{"2.1.300", "login", "install 2.1.281 again"},
		{"2.1.281", "api-key", "sign in with api-key"},
	} {
		if err := l.Check(c.version, c.signIn); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if err := (Lock{Method: "phase0"}).Check("2.1.281", "login"); err == nil || !strings.Contains(err.Error(), "method phase0") {
		t.Errorf("other method: %v", err)
	}
	a := NewLockedTask("fix", "Fix it.", taskSpec("base1", "true"))
	b := NewLockedTask("fix", "Fix it.", taskSpec("base1", "false"))
	if a.Digest == "" || a.Digest == b.Digest || a.Digest != NewLockedTask("fix", "Fix it.", taskSpec("base1", "true")).Digest {
		t.Errorf("digests %s %s", a.Digest, b.Digest)
	}
	if spec := a.Spec(); spec.Base != "base1" || spec.Verify[0] != "true" {
		t.Errorf("Spec() = %+v", spec)
	}
}

func taskSpec(base, verify string) task.Spec {
	return task.Spec{Base: base, Verify: []string{verify}}
}

// A slow run holds the schedule back: nothing more than the window past it starts until it finishes, so the runs that
// overlap it are the ones it was told about.
func TestExecuteWindowHoldsBackPastASlowRun(t *testing.T) {
	slots := scheduleOf(t, 6, 1) // 12 runs
	var mu sync.Mutex
	slowDone := false // every run that starts before slot 0 finishes runs alongside it
	var alongside []int
	run := func(_ context.Context, s Slot, _ int, overlap []int) (Result, error) {
		if s.Position == 0 {
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			slowDone = true
			mu.Unlock()
			return Result{Outcome: claude.OutcomeOK}, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if !slowDone {
			alongside = append(alongside, s.Position)
			if !slices.Contains(overlap, 0) {
				t.Errorf("slot %d ran alongside slot 0 without being told", s.Position)
			}
		}
		return Result{Outcome: claude.OutcomeOK}, nil
	}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3}, run)
	if err != nil || sum.Status != StatusDone {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if len(alongside) != Window(2)-1 || slices.Max(alongside) >= Window(2) {
		t.Errorf("ran alongside the slow slot 0: %v; want exactly slots 1 to %d", alongside, Window(2)-1)
	}
}

// A retry waiting out its backoff comes before a pair the budget holds back: the execution waits for it instead of
// stopping at the budget with half a pair run.
func TestExecuteWaitsForARetryBeforeABudgetStop(t *testing.T) {
	slots := scheduleOf(t, 3, 1) // 6 runs, 3 pairs
	var mu sync.Mutex
	var ran []int
	run := func(_ context.Context, s Slot, attempt int, _ []int) (Result, error) {
		mu.Lock()
		ran = append(ran, s.Position)
		mu.Unlock()
		if s.Position == 0 && attempt == 1 {
			return Result{Outcome: claude.OutcomeInfra, CostUSD: 0.1}, nil
		}
		return Result{Outcome: claude.OutcomeOK, CostUSD: 0.5}, nil
	}
	// After slot 1 ($0.50) and slot 0's failure ($0.10), the next pair ($2 of caps) no longer fits the $2.50, but slot
	// 0's retry ($1) does.
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 2.5, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return 50 * time.Millisecond }}, run)
	if err != nil || sum.Status != StatusBudget {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if len(ran) != 3 || ran[2] != 0 || sum.SpentUSD != 1.1 {
		t.Errorf("ran %v, spent %v: slot 0's retry fit the budget and should have run before the stop", ran, sum.SpentUSD)
	}
	settled := map[int]bool{}
	for _, pos := range ran {
		settled[pos] = true
	}
	for pair := 0; pair < 3; pair++ {
		if settled[2*pair] != settled[2*pair+1] {
			t.Errorf("pair %d stopped half run: %v", pair, ran)
		}
	}
}

// Runs that gave up waiting for another run's dependency warm-up are not an outage: they stop the experiment with their
// own note (resume to continue), not the "infrastructure outage" one.
func TestExecuteWarmUpWaitsHaveTheirOwnNote(t *testing.T) {
	slots := scheduleOf(t, 4, 1)
	f := &fake{outcome: func(s Slot, _ int) Result {
		if s.Position == 0 {
			return Result{Outcome: claude.OutcomeOK}
		}
		return Result{Outcome: claude.OutcomeInfra, WarmWait: true}
	}}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return 0 }}, f.run)
	if err != nil || sum.Status != StatusStopped || !strings.Contains(sum.Note, "waited for another run's dependency warm-up") ||
		strings.Contains(sum.Note, "in a row failed") {
		t.Fatalf("summary %+v, %v; ran %v", sum, err, f.ran)
	}
}
