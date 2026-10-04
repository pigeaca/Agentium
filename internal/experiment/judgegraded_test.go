package experiment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
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
