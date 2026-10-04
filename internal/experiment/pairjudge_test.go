package experiment

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// pairExecution is an execution of pairLock(t, 2) in a fresh store, with t1's two pairs stored, both runs passing and
// not yet compared (docs's are stored too, but its reference is no code). It returns the stored runs.
func pairExecution(t *testing.T) (*execution, []store.Run) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	proj, err := db.SaveProject(ctx, "/repo", "repo", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	exp, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: proj.ID, Name: "e", Template: TemplateContextAB, Design: []byte(`{}`), CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	l := pairLock(t, 2)
	for _, s := range l.Schedule {
		r := storedRun(t, "run-"+s.Task+"-"+s.Arm+string(rune('0'+s.Repeat)), s, agent.OutcomeOK, true, nil)
		r.ProjectID, r.ExperimentID, r.Kind, r.Started, r.Finished = proj.ID, exp.ID, "task", time.Now(), time.Now()
		if err := db.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := db.ExperimentRuns(ctx, exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &execution{r: Runner{Project: Project{DB: db, ID: proj.ID}}, stored: exp, lock: l}, runs
}

// comparison is a finished comparison that cost usd, preferring B.
func comparison(a run.Record, usd float64) run.PairJudgement {
	return run.PairJudgement{RunA: a.ID, Verdict: judge.PairVerdict{Version: judge.PairVersion, Prefer: judge.PreferB, CostUSD: usd,
		AB: judge.PairOrder{Answered: true, Answer: "second"}, BA: judge.PairOrder{Answered: true, Answer: "first"}}}
}

func wantOutside(t *testing.T, p *pairJudge, when string, spent, held float64) {
	t.Helper()
	gotSpent, gotHeld := p.outside()
	if math.Abs(gotSpent-spent) > 1e-9 || math.Abs(gotHeld-held) > 1e-9 {
		t.Errorf("%s: outside spent $%.2f and held $%.2f, want $%.2f and $%.2f", when, gotSpent, gotHeld, spent, held)
	}
}

// The comparisons' money: a queued pair holds its cap; each stored call moves its cost from held to spent, so spent
// plus held never drops below the caps less what the stored runs already count; a read of the stored runs for a budget
// (storedRuns) takes what was stored into the runs, once; what a comparison did not spend is released.
func TestPairJudgeAccountsForWhatItStores(t *testing.T) {
	ctx := context.Background()
	x, runs := pairExecution(t)
	p, err := newPairJudge(x, runs, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	x.pairs = p
	capUSD := x.lock.Design.PairJudgeCapUSD()
	if capUSD != 2 || len(p.queue) != 2 {
		t.Fatalf("cap $%v, %d queued: want t1's 2 pairs at $2", capUSD, len(p.queue))
	}
	wantOutside(t, p, "queued", 0, 2*capUSD)
	calls := 0
	p.judge = func(_ context.Context, _ run.Spec, _ judge.Settings, a, b run.Record, spent func(run.PairJudgement)) run.PairJudgement {
		calls++
		if a.Arm != "A" || b.Arm != "B" || a.Task != "t1" || b.Task != "t1" || b.PairJudge != nil {
			t.Errorf("compared %s (arm %s) with %s (arm %s), B holding %+v", a.ID, a.Arm, b.ID, b.Arm, b.PairJudge)
		}
		if calls == 2 { // stored only at its end
			return comparison(a, 0.5)
		}
		c := comparison(a, 0.3)
		c.Verdict.Stopped = judge.StoppedCall
		spent(c)
		wantOutside(t, p, "after the first call", 0.3, 2*capUSD-0.3)
		read, err := x.storedRuns(ctx) // a stage's read: what was stored is in the runs now
		if err != nil {
			t.Fatal(err)
		}
		total := 0.0
		for _, r := range read {
			total += storedSpend(r).PairJudgeUSD
		}
		if math.Abs(total-0.3) > 1e-9 {
			t.Errorf("the stored runs hold $%.2f of comparisons, want the first call's $0.30", total)
		}
		wantOutside(t, p, "after a read", 0, 2*capUSD-0.3)
		c.Verdict.CostUSD = 0.6
		spent(c)
		wantOutside(t, p, "after the second call", 0.3, 2*capUSD-0.6) // only what was stored since the read
		return comparison(a, 0.6)
	}
	p.start(ctx)
	unread, err := p.finish(true)
	if err != nil || calls != 2 || math.Abs(unread-0.8) > 1e-9 {
		t.Fatalf("finish: $%.2f unread, %d comparisons, %v: want $0.30 + $0.50", unread, calls, err)
	}
	wantOutside(t, p, "done", 0.8, 0)
	stored, err := x.r.Project.DB.ExperimentRuns(ctx, x.stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := uncompared(x.lock, stored); n != 0 {
		t.Errorf("%d pairs left uncompared", n)
	}
}

// While the execution waits for the usage window, no queued comparison starts; a pause at the usage limit drops them,
// and they wait for a resume, holding nothing.
func TestPairJudgeHoldsAndDropsAtTheUsageLimit(t *testing.T) {
	ctx := context.Background()
	x, runs := pairExecution(t)
	p, err := newPairJudge(x, runs, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	p.judge = func(_ context.Context, _ run.Spec, _ judge.Settings, a, _ run.Record, _ func(run.PairJudgement)) run.PairJudgement {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return comparison(a, 0.2)
	}
	p.hold(true)
	p.start(ctx)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	held := calls
	mu.Unlock()
	if _, err := p.finish(false); err != nil || held != 0 || calls != 0 {
		t.Fatalf("%d comparisons during the wait, %d by the pause (%v): want none", held, calls, err)
	}
	wantOutside(t, p, "dropped", 0, 0)
	stored, err := x.r.Project.DB.ExperimentRuns(ctx, x.stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := uncompared(x.lock, stored); n != 2 {
		t.Errorf("%d pairs wait for a resume, want 2", n)
	}

	// The resume: each compared once, the hold of a finished wait does not keep them back.
	p, err = newPairJudge(x, stored, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.judge = func(_ context.Context, _ run.Spec, _ judge.Settings, a, _ run.Record, _ func(run.PairJudgement)) run.PairJudgement {
		calls++
		return comparison(a, 0.2)
	}
	p.hold(true)
	p.start(ctx)
	if _, err := p.finish(true); err != nil || calls != 2 {
		t.Fatalf("resumed: %d comparisons, %v; want 2", calls, err)
	}
}
