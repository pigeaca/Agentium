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
	var migrations int
	if err := second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 1 {
		t.Errorf("recorded migrations = %d, want 1", migrations)
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
