package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// One experiment never mixes modes. A host experiment refuses a task validated in the sandbox; a sandbox experiment
// takes a task validated on the host (or under another sandbox version), which it validates again when it locks.
// Validations made before modes count as host ones.
func TestIneligibleByGrader(t *testing.T) {
	arms := validDesign().Arms
	validIn := func(grader string) Candidate {
		return Candidate{Name: "t", Validation: &task.Validation{Status: task.StatusValid, Grader: grader,
			Arms: []task.Arm{{Name: "base"}, {Name: "lean", Snapshot: "abc"}}}}
	}
	for _, c := range []struct {
		validated, experiment string
		refused, revalidate   bool
	}{
		{"", "", false, false},
		{"", task.GraderHost, false, false},
		{task.GraderHost, "", false, false},
		{task.GraderSandbox, task.GraderSandbox, false, false},
		{task.GraderSandbox, task.GraderHost, true, false},
		{task.GraderSandbox, "", true, false},
		{"", task.GraderSandbox, false, true},
		{task.GraderHost, task.GraderSandbox, false, true},
		{"sandbox-v0", task.GraderSandbox, false, true},
	} {
		cand := validIn(c.validated)
		why := Ineligible(cand, arms, c.experiment)
		if refused := why != ""; refused != c.refused {
			t.Errorf("validated %q, experiment %q: %q", c.validated, c.experiment, why)
		}
		if c.refused && (!strings.Contains(why, "validated in the sandbox (sandbox-v1), but this experiment grades on the host") ||
			!strings.Contains(why, "agentium task validate t --snapshot lean --grader host")) {
			t.Errorf("the refusal: %q", why)
		}
		if got := NeedsRevalidation(cand, c.experiment); got != c.revalidate {
			t.Errorf("validated %q, experiment %q: re-validate %v", c.validated, c.experiment, got)
		}
	}
	if NeedsRevalidation(Candidate{Name: "t"}, task.GraderSandbox) {
		t.Error("a task never validated is re-validated (it is refused)")
	}
}

// A sandbox design is stored under its own version, which an older Agentium refuses rather than grade it on the host;
// a host design keeps the version it always had.
func TestSandboxDesignVersion(t *testing.T) {
	d := validDesign()
	if d.WantVersion() != DesignVersion {
		t.Errorf("host: version %d", d.WantVersion())
	}
	d.Grader = task.GraderSandbox
	if d.WantVersion() != DesignVersionSandbox {
		t.Errorf("sandbox: version %d", d.WantVersion())
	}
	d.Method, d.Repeats = MethodSeq, 1
	if d.WantVersion() != DesignVersionSandbox {
		t.Errorf("a sandbox seq-v1 design: version %d", d.WantVersion())
	}
	d = validDesign()
	d.Grader = "sandbox-v0"
	d.Version = d.WantVersion()
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "grader sandbox-v0") {
		t.Errorf("an unknown grader: %v", err)
	}
}

// A lock made before modes (no grader) resumes, on the host; one under a mode this Agentium does not grade in is
// refused, since its later runs would not compare.
func TestLockCheckGrader(t *testing.T) {
	l := Lock{Method: MethodV2, ClaudeCode: "2.1.281", SignIn: "login"}
	for _, ok := range []string{"", task.GraderHost, task.GraderSandbox} {
		l.Grader = ok
		if err := l.Check("2.1.281", "login"); err != nil {
			t.Errorf("grader %q: %v", ok, err)
		}
	}
	l.Grader = "sandbox-v2"
	if err := l.Check("2.1.281", "login"); err == nil || !strings.Contains(err.Error(), "sandbox-v2") || !strings.Contains(err.Error(), "start a new experiment") {
		t.Errorf("an unknown grader: %v", err)
	}
}

// A re-validation covers each arm's context once: the base's own, then each distinct snapshot.
func TestValidationArms(t *testing.T) {
	got := ValidationArms([]Arm{{Name: "A", Context: "lean", Snapshot: "abc"}, {Name: "B", Context: "lean", Snapshot: "abc"}})
	if !slices.Equal(got, []task.Arm{{Name: "base"}, {Name: "lean", Snapshot: "abc"}}) {
		t.Errorf("an A/A of a snapshot: %v", got)
	}
	got = ValidationArms(validDesign().Arms)
	if !slices.Equal(got, []task.Arm{{Name: "base"}, {Name: "lean", Snapshot: "abc"}}) {
		t.Errorf("a context A/B: %v", got)
	}
}

