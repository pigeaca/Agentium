package experiment

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/stats"
)

// seqDesign is a valid seq-v1 cost design of tasks tasks.
func seqDesign(tasks int) Design {
	d := validDesign()
	d.Method, d.Repeats, d.Seed = MethodSeq, 1, 17
	d.Version = d.WantVersion()
	d.Tasks = nil
	for i := range tasks {
		d.Tasks = append(d.Tasks, fmt.Sprintf("t%02d", i))
	}
	return d
}

// seqLock locks d as Runner.buildLock does: method seq-v1, its sequential design, and a staged schedule.
func seqLock(t *testing.T, d Design) Lock {
	t.Helper()
	seq, err := NewSequential(len(d.Tasks), !d.NoFutility)
	if err != nil {
		t.Fatal(err)
	}
	l := Lock{Method: MethodSeq, Design: d, Sequential: &seq, Schedule: seq.stage(Schedule(d)), MaxAttempts: MaxAttempts,
		ClaudeCode: "2.1.281", SignIn: "login"}
	for _, name := range d.Tasks {
		l.Tasks = append(l.Tasks, LockedTask{Name: name})
	}
	return l
}

// seqRuns makes one ok run for each of the lock's slots before end, in schedule order: arm B costs ratio times arm A,
// with a task-level spread and a per-run jitter, so the per-task differences vary.
func seqRuns(l Lock, end int, ratio float64) []RunData {
	var runs []RunData
	for _, s := range l.Schedule[:end] {
		task := slices.Index(l.Design.Tasks, s.Task)
		cost := (0.5 + 0.1*float64(task%5)) * (1 + 0.04*float64((s.Position*7)%11))
		if s.Arm == "B" {
			cost *= ratio
		}
		yes := true
		runs = append(runs, RunData{Slot: s.Position, Task: s.Task, Arm: s.Arm, Outcome: claude.OutcomeOK, Passed: &yes, CostUSD: cost, DurationS: 60, OutputTokens: 100})
	}
	return runs
}

