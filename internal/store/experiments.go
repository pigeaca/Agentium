package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Experiment is a stored experiment (see migrations/0006_experiments.sql).
type Experiment struct {
	ID        int64
	ProjectID int64
	Name      string
	Template  string
	Design    []byte // JSON
	CreatedAt time.Time
}

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
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, name, template, design, created_at FROM experiments `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("query experiments: %w", err)
	}
	defer rows.Close()
	var found []Experiment
	for rows.Next() {
		var e Experiment
		var design, created string
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Name, &e.Template, &design, &created); err != nil {
			return nil, fmt.Errorf("read experiment: %w", err)
		}
		e.Design = []byte(design)
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
