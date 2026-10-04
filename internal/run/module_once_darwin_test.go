package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/task"
)

// In the grading sandbox the verification also runs in the module's folder (the decoy's tests and the root's would
// give other results), and a module the agent deleted or linked is a failed grade, not infrastructure.
func TestOnceGradesAModuleInTheSandbox(t *testing.T) {
	needSandbox(t)
	f := newModuleOnce(t, "svc", "decoy", "printf 'new\\n' > svc/value.txt")
	f.env.Grader = task.GraderSandbox
	rec, err := Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || !*rec.Passed {
		t.Fatalf("solved: %s, passed %v, notes %v", rec.Outcome, rec.Passed, rec.Notes)
	}
	needDenials(t, rec.Sandbox)
	if log, _ := os.ReadFile(filepath.Join(rec.RecordsDir, "verify.log")); !strings.Contains(string(log), "$ sh run_tests.sh") {
		t.Errorf("verify.log:\n%s", log)
	}

	for name, agent := range map[string]string{"deleted": "rm -rf svc", "linked": "rm -rf svc && ln -s decoy svc"} {
		f := newModuleOnce(t, "svc", "decoy", agent)
		f.env.Grader = task.GraderSandbox
		rec, err := Once(context.Background(), f.env, f.spec)
		skipLogBlind(t, err)
		if err != nil || rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed {
			t.Errorf("%s: %v, outcome %s, passed %v, notes %v", name, err, rec.Outcome, rec.Passed, rec.Notes)
		}
	}
}