func TestSeqDesignValidation(t *testing.T) {
	d := seqDesign(16)
	if err := d.Validate(); err != nil {
		t.Fatalf("a seq-v1 design: %v", err)
	}
	if d.Version != DesignVersionSeq || d.LockMethod() != MethodSeq || !d.Sequential() {
		t.Errorf("version %d, method %s", d.Version, d.LockMethod())
	}
	legacy := validDesign() // stored before designs named a method: locks under phase1-v2
	if legacy.LockMethod() != MethodV2 || legacy.Sequential() || legacy.WantVersion() != DesignVersion {
		t.Errorf("a legacy design locks under %s", legacy.LockMethod())
	}
	if NewMethod(GoalCheaper) != MethodSeq || NewMethod(GoalBetter) != MethodV2 {
		t.Error("new cost experiments are seq-v1, success experiments phase1-v2")
	}
	for _, c := range []struct {
		change func(*Design)
		want   string
	}{
		{func(d *Design) { d.Goal = GoalBetter }, "for cost experiments"},
		{func(d *Design) { d.Repeats = 3 }, "runs each task once per arm"},
		{func(d *Design) { d.Tasks = append(d.Tasks, "t16") }, "at most 16 tasks"},
		{func(d *Design) { d.Method = "seq-v9" }, `unknown method "seq-v9"`},
		{func(d *Design) { d.Method, d.NoFutility = MethodV2, true; d.Version = d.WantVersion() }, "futility stops belong to method seq-v1"},
		{func(d *Design) { d.Version = DesignVersion }, "design version 1"},
	} {
		bad := seqDesign(16)
		c.change(&bad)
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
}

// The lock records the looks, alpha, spending, levels and futility; a resume refuses any change to them, and a
// staged schedule whose stages do not match.
func TestSeqLockRefusesChanges(t *testing.T) {
	l := seqLock(t, seqDesign(16))
	s := l.Sequential
	if !slices.Equal(s.Looks, []int{8, 12, 16}) || s.Alpha != 0.035 || s.EquivalenceAlpha != 0.05 || s.Spending != stats.SpendingOBF || s.Futility != 0.10 {
		t.Errorf("the sequential design %+v", s)
	}
	if math.Abs(s.EfficacyLevels[0]-0.9984) > 1e-4 || math.Abs(s.EquivalenceLevels[2]-0.9146) > 1e-4 {
		t.Errorf("levels %v / %v", s.EfficacyLevels, s.EquivalenceLevels)
	}
	if err := l.Check("2.1.281", "login"); err != nil {
		t.Fatalf("a fresh seq-v1 lock: %v", err)
	}
	for _, c := range []struct {
		name   string
		change func(*Lock)
	}{
		{"no design", func(l *Lock) { l.Sequential = nil }},
		{"alpha", func(l *Lock) { l.Sequential.Alpha = 0.045 }},
		{"equivalence alpha", func(l *Lock) { l.Sequential.EquivalenceAlpha = 0.10 }},
		{"spending", func(l *Lock) { l.Sequential.Spending = "pocock" }},
		{"looks", func(l *Lock) { l.Sequential.Looks = []int{4, 8, 16} }},
		{"futility", func(l *Lock) { l.Sequential.Futility = 0.2 }},
		{"levels", func(l *Lock) { l.Sequential.EfficacyLevels = []float64{0.95, 0.95, 0.95} }},
		{"stages", func(l *Lock) { l.Schedule[16].Stage = 1 }},
		{"repeats", func(l *Lock) { l.Design.Repeats = 2 }},
		{"method in the design", func(l *Lock) { l.Design.Method = MethodV2 }},
	} {
		bad := seqLock(t, seqDesign(16))
		c.change(&bad)
		if err := bad.Check("2.1.281", "login"); err == nil || !strings.Contains(err.Error(), "seq-v1 lock cannot continue") {
			t.Errorf("%s changed: %v", c.name, err)
		}
	}
	off := seqDesign(16)
	off.NoFutility = true
	if l := seqLock(t, off); l.Sequential.Futility != 0 || l.Check("2.1.281", "login") != nil {
		t.Errorf("futility off is recorded as 0 and resumes: %+v", l.Sequential)
	}
}

// Stages: the schedule is today's for one repeat (seeded task order, adjacent pairs, seeded arm order), cut at the
// looks: stage k is the pairs of the tasks after look k−1's count up to look k's.
func TestSeqScheduleStages(t *testing.T) {
	d := seqDesign(16)
	l := seqLock(t, d)
	plain := Schedule(d)
	for i, s := range l.Schedule {
		want := 1
		switch {
		case i >= 24:
			want = 3
		case i >= 16:
			want = 2
		}
		if s.Stage != want {
			t.Errorf("slot %d in stage %d, want %d", i, s.Stage, want)
		}
		s.Stage = 0
		if s != plain[i] {
			t.Errorf("slot %d differs from the unstaged schedule: %+v, %+v", i, s, plain[i])
		}
	}
	if l.Sequential.StageEnd(1) != 16 || l.Sequential.StageEnd(3) != 32 {
		t.Errorf("stage ends %d and %d", l.Sequential.StageEnd(1), l.Sequential.StageEnd(3))
	}
	for _, c := range []struct{ tasks, looks int }{{15, 2}, {12, 2}, {11, 1}, {8, 1}, {3, 1}} {
		l := seqLock(t, seqDesign(c.tasks))
		if len(l.Sequential.Looks) != c.looks || l.Sequential.Looks[len(l.Sequential.Looks)-1] != c.tasks || l.Check("2.1.281", "login") != nil {
			t.Errorf("%d tasks: looks %v", c.tasks, l.Sequential.Looks)
		}
	}
}

func costResult(t *testing.T, a Analysis) MetricResult {
	t.Helper()
	return result(t, a, MetricCost)
}

// An early stop: a clear cut ends the experiment at look 1, on exactly its stage's 8 tasks, at the look's levels.
func TestSeqStopsAtTheFirstDecisiveLook(t *testing.T) {
	l := seqLock(t, seqDesign(16))
	runs := seqRuns(l, 16, 0.5)
	status, an, err := SequentialStatus(l, runs)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ended != LookStop || len(status.Looks) != 1 || status.Reported != 1 || status.NextStage != 0 {
		t.Fatalf("status %+v", status)
	}
	look := status.Looks[0]
	if look.Counted != 8 || look.Verdict != stats.Improved || math.Abs(look.EffLevel-0.9984) > 1e-4 || look.Interval == nil || look.Interval.High >= 1 {
		t.Errorf("look 1 = %+v", look)
	}
	cost := costResult(t, an)
	if cost.Tasks != 8 || cost.Verdict != stats.Improved || cost.Level != look.EffLevel || cost.EqLevel != look.EqLevel || cost.TasksToResolve != 0 {
		t.Errorf("the reported cost result %+v", cost)
	}
	if guard := result(t, an, MetricSuccess); guard.Verdict != stats.Exploratory || guard.Level != 0 {
		t.Errorf("success stays exploratory at 95%%: %+v", guard)
	}
	if got := status.Describe(); got != "stopped at look 1 of 3 (8 tasks): cost improved" {
		t.Errorf("Describe() = %q", got)
	}
	whole, err := Analyze(l, runs)
	if err != nil || whole.Sequential == nil || !reflect.DeepEqual(*whole.Sequential, status) || costResult(t, whole).Verdict != stats.Improved {
		t.Errorf("Analyze on a seq-v1 lock: %+v, %v", whole.Sequential, err)
	}
}

// A look counts exactly its stage's prefix, and an experiment stopped between looks (by its budget, the usage limit or
// the user: the analysis sees only the runs) keeps its last look's verdict: runs of the next stage, however lopsided,
// are not analysed until their stage is settled.
func TestSeqLookCountsItsPrefixAndKeepsItBetweenLooks(t *testing.T) {
	d := seqDesign(16)
	d.NoFutility = true
	l := seqLock(t, d)
	stage1 := seqRuns(l, 16, 1.0)
	before, an1, err := SequentialStatus(l, stage1)
	if err != nil {
		t.Fatal(err)
	}
	if before.Ended != "" || before.NextStage != 2 || len(before.Looks) != 1 || before.Looks[0].Decision != LookContinue {
		t.Fatalf("after stage 1: %+v", before)
	}
	partial := seqRuns(l, 22, 1.0) // stage 2 has 3 of its 4 pairs
	for i := 16; i < 22; i++ {
		if partial[i].Arm == "B" {
			partial[i].CostUSD *= 10 // a huge regression, if it were counted
		}
	}
	between, an, err := SequentialStatus(l, partial)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(between, before) {
		t.Errorf("a partial stage changed the looks:\n%+v\n%+v", between, before)
	}
	if cost := costResult(t, an); cost.Tasks != 8 || cost.Verdict != costResult(t, an1).Verdict || cost.Verdict != stats.Inconclusive {
		t.Errorf("between looks the results are look 1's: %+v", cost)
	}
	if an.Counted["A"] != 8 || an.Counted["B"] != 8 {
		t.Errorf("counted runs %v: only the prefix's", an.Counted)
	}
	// A stage with a slot left unsettled (an interrupted run, stored as cancelled) is not looked at either.
	cancelled := append(seqRuns(l, 24, 1.0)[:23], RunData{Slot: 23, Task: l.Schedule[23].Task, Arm: l.Schedule[23].Arm, Outcome: claude.OutcomeCancelled})
	if s, _, _ := SequentialStatus(l, cancelled); len(s.Looks) != 1 || s.NextStage != 2 {
		t.Errorf("an unsettled stage was looked at: %+v", s)
	}
	// Once stage 2 settles, look 2 counts its 12 tasks; look 1 is made the same way.
	full2 := seqRuns(l, 24, 1.0)
	after, an2, _ := SequentialStatus(l, full2)
	if len(after.Looks) != 2 || !reflect.DeepEqual(after.Looks[0], before.Looks[0]) || after.Looks[1].Counted != 12 || costResult(t, an2).Tasks != 12 {
		t.Errorf("after stage 2: %+v", after)
	}
	// And the last look ends it, decisive or not.
	all, _, _ := SequentialStatus(l, seqRuns(l, 32, 1.0))
	if all.Ended != LookFinal || len(all.Looks) != 3 || all.Looks[2].Decision != LookFinal || all.Looks[2].ConditionalPower != nil {
		t.Errorf("after the last stage: %+v", all)
	}
}

// Futility: with no effect, an interim look stops when the conditional power is below 10%; with futility off, it
// continues. The efficacy levels are the same either way (non-binding).
func TestSeqFutility(t *testing.T) {
	on := seqLock(t, seqDesign(16))
	off := seqDesign(16)
	off.NoFutility = true
	lockOff := seqLock(t, off)
	runs := seqRuns(on, 16, 1.0)
	s, _, err := SequentialStatus(on, runs)
	if err != nil {
		t.Fatal(err)
	}
	look := s.Looks[0]
	if s.Ended != LookFutility || look.ConditionalPower == nil || *look.ConditionalPower >= 0.10 {
		t.Fatalf("no effect at look 1: %+v (conditional power %v)", s, look.ConditionalPower)
	}
	if !strings.HasPrefix(s.Describe(), "stopped at look 1 of 3 (8 tasks) for futility") {
		t.Errorf("Describe() = %q", s.Describe())
	}
	sOff, _, _ := SequentialStatus(lockOff, runs)
	if sOff.Ended != "" || sOff.Looks[0].ConditionalPower != nil || sOff.Looks[0].EffLevel != look.EffLevel {
		t.Errorf("futility off: %+v", sOff)
	}
}

// Lost tasks: a look with fewer than 8 counted tasks gives no verdict and the experiment continues; the next analysed
// look's fraction is its counted tasks over the planned 16, and it spends what the spending function has by then.
func TestSeqLostTasks(t *testing.T) {
	d := seqDesign(16)
	d.NoFutility = true
	l := seqLock(t, d)
	runs := seqRuns(l, 24, 0.97)
	var kept []RunData
	lost := map[string]bool{l.Schedule[0].Task: true, l.Schedule[2].Task: true, l.Schedule[16].Task: true}
	for _, r := range runs {
		if lost[r.Task] && r.Arm == "A" { // arm A's run failed for good: three infrastructure failures
			for range MaxAttempts {
				kept = append(kept, RunData{Slot: r.Slot, Task: r.Task, Arm: r.Arm, Outcome: claude.OutcomeInfra})
			}
			continue
		}
		kept = append(kept, r)
	}
	s, an, err := SequentialStatus(l, kept)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Looks) != 2 || s.Looks[0].Analysed || s.Looks[0].Counted != 6 || s.Looks[0].Decision != LookContinue || !strings.Contains(s.Looks[0].Note, "below the floor of 8") {
		t.Fatalf("look 1 with 6 tasks: %+v", s.Looks)
	}
	want, _ := stats.SequentialLooks([]int{9}, 16, false, stats.SeqAlpha, stats.SeqEquivalenceAlpha)
	look2 := s.Looks[1]
	if !look2.Analysed || look2.Counted != 9 || look2.Fraction != 9.0/16 || look2.EffLevel != want[0].EffLevel || s.Reported != 2 || costResult(t, an).Tasks != 9 {
		t.Errorf("look 2 with 9 of 12 tasks: %+v, want level %.5f", look2, want[0].EffLevel)
	}
	if an.Excluded[claude.OutcomeInfra] != 2*MaxAttempts+MaxAttempts {
		t.Errorf("excluded %v", an.Excluded)
	}
}

