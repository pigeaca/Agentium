package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The kinds of a rule check (see run.CheckKind: the same words).
const (
	CheckRan        = "ran"         // a shell command the agent ran contains the pattern
	CheckChanged    = "changed"     // the agent's change touches a path that matches the pattern
	CheckNotChanged = "not-changed" // the agent's change touches no path that matches it
)

// RuleCheck is a project's rule check (see migrations/0014_rule_checks.sql).
type RuleCheck struct {
	ID        int64
	ProjectID int64
	Name      string
	Kind      string // CheckRan, CheckChanged or CheckNotChanged
	Pattern   string // a text (CheckRan) or a glob
	CreatedAt time.Time
}

// SaveRuleCheck adds a check; names are unique per project (ErrExists). The kind and a pattern that is not empty are
// the caller's to have checked; the table refuses others.
func (s *Store) SaveRuleCheck(ctx context.Context, c RuleCheck) (RuleCheck, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO rule_checks (project_id, name, kind, pattern, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.ProjectID, c.Name, c.Kind, c.Pattern, formatTime(c.CreatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return RuleCheck{}, fmt.Errorf("check %q: %w", c.Name, ErrExists)
		}
		return RuleCheck{}, fmt.Errorf("save check %q: %w", c.Name, err)
	}
	if c.ID, err = result.LastInsertId(); err != nil {
		return RuleCheck{}, fmt.Errorf("save check %q: %w", c.Name, err)
	}
	return c, nil
}

// RuleChecks lists a project's checks, oldest first.
func (s *Store) RuleChecks(ctx context.Context, projectID int64) ([]RuleCheck, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, name, kind, pattern, created_at FROM rule_checks WHERE project_id = ? ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list checks: %w", err)
	}
	defer rows.Close()
	var out []RuleCheck
	for rows.Next() {
		var c RuleCheck
		var created string
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Name, &c.Kind, &c.Pattern, &created); err != nil {
			return nil, fmt.Errorf("list checks: %w", err)
		}
		if c.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("list checks: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list checks: %w", err)
	}
	return out, nil
}

// DeleteRuleCheck removes a project's check, or returns ErrNotFound.
func (s *Store) DeleteRuleCheck(ctx context.Context, projectID int64, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM rule_checks WHERE project_id = ? AND name = ?`, projectID, name)
	if err != nil {
		return fmt.Errorf("delete check %q: %w", name, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("check %q: %w", name, ErrNotFound)
	}
	return nil
}
