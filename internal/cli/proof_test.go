package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// The rule is the lock's (issue #160). On the experiment fixture, a Go task whose hidden test wants Value() == 1 and an
// agent that leaves Value at 0 but adds a TestMain that exits 0: an experiment made now records the rule in its design
// and lock, validates the task again first when it was validated before the proof, and its runs fail with the note;
// an experiment designed before the proof (stored as it was then) locks without the rule, and its runs pass by the
// exit codes, as they did, with nothing about the proof in them or its report.
func TestTheLockDecidesHowRunsPass(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	base := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "HEAD"))
	writeFile(t, f.repo, "go.mod", "module example.com/fixture\n\ngo 1.22\n")
	writeFile(t, f.repo, "gov.go", "package fixture\n\nfunc Value() int { return 0 }\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "a Go package")
	goBase := strings.TrimSpace(gitIn(t, f.repo, "rev-parse", "HEAD"))
	writeFile(t, f.repo, "gov_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestHidden(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal(Value())\n\t}\n}\n")
	writeFile(t, f.repo, "gov.go", "package fixture\n\nfunc Value() int { return 1 }\n")
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "Value returns 1")
	gitIn(t, f.repo, "checkout", "-q", base) // the fixture's contexts and calibrations are at its own head
	expect(t, f.run(ctx, "task", "add", "gov", "--base", goBase, "--solution", "main", "--instruction", "Make Value return 1.",
		"--verify", "go test -count=1 ./...", "--accept-gaps"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "gov", "--snapshot", "lean"), ExitOK, "Result: valid")
	asValidatedBeforeTheProof(t, f, "gov")
	writeFile(t, ctrl, "agent-script", `printf 'package fixture\n\nimport (\n\t"os"\n\t"testing"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(0) }\n' > bypass_test.go`+"\n")

	expect(t, f.run(ctx, "experiment", "new", "proved", "--b", "lean", "--task", "gov", "--budget", "10"), ExitOK)
	var d experiment.Design
	if err := json.Unmarshal(storedExperiment(t, f, "proved").Design, &d); err != nil || d.PassRule != task.PassGoTests || d.Version != experiment.DesignVersionProof {
		t.Fatalf("the design: rule %q, version %d, %v", d.PassRule, d.Version, err)
	}
	expect(t, f.run(ctx, "experiment", "plan", "proved"), ExitOK,
		"1 task(s) validated before the proof that their hidden tests ran are validated again with it when the experiment runs, before it locks (time, no money): gov")
	expect(t, f.run(ctx, "experiment", "run", "proved"), ExitOK, "Validating 1 task(s) again with the proof that their hidden tests ran")
	if v := storedValidation(t, f, "gov"); v.PassRule != task.PassGoTests || v.Status != task.StatusValid {
		t.Errorf("the task's validation again: rule %q, %s", v.PassRule, v.Summary())
	}
	var lock experiment.Lock
	if err := json.Unmarshal(storedLock(t, f, "proved"), &lock); err != nil || lock.PassRule() != task.PassGoTests {
		t.Fatalf("the lock's rule %q, %v", lock.PassRule(), err)
	}
	proved := records(t, experimentRuns(t, f, "proved"))
	if len(proved) == 0 {
		t.Fatal("no runs")
	}
	for _, r := range proved {
		if r.Passed == nil || *r.Passed || r.Proof == nil || r.Proof.Proven() || !hasNote(r, task.NoteHiddenTestsNotRun) {
			t.Errorf("run %s: passed %v, proof %+v, notes %v", r.ID, r.Passed, r.Proof, r.Notes)
		}
	}
	expect(t, f.run(ctx, "experiment", "show", "proved"), ExitOK, "Passes: the verification's exit codes, and a pass of each hidden Go test")

	expect(t, f.run(ctx, "experiment", "new", "old", "--b", "lean", "--task", "gov", "--budget", "10"), ExitOK)
	storeAsBefore(t, f, "old", func(d *experiment.Design) { d.PassRule, d.Version = "", 0; d.Version = d.WantVersion() })
	expect(t, f.run(ctx, "experiment", "run", "old"), ExitOK)
	encoded := storedLock(t, f, "old")
	if strings.Contains(string(encoded), "pass_rule") {
		t.Errorf("an old design's lock names a rule:\n%s", encoded)
	}
	old := records(t, experimentRuns(t, f, "old"))
	if len(old) != len(proved) {
		t.Fatalf("%d runs, want %d", len(old), len(proved))
	}
	for _, r := range old {
		if r.Passed == nil || !*r.Passed || r.Proof != nil || hasNote(r, task.NoteHiddenTestsNotRun) {
			t.Errorf("run %s by the exit codes: passed %v, proof %+v, notes %v", r.ID, r.Passed, r.Proof, r.Notes)
		}
		if _, err := os.Stat(filepath.Join(f.data, "records", r.ID, task.ProofEvents)); err == nil {
			t.Errorf("run %s by the exit codes left the proof's events", r.ID)
		}
	}
	for _, args := range [][]string{{"experiment", "show", "old"}, {"experiment", "report", "old"}, {"experiment", "report", "old", "--markdown"}} {
		if out := f.run(ctx, args...); out.code != ExitOK || strings.Contains(out.stdout, "Passes:") || strings.Contains(out.stdout, "did not run") {
			t.Errorf("%v (%d):\n%s", args, out.code, out.stdout)
		}
	}
}

// asValidatedBeforeTheProof rewrites the task's stored validation as one made before the proof: no rule, no proof.
func asValidatedBeforeTheProof(t *testing.T, f runFixture, name string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	stored, err := db.TaskByName(ctx, projects[0].ID, name)
	if err != nil {
		t.Fatal(err)
	}
	v := task.ValidationOf(stored)
	v.PassRule = ""
	for i := range v.Stages {
		v.Stages[i].Proof = nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.SetTaskValidation(ctx, stored.ID, stored.Verify, stored.Setup, encoded, time.Now()); err != nil || !ok {
		t.Fatalf("store the old validation: %v, %v", ok, err)
	}
}

// storedValidation is the task's stored validation.
func storedValidation(t *testing.T, f runFixture, name string) task.Validation {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	stored, err := db.TaskByName(ctx, projects[0].ID, name)
	if err != nil {
		t.Fatal(err)
	}
	return task.ValidationOf(stored)
}

func hasNote(r run.Record, prefix string) bool {
	for _, n := range r.Notes {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}