// A sandbox experiment resumed on a machine that cannot grade in the sandbox now (nested, no sandbox-exec, a log that
// hides its denials) is refused before any run: never graded on the host instead. A host lock resumes there.
func TestResumeRefusesWhereTheSandboxIsUnusable(t *testing.T) {
	lock := Lock{Method: MethodV2, ClaudeCode: "2.1.281", SignIn: "login", Host: runtime.GOOS + "/" + runtime.GOARCH, Grader: task.GraderSandbox}
	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	runs := 0
	r := Runner{Out: io.Discard, SignIn: "login",
		SandboxUsable: func(_ context.Context, mode string) error {
			asked = append(asked, mode)
			if task.GraderOf(mode) == task.GraderHost {
				return nil
			}
			return errors.New("the grading sandbox is unavailable: nested")
		},
		ExecuteRun: func(context.Context, run.Env, RunMeta, run.Spec) (run.Record, error) {
			runs++
			return run.Record{}, nil
		}}
	_, err = r.resume(context.Background(), store.Experiment{Lock: encoded}, "boxed", "2.1.281")
	if err == nil || !strings.Contains(err.Error(), "experiment boxed cannot continue") || !strings.Contains(err.Error(), "nested") ||
		!strings.Contains(err.Error(), "--grader host") {
		t.Errorf("resume: %v", err)
	}
	if len(asked) != 1 || asked[0] != task.GraderSandbox || runs != 0 {
		t.Errorf("asked %v, %d run(s)", asked, runs)
	}
	lock.Grader = ""
	encoded, _ = json.Marshal(lock)
	r.Project = Project{Bare: t.TempDir()}
	if _, err := r.resume(context.Background(), store.Experiment{Lock: encoded}, "old", "2.1.281"); err != nil {
		t.Errorf("a host lock: %v", err)
	}
}

// sandboxLock is lockFor in sandbox mode, with a schedule whose pairs are synthetic's: slots 2k (A) and 2k+1 (B).
func sandboxLock(goal string, tasks, repeats int) Lock {
	l := lockFor(goal, tasks, repeats)
	l.Grader = task.GraderSandbox
	for s := range 2 * tasks * repeats {
		l.Schedule = append(l.Schedule, Slot{Position: s, Pair: s / 2, Arm: []string{"A", "B"}[s%2]})
	}
	return l
}

// leaveOut marks the runs at slots as left out for flagged sandbox denials.
func leaveOut(runs []RunData, slots ...int) []RunData {
	runs = slices.Clone(runs)
	for _, s := range slots {
		runs[s].Outcome, runs[s].Passed = run.OutcomeSandboxFlagged, nil
	}
	return runs
}

