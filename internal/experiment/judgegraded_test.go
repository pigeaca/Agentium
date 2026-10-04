package experiment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/task"
)

// judgedDesign is validDesign over tasks t00–t09 with the last judged of them judge-graded.
func judgedDesign(tasks, judged int) Design {
	d := validDesign()
	d.Tasks = nil
	for i := range tasks {
		d.Tasks = append(d.Tasks, fmt.Sprintf("t%02d", i))
	}
	d.JudgeGraded = append([]string(nil), d.Tasks[tasks-judged:]...)
	if judged > 0 {
		g := judge.GradingSettings()
		d.JudgeGrading = &g
	}
	d.Version = d.WantVersion()
	return d
}

// mixedLock is a lock of judgedDesign whose locked tasks carry their grading, as buildLock fixes them.
func mixedLock(goal string, tasks, judged, repeats int) Lock {
	d := judgedDesign(tasks, judged)
	d.Goal, d.Repeats, d.Seed = goal, repeats, 11
	l := Lock{Method: MethodV2, Design: d}
	for _, name := range d.Tasks {
		spec := task.Spec{Base: "b", Verify: []string{"true"}}
		if d.IsJudgeGraded(name) {
			spec.Grading = task.GradingJudge
		}
		l.Tasks = append(l.Tasks, NewLockedTask(name, "Fix it.", spec))
	}
	return l
}

// judgedRuns marks the runs of the lock's judge-graded tasks as graded by the judge.
func judgedRuns(l Lock, runs []RunData) []RunData {
	for i := range runs {
		if l.Design.IsJudgeGraded(runs[i].Task) {
			runs[i].Judged = true
		}
	}
	return runs
}

// A mixed experiment reports two success metrics over separate tasks, each with its own floor: the tests' over the
// test-graded tasks (a verdict when it reaches its floor) and the judge's over the judge-graded ones (never a verdict,
// whatever it shows). Neither counts the other's runs; cost counts both.
func TestAnalyzeMixedTasksKeepsTheTwoSuccessesApart(t *testing.T) {
	l := mixedLock(GoalBetter, 30, 8, 3) // 22 test-graded tasks reach success's floor of 20; the 8 judge-graded ones do not
	runs := judgedRuns(l, synthetic(30, 3, 1, 1, func(task, _ int, arm string) bool {
		if task >= 22 { // judge-graded: the judge says B fixes them all, A none
			return arm == "B"
		}
		return task%2 == 0 // the tests: the arms alike
	}))
	a := mustAnalyze(t, l, runs)
	success, judged, cost := result(t, a, MetricSuccess), result(t, a, MetricJudgeSuccess), result(t, a, MetricCost)
	if success.Tasks != 22 || judged.Tasks != 8 || cost.Tasks != 30 {
		t.Fatalf("tasks: success %d, the judge's %d, cost %d", success.Tasks, judged.Tasks, cost.Tasks)
	}
	if success.Role != RolePrimary || success.Verdict == stats.Exploratory || *success.A != 0.5 || *success.B != 0.5 {
		t.Errorf("the tests' success (primary): %+v", success)
	}
	// The judge sees B fix every judge-graded task and A none: a difference no verdict is computed on.
	if judged.Role != RoleSecondary || judged.Verdict != stats.Exploratory || judged.Note != NoteJudgeSuccess || *judged.A != 0 || *judged.B != 1 ||
		judged.FloorTasks != MinTasksSuccess || judged.FullTasks != 8 {
		t.Errorf("the judge's success: %+v", judged)
	}
	if a.PassAt1["B"] != 0.5 || a.JudgePassAt1["B"] != 1 || a.JudgePassAt1["A"] != 0 {
		t.Errorf("pass@1 %v, the judge's %v", a.PassAt1, a.JudgePassAt1)
	}
	// Without the judge-graded tasks the tests' success is the same: their runs never reach it.
	only := mustAnalyze(t, lockFor(GoalBetter, 30, 3), synthetic(22, 3, 1, 1, func(task, _ int, _ string) bool { return task%2 == 0 }))
	if s := result(t, only, MetricSuccess); s.Verdict != success.Verdict || *s.A != *success.A {
		t.Errorf("the tests' success moved with judge-graded tasks beside it: %+v vs %+v", s, success)
	}
	for _, r := range a.Results {
		if r.Metric == MetricJudgeSuccess && r.Role != RoleSecondary {
			t.Errorf("the judge's success has role %s", r.Role)
		}
	}
	// The floors are each metric's own: 8 judge-graded tasks never help success reach 20, nor 22 test-graded ones the
	// judge's.
	thin := mixedLock(GoalBetter, 30, 12, 3) // 18 test-graded: below the floor
	b := mustAnalyze(t, thin, judgedRuns(thin, synthetic(30, 3, 1, 1, func(task, _ int, arm string) bool { return task%2 == 0 || arm == "B" })))
	if s := result(t, b, MetricSuccess); s.Tasks != 18 || s.Verdict != stats.Exploratory || s.FullTasks != 18 {
		t.Errorf("success below its own floor: %+v", s)
	}
	if !thin.JudgeGraded() || lockFor(GoalBetter, 3, 1).JudgeGraded() {
		t.Error("Lock.JudgeGraded")
	}
}

