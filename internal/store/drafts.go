package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Drafts of a task's text (see migrations/0015_task_drafts.sql). A draft call's cost is counted before anything reads
// its reply (CountDraftCall), the draft is stored beside the instruction only when it passed its checks (SetTaskDraft),
// and only the owner puts it in place (AcceptTaskDraft).

// CountDraftCall adds what the draft call id cost to the drafting spend of the task with this id, once: a call already
// counted adds nothing, and counted is false. The call is listed (draft_calls) and the task's spend raised in one
// transaction, so a crash leaves either both or neither. costUSD must not be negative; a call that reported no cost is
// counted with 0, so its folder is never read again as an uncounted one. When the task is gone, the call is still
// listed (its cost kept there) and ErrNotFound is returned with counted true.
func (s *Store) CountDraftCall(ctx context.Context, id string, taskID int64, costUSD float64, now time.Time) (counted bool, err error) {
	if strings.TrimSpace(id) == "" || costUSD < 0 {
		return false, fmt.Errorf("count draft call %q: an id and a cost of 0 or more are required (got %v)", id, costUSD)
	}
	found := true
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO draft_calls (id, task_id, cost_usd, counted_at) VALUES (?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			id, taskID, costUSD, formatTime(now))
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			return err // listed already: counted once, never again
		}
		counted = true
		result, err = tx.ExecContext(ctx, `UPDATE tasks SET draft_spend_usd = draft_spend_usd + ? WHERE id = ?`, costUSD, taskID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		found = n > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("count draft call %s: %w", id, err)
	}
	if !found {
		return counted, fmt.Errorf("count draft call %s: task %d: %w", id, taskID, ErrNotFound)
	}
	return counted, nil
}

// DraftCallCounted reports whether the draft call id was counted (CountDraftCall).
func (s *Store) DraftCallCounted(ctx context.Context, id string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM draft_calls WHERE id = ?`, id).Scan(&n); err != nil {
		return false, fmt.Errorf("draft call %s: %w", id, err)
	}
	return n > 0, nil
}

// SetTaskDraft stores text as the draft of the task with this id, written at now by model, in place of an earlier
// draft. Nothing else is written: not the instruction, the review flag or the time of change, since the task the agent
// gets is the same. ErrNotFound when the task is gone.
func (s *Store) SetTaskDraft(ctx context.Context, id int64, text, model string, now time.Time) error {
	if strings.TrimSpace(text) == "" || model == "" {
		return fmt.Errorf("store the draft of task %d: a text and a model are required", id)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET draft = ?, draft_at = ?, draft_model = ? WHERE id = ?`, text, formatTime(now), model, id)
	if err != nil {
		return fmt.Errorf("store the draft of task %d: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("store the draft of task %d: %w", id, errors.Join(ErrNotFound, err))
	}
	return nil
}

// ErrDraftChanged is returned by AcceptTaskDraft when the task's stored draft is not the one the caller read.
var ErrDraftChanged = errors.New("the task's draft changed meanwhile")

// AcceptTaskDraft is UpdateTask for `task edit --accept-draft`: it stores task's editable fields (its Instruction is
// the draft being accepted) and clears the stored draft, in one statement, only while the stored draft is still draft
// (else ErrDraftChanged, and nothing is written; ErrNotFound when the task is gone). The drafting spend stays.
func (s *Store) AcceptTaskDraft(ctx context.Context, task Task, draft string, now time.Time) error {
	if draft == "" {
		return fmt.Errorf("accept the draft of task %q: no draft was given", task.Name)
	}
	lists, err := encodeLists(task.Verify, task.Setup)
	if err != nil {
		return fmt.Errorf("accept the draft of task %q: %w", task.Name, err)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE tasks SET instruction = ?, verify = ?, setup = ?, needs_review = ?, validation = ?, updated_at = ?,
			                 draft = '', draft_at = '', draft_model = ''
			WHERE project_id = ? AND name = ? AND draft = ?`,
			task.Instruction, lists[0], lists[1], task.NeedsReview, string(task.Validation), formatTime(now), task.ProjectID, task.Name, draft)
		if err != nil {
			return fmt.Errorf("accept the draft of task %q: %w", task.Name, err)
		}
		if n, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("accept the draft of task %q: %w", task.Name, err)
		} else if n > 0 {
			return nil
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE project_id = ? AND name = ?`, task.ProjectID, task.Name).Scan(&exists); err != nil {
			return fmt.Errorf("accept the draft of task %q: %w", task.Name, err)
		}
		if exists == 0 {
			return fmt.Errorf("task %q: %w", task.Name, ErrNotFound)
		}
		return fmt.Errorf("task %q: %w", task.Name, ErrDraftChanged)
	})
}
