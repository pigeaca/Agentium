package experiment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
)

// How an execution ended (the store's experiment statuses).
const (
	StatusStopped = "stopped" // interrupted, repeated infrastructure failures, a changed environment, or an error
	StatusBudget  = "budget"  // the next run would not fit the budget
	StatusDone    = "done"    // every slot settled or failed for good
)

// InfraStreak is how many runs in a row failing for infrastructure reasons, on more than one slot, stop an experiment:
// an outage or a rate limit, which retries would only waste. One slot failing alone uses up its attempts instead.
const InfraStreak = 3

// Attempt is a stored run of a slot.
type Attempt struct {
	Slot    int
	Outcome string
	CostUSD float64 // everything it spent: the agent's run and its judgement (Result.CostUSD)
}

// Result is what running one attempt found.
type Result struct {
	Outcome string
	// CostUSD is everything the attempt spent, what the budget counts: the agent's run and its judgement. The agent's
	// own cost (the analysis's cost metric) is the run record's; JudgeUSD is the judgement's share of CostUSD.
	CostUSD  float64
	JudgeUSD float64
	// Judge is the judge's verdict in words, for progress lines; empty when the run was not judged.
	Judge string
	// Pause, when set, says why no new run should start for now (the judge hit a usage limit or a sign-in failure, which
	// later calls would hit too): runs in flight finish, then the execution pauses with StatusUsage.
	Pause string
	// Stop, when set, says why no later run can be fair (Claude Code's version changed, say): the experiment stops.
	Stop string
	// Usage is the run's last subscription usage reading, if it reported one: the gate's latest reading.
	Usage *claude.UsageReading
}

// AgentUSD is the agent's share of what the attempt spent: CostUSD without the judgement's.
func (r Result) AgentUSD() float64 { return r.CostUSD - r.JudgeUSD }

// Executor runs an attempt of a slot. overlap lists the positions of the slots whose runs may overlap it: their
// folders must be denied to it before it starts.
type Executor func(ctx context.Context, slot Slot, attempt int, overlap []int) (Result, error)

// Event reports progress: a run starting, finishing, or waiting to be retried, or the execution waiting for the usage
// window to reset (Kind "wait": Until and Usage, the window's share used).
type Event struct {
	Kind     string // "start", "finish", "retry" or "wait"
	Slot     Slot
	Attempt  int
	Result   Result
	Requeued bool // "finish": the run was stopped, not counted as an attempt, and runs again on resume
	SpentUSD float64
	RetryIn  time.Duration
	Until    time.Time
	Usage    float64
}

// Plan is an execution's input.
type Plan struct {
	Schedule    []Slot
	Concurrency int
	RunCapUSD   float64 // what one run may spend, its judgement included (Design.RunCapUSD)
	BudgetUSD   float64
	MaxAttempts int
	Prior       []Attempt                       // the experiment's stored runs
	Backoff     func(attempt int) time.Duration // before retrying a slot whose attempt failed for infrastructure
	Progress    func(Event)                     // optional
	Usage       *UsageGate                      // optional: pause before the subscription's usage limit
}

// Summary is how an execution ended.
type Summary struct {
	Status   string
	Note     string
	SpentUSD float64
	Settled  int // slots with a fair or unfair run
	Failed   int // slots out of attempts
	Pending  int // slots still to run
	// ResumeAt, for StatusUsage, is when the five-hour window resets.
	ResumeAt time.Time
}

// Window is how far past the earliest unfinished slot a run may start: twice the concurrency. It keeps a pair's runs
// close in time, and bounds which runs can overlap (see Overlap).
func Window(concurrency int) int { return 2 * concurrency }

// Overlap lists the positions whose runs may overlap a run at pos. A run at p starts only when p < L + W, where L is
// the earliest unfinished slot, so every slot before p − W had finished; and while p is unfinished, nothing at p + W
// or later starts. So only slots within the window on either side can overlap.
func Overlap(pos, concurrency, slots int) []int {
	var out []int
	for q := max(0, pos-Window(concurrency)+1); q < min(slots, pos+Window(concurrency)); q++ {
		if q != pos {
			out = append(out, q)
		}
	}
	return out
}

type slotState struct {
	attempts  int // infrastructure failures so far; cancelled runs are not attempts
	settled   bool
	failed    bool
	running   bool
	held      bool // its cap is reserved: its pair's other run has started
	notBefore time.Time
}

func (s slotState) finished() bool { return s.settled || s.failed }