// A lock without judge-graded tasks analyses as it always did: no judge metric, no judge pass@1, and the same JSON.
func TestAnalyzeWithoutJudgeGradedTasksIsUnchanged(t *testing.T) {
	l := lockFor(GoalCheaper, 10, 3)
	a := mustAnalyze(t, l, synthetic(10, 3, 1, 0.8, func(task, _ int, _ string) bool { return task%3 != 0 }))
	if len(a.Results) != 4 || a.JudgePassAt1 != nil {
		t.Errorf("results %d, judge pass@1 %v", len(a.Results), a.JudgePassAt1)
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "judge") {
		t.Errorf("the analysis' JSON mentions the judge: %s", encoded)
	}
}

// Cost uses only graded runs: a fair judge-graded run without a grade (none is stored so, but an older or edited record
// could be) is left out of every metric, cost included, and of a seq-v1 look's count.
func TestCostUsesOnlyGradedRuns(t *testing.T) {
	l := mixedLock(GoalCheaper, 10, 2, 1)
	runs := judgedRuns(l, synthetic(10, 1, 1, 0.8, func(int, int, string) bool { return true }))
	for i := range runs {
		if runs[i].Task == "t09" && runs[i].Arm == "B" {
			runs[i].Passed = nil
		}
	}
	a := mustAnalyze(t, l, runs)
	if c := result(t, a, MetricCost); c.Tasks != 9 || a.Excluded[OutcomeUngraded] != 1 || a.Counted["B"] != 9 {
		t.Errorf("cost tasks %d, excluded %v, counted %v", c.Tasks, a.Excluded, a.Counted)
	}
	if n := costPairs(runs, "A", "B"); n != 9 {
		t.Errorf("a look counts %d tasks with cost in both arms, want 9", n)
	}
}

