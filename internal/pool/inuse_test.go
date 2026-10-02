package pool

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// store.TasksInUse reads the task names out of experiment locks as internal/experiment writes them: this pins that
// contract from both sides, so a renamed JSON key in the lock fails here rather than letting re-validations through.
func TestTasksInUseReadsExperimentLocks(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	p, err := db.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	lock := experiment.Lock{Tasks: []experiment.LockedTask{
		experiment.NewLockedTask("fix-parser", "Fix it.", task.Spec{Base: "b", Verify: []string{"go test ./..."}}),
		experiment.NewLockedTask("add-flag", "Add it.", task.Spec{Base: "b", Verify: []string{"go test ./..."}}),
	}}
	raw, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	e, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: p.ID, Name: "lean", Template: "context-ab", Design: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.LockExperiment(ctx, e.ID, raw); err != nil {
		t.Fatal(err)
	}
	users, err := db.TasksInUse(ctx, p.ID)
	if err != nil || !slices.Equal(users["fix-parser"], []string{"lean"}) || !slices.Equal(users["add-flag"], []string{"lean"}) || len(users) != 2 {
		t.Errorf("TasksInUse = %v, %v", users, err)
	}
}
