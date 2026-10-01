package experiment

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// An experiment on a Gradle project is refused before it locks or spends anything unless the user opted in; with the
// opt-in the lock records it, and a project that does not need it records nothing.
func TestCheckLocalBinding(t *testing.T) {
	lock := Lock{Tasks: []LockedTask{{Base: "c1"}, {Base: "c2"}}}
	check := func(needed, allowed bool, err error) (bool, error) {
		var asked []string
		r := Runner{NeedsLocalBinding: func(_ context.Context, bases []string) (bool, bool, error) {
			asked = bases
			return needed, allowed, err
		}}
		got, gotErr := r.checkLocalBinding(context.Background(), lock)
		if len(asked) != 2 || asked[0] != "c1" || asked[1] != "c2" {
			t.Errorf("asked about %v", asked)
		}
		return got, gotErr
	}
	if got, err := check(true, false, nil); err == nil || got || !strings.Contains(err.Error(), "agentium init --allow-local-binding") {
		t.Errorf("without the opt-in: %v, %v", got, err)
	}
	if got, err := check(true, true, nil); err != nil || !got {
		t.Errorf("with the opt-in: %v, %v", got, err)
	}
	if got, err := check(false, true, nil); err != nil || got {
		t.Errorf("a project that needs none, though allowed: %v, %v", got, err)
	}
	if _, err := check(false, false, errors.New("boom")); err == nil {
		t.Error("an error is lost")
	}
	if got, err := (Runner{}).checkLocalBinding(context.Background(), lock); err != nil || got {
		t.Errorf("no check wired: %v, %v", got, err)
	}
}

// An experiment locked before Gradle projects needed local binding (no opt-in recorded) is refused at resume with its
// own message: running `init --allow-local-binding` cannot help it, as every run would still refuse.
func TestResumeRefusesAnOldLockOnAGradleProject(t *testing.T) {
	needs := func(needed bool) *Runner {
		return &Runner{NeedsLocalBinding: func(context.Context, []string) (bool, bool, error) { return needed, true, nil }}
	}
	old := Lock{Tasks: []LockedTask{{Base: "c1"}}}
	err := needs(true).checkResumeLocalBinding(context.Background(), "ab", old)
	if err == nil || !strings.Contains(err.Error(), "locked before") || !strings.Contains(err.Error(), "start a new experiment") {
		t.Errorf("an old lock on a Gradle project: %v", err)
	}
	if err := needs(false).checkResumeLocalBinding(context.Background(), "ab", old); err != nil {
		t.Errorf("an old lock on another project: %v", err)
	}
	recorded := old
	recorded.LocalBinding = true
	if err := needs(true).checkResumeLocalBinding(context.Background(), "ab", recorded); err != nil {
		t.Errorf("a lock that records the opt-in: %v", err)
	}
	if err := (&Runner{}).checkResumeLocalBinding(context.Background(), "ab", old); err != nil {
		t.Errorf("no check wired: %v", err)
	}
}