// The design holds a judge-graded run's grading at its cap: 5 calls, each asked twice at most, at $0.50. Its runs cap,
// reserve, worst case and estimates include it, and a test-graded task's stay as they were.
func TestDesignCapsIncludeGrading(t *testing.T) {
	plain := judgedDesign(10, 0)
	d := judgedDesign(10, 2)
	if d.WantVersion() != DesignVersionJudge || plain.WantVersion() != DesignVersion {
		t.Errorf("versions %d, %d", d.WantVersion(), plain.WantVersion())
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("a design with judge-graded tasks: %v", err)
	}
	overshoot := claude.CapOvershootUSD(3, d.Model)
	if got := d.GradingCapUSD(); math.Abs(got-5) > 1e-9 {
		t.Errorf("GradingCapUSD = %v, want 5 calls × 2 × $0.50", got)
	}
	if got, want := d.TaskRunCapUSD(d.Arms[0], "t09"), 3+overshoot+5; math.Abs(got-want) > 1e-9 {
		t.Errorf("a judge-graded run's cap %v, want %v", got, want)
	}
	if d.TaskRunCapUSD(d.Arms[0], "t00") != plain.RunCapUSD() || d.PairCapUSD() != plain.PairCapUSD() {
		t.Errorf("a test-graded run's cap changed: %v vs %v", d.TaskRunCapUSD(d.Arms[0], "t00"), plain.RunCapUSD())
	}
	if got, want := d.RunCapUSD(), 3+overshoot+5; math.Abs(got-want) > 1e-9 {
		t.Errorf("RunCapUSD %v, want the larger, %v", got, want)
	}
	if Reserve(d) <= Reserve(plain) || d.MaxPairCapUSD() != 2*(3+overshoot+5) {
		t.Errorf("reserve %v vs %v, max pair %v", Reserve(d), Reserve(plain), d.MaxPairCapUSD())
	}
	if got, want := d.WorstUSD(), float64(8*3)*plain.PairCapUSD()+float64(2*3)*2*(3+overshoot+5); math.Abs(got-want) > 1e-9 {
		t.Errorf("WorstUSD %v, want %v", got, want)
	}
	if plain.WorstUSD() != float64(10*3)*plain.PairCapUSD() {
		t.Errorf("a test-graded design's worst case moved: %v", plain.WorstUSD())
	}
	// 2 tasks × 3 repeats × 2 arms × 5 calls at the pilot's mean call; the second opinion skips them.
	if got, want := d.GradingEstimateUSD(), float64(2*3*2*5)*judge.EstimateUSD; math.Abs(got-want) > 1e-9 || plain.GradingEstimateUSD() != 0 {
		t.Errorf("grading estimate %v, want %v", got, want)
	}
	d.Judge, plain.Judge = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 3}, &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 3}
	if got, want := d.JudgeEstimateUSD(), float64(8*3*2*3)*judge.EstimateUSD; math.Abs(got-want) > 1e-9 {
		t.Errorf("second-opinion estimate %v, want test-graded runs only (%v)", got, want)
	}
	if d.SlotCapUSD("B", "t09") != d.TaskRunCapUSD(d.Arms[1], "t09") || d.SlotCapUSD("B", "t00") != d.ArmRunCapUSD(d.Arms[1]) {
		t.Error("SlotCapUSD")
	}
	// A budget below the dearest pair is refused.
	d.BudgetUSD = d.PairCapUSD() + 1
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "below one pair") {
		t.Errorf("a budget below a judge-graded pair: %v", err)
	}
	for name, change := range map[string]func(*Design){
		"not a task":     func(d *Design) { d.JudgeGraded = append(d.JudgeGraded, "nope") },
		"twice":          func(d *Design) { d.JudgeGraded = append(d.JudgeGraded, d.JudgeGraded[0]) },
		"no judge":       func(d *Design) { d.JudgeGrading = nil },
		"judge, no task": func(d *Design) { d.JudgeGraded = nil; d.Version = d.WantVersion() },
	} {
		x := judgedDesign(10, 2)
		change(&x)
		if err := x.Validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The executor holds each slot's own cap when the plan gives one (Design.SlotCapUSD): a judge-graded task's run holds
// its grading's, a test-graded one only its own, not the larger RunCapUSD.
func TestExecuteHoldsEachSlotsCap(t *testing.T) {
	slots := scheduleOf(t, 4, 1) // 8 runs
	judged := slots[len(slots)-1].Task
	capOf := func(s Slot) float64 {
		if s.Task == judged {
			return 4
		}
		return 1
	}
	var mu sync.Mutex
	committed, spent, inFlight := 0.0, 0.0, map[int]bool{}
	run := func(ctx context.Context, s Slot, attempt int, overlap []int) (Result, error) {
		return Result{Outcome: claude.OutcomeCapped, CostUSD: capOf(s)}, nil // every run at its cap
	}
	plan := Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 4, BudgetUSD: 7, MaxAttempts: 3, CapOf: capOf, Progress: func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		switch e.Kind {
		case "start":
			inFlight[e.Slot.Position] = true
		case "finish":
			delete(inFlight, e.Slot.Position)
			spent = e.SpentUSD
		}
		held := 0.0
		for pos := range inFlight {
			held += capOf(slots[pos])
		}
		committed = max(committed, spent+held)
	}}
	sum, err := Execute(context.Background(), plan, run)
	if err != nil || sum.Status != StatusBudget || sum.Settled != 6 || committed > 7 {
		t.Errorf("with each slot's cap: %+v, %v, committed up to $%.2f: the test-graded pairs fit, the judge-graded one ($8) does not", sum, err, committed)
	}
	plan.CapOf, plan.Progress = nil, nil
	if sum, err := Execute(context.Background(), plan, run); err != nil || sum.Settled != 0 {
		t.Errorf("with RunCapUSD for every slot: %+v, %v", sum, err)
	}
}