// Runs left out for flagged sandbox denials are counted per arm against the counted pairs. When the arms differ (one
// has some, the other none, or they differ by more than max(1, 10% of the pairs)) the cost and success verdicts are
// demoted to inconclusive, with what counting them as fails would give; balanced arms keep their verdicts, and so does
// a host lock, whatever its runs.
func TestSandboxCheckDemotesImbalancedArms(t *testing.T) {
	runs := synthetic(24, 3, 1.0, 0.7, func(task, repeat int, _ string) bool { return (task+repeat)%3 != 0 })
	l := sandboxLock(GoalCheaper, 24, 3)
	if cost := result(t, mustAnalyze(t, l, runs), MetricCost); cost.Verdict != stats.Improved {
		t.Fatalf("no exclusions: %+v", cost)
	}

	imbalanced := mustAnalyze(t, l, leaveOut(runs, 1, 3)) // two of B's runs (odd slots)
	c := imbalanced.Sandbox
	if c == nil || c.Flagged["A"] != 0 || c.Flagged["B"] != 2 || c.Pairs != 70 || !c.Imbalanced {
		t.Fatalf("the check: %+v", c)
	}
	for _, m := range []string{MetricCost, MetricSuccess} {
		r := result(t, imbalanced, m)
		if r.Verdict != stats.Inconclusive || !strings.Contains(r.Note, "demoted to inconclusive") ||
			!strings.Contains(r.Note, "(A 0, B 2)") || !strings.Contains(r.Note, "counting them as fails gives "+c.AsFails[m]) {
			t.Errorf("%s: %+v", m, r)
		}
	}
	if c.AsFails[MetricCost] != stats.Improved {
		t.Errorf("the sensitivity check: %v", c.AsFails)
	}

	balanced := mustAnalyze(t, l, leaveOut(runs, 0, 3)) // one each
	if c := balanced.Sandbox; c == nil || c.Imbalanced || c.Flagged["A"] != 1 || c.Flagged["B"] != 1 || len(c.AsFails) != 0 {
		t.Errorf("balanced: %+v", c)
	}
	if cost := result(t, balanced, MetricCost); cost.Verdict != stats.Improved {
		t.Errorf("balanced arms keep the verdict: %+v", cost)
	}
	// More than max(1, 10% of 68 pairs) apart: 9 against 1.
	var nine []int
	for k := range 9 {
		nine = append(nine, 2*k+1)
	}
	if c := mustAnalyze(t, l, leaveOut(runs, append(nine, 40)...)).Sandbox; c == nil || !c.Imbalanced {
		t.Errorf("9 against 1: %+v", c)
	}
	if c := mustAnalyze(t, l, leaveOut(runs, 1, 3, 5, 0)).Sandbox; c == nil || c.Imbalanced {
		t.Errorf("3 against 1 on about 70 pairs: %+v", c)
	}

	host := lockFor(GoalCheaper, 24, 3)
	if a := mustAnalyze(t, host, leaveOut(runs, 1, 3)); a.Sandbox != nil || result(t, a, MetricCost).Verdict != stats.Improved {
		t.Errorf("a host lock: %+v", a.Sandbox)
	}
}

// A seq-v1 look runs the same check, so it never stops early on a verdict the check demotes.
func TestSeqLookDoesNotStopOnADemotedVerdict(t *testing.T) {
	l := seqLock(t, seqDesign(16))
	l.Grader = task.GraderSandbox
	runs := seqRuns(l, 24, 0.5) // two stages: 12 tasks
	if status, _, err := SequentialStatus(l, runs); err != nil || status.Ended != LookStop {
		t.Fatalf("without exclusions the first look stops: %+v, %v", status, err)
	}
	for i, r := range runs {
		if r.Arm == "B" {
			runs[i].Outcome, runs[i].Passed = run.OutcomeSandboxFlagged, nil
			break
		}
	}
	status, an, err := SequentialStatus(l, runs)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ended == LookStop || status.Reported != 2 || an.Sandbox == nil || !an.Sandbox.Imbalanced || costResult(t, an).Verdict != stats.Inconclusive {
		t.Errorf("status %+v, check %+v", status, an.Sandbox)
	}
}

// A run left out for flagged sandbox denials settles its slot at once: the scheduler does not try it again, so an
// arm cannot re-roll its failures. A canary failure (ordinary infrastructure) is still retried.
func TestFlaggedFailureSettlesWithoutARetry(t *testing.T) {
	if !Settles(run.OutcomeSandboxFlagged) || Fair(run.OutcomeSandboxFlagged) || Settles(claude.OutcomeInfra) {
		t.Fatal("Settles or Fair")
	}
	d := validDesign()
	d.Tasks, d.Repeats = []string{"t1"}, 1
	attempts := map[int]int{}
	sum, err := Execute(context.Background(), Plan{Schedule: Schedule(d), Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: MaxAttempts,
		Backoff: func(int) time.Duration { return 0 }}, func(_ context.Context, s Slot, _ int, _ []int) (Result, error) {
		attempts[s.Position]++
		if s.Arm == "B" {
			return Result{Outcome: run.OutcomeSandboxFlagged, CostUSD: 0.2}, nil
		}
		if attempts[s.Position] == 1 {
			return Result{Outcome: claude.OutcomeInfra}, nil // a canary failure: tried again
		}
		return Result{Outcome: claude.OutcomeOK, CostUSD: 0.2}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b := slices.IndexFunc(Schedule(d), func(s Slot) bool { return s.Arm == "B" })
	if attempts[b] != 1 || attempts[1-b] != 2 || sum.Settled != 2 {
		t.Errorf("attempts %v, summary %+v", attempts, sum)
	}
}
