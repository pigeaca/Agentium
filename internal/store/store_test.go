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
