// Package store keeps Agentium's state in SQLite. Migrations are embedded and applied in order when the store opens.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
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
	// AllowLocalBinding is the user's opt-in for the sandbox's local binding in agent runs (see claude.Invocation).
	AllowLocalBinding bool
	// Settings are the project's own defaults for mining, importing and validating tasks (SetSettings).
	Settings  Settings
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Settings are a project's defaults, set with `agentium init` and kept here, never in the repository. The zero value
// is "nothing set": every command then uses its built-in default, as before the settings existed. A command's own
// flag wins over a setting for that call only.
type Settings struct {
	Verify []string // the verification commands mined and imported tasks get; empty: the detected ones
	Setup  []string // the commands a fresh checkout of those tasks runs first; empty: none
	// RequireLock sets aside Python commits whose base pins no dependencies when mining (mine.Options.RequireLock).
	RequireLock   bool
	Jobs          int           // how many tasks to validate at once; 0: the built-in default
	VerifyTimeout time.Duration // the time limit of each setup or verification command; 0: the built-in default
	// Module is the monorepo folder the project measures, slash-separated and relative to the repository root (checked by
	// project.ValidateModule); "": the repository's root. Setup and verification commands run in it, and the build tools
	// are detected there.
	Module string
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
		s, err := openOnce(ctx, file, 0)
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

// openOnce opens the database once, applying the pending migrations below version below (all of them when it is 0:
// tests build an older schema with it).
func openOnce(ctx context.Context, file string, below int) (*Store, error) {
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
	if err := s.migrate(ctx, below); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ErrSchema is returned by OpenReadOnly when the database's schema is not the one this binary knows (older, newer, or
// not initialized); ErrSchemaNewer is also wrapped when it is newer. Other failures (busy, corrupt) are not ErrSchema.
var (
	ErrSchema      = errors.New("database schema differs from this Agentium's")
	ErrSchemaNewer = errors.New("database schema is newer than this Agentium's")
)

// FileState is what OpenReadOnly callers compare before and after reading, to detect a writer that started
// meanwhile: the database file's size and modification time, and whether its -wal file exists.
type FileState struct {
	Size   int64
	ModNs  int64
	HasWAL bool
}

// StateOf reads the FileState of the database at file; a missing file is the zero state.
func StateOf(file string) FileState {
	var st FileState
	if info, err := os.Stat(file); err == nil {
		st.Size, st.ModNs = info.Size(), info.ModTime().UnixNano()
	}
	_, err := os.Stat(file + "-wal")
	st.HasWAL = err == nil
	return st
}

// readOnlyBusy is how long OpenReadOnly waits on a lock before giving up: it is for callers (editor hooks) that must
// answer in well under a second.
const readOnlyBusy = 200 * time.Millisecond

// OpenReadOnly opens an existing database for reading only: it never creates the file, migrates, switches the journal
// mode or takes the write lock, waits at most readOnlyBusy for any lock and does not retry. A schema other than the
// one this binary knows gives ErrSchema, since the queries here would not be valid on it.
func OpenReadOnly(ctx context.Context, file string) (*Store, error) {
	file, err := filepath.Abs(file)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", file, err)
	}
	// A WAL database read normally gets -wal and -shm files created beside it, which a read-only caller must not leave
	// behind. While no process has the database open there is no -wal file and nothing can change under us, so read
	// it as immutable (no locks, no side files); with a -wal file, another process is active, and a normal read-only
	// connection finds the files in place.
	query := "mode=ro&_busy_timeout=" + strconv.Itoa(int(readOnlyBusy.Milliseconds()))
	if _, err := os.Stat(file + "-wal"); err != nil {
		query = "mode=ro&immutable=1"
	}
	dsn := (&url.URL{Scheme: "file", Path: file, RawQuery: query}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", file, err)
	}
	db.SetMaxOpenConns(1)
	latest, _, err := latestMigration()
	if err != nil {
		db.Close()
		return nil, err
	}
	var newest sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&newest); err != nil {
		db.Close()
		if strings.Contains(err.Error(), "no such table") {
			err = errors.Join(ErrSchema, err)
		}
		return nil, fmt.Errorf("open database %s: read schema version: %w", file, err)
	}
	if !newest.Valid || int(newest.Int64) != latest {
		db.Close()
		err := ErrSchema
		if newest.Int64 > int64(latest) {
			err = errors.Join(ErrSchema, ErrSchemaNewer)
		}
		return nil, fmt.Errorf("open database %s: schema version %d, this Agentium knows %d: %w", file, newest.Int64, latest, err)
	}
	return &Store{db: db}, nil
}

