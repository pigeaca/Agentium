package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// TaskGrade is the part of a graded run that says how a task went: which task, how the run ended, whether the hidden
// tests (or the judge) passed it, and when it started. It carries no run record.
type TaskGrade struct {
	TaskID  int64
	Outcome string // the stored outcome; the caller decides which outcomes count
	Passed  bool
	Started time.Time
}

// TaskGrades lists a project's graded task runs, oldest first (by start time, then ID): runs of kind "task" (not
// calibrations) of a task that still exists, with a recorded grade. Runs of every version and every experiment (or
// none) are together. It is read-only.
func (s *Store) TaskGrades(ctx context.Context, projectID int64) ([]TaskGrade, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, outcome, passed, started_at
		FROM runs
		WHERE project_id = ? AND kind = 'task' AND task_id IS NOT NULL AND passed IS NOT NULL
		ORDER BY started_at, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query task grades: %w", err)
	}
	defer rows.Close()
	var grades []TaskGrade
	for rows.Next() {
		var g TaskGrade
		var passed sql.NullBool
		var started string
		if err := rows.Scan(&g.TaskID, &g.Outcome, &passed, &started); err != nil {
			return nil, fmt.Errorf("read task grade: %w", err)
		}
		g.Passed = passed.Bool
		if g.Started, err = parseTime(started); err != nil {
			return nil, err
		}
		grades = append(grades, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query task grades: %w", err)
	}
	return grades, nil
}
