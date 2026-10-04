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

// The verification runs the agent's code in a writable copy: a test that replaces the module's folder with a link to
// the decoy (whose run_tests.sh always passes) before the next command gets a failed grade with a note, neither a pass
// nor infrastructure (which would be tried again, at a cost).
func TestOnceSandboxFailsAModuleSwappedBetweenCommands(t *testing.T) {
	needSandbox(t)
	f := newModuleOnce(t, "svc", "decoy", "printf 'new\\n' > svc/value.txt; mkdir -p svc/tests; "+
		"printf 'cd .. && rm -rf svc && ln -s decoy svc\\n' > svc/tests/zz_swap.sh")
	f.env.Grader = task.GraderSandbox
	f.spec.Task.Verify = []string{"sh run_tests.sh", "sh run_tests.sh"}
	rec, err := Once(context.Background(), f.env, f.spec)
	skipLogBlind(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != claude.OutcomeOK || rec.Passed == nil || *rec.Passed {
		t.Fatalf("outcome %s, passed %v, notes %v: want a graded fail", rec.Outcome, rec.Passed, rec.Notes)
	}
	if !strings.Contains(strings.Join(rec.Notes, "\n"), "the module's folder left the agent's tree during the grade") {
		t.Errorf("no note says why: %v", rec.Notes)
	}
	if len(rec.Verify) != 1 || rec.Verify[0].ExitCode != 0 {
		t.Errorf("commands %+v: want only the first, which passed", rec.Verify)
	}
}
