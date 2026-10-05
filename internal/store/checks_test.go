package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A data folder at version 13 opens and gains the checks table; the rows it had are untouched, and a check belongs to
// one project.
func TestRuleChecksMigrationAndRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agentium.db")
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	old, err := openOnce(ctx, file, migrationVersion(t, "rule_checks"))
	if err != nil {
		t.Fatal(err)
	}
	if got := migrationVersion(t, "rule_checks"); got != 14 {
		t.Fatalf("the rule checks migration is %d, want 14", got)
	}
	app := insertOldProject(t, old, "/work/app", now)
	if _, err := old.db.ExecContext(ctx, `INSERT INTO tasks (project_id, name, instruction, source, base_commit, verify, created_at, updated_at)
		VALUES (?, 'old', 'Fix it.', 'manual', 'base', '["make test"]', '2026-09-28T10:00:00Z', '2026-09-28T10:00:00Z')`, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.ExecContext(ctx, `SELECT 1 FROM rule_checks`); err == nil {
		t.Fatal("the table exists before its migration")
	}
	old.Close()

	s := open(t, file)
	if got, err := s.TaskByName(ctx, app.ID, "old"); err != nil || got.Instruction != "Fix it." {
		t.Fatalf("a task after the migration: %+v, %v", got, err)
	}
	if got, err := s.RuleChecks(ctx, app.ID); err != nil || len(got) != 0 {
		t.Fatalf("checks of a migrated project = %v, %v", got, err)
	}
	other, err := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []RuleCheck{{Name: "ran-tests", Kind: CheckRan, Pattern: "go test"}, {Name: "no-docs", Kind: CheckNotChanged, Pattern: "docs/**"}} {
		c.ProjectID, c.CreatedAt = app.ID, now.Add(time.Duration(i)*time.Second)
		if _, err := s.SaveRuleCheck(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveRuleCheck(ctx, RuleCheck{ProjectID: other.ID, Name: "ran-tests", Kind: CheckRan, Pattern: "make", CreatedAt: now}); err != nil {
		t.Errorf("the same name in another project: %v", err)
	}
	_, err = s.SaveRuleCheck(ctx, RuleCheck{ProjectID: app.ID, Name: "ran-tests", Kind: CheckChanged, Pattern: "x", CreatedAt: now})
	if !errors.Is(err, ErrExists) {
		t.Errorf("a duplicate name: %v, want ErrExists", err)
	}
	if _, err := s.SaveRuleCheck(ctx, RuleCheck{ProjectID: app.ID, Name: "bad", Kind: "other", Pattern: "x", CreatedAt: now}); err == nil {
		t.Error("an unknown kind was saved")
	}
	if _, err := s.SaveRuleCheck(ctx, RuleCheck{ProjectID: app.ID, Name: "empty", Kind: CheckRan, CreatedAt: now}); err == nil {
		t.Error("an empty pattern was saved")
	}
	got, err := s.RuleChecks(ctx, app.ID)
	if err != nil || len(got) != 2 || got[0].Name != "ran-tests" || got[0].Pattern != "go test" || got[1].Kind != CheckNotChanged || !got[1].CreatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("checks = %+v, %v", got, err)
	}
	if err := s.DeleteRuleCheck(ctx, app.ID, "ran-tests"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRuleCheck(ctx, app.ID, "ran-tests"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing a missing check: %v, want ErrNotFound", err)
	}
	if left, _ := s.RuleChecks(ctx, app.ID); len(left) != 1 || left[0].Name != "no-docs" {
		t.Errorf("after remove: %+v", left)
	}
	if left, _ := s.RuleChecks(ctx, other.ID); len(left) != 1 {
		t.Errorf("the other project's check: %+v", left)
	}
}