// A locked task's digest covers its grading, and a test-graded task's digest is the one it had before judge grading.
func TestLockedTaskDigestCoversGrading(t *testing.T) {
	spec := task.Spec{Base: "b", Solution: "s", Reference: []string{"a.go"}, Verify: []string{"true"}}
	plain := NewLockedTask("fix", "Fix it.", spec)
	spec.Grading = task.GradingTests
	tests := NewLockedTask("fix", "Fix it.", spec)
	spec.Grading = task.GradingJudge
	judged := NewLockedTask("fix", "Fix it.", spec)
	before, _ := json.Marshal(struct {
		Name        string   `json:"name"`
		Instruction string   `json:"instruction"`
		Base        string   `json:"base"`
		Solution    string   `json:"solution,omitempty"`
		HiddenTests []string `json:"hidden_tests,omitempty"`
		Reference   []string `json:"reference,omitempty"`
		Setup       []string `json:"setup,omitempty"`
		Verify      []string `json:"verify"`
		Digest      string   `json:"digest"`
	}{Name: "fix", Instruction: "Fix it.", Base: "b", Solution: "s", Reference: []string{"a.go"}, Verify: []string{"true"}})
	sum := sha256.Sum256(before)
	if plain.Digest != hex.EncodeToString(sum[:]) || tests.Digest != plain.Digest || tests.Grading != "" {
		t.Errorf("a test-graded task's digest changed: %s, %s", plain.Digest, tests.Digest)
	}
	if judged.Digest == plain.Digest || !judged.JudgeGraded() || !judged.Spec().JudgeGraded() || plain.Spec().JudgeGraded() {
		t.Errorf("a judge-graded task: %+v", judged)
	}
}

// ungradeAt marks the runs at the slots as left without a grade for good (pending false) or with their grade pending.
func ungradeAt(runs []RunData, pending bool, slots ...int) []RunData {
	runs = slices.Clone(runs)
	for _, s := range slots {
		runs[s].Passed, runs[s].Pending = nil, pending
	}
	return runs
}

