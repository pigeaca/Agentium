package experiment

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// pairJudge compares an execution's pairs (Design.JudgePairs) beside its runs, one pair at a time, in a goroutine of
// its own: judging never holds a run's slot, a seq-v1 look or a stage's barrier. A pair is queued once both of its runs
// have settled and pass (PairRuns.NeedsComparing), at most once per execution, and its comparison is stored on its
// arm-B run.
//
// Money: a queued or running comparison holds Design.PairJudgeCapUSD. A pair completed by a run of this execution takes
// over the hold Execute kept for it (Plan.PairHoldUSD): the executor queues it before the run's result returns. A pair
// left from an earlier execution is queued at the start only when its cap fits the budget beside what is spent; one that
// does not is unfunded, and waits for a higher budget. Execute counts the holds and what comparisons stored since it read
// its spend (outside), so runs and comparisons together stay within the budget.
//
// Usage window: while the execution waits for the five-hour window to reset (hold), no queued comparison starts; when it
// pauses at the usage limit, the queued comparisons are dropped (finish) and compared on resume. A running one may
// finish either way.
//
// Crash and resume: each call's reported cost is stored as soon as it lands, as a comparison stopped early, which a
// resume compares again keeping that spend; a pair whose comparison never started has none, and is queued again.
type pairJudge struct {
	x      *execution
	capUSD float64
	emit   func(Event)
	// judge makes one comparison: the run environment's JudgePair (a fake in tests).
	judge   func(ctx context.Context, spec run.Spec, s llmjudge.Settings, a, b run.Record, spent func(run.PairJudgement)) run.PairJudgement
	mu      sync.Mutex
	pairs   map[int]*PairRuns // by Slot.Pair: each arm's settled run so far, stored or finished in this execution
	queued  map[int]bool      // pairs queued in this execution, compared or not: none twice
	queue   []*pairJob
	heldUSD float64 // caps of the queued and running comparisons, less what the running one has stored
	// unread is what comparisons stored since the stored runs were last read for a budget (read): their spend is in
	// the records only from that read on.
	unread   float64
	unfunded int  // pairs left uncompared for want of budget at the start
	stopped  bool // a comparison hit a usage limit or sign-in failure, or a record could not be stored: no more
	closed   bool // no more pairs will be queued
	waiting  bool // the execution waits for the usage window to reset: no queued comparison starts
	drop     bool // the execution paused at the usage limit: the queued comparisons are left for a resume
	err      error
	wake     chan struct{}
	done     chan struct{}
}

type pairJob struct {
	PairRuns
	leftUSD float64 // of the held cap, what the comparison may still spend
}

