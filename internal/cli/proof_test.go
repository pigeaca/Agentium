package cli

import (
	"context"
	"encoding/json"
	"errors"
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
// an experiment made before the proof and locked now takes up the rule; one locked before the proof keeps the exit
// codes, and its runs pass as they did, with nothing about the proof in them or its report.
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

	// An experiment made before the proof and not locked yet takes up the rule when it locks: its stored design changes
	// (so an older Agentium refuses it), its task is validated again first, and its runs need the proof.
	asValidatedBeforeTheProof(t, f, "gov")
	expect(t, f.run(ctx, "experiment", "new", "before", "--b", "lean", "--task", "gov", "--budget", "10"), ExitOK)
	storeAsBefore(t, f, "before", func(d *experiment.Design) { d.PassRule = ""; d.Version = d.WantVersion() })
	if d := storedDesign(t, f, "before"); d.PassRule != "" || d.Version == experiment.DesignVersionProof {
		t.Fatalf("not stored as before: %+v", d)
	}
	expect(t, f.run(ctx, "experiment", "plan", "before"), ExitOK,
		"made before Agentium proved that hidden Go tests ran: the experiment takes up that proof when it locks (design version 6)",
		"1 task(s) validated before the proof that their hidden tests ran are validated again with it when the experiment runs, before it locks (time, no money): gov")
	if d := storedDesign(t, f, "before"); d.PassRule != "" {
		t.Fatalf("the preview stored the rule: %+v", d)
	}
	expect(t, f.run(ctx, "experiment", "run", "before"), ExitOK,
		"Experiment before was made before Agentium proved that hidden Go tests ran: it grades by that proof from now on (design version 6)",
		"Validating 1 task(s) again with the proof that their hidden tests ran")
	if d := storedDesign(t, f, "before"); d.PassRule != task.PassGoTests || d.Version != experiment.DesignVersionProof {
		t.Errorf("the stored design: rule %q, version %d", d.PassRule, d.Version)
	}
	if err := json.Unmarshal(storedLock(t, f, "before"), &lock); err != nil || lock.PassRule() != task.PassGoTests {
		t.Fatalf("the lock's rule %q, %v", lock.PassRule(), err)
	}
	for _, r := range records(t, experimentRuns(t, f, "before")) {
		if r.Passed == nil || *r.Passed || !hasNote(r, task.NoteHiddenTestsNotRun) {
			t.Errorf("run %s: passed %v, notes %v", r.ID, r.Passed, r.Notes)
		}
	}

	// A locked experiment keeps its rule: one whose design and lock are as they were before the proof resumes by the exit
	// codes, the TestMain bypass passes as it did, and its stored design is left as it is.
	legacyDesign, legacyLock := asBeforeTheProof(t, storedExperiment(t, f, "before").Design, storedLock(t, f, "before"))
	saveLocked(t, f, "legacy", legacyDesign, legacyLock)
	expect(t, f.run(ctx, "experiment", "run", "legacy"), ExitOK, "Resuming experiment legacy")
	if got := storedExperiment(t, f, "legacy").Design; string(got) != string(legacyDesign) {
		t.Errorf("a locked experiment's design changed:\n%s\nwas\n%s", got, legacyDesign)
	}
	old := records(t, experimentRuns(t, f, "legacy"))
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
	for _, args := range [][]string{{"experiment", "show", "legacy"}, {"experiment", "report", "legacy"}, {"experiment", "report", "legacy", "--markdown"}} {
		if out := f.run(ctx, args...); out.code != ExitOK || strings.Contains(out.stdout, "Passes:") || strings.Contains(out.stdout, "did not run") {
			t.Errorf("%v (%d):\n%s", args, out.code, out.stdout)
		}
	}
}

// asBeforeTheProof is a design and its lock as an Agentium before the proof stored them: no rule, the design's
// version as it was.
func asBeforeTheProof(t *testing.T, design, lock []byte) ([]byte, []byte) {
	t.Helper()
	var d experiment.Design
	if err := json.Unmarshal(design, &d); err != nil {
		t.Fatal(err)
	}
	d.PassRule = ""
	d.Version = d.WantVersion()
	oldDesign, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var l map[string]any
	if err := json.Unmarshal(lock, &l); err != nil {
		t.Fatal(err)
	}
	delete(l, "pass_rule")
	var inLock map[string]any
	if err := json.Unmarshal(oldDesign, &inLock); err != nil {
		t.Fatal(err)
	}
	l["design"] = inLock
	oldLock, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(oldDesign)+string(oldLock), "pass_rule") {
		t.Fatal("the rule is still there")
	}
	return oldDesign, oldLock
}

// saveLocked stores an experiment with this design and lock, and no runs.
func saveLocked(t *testing.T, f runFixture, name string, design, lock []byte) {
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
	e, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: projects[0].ID, Name: name, Template: experiment.TemplateContextAB, Design: design,
		CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.LockExperiment(ctx, e.ID, lock); err != nil {
		t.Fatal(err)
	}
	if err := db.AmendDesign(ctx, e.ID, []byte(`{}`)); !errors.Is(err, store.ErrLocked) {
		t.Errorf("a locked experiment's design was amended: %v", err)
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