// Execute runs the schedule's unsettled slots in order, up to Concurrency at a time within the window, and returns
// when every slot is settled or failed, or it has to stop:
//   - budget: a run starts only when the spend so far, the caps of the runs in flight (and of pair partners held for),
//     and its own cap (both caps for a pair's first run) fit BudgetUSD, so spending never passes it;
//   - retries: an infrastructure failure is retried after Backoff, up to MaxAttempts per slot; InfraStreak failures in
//     a row, on more than one slot, stop the experiment;
//   - usage (Usage set): a new pair starts only when the latest usage reading, plus the expected use of the runs in
//     flight and of the pair, stays within the limit; runs in flight finish, then the execution waits for the window
//     to reset (Usage.Wait) or pauses with StatusUsage;
//   - a Result with Stop, an executor error, or ctx's cancellation stop it; runs in flight finish first (a cancelled
//     ctx interrupts them, and they come back cancelled);
//   - a Result with Pause pauses it as a usage pause does (StatusUsage, with Pause as the note), without waiting: runs
//     in flight finish first.
//
// An executor error while ctx is live is Agentium's own failure: it is returned.
func Execute(ctx context.Context, p Plan, run Executor) (Summary, error) {
	if p.Concurrency < 1 || p.MaxAttempts < 1 {
		return Summary{}, errors.New("execute: concurrency and attempts must be positive")
	}
	state := make([]slotState, len(p.Schedule))
	partner := make([]int, len(p.Schedule))
	byPair := map[int][]int{}
	for i, s := range p.Schedule {
		if s.Position != i {
			return Summary{}, fmt.Errorf("execute: slot %d is at position %d", s.Position, i)
		}
		byPair[s.Pair] = append(byPair[s.Pair], i)
	}
	for i, s := range p.Schedule {
		partner[i] = -1
		for _, q := range byPair[s.Pair] {
			if q != i {
				partner[i] = q
			}
		}
	}
	var gate *UsageGate // a copy: finished runs update its latest reading
	if p.Usage != nil {
		g := *p.Usage
		gate = &g
	}
	var spent float64
	for _, a := range p.Prior {
		if a.Slot < 0 || a.Slot >= len(state) {
			return Summary{}, fmt.Errorf("execute: a stored run names slot %d of %d", a.Slot, len(state))
		}
		spent += a.CostUSD
		switch s := &state[a.Slot]; {
		case Settles(a.Outcome):
			s.settled = true
		case a.Outcome != claude.OutcomeCancelled:
			s.attempts++
		}
	}
	for i := range state {
		state[i].failed = !state[i].settled && state[i].attempts >= p.MaxAttempts
	}
	emit := func(e Event) {
		if p.Progress != nil {
			e.SpentUSD = spent
			p.Progress(e)
		}
	}

	type finished struct {
		pos, attempt int
		result       Result
		err          error
	}
	results := make(chan finished)
	running := 0
	var stopNote, pauseNote string
	var runErr error
	var streak []int // slots of the infrastructure failures in a row
	done := ctx.Done()
	for {
		now := time.Now()
		blocked, usageBlocked := false, false
		var wake time.Time
		if stopNote == "" && pauseNote == "" && runErr == nil && ctx.Err() == nil {
			low := len(state)
			for i := range state {
				if !state[i].finished() {
					low = i
					break
				}
			}
			reserved, held := 0.0, 0
			for i := range state {
				if state[i].running || state[i].held {
					reserved += p.RunCapUSD
				}
				if state[i].held && !state[i].running {
					held++
				}
			}
			for pos := low; pos < len(state) && pos < low+Window(p.Concurrency) && running < p.Concurrency; pos++ {
				s := &state[pos]
				if s.finished() || s.running {
					continue
				}
				if now.Before(s.notBefore) {
					if wake.IsZero() || s.notBefore.Before(wake) {
						wake = s.notBefore
					}
					continue
				}
				extra := 0.0
				if !s.held {
					extra += p.RunCapUSD
				}
				var hold *slotState
				if q := partner[pos]; q >= 0 {
					if qs := &state[q]; !qs.finished() && !qs.running && !qs.held {
						extra += p.RunCapUSD
						hold = qs
					}
				}
				// Only a slot that opens a pair (or has none) is gated: a pair's second run, its retry, or the half pair left
				// by an earlier execution always runs, so pairs stay whole.
				if gate != nil && (hold != nil || partner[pos] < 0) {
					starting := 1
					if hold != nil {
						starting++
					}
					if gate.projected(now, running+held+starting) > gate.Limit+1e-9 {
						usageBlocked = true
						break // in order, as for the budget
					}
				}
				if spent+reserved+extra > p.BudgetUSD+1e-9 {
					blocked = true
					break // in order: a later run must not overtake one the budget holds back
				}
				reserved += extra
				if hold != nil {
					hold.held = true
					held++
				}
				if s.held {
					held--
				}
				s.held, s.running = false, true
				running++
				slot, attempt, overlap := p.Schedule[pos], s.attempts+1, Overlap(pos, p.Concurrency, len(state))
				emit(Event{Kind: "start", Slot: slot, Attempt: attempt})
				go func() {
					result, err := run(ctx, slot, attempt, overlap)
					results <- finished{slot.Position, attempt, result, err}
				}()
			}
		}
		if running == 0 {
			sum := Summary{SpentUSD: spent}
			for _, s := range state {
				switch {
				case s.settled:
					sum.Settled++
				case s.failed:
					sum.Failed++
				default:
					sum.Pending++
				}
			}
			switch {
			case runErr != nil:
				sum.Status, sum.Note = StatusStopped, "Agentium could not carry out a run: "+runErr.Error()
				return sum, runErr
			case sum.Pending == 0:
				sum.Status = StatusDone
				return sum, nil
			case ctx.Err() != nil:
				sum.Status, sum.Note = StatusStopped, "interrupted"
				return sum, nil
			case stopNote != "":
				sum.Status, sum.Note = StatusStopped, stopNote
				return sum, nil
			case pauseNote != "":
				sum.Status, sum.Note = StatusUsage, pauseNote
				return sum, nil
			case usageBlocked && wake.IsZero(): // a retry that is due first goes before any pause
				until := gate.Latest.FiveHourResets
				if gate.Wait == nil || until.IsZero() || gate.PerRun*2 > gate.Limit {
					sum.Status, sum.ResumeAt = StatusUsage, until
					sum.Note = fmt.Sprintf("the five-hour usage window is at %.0f%%, and the next pair (about %.0f%% a run) would pass the %.0f%% limit",
						100*gate.Latest.FiveHourAt(now), 100*gate.PerRun, 100*gate.Limit)
					if gate.PerRun*2 > gate.Limit {
						sum.Note = fmt.Sprintf("a pair needs about %.0f%% of the five-hour usage window, more than the %.0f%% limit", 200*gate.PerRun, 100*gate.Limit)
					}
					return sum, nil
				}
				emit(Event{Kind: "wait", Until: until, Usage: gate.Latest.FiveHourAt(now)})
				if err := gate.Wait(ctx, until); err == nil {
					gate.Latest = claude.UsageReading{} // the window has reset: nothing of the new one is used yet
				}
				continue // a cancelled wait ends as interrupted
			case blocked && wake.IsZero(): // a run waiting to be retried comes first in order, and may still fit
				sum.Status = StatusBudget
				sum.Note = fmt.Sprintf("the next run would not fit the $%.2f budget ($%.2f spent, $%.2f per run at most)", p.BudgetUSD, spent, p.RunCapUSD)
				return sum, nil
			case wake.IsZero():
				return sum, errors.New("execute: nothing can start and nothing is waiting") // unreachable: the earliest unfinished slot can always start
			}
		}
		var timer *time.Timer
		var tick <-chan time.Time
		if !wake.IsZero() && running < p.Concurrency && stopNote == "" && pauseNote == "" && runErr == nil {
			timer = time.NewTimer(time.Until(wake))
			tick = timer.C
		}
		select {
		case f := <-results:
			s := &state[f.pos]
			s.running = false
			running--
			spent += f.result.CostUSD
			if gate != nil && f.result.Usage != nil && f.result.Usage.Newer(gate.Latest) {
				gate.Latest = *f.result.Usage
			}
			requeued := f.result.Outcome == claude.OutcomeCancelled || f.err != nil && ctx.Err() != nil
			emit(Event{Kind: "finish", Slot: p.Schedule[f.pos], Attempt: f.attempt, Result: f.result, Requeued: requeued && !Settles(f.result.Outcome)})
			if f.err != nil && ctx.Err() == nil && runErr == nil {
				runErr = fmt.Errorf("slot %d (task %s, arm %s): %w", f.pos, p.Schedule[f.pos].Task, p.Schedule[f.pos].Arm, f.err)
			}
			switch outcome := f.result.Outcome; {
			case Settles(outcome):
				s.settled, streak = true, nil
			case requeued:
				// run again later; not an attempt
			default: // infrastructure, or no outcome at all
				s.attempts++
				streak = append(streak, f.pos)
				if s.attempts >= p.MaxAttempts {
					s.failed = true
				} else if p.Backoff != nil {
					wait := p.Backoff(s.attempts)
					s.notBefore = time.Now().Add(wait)
					emit(Event{Kind: "retry", Slot: p.Schedule[f.pos], Attempt: f.attempt, Result: f.result, RetryIn: wait})
				}
				if len(streak) >= InfraStreak && distinct(streak) > 1 && stopNote == "" {
					stopNote = fmt.Sprintf("%d runs in a row failed for infrastructure reasons (an outage or a rate limit?): resume later", len(streak))
				}
			}
			if f.result.Stop != "" && stopNote == "" {
				stopNote = f.result.Stop
			}
			if f.result.Pause != "" && pauseNote == "" {
				pauseNote = f.result.Pause
			}
		case <-tick:
		case <-done:
			done = nil // runs in flight see the cancellation themselves; wait for them
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func distinct(slots []int) int {
	seen := map[int]bool{}
	for _, s := range slots {
		seen[s] = true
	}
	return len(seen)
}