// Before any look, the results cover every run so far, and cost has no verdict whatever the runs show: an experiment
// of 8–11 tasks has one stage, which a partial run must not decide.
func TestSeqNoLookYet(t *testing.T) {
	l := seqLock(t, seqDesign(11))
	runs := seqRuns(l, 20, 0.5) // 10 of 11 tasks: more than the floor, but the stage is not settled
	s, an, err := SequentialStatus(l, runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Looks) != 0 || s.Reported != 0 || s.NextStage != 1 {
		t.Fatalf("status %+v", s)
	}
	if cost := costResult(t, an); cost.Verdict != stats.Exploratory || cost.Tasks != 10 || cost.Level != l.Sequential.EfficacyLevels[0] {
		t.Errorf("cost before any look: %+v", cost)
	}
	if !strings.HasPrefix(s.Describe(), "no look yet") {
		t.Errorf("Describe() = %q", s.Describe())
	}
}

// The analysis of a phase1-v2 lock is the one it always was: same intervals, same draws, no looks.
func TestSeqLeavesPhase1Unchanged(t *testing.T) {
	l := lockFor(GoalCheaper, 8, 1)
	runs := synthetic(8, 1, 1, 0.8, func(int, int, string) bool { return true })
	a, err := Analyze(l, runs)
	if err != nil {
		t.Fatal(err)
	}
	cost := costResult(t, a)
	if a.Sequential != nil || cost.Level != 0 || cost.EqLevel != 0 {
		t.Errorf("a phase1-v2 analysis has sequential fields: %+v", a.Sequential)
	}
	// The bootstrap's draws are the analysis's own: the same as drawing them directly, as it always did.
	tb := stats.NewTable()
	for _, r := range runs {
		tb.Add(r.Task, r.Arm, r.CostUSD)
	}
	boot, _ := stats.NewBootstrap(tb, "A", "B", math.Log, BootstrapDraws, rand.New(rand.NewPCG(l.Design.Seed, 4))) // cost is metric 1: stream 3+1
	if got := boot.Percentile(0.95).Map(math.Exp); got != cost.Boot95 {
		t.Errorf("phase1-v2's bootstrap changed: %+v, want %+v", cost.Boot95, got)
	}
}

