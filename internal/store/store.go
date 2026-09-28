// Package store keeps Agentium's state in SQLite. Migrations are embedded and applied in order when the store opens.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3" // also registers the "sqlite3" driver (cgo)
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a looked-up record does not exist.
var ErrNotFound = errors.New("not found")

// Store is safe for concurrent use. SQLite serializes writers; the busy timeout absorbs short contention.
type Store struct {
	db *sql.DB
}

// Project is a registered repository.
type Project struct {
	ID        int64
	Root      string
	Name      string
	Discovery []byte // JSON from the last `agentium init`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// openRetry bounds how long Open waits for other processes opening the same database. Switching a new database to WAL
// fails at once with "database is locked" (the busy timeout does not apply), so Open retries it.
const openRetry = 10 * time.Second

// Open opens the database at file (creating it if needed) and applies pending migrations. Several processes may open the
// same database at once.
func Open(ctx context.Context, file string) (*Store, error) {
	file, err := filepath.Abs(file) // a relative path would become a URI authority
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", file, err)
	}
	deadline := time.Now().Add(openRetry)
	for wait := 10 * time.Millisecond; ; wait = min(2*wait, 250*time.Millisecond) {
		s, err := openOnce(ctx, file)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return s, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("open database %s: %w", file, ctx.Err())
		case <-time.After(wait):
		}
	}
}

func isBusy(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked)
}

func openOnce(ctx context.Context, file string) (*Store, error) {
	// _txlock=immediate: transactions take the write lock at BEGIN, so two writers wait (busy timeout) instead of one
	// failing when it upgrades a read lock.
	dsn := (&url.URL{Scheme: "file", Path: file, RawQuery: "_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL&_txlock=immediate"}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", file, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", file, err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies each embedded migrations/NNNN_name.sql not yet recorded, one transaction per file, in order. Each
// transaction re-checks the record under the write lock, so concurrent processes apply a migration once.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("prepare migrations: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	latest := 0
	versions := make([]int, len(names))
	for i, name := range names { // fs.Glob returns lexical order, so zero-padded versions apply in sequence
		if versions[i], err = strconv.Atoi(strings.SplitN(path.Base(name), "_", 2)[0]); err != nil {
			return fmt.Errorf("migration %s: version prefix: %w", name, err)
		}
		latest = max(latest, versions[i])
	}
	var newest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&newest); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if newest.Valid && int(newest.Int64) > latest {
		return fmt.Errorf("the database has schema version %d, but this Agentium knows up to %d: upgrade Agentium", newest.Int64, latest)
	}
	for i, name := range names {
		version := versions[i]
		body, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if err := s.inTx(ctx, func(tx *sql.Tx) error {
			var applied int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied); err != nil || applied > 0 {
				return err
			}
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, formatTime(time.Now()))
			return err
		}); err != nil {
			return fmt.Errorf("migration %d: %w", version, err)
		}
	}
	return nil
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SaveProject registers root, or refreshes its name and discovery if it is already registered.
func (s *Store) SaveProject(ctx context.Context, root, name string, discovery []byte, now time.Time) (Project, error) {
	stamp := formatTime(now)
	var id int64
	var created string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO projects (root, name, discovery, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (root) DO UPDATE SET name = excluded.name, discovery = excluded.discovery, updated_at = excluded.updated_at
		RETURNING id, created_at`, root, name, string(discovery), stamp, stamp).Scan(&id, &created)
	if err != nil {
		return Project{}, fmt.Errorf("save project %s: %w", root, err)
	}
	createdAt, err := parseTime(created)
	if err != nil {
		return Project{}, fmt.Errorf("save project %s: %w", root, err)
	}
	return Project{ID: id, Root: root, Name: name, Discovery: discovery, CreatedAt: createdAt, UpdatedAt: now.UTC()}, nil
}

// ProjectByRoot returns the project registered at root, or ErrNotFound.
func (s *Store) ProjectByRoot(ctx context.Context, root string) (Project, error) {
	projects, err := s.queryProjects(ctx, `WHERE root = ?`, root)
	if err != nil {
		return Project{}, err
	}
	if len(projects) == 0 {
		return Project{}, fmt.Errorf("project %s: %w", root, ErrNotFound)
	}
	return projects[0], nil
}

// Projects lists registered projects by name.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	return s.queryProjects(ctx, `ORDER BY name, id`)
}

func (s *Store) queryProjects(ctx context.Context, clause string, args ...any) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, root, name, discovery, created_at, updated_at FROM projects `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		var p Project
		var discovery, created, updated string
		if err := rows.Scan(&p.ID, &p.Root, &p.Name, &discovery, &created, &updated); err != nil {
			return nil, fmt.Errorf("read project: %w", err)
		}
		p.Discovery = []byte(discovery)
		if p.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if p.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	return projects, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", value, err)
	}
	return t, nil
}

