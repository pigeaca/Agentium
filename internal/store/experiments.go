package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Experiment is a stored experiment (see migrations/0006_experiments.sql).
type Experiment struct {
	ID         int64
	ProjectID  int64
	Name       string
	Template   string
	Design     []byte // JSON
	CreatedAt  time.Time
	Lock       []byte // JSON; nil until the first run
	Status     string // draft, running, stopped, budget, usage, done
	StatusNote string
}

// Experiment statuses.
const (
	StatusDraft   = "draft"   // not locked: no run yet
	StatusRunning = "running" // or its process died: a resume tells
	StatusStopped = "stopped" // interrupted, or stopped by repeated infrastructure failures or a changed environment
	StatusBudget  = "budget"  // the next run would not fit the budget
	StatusUsage   = "usage"   // paused before the subscription's five-hour usage limit
	StatusDone    = "done"    // every slot settled
)

// SaveExperiment records a new experiment; names are unique per project (ErrExists).
func (s *Store) SaveExperiment(ctx context.Context, e Experiment) (Experiment, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO experiments (project_id, name, template, design, created_at) VALUES (?, ?, ?, ?, ?)`,
		e.ProjectID, e.Name, e.Template, string(e.Design), formatTime(e.CreatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Experiment{}, fmt.Errorf("experiment %q: %w", e.Name, ErrExists)
		}
		return Experiment{}, fmt.Errorf("save experiment %q: %w", e.Name, err)
	}
	if e.ID, err = result.LastInsertId(); err != nil {
		return Experiment{}, fmt.Errorf("save experiment %q: %w", e.Name, err)
	}
	e.CreatedAt = e.CreatedAt.UTC()
	return e, nil
}

// ExperimentByName returns a project's experiment, or ErrNotFound.
func (s *Store) ExperimentByName(ctx context.Context, projectID int64, name string) (Experiment, error) {
	found, err := s.queryExperiments(ctx, `WHERE project_id = ? AND name = ?`, projectID, name)
	if err != nil {
		return Experiment{}, err
	}
	if len(found) == 0 {
		return Experiment{}, fmt.Errorf("experiment %q: %w", name, ErrNotFound)
	}
	return found[0], nil
}

// Experiments lists a project's experiments, oldest first.
func (s *Store) Experiments(ctx context.Context, projectID int64) ([]Experiment, error) {
	return s.queryExperiments(ctx, `WHERE project_id = ? ORDER BY created_at, id`, projectID)
}

// DeleteExperiment removes a project's experiment, or returns ErrNotFound.
func (s *Store) DeleteExperiment(ctx context.Context, projectID int64, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM experiments WHERE project_id = ? AND name = ?`, projectID, name)
	if err != nil {
		return fmt.Errorf("delete experiment %q: %w", name, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("experiment %q: %w", name, errors.Join(ErrNotFound, err))
	}
	return nil
}

func (s *Store) queryExperiments(ctx context.Context, clause string, args ...any) ([]Experiment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, name, template, design, created_at, lock, status, status_note FROM experiments `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query experiments: %w", err)
	}
	defer rows.Close()
	var found []Experiment
	for rows.Next() {
		var e Experiment
		var design, created string
		var lock sql.NullString
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Name, &e.Template, &design, &created, &lock, &e.Status, &e.StatusNote); err != nil {
			return nil, fmt.Errorf("read experiment: %w", err)
		}
		e.Design = []byte(design)
		if lock.Valid {
			e.Lock = []byte(lock.String)
		}
		if e.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		found = append(found, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query experiments: %w", err)
	}
	return found, nil
}

// ErrLocked is returned when an experiment is locked already.
var ErrLocked = errors.New("already locked")

// LockExperiment writes an experiment's lock, once, and marks it running.
func (s *Store) LockExperiment(ctx context.Context, id int64, lock []byte) error {
	result, err := s.db.ExecContext(ctx, `UPDATE experiments SET lock = ?, status = ?, status_note = '' WHERE id = ? AND lock IS NULL`,
		string(lock), StatusRunning, id)
	if err != nil {
		return fmt.Errorf("lock experiment %d: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("experiment %d: %w", id, errors.Join(ErrLocked, err))
	}
	return nil
}

// AmendLock replaces a locked experiment's lock (only to record a raised budget).
func (s *Store) AmendLock(ctx context.Context, id int64, lock []byte) error {
	result, err := s.db.ExecContext(ctx, `UPDATE experiments SET lock = ? WHERE id = ? AND lock IS NOT NULL`, string(lock), id)
	if err != nil {
		return fmt.Errorf("amend the lock of experiment %d: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("experiment %d is not locked: %w", id, errors.Join(ErrNotFound, err))
	}
	return nil
}

// SetExperimentStatus records where an experiment stands, and why.
func (s *Store) SetExperimentStatus(ctx context.Context, id int64, status, note string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE experiments SET status = ?, status_note = ? WHERE id = ?`, status, note, id); err != nil {
		return fmt.Errorf("experiment %d status: %w", id, err)
	}
	return nil
}

// ExperimentRuns lists an experiment's task runs (its slots' attempts), oldest first. The calibration runs the
// experiment made for its arms are not slots: ExperimentCalibrationRuns lists them.
func (s *Store) ExperimentRuns(ctx context.Context, experimentID int64) ([]Run, error) {
	return s.queryRuns(ctx, `WHERE experiment_id = ? AND kind <> 'calibration' ORDER BY started_at, id`, experimentID)
}

// ExperimentCalibrationRuns lists the calibration runs an experiment made before its first pair, oldest first. They
// count against the experiment's budget.
func (s *Store) ExperimentCalibrationRuns(ctx context.Context, experimentID int64) ([]Run, error) {
	return s.queryRuns(ctx, `WHERE experiment_id = ? AND kind = 'calibration' ORDER BY started_at, id`, experimentID)
}
