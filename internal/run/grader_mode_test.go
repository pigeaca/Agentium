package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/task"
)

// container-v1 is named but not graded in until the containers plan's step 4. Every place in this package that tells
// the grader modes apart refuses it as an unknown mode, with the unknown mode's error, and none reads "not host" as
// "sandbox": a table over the mode switches.
func TestContainerModeIsRefusedLikeAnUnknownMode(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{task.GraderContainer, "sandbox-v0"} {
		want := unknownGrader(mode).Error()
		if !strings.Contains(want, "grader "+mode+": this Agentium grades on the host or in sandbox-v1") {
			t.Fatalf("the unknown mode's error: %q", want)
		}
		if err := SandboxUsable(ctx, mode); err == nil || err.Error() != want {
			t.Errorf("SandboxUsable(%s): %v", mode, err)
		}
		if err := sandboxApplies(ctx, mode); err == nil || err.Error() != want {
			t.Errorf("sandboxApplies(%s): %v", mode, err)
		}
		if (Env{Grader: mode}).gradesInSandbox() {
			t.Errorf("Env.gradesInSandbox(%s)", mode)
		}
		if _, err := (Env{Grader: mode}).verifyIsolated(ctx, Spec{}, "", &Record{}, nil); err == nil || err.Error() != want {
			t.Errorf("verifyIsolated(%s): %v", mode, err)
		}
		// Validation's commands: refused before anything is warmed or offered, never host (nil Isolated) or sandbox.
		cc, err := CheckoutCommands(ctx, CommandsEnv{Grader: mode}, "base", []string{"true"}, filepath.Join(t.TempDir(), "warm.log"))
		if err == nil || err.Error() != want || cc.Isolated != nil {
			t.Errorf("CheckoutCommands(%s): %v, isolated %v", mode, err, cc.Isolated != nil)
		}
	}
	// The modes this Agentium grades in are untouched.
	for mode, sandboxed := range map[string]bool{"": false, task.GraderHost: false, task.GraderSandbox: true} {
		if got := (Env{Grader: mode}).gradesInSandbox(); got != sandboxed {
			t.Errorf("Env.gradesInSandbox(%q) = %v", mode, got)
		}
	}
	if err := SandboxUsable(ctx, task.GraderHost); err != nil {
		t.Errorf("SandboxUsable(host): %v", err)
	}
	if err := sandboxApplies(ctx, ""); err != nil {
		t.Errorf("sandboxApplies(empty): %v", err)
	}
}

// A run in container-v1 stops before it starts anything: no agent runs, nothing is graded, not on the host and not in
// the sandbox.
func TestOnceRefusesTheContainerMode(t *testing.T) {
	f := newModuleOnce(t, "svc", "decoy", "printf 'new\\n' > svc/value.txt")
	f.env.Grader = task.GraderContainer
	rec, err := Once(context.Background(), f.env, f.spec)
	if err == nil || !strings.Contains(err.Error(), "grader container-v1: this Agentium grades on the host or in sandbox-v1") {
		t.Fatalf("Once: %v", err)
	}
	if rec.Passed != nil || len(rec.Verify) != 0 {
		t.Errorf("graded: passed %v, %d command(s)", rec.Passed, len(rec.Verify))
	}
	if _, err := os.Stat(f.started); err == nil {
		t.Error("the agent started")
	}
}