// newPairJudge reads the stored runs' pairs, and queues those left uncompared by earlier executions whose caps fit the
// budget beside spentUSD, all the experiment has spent. Nothing is compared before start.
func newPairJudge(x *execution, runs []store.Run, spentUSD float64, emit func(Event)) (*pairJudge, error) {
	pairs, err := PairsOf(x.lock, runs)
	if err != nil {
		return nil, err
	}
	p := &pairJudge{x: x, capUSD: x.lock.Design.PairJudgeCapUSD(), emit: emit, judge: x.runEnv.JudgePair, pairs: map[int]*PairRuns{},
		queued: map[int]bool{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	for i := range pairs {
		pr := pairs[i]
		p.pairs[pr.Pair] = &pr
		if !pr.NeedsComparing(x.lock) {
			continue
		}
		if spentUSD+p.heldUSD+p.capUSD > x.lock.Design.BudgetUSD+1e-9 {
			p.unfunded++
			continue
		}
		p.enqueue(&pr)
	}
	return p, nil
}

// enqueue queues pr and holds its cap; p.mu must be held, or p not yet shared.
func (p *pairJudge) enqueue(pr *PairRuns) {
	p.queued[pr.Pair] = true
	p.queue = append(p.queue, &pairJob{PairRuns: *pr, leftUSD: p.capUSD})
	p.heldUSD += p.capUSD
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// settled records a run of this execution that settled its slot, stored as id, and queues its pair when that completes
// a pair to compare. The executor calls it before the run's result returns to Execute, so the comparison's hold takes
// over from the pair's (Plan.PairHoldUSD) without a gap.
func (p *pairJudge) settled(slot Slot, id string, rec run.Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.pairs[slot.Pair]
	if pr == nil {
		pr = &PairRuns{Pair: slot.Pair, Task: slot.Task, Repeat: slot.Repeat}
		p.pairs[slot.Pair] = pr
	}
	settled := &PairRun{ID: id, Slot: slot.Position, Rec: rec}
	if slot.Arm == p.x.lock.Design.Arms[0].Name {
		pr.A = settled
	} else {
		pr.B = settled
	}
	if p.closed || p.stopped || p.queued[pr.Pair] || !pr.NeedsComparing(p.x.lock) {
		return
	}
	p.enqueue(pr)
}

// outside is Plan.Outside: what comparisons stored since the last read, and what they hold.
func (p *pairJudge) outside() (spentUSD, heldUSD float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unread, p.heldUSD
}

// read reads the stored runs a budget is computed from (fn), with no comparison stored meanwhile: their spend is then
// in what fn read, and unread starts again.
func (p *pairJudge) read(fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := fn(); err != nil {
		return err
	}
	p.unread = 0
	return nil
}

// start compares the queued pairs, and those queued later, until finish.
func (p *pairJudge) start(ctx context.Context) { go p.work(ctx) }

// hold stops queued comparisons from starting while the execution waits for the usage window to reset (on), and lets
// them go again once a run starts in the new window (off). A running comparison finishes.
func (p *pairJudge) hold(on bool) {
	p.mu.Lock()
	p.waiting = on
	p.mu.Unlock()
	p.poke()
}

func (p *pairJudge) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// finish ends the comparisons and returns what they stored since the last read and the first error that stopped them.
// With drain it waits for the queued ones; without (a pause at the usage limit, which no comparison may spend more of)
// it drops them for a resume. With ctx cancelled, the running comparison stops (stored as stopped early) and the queued
// ones are left for a resume.
func (p *pairJudge) finish(drain bool) (unreadUSD float64, err error) {
	p.mu.Lock()
	p.closed, p.drop, p.waiting = true, !drain, false
	p.mu.Unlock()
	p.poke()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unread, p.err
}

func (p *pairJudge) work(ctx context.Context) {
	defer close(p.done)
	for {
		p.mu.Lock()
		if ctx.Err() != nil || p.stopped || p.drop || (p.closed && len(p.queue) == 0) {
			for _, j := range p.queue { // left for a resume: their holds go
				p.heldUSD -= j.leftUSD
			}
			p.queue = nil
			p.mu.Unlock()
			return
		}
		if len(p.queue) == 0 || p.waiting {
			p.mu.Unlock()
			select {
			case <-p.wake:
			case <-ctx.Done():
			}
			continue
		}
		job := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()
		if err := p.compare(ctx, job); err != nil {
			p.mu.Lock()
			p.stopped, p.err = true, err
			p.mu.Unlock()
		}
	}
}

// compare compares one pair and stores the comparison on its arm-B run, as each call's cost lands and when it ends. An
// error is Agentium's own: the record could not be read or stored.
func (p *pairJudge) compare(ctx context.Context, job *pairJob) error {
	x, lock := p.x, p.x.lock
	defer func() { // what it did not spend is no longer held
		p.mu.Lock()
		p.heldUSD -= job.leftUSD
		job.leftUSD = 0
		p.mu.Unlock()
	}()
	t, ok := lock.Task(job.Task)
	if !ok {
		return fmt.Errorf("the lock has no task %s", job.Task)
	}
	storeCtx := context.WithoutCancel(ctx) // an interrupted comparison's spend is stored all the same
	stored, err := x.r.Project.DB.RunByID(storeCtx, x.r.Project.ID, job.B.ID)
	if err != nil {
		return err
	}
	var rec run.Record // as stored now: its slot has settled, and nothing else writes it while the experiment runs
	if err := json.Unmarshal(stored.Record, &rec); err != nil {
		return fmt.Errorf("run %s: %w", stored.ID, err)
	}
	written := rec.Spend().PairJudgeUSD // already in what the budget read
	var storeErr error
	keep := func(c run.PairJudgement) error {
		next := rec
		next.PairJudge = &c
		encoded, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode run %s: %w", stored.ID, err)
		}
		p.mu.Lock() // the record and the money move together: read never sees one without the other
		defer p.mu.Unlock()
		if err := x.r.Project.DB.SetRunRecord(storeCtx, stored.ID, encoded); err != nil {
			return err
		}
		delta := c.Verdict.CostUSD - written
		written = c.Verdict.CostUSD
		p.unread += delta
		take := min(max(delta, 0), job.leftUSD)
		job.leftUSD -= take
		p.heldUSD -= take
		return nil
	}
	spent := func(c run.PairJudgement) {
		if storeErr == nil {
			storeErr = keep(c)
		}
	}
	before := written
	c := p.judge(ctx, run.Spec{TaskName: t.Name, Instruction: t.Instruction, Task: t.Spec()}, *lock.Design.JudgePairs, job.A.Rec, rec, spent)
	if err := keep(c); err != nil {
		return err
	}
	if storeErr != nil {
		return storeErr
	}
	if c.Verdict.Stopped == llmjudge.StoppedLimit {
		p.mu.Lock()
		p.stopped = true
		p.mu.Unlock()
		x.judgePaused.Store(true)
	}
	if p.emit != nil {
		cost := c.Verdict.CostUSD - before
		p.emit(Event{Kind: "pair", Slot: lock.Schedule[job.B.Slot], Result: Result{Judge: run.DescribePair(c.Verdict), CostUSD: cost, JudgeUSD: cost}})
	}
	return nil
}
