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
	Validation     []byte // JSON; nil until validated
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SaveTask records a new task; names are unique per project (ErrExists).
func (s *Store) SaveTask(ctx context.Context, task Task) (Task, error) {
	lists, err := encodeLists(task.HiddenTests, task.Reference, task.Verify, task.Setup)
	if err != nil {
		return Task{}, fmt.Errorf("save task %q: %w", task.Name, err)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (project_id, name, instruction, source, base_commit, solution_commit, hidden_tests,
		                   reference_files, verify, setup, needs_review, validation, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ProjectID, task.Name, task.Instruction, task.Source, task.BaseCommit, task.SolutionCommit, lists[0], lists[1],
		lists[2], lists[3], task.NeedsReview, string(task.Validation), formatTime(task.CreatedAt), formatTime(task.CreatedAt))
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
		       verify, setup, needs_review, validation, created_at, updated_at
		FROM tasks `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		var hidden, reference, verify, setup, validation, created, updated string
		if err := rows.Scan(&task.ID, &task.ProjectID, &task.Name, &task.Instruction, &task.Source, &task.BaseCommit,
			&task.SolutionCommit, &hidden, &reference, &verify, &setup, &task.NeedsReview, &validation, &created, &updated); err != nil {
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
	Arm       string
	Outcome   string
	Passed    *bool
	CostUSD   float64
	Record    []byte // JSON
	Started   time.Time
	Finished  time.Time
}

// SaveRun records a finished run.
func (s *Store) SaveRun(ctx context.Context, run Run) error {
	var taskID, passed any
	if run.TaskID != 0 {
		taskID = run.TaskID
	}
	if run.Passed != nil {
		passed = *run.Passed
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, project_id, task_id, task_name, arm, outcome, passed, cost_usd, record, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, run.ProjectID, taskID, run.TaskName, run.Arm, run.Outcome, passed,
		run.CostUSD, string(run.Record), formatTime(run.Started), formatTime(run.Finished)); err != nil {
		return fmt.Errorf("save run %s: %w", run.ID, err)
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

// Runs lists a project's runs, oldest first.
func (s *Store) Runs(ctx context.Context, projectID int64) ([]Run, error) {
	return s.queryRuns(ctx, `WHERE project_id = ? ORDER BY started_at, id`, projectID)
}

func (s *Store) queryRuns(ctx context.Context, clause string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, task_id, task_name, arm, outcome, passed, cost_usd, record, started_at, finished_at
		FROM runs `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var run Run
		var taskID sql.NullInt64
		var passed sql.NullBool
		var record, started, finished string
		if err := rows.Scan(&run.ID, &run.ProjectID, &taskID, &run.TaskName, &run.Arm, &run.Outcome, &passed, &run.CostUSD,
			&record, &started, &finished); err != nil {
			return nil, fmt.Errorf("read run: %w", err)
		}
		run.TaskID, run.Record = taskID.Int64, []byte(record)
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
