package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
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
		if got := NeedsRevalidation(cand, c.experiment, ""); got != c.revalidate {
			t.Errorf("validated %q, experiment %q: re-validate %v", c.validated, c.experiment, got)
		}
	}
	if NeedsRevalidation(Candidate{Name: "t"}, task.GraderSandbox, "") {
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

	// Balanced arms: the sensitivity check still runs. A verdict it agrees with stands (cost); one it changes is demoted
	// with a note saying so (success here: the left-out runs were passes, counted as fails), a stricter reading of
	// decision 3 than the threshold alone.
	main := mustAnalyze(t, lockFor(GoalCheaper, 24, 3), runs) // the same runs on the host: nothing left out
	balanced := mustAnalyze(t, l, leaveOut(runs, 0, 3))       // one each
	c = balanced.Sandbox
	if c == nil || c.Imbalanced || c.Flagged["A"] != 1 || c.Flagged["B"] != 1 || len(c.AsFails) != 2 {
		t.Fatalf("balanced: %+v", c)
	}
	if cost := result(t, balanced, MetricCost); cost.Verdict != stats.Improved || cost.Note != "" || c.AsFails[MetricCost] != stats.Improved {
		t.Errorf("balanced arms keep an agreeing verdict: %+v", cost)
	}
	success := result(t, balanced, MetricSuccess)
	if result(t, main, MetricSuccess).Verdict == stats.Inconclusive {
		t.Fatal("the scenario needs a success verdict to demote")
	}
	if !slices.Equal(c.Disagrees, []string{MetricSuccess}) || success.Verdict != stats.Inconclusive ||
		!strings.Contains(success.Note, "counted as fails it is "+c.AsFails[MetricSuccess]+", not ") {
		t.Errorf("a disagreeing verdict: %+v (check %+v)", success, c)
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
	if !Settles(run.OutcomeSandboxFlagged) || Fair(run.OutcomeSandboxFlagged) || Settles(agent.OutcomeInfra) {
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
			return Result{Outcome: agent.OutcomeInfra}, nil // a canary failure: tried again
		}
		return Result{Outcome: agent.OutcomeOK, CostUSD: 0.2}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b := slices.IndexFunc(Schedule(d), func(s Slot) bool { return s.Arm == "B" })
	if attempts[b] != 1 || attempts[1-b] != 2 || sum.Settled != 2 {
		t.Errorf("attempts %v, summary %+v", attempts, sum)
	}
}

// asFails counts the runs left out for flagged sandbox denials as fair failures at their cost, and leaves every other
// run, and its input, as they were.
func TestAsFails(t *testing.T) {
	yes := true
	runs := []RunData{
		{Slot: 0, Arm: "A", Outcome: agent.OutcomeOK, Passed: &yes, CostUSD: 1},
		{Slot: 1, Arm: "B", Outcome: run.OutcomeSandboxFlagged, CostUSD: 2},
		{Slot: 2, Arm: "B", Outcome: agent.OutcomeInfra},
	}
	got := asFails(runs)
	if !reflect.DeepEqual(got[0], runs[0]) || !reflect.DeepEqual(got[2], runs[2]) {
		t.Errorf("other runs changed: %+v", got)
	}
	if r := got[1]; r.Outcome != agent.OutcomeOK || r.Passed == nil || *r.Passed || r.CostUSD != 2 || !Fair(r.Outcome) || Success(r.Outcome, r.Passed, nil) {
		t.Errorf("the left-out run: %+v", r)
	}
	if runs[1].Outcome != run.OutcomeSandboxFlagged || runs[1].Passed != nil {
		t.Error("the input changed")
	}
}

// Runs left out for sandbox denials settle without a retry, but many in a row on several slots stop the experiment, as
// an outage does: a toolchain the grading sandbox breaks would otherwise run, and pay for, every slot. A counted run in
// between starts the count again.
func TestFlaggedStreakStopsTheExperiment(t *testing.T) {
	d := validDesign()
	d.Tasks, d.Repeats = []string{"t1", "t2", "t3"}, 1
	exec := func(counted map[int]bool) (Summary, map[int]int) {
		attempts := map[int]int{}
		sum, err := Execute(context.Background(), Plan{Schedule: Schedule(d), Concurrency: 1, RunCapUSD: 1, BudgetUSD: 100, MaxAttempts: MaxAttempts,
			Backoff: func(int) time.Duration { return 0 }}, func(_ context.Context, s Slot, _ int, _ []int) (Result, error) {
			attempts[s.Position]++
			if counted[s.Position] {
				return Result{Outcome: agent.OutcomeOK, CostUSD: 0.1}, nil
			}
			return Result{Outcome: run.OutcomeSandboxFlagged, CostUSD: 0.1}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return sum, attempts
	}
	sum, attempts := exec(nil)
	if sum.Status != StatusStopped || !strings.Contains(sum.Note, "3 runs in a row were left out for sandbox denials") || len(attempts) != 3 {
		t.Errorf("every run left out: %+v, attempts %v", sum, attempts)
	}
	for _, n := range attempts {
		if n != 1 {
			t.Errorf("a left-out run was tried again: %v", attempts)
		}
	}
	sum, _ = exec(map[int]bool{2: true, 4: true})
	if sum.Status != StatusDone || sum.Settled != 6 {
		t.Errorf("counted runs between them: %+v", sum)
	}
}

// container-v1 is named (task.GraderContainer) but not graded in until the containers plan's step 4: every place that
// takes an experiment's mode refuses it as an unknown one, and none reads it as the sandbox (not host) or the host.
func TestContainerModeIsRefused(t *testing.T) {
	const mode = task.GraderContainer
	d := validDesign()
	d.Grader = mode
	if d.WantVersion() == DesignVersionSandbox {
		t.Error("a container design is stored under the sandbox's version")
	}
	d.Version = d.WantVersion()
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "grader "+mode) {
		t.Errorf("design: %v", err)
	}

	l := Lock{Method: MethodV2, ClaudeCode: "2.1.281", SignIn: "login", Grader: mode}
	if err := l.Check("2.1.281", "login"); err == nil || !strings.Contains(err.Error(), mode) || !strings.Contains(err.Error(), "start a new experiment") {
		t.Errorf("lock: %v", err)
	}

	// Readiness: no task is ready for the mode, whatever it was validated in, and none is validated again for it.
	arms := validDesign().Arms
	for _, validated := range []string{"", task.GraderHost, task.GraderSandbox, mode} {
		cand := Candidate{Name: "t", Validation: &task.Validation{Status: task.StatusValid, Grader: validated,
			Arms: []task.Arm{{Name: "base"}, {Name: "lean", Snapshot: "abc"}}}}
		if why := Ineligible(cand, arms, mode); !strings.Contains(why, "does not grade in "+mode) {
			t.Errorf("validated %q: eligible for %s: %q", validated, mode, why)
		}
		if NeedsRevalidation(cand, mode, "") {
			t.Errorf("validated %q: re-validated for %s", validated, mode)
		}
	}

	// Resume and lock go through run.SandboxUsable (the default): refused before any run, never graded on another mode.
	lock := Lock{Method: MethodV2, ClaudeCode: "2.1.281", SignIn: "login", Host: runtime.GOOS + "/" + runtime.GOARCH, Grader: mode}
	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	r := Runner{Out: io.Discard, SignIn: "login", ExecuteRun: func(context.Context, run.Env, RunMeta, run.Spec) (run.Record, error) {
		runs++
		return run.Record{}, nil
	}}
	if _, err := r.resume(context.Background(), store.Experiment{Lock: encoded}, "boxed", "2.1.281"); err == nil || !strings.Contains(err.Error(), "graded in "+mode+", which this Agentium does not grade in") || runs != 0 {
		t.Errorf("resume: %v, %d run(s)", err, runs)
	}
	if err := r.sandboxUsable(context.Background(), mode); err == nil || !strings.Contains(err.Error(), "grader "+mode) {
		t.Errorf("usable: %v", err)
	}
}

// The sandbox's per-arm check and the version of a design belong to the sandbox alone: a mode that is not host is not
// thereby the sandbox.
func TestSandboxCheckIsOnlyTheSandboxs(t *testing.T) {
	l := lockFor(GoalCheaper, 2, 1)
	for mode, want := range map[string]bool{"": false, task.GraderHost: false, task.GraderSandbox: true, task.GraderContainer: false} {
		l.Grader = mode
		if got := sandboxCheck(l, nil) != nil; got != want {
			t.Errorf("mode %q: sandbox check %v", mode, got)
		}
	}
}