// The barrier: Execute with Until runs no slot at or after it, retries and concurrency included, and is done once
// every slot before it is settled or out of attempts; the next stage then runs from the stored attempts.
func TestExecuteStopsAtTheStageEnd(t *testing.T) {
	slots := seqLock(t, seqDesign(4)).Schedule // 8 slots, one stage; cut here at 4 as two stages would be
	var mu sync.Mutex
	var started []int
	var attempts []Attempt // what a store would hold
	failed := false
	run := func(ctx context.Context, slot Slot, attempt int, overlap []int) (Result, error) {
		mu.Lock()
		started = append(started, slot.Position)
		first := slot.Position == 1 && !failed
		failed = failed || first
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		r := Result{Outcome: claude.OutcomeOK, CostUSD: 0.5}
		if first {
			r = Result{Outcome: claude.OutcomeInfra, CostUSD: 0.1}
		}
		mu.Lock()
		attempts = append(attempts, Attempt{Slot: slot.Position, Outcome: r.Outcome, CostUSD: r.CostUSD})
		mu.Unlock()
		return r, nil
	}
	p := Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: 3, Until: 4, Backoff: func(int) time.Duration { return time.Millisecond }}
	sum, err := Execute(context.Background(), p, run)
	if err != nil || sum.Status != StatusDone || sum.Settled != 4 || sum.Pending != 0 {
		t.Fatalf("stage 1: %+v, %v", sum, err)
	}
	if len(started) != 5 || slices.Max(started) >= 4 {
		t.Errorf("stage 1 started %v: its 4 slots and the retry, none of the next stage", started)
	}
	p.Prior, p.Until, started = slices.Clone(attempts), 0, nil
	if sum, err := Execute(context.Background(), p, run); err != nil || sum.Status != StatusDone || sum.Settled != 8 {
		t.Fatalf("stage 2: %+v, %v", sum, err)
	}
	if len(started) != 4 || slices.Min(started) < 4 {
		t.Errorf("stage 2 ran %v: only its own 4 slots", started)
	}
	for _, bad := range []int{3, 9} {
		p.Until = bad
		if _, err := Execute(context.Background(), p, run); err == nil {
			t.Errorf("Until %d (inside a pair, or past the end) was accepted", bad)
		}
	}
}