// Judge-graded runs left without a grade (a tie, refusals, errors on every attempt) are counted per arm, as flagged
// sandbox exclusions are: when the arms differ, every verdict those runs would feed (cost here) is demoted to
// inconclusive, with what counting them gives; balanced arms keep an agreeing verdict. The tests' success never counts
// a judge-graded run, so it is never demoted for them. A grade still pending is no exclusion of that kind.
func TestUngradedCheckDemotesImbalancedArms(t *testing.T) {
	l := mixedLock(GoalCheaper, 24, 4, 3)
	runs := judgedRuns(l, synthetic(24, 3, 1.0, 0.7, func(task, repeat int, _ string) bool { return (task+repeat)%3 != 0 }))
	var judgedB, judgedA []int
	for i, r := range runs {
		if r.Judged && r.Arm == "B" {
			judgedB = append(judgedB, i)
		}
		if r.Judged && r.Arm == "A" {
			judgedA = append(judgedA, i)
		}
	}
	base := mustAnalyze(t, l, runs)
	if cost := result(t, base, MetricCost); cost.Verdict != stats.Improved || base.Ungraded == nil || base.Ungraded.Imbalanced || base.Ungraded.AsCounted != nil {
		t.Fatalf("nothing ungraded: cost %+v, check %+v", cost, base.Ungraded)
	}
	success := result(t, base, MetricSuccess)

	imbalanced := mustAnalyze(t, l, ungradeAt(runs, false, judgedB[0], judgedB[1]))
	u := imbalanced.Ungraded
	if u == nil || u.Ungraded["A"] != 0 || u.Ungraded["B"] != 2 || !u.Imbalanced || u.AsCounted[MetricCost] != stats.Improved || imbalanced.Excluded[OutcomeUngraded] != 2 {
		t.Fatalf("the check: %+v, excluded %v", u, imbalanced.Excluded)
	}
	if cost := result(t, imbalanced, MetricCost); cost.Verdict != stats.Inconclusive || !strings.Contains(cost.Note, "left without a grade differ (A 0, B 2)") ||
		!strings.Contains(cost.Note, "counting them gives improved") {
		t.Errorf("cost: %+v", cost)
	}
	if s := result(t, imbalanced, MetricSuccess); s.Verdict != success.Verdict || strings.Contains(s.Note, "without a grade") {
		t.Errorf("the tests' success moved for judge-graded runs: %+v vs %+v", s, success)
	}
	if _, ok := u.AsCounted[MetricSuccess]; ok {
		t.Errorf("as counted names success: %v", u.AsCounted)
	}

	balanced := mustAnalyze(t, l, ungradeAt(runs, false, judgedA[0], judgedB[0]))
	if u := balanced.Ungraded; u.Imbalanced || u.Ungraded["A"] != 1 || u.Ungraded["B"] != 1 || len(u.Disagrees) != 0 {
		t.Errorf("balanced: %+v", u)
	}
	if cost := result(t, balanced, MetricCost); cost.Verdict != stats.Improved || cost.Note != "" {
		t.Errorf("balanced arms keep an agreeing verdict: %+v", cost)
	}

	// Pending grades are waiting, not left out for good: no per-arm count, no demotion, an exclusion of their own.
	pending := mustAnalyze(t, l, ungradeAt(runs, true, judgedB[0], judgedB[1]))
	if u := pending.Ungraded; u.Ungraded["B"] != 0 || u.Imbalanced || pending.Excluded[OutcomeGradePending] != 2 || pending.Excluded[OutcomeUngraded] != 0 {
		t.Errorf("pending: %+v, excluded %v", u, pending.Excluded)
	}
	if cost := result(t, pending, MetricCost); cost.Verdict != stats.Improved {
		t.Errorf("pending grades demoted cost: %+v", cost)
	}
	// A lock without judge-graded tasks has no such check.
	if a := mustAnalyze(t, lockFor(GoalCheaper, 24, 3), synthetic(24, 3, 1.0, 0.7, func(int, int, string) bool { return true })); a.Ungraded != nil {
		t.Errorf("a test-graded lock: %+v", a.Ungraded)
	}
}

// A seq-v1 look waits for every grade of its stages: a judge-graded run whose grade is pending settles its slot for
// the scheduler (the agent never runs again) but leaves the stage not done, so no look reads a grade that may still
// change. Once graded, or left ungraded for good, the look is made.
func TestSeqLookWaitsForPendingGrades(t *testing.T) {
	d := seqDesign(16)
	d.JudgeGraded = []string{d.Tasks[3]}
	g := judge.GradingSettings()
	d.JudgeGrading = &g
	d.Version = d.WantVersion()
	l := seqLock(t, d)
	runs := seqRuns(l, 16, 0.5)
	var at int
	for i := range runs {
		if runs[i].Task == d.Tasks[3] {
			runs[i].Judged = true
			at = i
		}
	}
	if s, _, err := SequentialStatus(l, runs); err != nil || len(s.Looks) != 1 {
		t.Fatalf("premise: every grade in, look 1 is made: %+v, %v", s, err)
	}
	waiting := ungradeAt(runs, true, at)
	s, _, err := SequentialStatus(l, waiting)
	if err != nil || len(s.Looks) != 0 || s.NextStage != 1 || s.Ended != "" {
		t.Errorf("a pending grade in stage 1: %+v, %v", s, err)
	}
	if done := slotsDone(l, waiting); done[waiting[at].Slot] {
		t.Error("the pending grade's slot is done")
	}
	if !Settles(waiting[at].Outcome) {
		t.Error("the pending run does not settle its slot for the scheduler")
	}
	if s, _, err := SequentialStatus(l, ungradeAt(runs, false, at)); err != nil || len(s.Looks) != 1 {
		t.Errorf("left ungraded for good: the look is made: %+v, %v", s, err)
	}
}

