package experiment

import (
	"slices"
	"strings"
	"testing"

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