// The budget and the reserve are a seq-v1 design's maximum's: every task, whatever the looks may save. The preview
// says so, and gives the expected spend beside it.
func TestSeqBudgetAndPreviewUseTheMaximum(t *testing.T) {
	d := seqDesign(16)
	est := Same(Estimate{PerRunUSD: 0.5, Known: true})
	if got, want := DefaultBudget(d, est[0]), math.Ceil(1.25*32*0.5+Reserve(d)); got != want {
		t.Errorf("the default budget is $%.2f, want $%.2f: sized for all 16 tasks", got, want)
	}
	p, err := PreviewSequential(d, est)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Known || len(p.Looks) != 3 || p.MaxUSD != 16 || p.WorstUSD != 16*d.PairCapUSD() || p.Looks[0].CostUSD != 8 || p.Looks[1].WorstUSD != 12*d.PairCapUSD() {
		t.Errorf("preview %+v", p)
	}
	if math.Abs(p.TasksNone-10.3) > 0.3 || math.Abs(p.TasksCut-13.2) > 0.3 || math.Abs(p.NoneUSD-p.TasksNone*1.0) > 1e-9 || math.Abs(p.CutUSD-p.TasksCut*0.9) > 1e-9 {
		t.Errorf("expected spend: %.2f tasks ($%.2f) with no change, %.2f tasks ($%.2f) at a 20%% cut", p.TasksNone, p.NoneUSD, p.TasksCut, p.CutUSD)
	}
	if p.NoneUSD >= p.MaxUSD || p.CutUSD >= p.MaxUSD {
		t.Error("the expected spend is below the maximum")
	}
	unknown, _ := PreviewSequential(d, ArmEstimates{})
	if unknown.Known || unknown.MaxUSD != 0 || unknown.WorstUSD == 0 {
		t.Errorf("an unknown estimate: %+v", unknown)
	}
}

