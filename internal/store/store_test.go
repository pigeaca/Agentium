package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, file string) *Store {
	t.Helper()
	s, err := Open(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationsApplyOnceAndSurviveReopen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "space in path", "agentium.db")
	if err := mkdirAll(filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	first := open(t, file)
	ctx := context.Background()
	if _, err := first.SaveProject(ctx, "/repo", "repo", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second := open(t, file) // reopening must not re-run migrations or lose data
	var applied int
	if err := second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if applied != len(files) {
		t.Errorf("recorded migrations = %d, want %d", applied, len(files))
	}
	if _, err := second.ProjectByRoot(ctx, "/repo"); err != nil {
		t.Errorf("project lost after reopen: %v", err)
	}
}

func TestSaveProjectUpsertsByRoot(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	created := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	first, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{"v":1}`), created)
	if err != nil {
		t.Fatal(err)
	}
	later := created.Add(time.Hour)
	second, err := s.SaveProject(ctx, "/work/app", "app2", []byte(`{"v":2}`), later)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || !second.CreatedAt.Equal(created) {
		t.Errorf("re-registering changed identity: first %+v, second %+v", first, second)
	}
	got, err := s.ProjectByRoot(ctx, "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "app2" || string(got.Discovery) != `{"v":2}` || !got.UpdatedAt.Equal(later) || !got.CreatedAt.Equal(created) {
		t.Errorf("stored project = %+v", got)
	}
	if _, err := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), later); err != nil {
		t.Fatal(err)
	}
	all, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name != "app2" || all[1].Name != "other" {
		t.Errorf("projects = %+v", all)
	}
}

func TestProjectByRootNotFound(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	if _, err := s.ProjectByRoot(context.Background(), "/nowhere"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestOpenFailsClearly(t *testing.T) {
	file := filepath.Join(t.TempDir(), "missing-dir", "agentium.db")
	if _, err := Open(context.Background(), file); err == nil || !strings.Contains(err.Error(), "open database "+file) {
		t.Errorf("err = %v, want one naming %s", err, file)
	}
}

func TestOpenRefusesANewerSchemaAndAcceptsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := open(t, "agentium.db") // relative: must not become a URI authority
	ctx := context.Background()
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", foreignKeys, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (9999, 'later')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, filepath.Join(dir, "agentium.db")); err == nil || !strings.Contains(err.Error(), "schema version 9999") {
		t.Errorf("newer schema: err = %v", err)
	}
}

func mkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func TestSnapshotsAreUniquePerProjectAndCascade(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot{ProjectID: app.ID, Name: "baseline", Source: "HEAD", SourceCommit: "abc", CommitID: "def",
		Manifest: []byte(`{"files":[]}`), CreatedAt: now}
	saved, err := s.SaveSnapshot(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSnapshot(ctx, snap); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate name: err = %v, want ErrExists", err)
	}
	snap.ProjectID = other.ID // names are per project
	if _, err := s.SaveSnapshot(ctx, snap); err != nil {
		t.Errorf("same name in another project: %v", err)
	}
	later := Snapshot{ProjectID: app.ID, Name: "minimal", Source: "working tree", SourceCommit: "abc", CommitID: "fed",
		Manifest: []byte(`{}`), CreatedAt: now.Add(time.Minute)}
	if _, err := s.SaveSnapshot(ctx, later); err != nil {
		t.Fatal(err)
	}
	got, err := s.SnapshotByName(ctx, app.ID, "baseline")
	if err != nil || got.ID != saved.ID || got.CommitID != "def" || string(got.Manifest) != `{"files":[]}` || !got.CreatedAt.Equal(now) {
		t.Errorf("SnapshotByName = %+v, %v", got, err)
	}
	if _, err := s.SnapshotByName(ctx, app.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing snapshot: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSnapshot(ctx, other.ID, "baseline"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := s.DeleteSnapshot(ctx, other.ID, "baseline"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete again: err = %v, want ErrNotFound", err)
	}
	list, err := s.Snapshots(ctx, app.ID)
	if err != nil || len(list) != 2 || list[0].Name != "baseline" || list[1].Name != "minimal" {
		t.Errorf("Snapshots = %+v, %v", list, err)
	}
	// Foreign keys are on: deleting a project deletes its snapshots, and a snapshot needs a project.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Snapshots(ctx, app.ID); err != nil || len(list) != 0 {
		t.Errorf("snapshots after project delete = %+v, %v", list, err)
	}
	if _, err := s.SaveSnapshot(ctx, Snapshot{ProjectID: 999, Name: "orphan", Manifest: []byte(`{}`), CreatedAt: now}); err == nil {
		t.Error("a snapshot without a project must be rejected")
	}
}

// Separate *sql.DB handles behave like separate processes: each has its own connections and locks.
func TestConcurrentOpensOfAFreshDatabase(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agentium.db")
	const openers = 8
	errs := make(chan error, openers)
	for range openers {
		go func() {
			s, err := Open(context.Background(), file)
			if err == nil {
				_, err = s.SaveProject(context.Background(), "/work/app", "app", []byte(`{}`), time.Now())
				s.Close()
			}
			errs <- err
		}()
	}
	for range openers {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestTasksRoundTripUpdateAndCascade(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ProjectID: app.ID, Name: "fix-parser", Instruction: "Fix the parser.", Source: "commit abc",
		BaseCommit: "base", SolutionCommit: "sol", HiddenTests: []string{"p/parser_test.go"}, Reference: []string{"p/parser.go"},
		Verify: []string{"go test ./..."}, Setup: []string{"make assets"}, NeedsReview: true, CreatedAt: now}
	if _, err := s.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTask(ctx, task); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate: err = %v, want ErrExists", err)
	}
	manual := Task{ProjectID: app.ID, Name: "manual", Instruction: "Do it.", Source: "manual", BaseCommit: "base",
		Verify: []string{"make test"}, CreatedAt: now.Add(time.Minute)}
	if _, err := s.SaveTask(ctx, manual); err != nil {
		t.Fatal(err)
	}
	got, err := s.TaskByName(ctx, app.ID, "fix-parser")
	if err != nil || got.SolutionCommit != "sol" || got.HiddenTests[0] != "p/parser_test.go" || got.Reference[0] != "p/parser.go" || got.Setup[0] != "make assets" ||
		!got.NeedsReview || got.Validation != nil || !got.CreatedAt.Equal(now) {
		t.Errorf("TaskByName = %+v, %v", got, err)
	}
	got.Instruction, got.NeedsReview, got.Validation = "Make the parser accept empty input.", false, []byte(`{"status":"valid"}`)
	got.Setup = nil
	if err := s.UpdateTask(ctx, got, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.TaskByName(ctx, app.ID, "fix-parser"); again.NeedsReview || string(again.Validation) != `{"status":"valid"}` || len(again.Setup) != 0 ||
		!again.UpdatedAt.Equal(now.Add(time.Hour)) || again.Instruction != "Make the parser accept empty input." {
		t.Errorf("after update: %+v", again)
	}
	list, err := s.Tasks(ctx, app.ID)
	if err != nil || len(list) != 2 || list[1].Name != "manual" || list[1].HiddenTests == nil || len(list[1].HiddenTests) != 0 {
		t.Errorf("Tasks = %+v, %v", list, err)
	}
	if err := s.DeleteTask(ctx, app.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, Task{ProjectID: app.ID, Name: "manual"}, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of a deleted task: err = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Tasks(ctx, app.ID); len(list) != 0 {
		t.Errorf("tasks survive their project: %+v", list)
	}
}