type migration struct {
	name    string
	version int
}

// latestMigration is the newest embedded migration's version, and every migration in order.
func latestMigration() (int, []migration, error) {
	return migrationsIn(migrations)
}

// migrationsIn lists the migrations/NNNN_name.sql files of fsys, in order, and the newest version. Two files with one
// version are an error: parallel branches that each took the next free number must renumber when the second merges,
// or one of the migrations would never apply on databases that recorded the other.
func migrationsIn(fsys fs.FS) (int, []migration, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return 0, nil, fmt.Errorf("list migrations: %w", err)
	}
	latest := 0
	all := make([]migration, len(names))
	seen := map[int]string{}
	for i, name := range names { // fs.Glob returns lexical order, so zero-padded versions apply in sequence
		version, err := strconv.Atoi(strings.SplitN(path.Base(name), "_", 2)[0])
		if err != nil {
			return 0, nil, fmt.Errorf("migration %s: version prefix: %w", name, err)
		}
		if other, ok := seen[version]; ok {
			return 0, nil, fmt.Errorf("migrations %s and %s share version %d: renumber one", other, name, version)
		}
		seen[version] = name
		all[i] = migration{name, version}
		latest = max(latest, version)
	}
	return latest, all, nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies each embedded migrations/NNNN_name.sql not yet recorded (below version below, unless it is 0), one