// The judges' estimate holds the grading's: every judge-graded run's calls at the pilot's mean, beside the second
// opinion's and the pair judge's.
func TestJudgingEstimateHoldsGrading(t *testing.T) {
	d := judgedDesign(10, 2)
	if got := d.JudgingEstimateUSD(); got <= 0 || math.Abs(got-d.GradingEstimateUSD()) > 1e-9 {
		t.Errorf("JudgingEstimateUSD = %v, want the grading's %v", got, d.GradingEstimateUSD())
	}
	d.Judge = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 3}
	if got, want := d.JudgingEstimateUSD(), d.JudgeEstimateUSD()+d.GradingEstimateUSD()+d.PairJudgeEstimateUSD(); math.Abs(got-want) > 1e-9 {
		t.Errorf("JudgingEstimateUSD = %v, want %v", got, want)
	}
}

// Calibrations must leave room for the dearest pair: a judge-graded task's, with its grading, not a test-graded one's.
func TestCalibrationBudgetHoldsTheDearestPair(t *testing.T) {
	d := judgedDesign(10, 2)
	d.BudgetUSD = d.PairCapUSD() + 2 // a test-graded pair and the calibrations fit; a judge-graded pair does not
	if d.PairCapUSD()+1 > d.BudgetUSD || d.MaxPairCapUSD()+1 <= d.BudgetUSD {
		t.Fatalf("premise: pair $%.2f, max pair $%.2f, budget $%.2f", d.PairCapUSD(), d.MaxPairCapUSD(), d.BudgetUSD)
	}
	if err := calibrationBudget(d, 0, 1); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("($%.2f)", d.MaxPairCapUSD())) {
		t.Errorf("calibrations beside a judge-graded pair: %v", err)
	}
	d.BudgetUSD = d.MaxPairCapUSD() + 1
	if err := calibrationBudget(d, 0, 1); err != nil {
		t.Errorf("room for both: %v", err)
	}
}

// The scheduler's plan holds each slot's own cap when the lock has judge-graded tasks (a judge-graded run its
// grading's, a test-graded one only its own), and the arms' caps as before without them.
func TestLockPlanHoldsEachSlotsCap(t *testing.T) {
	l := mixedLock(GoalCheaper, 4, 1, 1)
	l.Schedule, l.MaxAttempts = Schedule(l.Design), MaxAttempts
	p := lockPlan(l, 0.5)
	if p.CapOf == nil || p.SpentUSD != 0.5 || p.BudgetUSD != l.Design.BudgetUSD || p.MaxAttempts != MaxAttempts || len(p.Schedule) != 8 {
		t.Fatalf("plan %+v", p)
	}
	for _, s := range p.Schedule {
		if got, want := p.CapOf(s), l.Design.SlotCapUSD(s.Arm, s.Task); got != want || (l.Design.IsJudgeGraded(s.Task) != (got > l.Design.ArmRunCapUSD(l.Design.Arms[0]))) {
			t.Errorf("slot %d (%s): cap %v, want %v", s.Position, s.Task, got, want)
		}
	}
	plain := lockFor(GoalCheaper, 4, 1)
	if lockPlan(plain, 0).CapOf != nil {
		t.Error("a test-graded lock's plan gives each slot a cap of its own")
	}
}

// experiment new warns, without refusing, when the test-graded tasks beside judge-graded ones are fewer than success's
// floor: the tests' success cannot reach a verdict, and the judge-graded tasks never count toward it.
func TestSuccessFloorWarning(t *testing.T) {
	d := judgedDesign(10, 2)
	d.Goal = GoalCheaper
	if w := SuccessFloorWarning(d); !strings.Contains(w, "only 8 test-graded task(s), below success's floor of 20: the success guard cannot reach a verdict") {
		t.Errorf("warning %q", w)
	}
	d.Goal = GoalBetter
	if w := SuccessFloorWarning(d); !strings.Contains(w, "success, the primary metric, cannot") {
		t.Errorf("warning %q", w)
	}
	if w := SuccessFloorWarning(judgedDesign(10, 0)); w != "" {
		t.Errorf("a test-graded design warns: %q", w)
	}
	if w := SuccessFloorWarning(judgedDesign(22, 2)); w != "" {
		t.Errorf("20 test-graded tasks warn: %q", w)
	}
}