// A new cost experiment is seq-v1: one run per task and arm, sampled up to 16 tasks; tiers and other repeats are a
// success experiment's.
func TestPrepareMakesCostExperimentsSequential(t *testing.T) {
	o := NewOptions{Template: TemplateContextAB, ContextB: "lean", Goal: GoalCheaper}
	if err := o.Prepare("x"); err != nil || o.Repeats != 1 || o.tier != SeqTier() {
		t.Fatalf("a cost experiment: repeats %d, tier %+v, %v", o.Repeats, o.tier, err)
	}
	if o := (NewOptions{Template: TemplateContextAB, ContextB: "lean", Goal: GoalCheaper, Repeats: 1}); o.Prepare("x") != nil {
		t.Error("--repeats 1 is a cost experiment's own")
	}
	better := NewOptions{Template: TemplateContextAB, ContextB: "lean", Goal: GoalBetter}
	if err := better.Prepare("x"); err != nil || better.Repeats != Tiers()[0].Repeats {
		t.Errorf("a success experiment keeps the Quick tier: repeats %d, %v", better.Repeats, err)
	}
	for _, o := range []NewOptions{
		{Template: TemplateContextAB, ContextB: "lean", Goal: GoalCheaper, Tier: "quick"},
		{Template: TemplateContextAB, ContextB: "lean", Goal: GoalCheaper, Repeats: 3},
		{Template: TemplateContextAB, ContextB: "lean", Goal: GoalBetter, NoFutility: true},
	} {
		if _, usage := o.Prepare("x").(UsageError); !usage {
			t.Errorf("%+v was accepted", o)
		}
	}
}