// Snapshot is a named version of a project's context.
type Snapshot struct {
	ID           int64
	ProjectID    int64
	Name         string
	Source       string
	SourceCommit string
	CommitID     string
	Manifest     []byte // JSON
	CreatedAt    time.Time
}

// ErrExists is returned when a name is already taken.
var ErrExists = errors.New("already exists")

// SaveSnapshot records a new snapshot; names are unique per project.
func (s *Store) SaveSnapshot(ctx context.Context, snap Snapshot) (Snapshot, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO snapshots (project_id, name, source, source_commit, commit_id, manifest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, snap.ProjectID, snap.Name, snap.Source, snap.SourceCommit, snap.CommitID,
		string(snap.Manifest), formatTime(snap.CreatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Snapshot{}, fmt.Errorf("snapshot %q: %w", snap.Name, ErrExists)
		}
		return Snapshot{}, fmt.Errorf("save snapshot %q: %w", snap.Name, err)
	}
	if snap.ID, err = result.LastInsertId(); err != nil {
		return Snapshot{}, fmt.Errorf("save snapshot %q: %w", snap.Name, err)
	}
	snap.CreatedAt = snap.CreatedAt.UTC()
	return snap, nil
}

// SnapshotByName returns a project's snapshot, or ErrNotFound.
func (s *Store) SnapshotByName(ctx context.Context, projectID int64, name string) (Snapshot, error) {
	snaps, err := s.querySnapshots(ctx, `WHERE project_id = ? AND name = ?`, projectID, name)
	if err != nil {
		return Snapshot{}, err
	}
	if len(snaps) == 0 {
		return Snapshot{}, fmt.Errorf("snapshot %q: %w", name, ErrNotFound)
	}
	return snaps[0], nil
}

// DeleteSnapshot removes a project's snapshot record, or returns ErrNotFound.
func (s *Store) DeleteSnapshot(ctx context.Context, projectID int64, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE project_id = ? AND name = ?`, projectID, name)
	if err != nil {
		return fmt.Errorf("delete snapshot %q: %w", name, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("snapshot %q: %w", name, errors.Join(ErrNotFound, err))
	}
	return nil
}

// Snapshots lists a project's snapshots, oldest first.
func (s *Store) Snapshots(ctx context.Context, projectID int64) ([]Snapshot, error) {
	return s.querySnapshots(ctx, `WHERE project_id = ? ORDER BY created_at, id`, projectID)
}

func (s *Store) querySnapshots(ctx context.Context, clause string, args ...any) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, name, source, source_commit, commit_id, manifest, created_at FROM snapshots `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
	}
	defer rows.Close()
	var snaps []Snapshot
	for rows.Next() {
		var snap Snapshot
		var manifest, created string
		if err := rows.Scan(&snap.ID, &snap.ProjectID, &snap.Name, &snap.Source, &snap.SourceCommit, &snap.CommitID, &manifest, &created); err != nil {
			return nil, fmt.Errorf("read snapshot: %w", err)
		}
		snap.Manifest = []byte(manifest)
		if snap.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
	}
	return snaps, nil
}
