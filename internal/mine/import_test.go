package mine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

// An interrupt during an import keeps what was imported, reports Interrupted, and does not count the candidate it
// stopped as a failure: that one is neither a task nor a reason to blame the commit.
func TestImportInterruptKeepsTasksAndHidesTheStoppedCandidate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.SaveProject(ctx, "/repo", "repo", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	candidates := []Candidate{{Hash: "aaaa111"}, {Hash: "bbbb222"}, {Hash: "cccc333"}}
	calls := 0
	imp := Importer{DB: db, ProjectID: project.ID, Names: map[string]bool{},
		Commit: func(ctx context.Context, c Candidate, task store.Task) (store.Task, error) {
			calls++
			if calls == 2 { // the user interrupts while the second candidate is being read
				cancel()
				return task, ctx.Err()
			}
			task.Name, task.SolutionCommit, task.Source = "task-"+c.Hash, c.Hash, "test"
			return task, nil
		},
		Complete: func(_ context.Context, task store.Task) (store.Task, error) { return task, nil }}
	var told []string
	got := Import(ctx, ImportInput{Importer: imp, Candidates: candidates, Limit: 3,
		NewTask:  func() store.Task { return store.Task{ProjectID: project.ID, CreatedAt: time.Now()} },
		Progress: func(_ int, c Candidate) { told = append(told, c.Hash) }})
	if !got.Interrupted {
		t.Error("Interrupted = false")
	}
	if len(got.Tasks) != 1 || got.Tasks[0].Name != "task-aaaa111" {
		t.Errorf("Tasks = %+v, want the first candidate's task, kept", got.Tasks)
	}
	if len(got.Failed) != 0 {
		t.Errorf("Failed = %+v: the stopped candidate is not a failure", got.Failed)
	}
	if got.Tried != 2 || len(told) != 2 || calls != 2 {
		t.Errorf("tried %d, told %v, commit calls %d: the import must stop at the interrupt", got.Tried, told, calls)
	}
	saved, err := db.Tasks(context.Background(), project.ID)
	if err != nil || len(saved) != 1 {
		t.Errorf("stored tasks = %d, %v", len(saved), err)
	}
}
