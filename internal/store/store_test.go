package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "missing-dir", "agentium.db"))
	if err == nil {
		t.Fatal("expected an error for a database in a missing folder")
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