// transaction per file, in order. Each transaction re-checks the record under the write lock, so concurrent processes
// apply a migration once.
func (s *Store) migrate(ctx context.Context, below int) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("prepare migrations: %w", err)
	}
	latest, all, err := latestMigration()
	if err != nil {
		return err
	}
	var newest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&newest); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if newest.Valid && int(newest.Int64) > latest {
		return fmt.Errorf("the database has schema version %d, but this Agentium knows up to %d: upgrade Agentium", newest.Int64, latest)
	}
	for _, m := range all {
		name, version := m.name, m.version
		if below > 0 && version >= below {
			continue
		}
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

// SaveProject registers root, or refreshes its name and discovery if it is already registered; its settings and
// local-binding choice are kept.
func (s *Store) SaveProject(ctx context.Context, root, name string, discovery []byte, now time.Time) (Project, error) {
	stamp := formatTime(now)
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO projects (root, name, discovery, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (root) DO UPDATE SET name = excluded.name, discovery = excluded.discovery, updated_at = excluded.updated_at
		RETURNING id`, root, name, string(discovery), stamp, stamp).Scan(&id)
	if err != nil {
		return Project{}, fmt.Errorf("save project %s: %w", root, err)
	}
	projects, err := s.queryProjects(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Project{}, fmt.Errorf("save project %s: %w", root, err)
	}
	if len(projects) != 1 {
		return Project{}, fmt.Errorf("save project %s: %w", root, ErrNotFound)
	}
	return projects[0], nil
}

// SetSettings replaces the project's settings. Jobs and VerifyTimeout must not be negative; a VerifyTimeout is kept
// to the millisecond.
func (s *Store) SetSettings(ctx context.Context, projectID int64, set Settings) error {
	if set.Jobs < 0 || set.VerifyTimeout < 0 {
		return fmt.Errorf("save the project's settings: jobs %d and verify timeout %s must not be negative", set.Jobs, set.VerifyTimeout)
	}
	lists, err := encodeLists(set.Verify, set.Setup)
	if err != nil {
		return fmt.Errorf("save the project's settings: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE projects SET verify = ?, setup = ?, require_lock = ?, jobs = ?, verify_timeout_ms = ?, module = ? WHERE id = ?`,
		lists[0], lists[1], set.RequireLock, set.Jobs, set.VerifyTimeout.Milliseconds(), set.Module, projectID)
	if err != nil {
		return fmt.Errorf("save the project's settings: %w", err)
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("save the project's settings: project %d: %w", projectID, ErrNotFound)
	}
	return nil
}

// SetLocalBinding stores the project's opt-in for the sandbox's local binding.
func (s *Store) SetLocalBinding(ctx context.Context, projectID int64, allow bool) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE projects SET allow_local_binding = ? WHERE id = ?`, allow, projectID); err != nil {
		return fmt.Errorf("save the local binding setting: %w", err)
	}
	return nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, root, name, discovery, allow_local_binding, verify, setup, require_lock, jobs, verify_timeout_ms, module,
		created_at, updated_at FROM projects `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		var p Project
		var discovery, verify, setup, created, updated string
		var timeoutMS int64
		if err := rows.Scan(&p.ID, &p.Root, &p.Name, &discovery, &p.AllowLocalBinding, &verify, &setup, &p.Settings.RequireLock, &p.Settings.Jobs,
			&timeoutMS, &p.Settings.Module, &created, &updated); err != nil {
			return nil, fmt.Errorf("read project: %w", err)
		}
		p.Discovery = []byte(discovery)
		if err := json.Unmarshal([]byte(verify), &p.Settings.Verify); err != nil {
			return nil, fmt.Errorf("project %s: verify setting: %w", p.Name, err)
		}
		if err := json.Unmarshal([]byte(setup), &p.Settings.Setup); err != nil {
			return nil, fmt.Errorf("project %s: setup setting: %w", p.Name, err)
		}
		p.Settings.VerifyTimeout = time.Duration(timeoutMS) * time.Millisecond
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

// Task is a coding task (see migrations/0003_tasks.sql).
type Task struct {
	ID             int64
	ProjectID      int64
	Name           string
	Instruction    string
	Source         string
	BaseCommit     string
	SolutionCommit string
	HiddenTests    []string
	Reference      []string
	Setup          []string // run in a fresh checkout before anything else
	Verify         []string
	NeedsReview    bool
	Grading        string // "tests" or "judge" (task.GradingTests, task.GradingJudge); SaveTask stores "" as "tests"
	Validation     []byte // JSON; nil until validated
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// RetiredAt is when the task pool retired the task (zero while it is active), and RetiredReason why. A retired task
	// keeps its row, runs and place in locked experiments; only new experiments leave it out. SaveTask and UpdateTask
	// never write them: RetireTask and RestoreTask do.
	RetiredAt     time.Time
	RetiredReason string
}

// Retired reports whether the task is retired.
func (t Task) Retired() bool {
	return !t.RetiredAt.IsZero()
}

// SaveTask records a new task; names are unique per project (ErrExists).
func (s *Store) SaveTask(ctx context.Context, task Task) (Task, error) {
	if task.Grading == "" {
		task.Grading = "tests" // the column's default, and every task's mode before judge grading
	}
	lists, err := encodeLists(task.HiddenTests, task.Reference, task.Verify, task.Setup)
	if err != nil {
		return Task{}, fmt.Errorf("save task %q: %w", task.Name, err)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (project_id, name, instruction, source, base_commit, solution_commit, hidden_tests,
		                   reference_files, verify, setup, needs_review, grading, validation, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ProjectID, task.Name, task.Instruction, task.Source, task.BaseCommit, task.SolutionCommit, lists[0], lists[1],
		lists[2], lists[3], task.NeedsReview, task.Grading, string(task.Validation), formatTime(task.CreatedAt), formatTime(task.CreatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Task{}, fmt.Errorf("task %q: %w", task.Name, ErrExists)
		}
		return Task{}, fmt.Errorf("save task %q: %w", task.Name, err)
	}
	if task.ID, err = result.LastInsertId(); err != nil {
		return Task{}, fmt.Errorf("save task %q: %w", task.Name, err)
	}
	task.CreatedAt = task.CreatedAt.UTC()
	task.UpdatedAt = task.CreatedAt
	return task, nil
}

// UpdateTask stores a task's editable fields: instruction, setup and verification commands, review flag and validation.
// The grading mode is fixed when the task is saved.
func (s *Store) UpdateTask(ctx context.Context, task Task, now time.Time) error {
	lists, err := encodeLists(task.Verify, task.Setup)
	if err != nil {
		return fmt.Errorf("update task %q: %w", task.Name, err)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET instruction = ?, verify = ?, setup = ?, needs_review = ?, validation = ?, updated_at = ?
		WHERE project_id = ? AND name = ?`,
		task.Instruction, lists[0], lists[1], task.NeedsReview, string(task.Validation), formatTime(now), task.ProjectID, task.Name)
	if err != nil {
		return fmt.Errorf("update task %q: %w", task.Name, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("task %q: %w", task.Name, errors.Join(ErrNotFound, err))
	}
	return nil
}

// SetTaskValidation stores a validation of the task with this id, and its update time, only while the task still has
// the verify and setup commands it was validated with: no other field is written, so edits made while it ran (an
// instruction, the review flag) stay. It reports whether a row matched; false means the task was removed, or its
// commands changed and the validation no longer applies.
func (s *Store) SetTaskValidation(ctx context.Context, id int64, verify, setup []string, validation []byte, now time.Time) (bool, error) {
	lists, err := encodeLists(verify, setup)
	if err != nil {
		return false, fmt.Errorf("store validation of task %d: %w", id, err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET validation = ?, updated_at = ? WHERE id = ? AND verify = ? AND setup = ?`,
		string(validation), formatTime(now), id, lists[0], lists[1])
	if err != nil {
		return false, fmt.Errorf("store validation of task %d: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store validation of task %d: %w", id, err)
	}
	return n > 0, nil
}

// ErrTaskInUse is returned by SetTaskValidationIdle when a locked experiment that can still run uses the task.
var ErrTaskInUse = errors.New("a locked experiment that can still run uses the task")

// SetTaskValidationIdle is SetTaskValidation for the task pool's re-validations: it also stores nothing, and returns
// ErrTaskInUse naming the experiments, while a locked experiment that can still run (any status but done) uses a task
// of this name. The check and the write are one transaction, which holds the write lock from its start
// (_txlock=immediate), so no experiment can lock the task between them.
func (s *Store) SetTaskValidationIdle(ctx context.Context, id int64, verify, setup []string, validation []byte, now time.Time) (bool, error) {
	lists, err := encodeLists(verify, setup)
	if err != nil {
		return false, fmt.Errorf("store validation of task %d: %w", id, err)
	}
	stored := false
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var projectID int64
		var name string
		err := tx.QueryRowContext(ctx, `SELECT project_id, name FROM tasks WHERE id = ?`, id).Scan(&projectID, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // removed: nothing to store, as SetTaskValidation
		} else if err != nil {
			return err
		}
		users, err := tasksInUse(ctx, tx, projectID, name)
		if err != nil {
			return err
		}
		if len(users[name]) > 0 {
			return fmt.Errorf("task %q: %w: %s", name, ErrTaskInUse, strings.Join(users[name], ", "))
		}
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET validation = ?, updated_at = ? WHERE id = ? AND verify = ? AND setup = ?`,
			string(validation), formatTime(now), id, lists[0], lists[1])
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		stored = n > 0
		return err
	})
	if err != nil {
		if errors.Is(err, ErrTaskInUse) {
			return false, err
		}
		return false, fmt.Errorf("store validation of task %d: %w", id, err)
	}
	return stored, nil
}

// TasksInUse maps the names of a project's tasks that locked experiments able to run still (any status but done) use
// to those experiments' names, sorted. Such an experiment runs its tasks as its lock fixed them, never from their rows.
func (s *Store) TasksInUse(ctx context.Context, projectID int64) (map[string][]string, error) {
	users, err := tasksInUse(ctx, s.db, projectID, "")
	if err != nil {
		return nil, fmt.Errorf("tasks in use: %w", err)
	}
	return users, nil
}

// queryer is what tasksInUse reads through: the database or a transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// tasksInUse is TasksInUse, for one task name only unless name is "". It reads the experiments' locks as
// internal/experiment writes them: {"tasks": [{"name": ...}, ...]}. A lock that is not valid JSON names no task: such an
// experiment cannot be resumed.
func tasksInUse(ctx context.Context, q queryer, projectID int64, name string) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, `
		WITH locks AS (
			SELECT name, CASE WHEN json_valid(lock) THEN lock ELSE '{}' END AS lock FROM experiments
			WHERE project_id = ? AND lock IS NOT NULL AND status <> ?)
		SELECT DISTINCT task, experiment FROM (
			SELECT json_extract(locks.lock, j.fullkey || '.name') AS task, locks.name AS experiment
			FROM locks, json_each(locks.lock, '$.tasks') AS j)
		WHERE task IS NOT NULL AND (? = '' OR task = ?)
		ORDER BY task, experiment`, projectID, StatusDone, name, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := map[string][]string{}
	for rows.Next() {
		var task, experiment string
		if err := rows.Scan(&task, &experiment); err != nil {
			return nil, err
		}
		users[task] = append(users[task], experiment)
	}
	return users, rows.Err()
}

// RetireTask flags the task with this id as retired, with a reason, and reports whether it did: false when the task is
// gone or retired already (its first retirement stays). Nothing else is written.
func (s *Store) RetireTask(ctx context.Context, id int64, reason string, now time.Time) (bool, error) {
	if strings.TrimSpace(reason) == "" {
		return false, fmt.Errorf("retire task %d: a reason is required", id)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET retired_at = ?, retired_reason = ? WHERE id = ? AND retired_at = ''`,
		formatTime(now), reason, id)
	if err != nil {
		return false, fmt.Errorf("retire task %d: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("retire task %d: %w", id, err)
	}
	return n > 0, nil
}

// RestoreTask makes a retired task of the project active again, reporting whether it was retired; ErrNotFound when
// there is no such task.
func (s *Store) RestoreTask(ctx context.Context, projectID int64, name string) (bool, error) {
	t, err := s.TaskByName(ctx, projectID, name)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET retired_at = '', retired_reason = '' WHERE id = ? AND retired_at <> ''`, t.ID)
	if err != nil {
		return false, fmt.Errorf("restore task %q: %w", name, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("restore task %q: %w", name, err)
	}
	return n > 0, nil
}

// TaskByName returns a project's task, or ErrNotFound.
func (s *Store) TaskByName(ctx context.Context, projectID int64, name string) (Task, error) {
	tasks, err := s.queryTasks(ctx, `WHERE project_id = ? AND name = ?`, projectID, name)
	if err != nil {
		return Task{}, err
	}
	if len(tasks) == 0 {
		return Task{}, fmt.Errorf("task %q: %w", name, ErrNotFound)
	}
	return tasks[0], nil
}

// Tasks lists a project's tasks, oldest first.
func (s *Store) Tasks(ctx context.Context, projectID int64) ([]Task, error) {
	return s.queryTasks(ctx, `WHERE project_id = ? ORDER BY created_at, id`, projectID)
}

// DeleteTask removes a project's task, or returns ErrNotFound.
func (s *Store) DeleteTask(ctx context.Context, projectID int64, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE project_id = ? AND name = ?`, projectID, name)
	if err != nil {
		return fmt.Errorf("delete task %q: %w", name, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("task %q: %w", name, errors.Join(ErrNotFound, err))
	}
	return nil
}

func (s *Store) queryTasks(ctx context.Context, clause string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, name, instruction, source, base_commit, solution_commit, hidden_tests, reference_files,
		       verify, setup, needs_review, grading, validation, created_at, updated_at, retired_at, retired_reason
		FROM tasks `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		var hidden, reference, verify, setup, validation, created, updated, retired string
		if err := rows.Scan(&task.ID, &task.ProjectID, &task.Name, &task.Instruction, &task.Source, &task.BaseCommit,
			&task.SolutionCommit, &hidden, &reference, &verify, &setup, &task.NeedsReview, &task.Grading, &validation, &created, &updated,
			&retired, &task.RetiredReason); err != nil {
			return nil, fmt.Errorf("read task: %w", err)
		}
		for _, field := range []struct {
			raw  string
			into *[]string
		}{{hidden, &task.HiddenTests}, {reference, &task.Reference}, {verify, &task.Verify}, {setup, &task.Setup}} {
			if err := json.Unmarshal([]byte(field.raw), field.into); err != nil {
				return nil, fmt.Errorf("task %q: %w", task.Name, err)
			}
		}
		if validation != "" {
			task.Validation = []byte(validation)
		}
		if task.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if task.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		if retired != "" {
			if task.RetiredAt, err = parseTime(retired); err != nil {
				return nil, err
			}
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	return tasks, nil
}

// encodeLists encodes string lists as JSON arrays ([] rather than null for empty ones).
func encodeLists(lists ...[]string) ([]string, error) {
	out := make([]string, len(lists))
	for i, list := range lists {
		data, err := json.Marshal(nonNil(list))
		if err != nil {
			return nil, err
		}
		out[i] = string(data)
	}
	return out, nil
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// Run is a stored agent run (see migrations/0004_runs.sql).
type Run struct {
	ID        string
	ProjectID int64
	TaskID    int64 // 0 when the task was removed
	TaskName  string
	Kind      string // "task" (default) or "calibration"
	Arm       string
	Outcome   string
	Passed    *bool
	CostUSD   float64
	Record    []byte // JSON
	Started   time.Time
	Finished  time.Time
	// ExperimentID, Slot and Attempt place an experiment's run in its schedule; zero for other runs.
	ExperimentID int64
	Slot         int
	Attempt      int
}

// SaveRun records a finished run.
func (s *Store) SaveRun(ctx context.Context, run Run) error {
	var taskID, passed, experimentID, slot, attempt any
	if run.TaskID != 0 {
		taskID = run.TaskID
	}
	if run.ExperimentID != 0 {
		experimentID, slot, attempt = run.ExperimentID, run.Slot, run.Attempt
	}
	if run.Passed != nil {
		passed = *run.Passed
	}
	if run.Kind == "" {
		run.Kind = "task"
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, project_id, task_id, task_name, kind, arm, outcome, passed, cost_usd, record, started_at, finished_at,
		                  experiment_id, slot, attempt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, run.ProjectID, taskID, run.TaskName, run.Kind, run.Arm, run.Outcome, passed,
		run.CostUSD, string(run.Record), formatTime(run.Started), formatTime(run.Finished), experimentID, slot, attempt); err != nil {
		return fmt.Errorf("save run %s: %w", run.ID, err)
	}
	return nil
}

// SetRunRecord replaces a stored run's record and nothing else: a resume adds the judge's verdict this way. Its
// columns (outcome, passed, cost) stay as they were stored.
func (s *Store) SetRunRecord(ctx context.Context, id string, record []byte) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET record = ? WHERE id = ?`, string(record), id)
	if err != nil {
		return fmt.Errorf("update run %s: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("run %s: %w", id, errors.Join(ErrNotFound, err))
	}
	return nil
}

// RunByID returns a project's run, or ErrNotFound.
func (s *Store) RunByID(ctx context.Context, projectID int64, id string) (Run, error) {
	runs, err := s.queryRuns(ctx, `WHERE project_id = ? AND id = ?`, projectID, id)
	if err != nil {
		return Run{}, err
	}
	if len(runs) == 0 {
		return Run{}, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	return runs[0], nil
}

// HasRun reports whether any project has stored a run with this id.
func (s *Store) HasRun(ctx context.Context, id string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE id = ?`, id).Scan(&n); err != nil {
		return false, fmt.Errorf("run %s: %w", id, err)
	}
	return n > 0, nil
}

// Runs lists a project's runs, oldest first.
func (s *Store) Runs(ctx context.Context, projectID int64) ([]Run, error) {
	return s.queryRuns(ctx, `WHERE project_id = ? ORDER BY started_at, id`, projectID)
}

func (s *Store) queryRuns(ctx context.Context, clause string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, task_id, task_name, kind, arm, outcome, passed, cost_usd, record, started_at, finished_at,
		       experiment_id, slot, attempt
		FROM runs `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var run Run
		var taskID, experimentID, slot, attempt sql.NullInt64
		var passed sql.NullBool
		var record, started, finished string
		if err := rows.Scan(&run.ID, &run.ProjectID, &taskID, &run.TaskName, &run.Kind, &run.Arm, &run.Outcome, &passed, &run.CostUSD,
			&record, &started, &finished, &experimentID, &slot, &attempt); err != nil {
			return nil, fmt.Errorf("read run: %w", err)
		}
		run.TaskID, run.Record = taskID.Int64, []byte(record)
		run.ExperimentID, run.Slot, run.Attempt = experimentID.Int64, int(slot.Int64), int(attempt.Int64)
		if passed.Valid {
			run.Passed = &passed.Bool
		}
		if run.Started, err = parseTime(started); err != nil {
			return nil, err
		}
		if run.Finished, err = parseTime(finished); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	return runs, nil
}

// Calibration is a stored calibration of one arm (see migrations/0005_calibrations.sql).
type Calibration struct {
	ID        int64
	ProjectID int64
	Arm       string
	Snapshot  string
	RunID     string
	Result    []byte // JSON
	CreatedAt time.Time
}

// SaveCalibration records a calibration.
func (s *Store) SaveCalibration(ctx context.Context, c Calibration) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO calibrations (project_id, arm, snapshot, run_id, result, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ProjectID, c.Arm, c.Snapshot, c.RunID, string(c.Result), formatTime(c.CreatedAt)); err != nil {
		return fmt.Errorf("save calibration of %s: %w", c.Arm, err)
	}
	return nil
}

// Calibrations returns every calibration of a project's arm with this snapshot commit, newest first (empty when none).
func (s *Store) Calibrations(ctx context.Context, projectID int64, arm, snapshot string) ([]Calibration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, arm, snapshot, run_id, result, created_at FROM calibrations
		WHERE project_id = ? AND arm = ? AND snapshot = ? ORDER BY created_at DESC, id DESC`, projectID, arm, snapshot)
	if err != nil {
		return nil, fmt.Errorf("calibrations of %s: %w", arm, err)
	}
	defer rows.Close()
	var out []Calibration
	for rows.Next() {
		var c Calibration
		var result, created string
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Arm, &c.Snapshot, &c.RunID, &result, &created); err != nil {
			return nil, fmt.Errorf("calibrations of %s: %w", arm, err)
		}
		c.Result = []byte(result)
		if c.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("calibrations of %s: %w", arm, err)
	}
	return out, nil
}

// LatestCalibration returns the newest calibration of a project's arm with this snapshot commit, or ErrNotFound.
func (s *Store) LatestCalibration(ctx context.Context, projectID int64, arm, snapshot string) (Calibration, error) {
	var c Calibration
	var result, created string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, project_id, arm, snapshot, run_id, result, created_at FROM calibrations
		WHERE project_id = ? AND arm = ? AND snapshot = ? ORDER BY created_at DESC, id DESC LIMIT 1`, projectID, arm, snapshot).
		Scan(&c.ID, &c.ProjectID, &c.Arm, &c.Snapshot, &c.RunID, &result, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Calibration{}, fmt.Errorf("calibration of %s: %w", arm, ErrNotFound)
	}
	if err != nil {
		return Calibration{}, fmt.Errorf("calibration of %s: %w", arm, err)
	}
	c.Result = []byte(result)
	if c.CreatedAt, err = parseTime(created); err != nil {
		return Calibration{}, err
	}
	return c, nil
}
