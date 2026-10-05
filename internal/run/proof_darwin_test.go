package run

import (
	"context"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/sandbox"
	"github.com/pigeaca/agentium/internal/task"
)

func init() { skipErr = skipLogBlind }

// Issue #160 in the grading sandbox: the proof runs in the same sandbox, profile and folder as the verification, and
// decides alike. What the sandbox denied is not this test's subject, so the unified log is not read (a stand-in reports
// no denials): the test runs wherever sandbox-exec applies a profile, log-blind accounts too.
func TestOnceNeedsTheProofInTheSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("starts sandboxed processes")
	}
	if err := sandbox.Applies(context.Background()); err != nil {
		t.Skipf("the grading sandbox: %v", err)
	}
	checkIssue160(t, func(t *testing.T, agent string) moduleOnce {
		f := newGoOnce(t, agent)
		f.env.Grader = task.GraderSandbox
		f.env.readDenials = func(context.Context, string, sandbox.Profile, time.Time, time.Duration, []int) ([]sandbox.Denial, error) {
			return nil, nil
		}
		return f
	})
}
