package experiment

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

// A task in a monorepo module is locked with its module, and its runs grade there: the lock, not the project's setting
// or the task as it is now, says where. A root task's lock has no module at all, so its digest is as before modules.
func TestLockedTasksKeepTheirModule(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	proj, err := db.SaveProject(ctx, "/repo", "repo", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string]store.Task{}
	for name, module := range map[string]string{"billing": "services/billing", "root": ""} {
		task, err := db.SaveTask(ctx, store.Task{ProjectID: proj.ID, Name: name, Instruction: "Do it.", Source: "manual", BaseCommit: "b",
			SolutionCommit: "s", HiddenTests: []string{"t_test.go"}, Reference: []string{"x.go"}, Verify: []string{"go test ./..."}, Module: module})
		if err != nil {
			t.Fatal(err)
		}
		saved[name] = task
	}
	d := validDesign()
	d.Template, d.Arms = TemplateAA, []Arm{{Name: "A", Context: BaseContext}, {Name: "B", Context: BaseContext}}
	d.Tasks = []string{"billing", "root"}
	if err := db.SaveCalibration(ctx, store.Calibration{ProjectID: proj.ID, Arm: BaseContext, RunID: "cal",
		Result: []byte(`{"requested_model":"` + d.Model + `"}`), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := Runner{Project: Project{DB: db, ID: proj.ID}, Now: time.Now}
	l, err := r.buildLock(ctx, d, "claude", "2.1.281")
	if err != nil {
		t.Fatal(err)
	}
	billing, ok := l.Task("billing")
	root, ok2 := l.Task("root")
	if !ok || !ok2 || billing.Module != "services/billing" || billing.Spec().Module != "services/billing" || root.Module != "" {
		t.Fatalf("locked tasks %+v", l.Tasks)
	}
	if encoded, _ := json.Marshal(root); strings.Contains(string(encoded), `"module"`) {
		t.Errorf("a root task's lock names a module: %s", encoded)
	}
	if billing.Digest == NewLockedTask("billing", "Do it.", root.Spec()).Digest {
		t.Error("the module is not part of the task's digest")
	}

	// A run of the locked task gets its module, and is linked to the stored task the lock ran.
	first := l.Schedule[0]
	for _, s := range l.Schedule {
		if s.Task == "billing" {
			first = s
			break
		}
	}
	exp, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: proj.ID, Name: "e", Template: TemplateContextAB, Design: []byte(`{}`), CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{r: r, stored: exp, lock: l, tries: map[int]int{}, storedTries: map[int]int{}, seenSubagents: map[string]map[string][]string{}}
	passed := true
	var got run.Spec
	var meta RunMeta
	x.r.ExecuteRun = func(_ context.Context, _ run.Env, m RunMeta, s run.Spec) (run.Record, error) {
		got, meta = s, m
		return run.Record{Outcome: claude.OutcomeOK, Passed: &passed}, nil
	}
	if _, err := x.slot(ctx, first, 1, nil); err != nil {
		t.Fatal(err)
	}
	if got.Task.Module != "services/billing" || meta.TaskID != saved["billing"].ID {
		t.Errorf("the run's module %q, task %d: want services/billing, task %d", got.Task.Module, meta.TaskID, saved["billing"].ID)
	}
}
