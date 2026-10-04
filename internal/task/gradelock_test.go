package task

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

// A sandboxed validation holds each grade folder's lock (GradeLock) while it grades there, from before the folder
// exists, so cleanup never takes a grade in progress for one a stopped validation left; the lock file goes after.
func TestValidationHoldsTheGradeLock(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	hidden, reference, err := Split(ctx, f.base, f.solution, "--git-dir", f.bare)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSandbox{}
	held := 0
	v, _ := validator(t, f.bare)
	v.Grader = GraderSandbox
	v.Checkout = func(context.Context, string, string, []string, string) (CheckoutCommands, error) {
		return CheckoutCommands{Isolated: func(ctx context.Context, dir, root string, keep bool, commands []string, timeout time.Duration, log io.Writer) ([]Command, bool, *SandboxGrade, error) {
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the grade folder %s exists before its grade: %v", root, err)
			}
			lock, err := os.Open(GradeLock(root))
			if err != nil {
				t.Errorf("no lock beside %s: %v", root, err)
			} else {
				if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); errors.Is(err, syscall.EWOULDBLOCK) {
					held++
				}
				lock.Close()
			}
			return fake.run(ctx, dir, root, keep, commands, timeout, log)
		}}, nil
	}
	spec := Spec{Base: f.base, Solution: f.solution, HiddenTests: hidden, Reference: reference, Verify: []string{"sh run_tests.sh"}}
	if result, err := v.Validate(ctx, spec, []Arm{{Name: "base"}}); err != nil || result.Status != StatusValid {
		t.Fatalf("%s, %v", result.Summary(), err)
	}
	if len(fake.roots) != 2 || held != 2 {
		t.Errorf("%d grade(s), the lock held in %d", len(fake.roots), held)
	}
	for _, root := range fake.roots {
		if _, err := os.Lstat(GradeLock(root)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the lock of %s is left behind: %v", root, err)
		}
	}
}
