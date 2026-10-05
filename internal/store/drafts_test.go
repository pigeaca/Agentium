package store

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// A data folder from before the drafts migration opens, gains the draft columns empty, and its tasks are unchanged.
func TestDraftsMigrationKeepsOldTasks(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agentium.db")
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	old, err := openOnce(ctx, file, migrationVersion(t, "task_drafts")) // every migration before it, 0014 included once it lands
	if err != nil {
		t.Fatal(err)
	}
	app := insertOldProject(t, old, "/work/app", now)
	before, err := old.SaveTask(ctx, Task{ProjectID: app.ID, Name: "old", Instruction: "Fix it.", Source: "commit abc", BaseCommit: "b", SolutionCommit: "s",
		HiddenTests: []string{"a_test.go"}, Reference: []string{"a.go"}, Verify: []string{"go test ./..."}, NeedsReview: true, Validation: []byte(`{"status":"valid"}`),
		Module: "svc", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := old.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name LIKE 'draft%'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("the old schema has %d draft column(s), %v", columns, err)
	}
	old.Close()

	s := open(t, file)
	got, err := s.TaskByName(ctx, app.ID, "old")
	if err != nil {
		t.Fatal(err)
	}
	if got.Draft != "" || !got.DraftAt.IsZero() || got.DraftModel != "" || got.DraftSpendUSD != 0 {
		t.Errorf("an old task gained a draft: %+v", got)
	}
	got.Draft, got.DraftAt, got.DraftModel, got.DraftSpendUSD = before.Draft, before.DraftAt, before.DraftModel, before.DraftSpendUSD
	before.Grading, before.Setup = "tests", []string{} // as stored: the column's default, an empty list
	if !reflect.DeepEqual(got, before) {
		t.Errorf("the task changed in the migration:\n got %+v\nwant %+v", got, before)
	}
}

// A draft is stored beside the instruction, which it does not change; accepting it puts it in place and clears it, only
// while it is the draft read; the drafting spend counts each call once and survives both.
func TestTaskDraftRoundTrip(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveTask(ctx, Task{ProjectID: app.ID, Name: "t", Instruction: "Fix it.", Source: "commit abc", BaseCommit: "b", Verify: []string{"make test"},
		NeedsReview: false, Validation: []byte(`{"status":"valid"}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}

	// Counted once per call, whatever is asked again.
	for i, c := range []struct {
		id      string
		cost    float64
		counted bool
	}{{"c1", 0.12, true}, {"c1", 0.12, false}, {"c2", 0.30, true}, {"c3", 0, true}} {
		counted, err := s.CountDraftCall(ctx, c.id, saved.Ref(), c.cost, now)
		if err != nil || counted != c.counted {
			t.Errorf("count %d (%s): counted %v, %v; want %v", i, c.id, counted, err, c.counted)
		}
	}
	if listed, err := s.DraftCallCounted(ctx, "c1"); err != nil || !listed {
		t.Errorf("c1 listed: %v, %v", listed, err)
	}
	if listed, err := s.DraftCallCounted(ctx, "nope"); err != nil || listed {
		t.Errorf("an unknown call listed: %v, %v", listed, err)
	}
	if _, err := s.CountDraftCall(ctx, "neg", saved.Ref(), -1, now); err == nil {
		t.Error("a negative cost was counted")
	}
	gone := saved.Ref()
	gone.ID += 99
	if counted, err := s.CountDraftCall(ctx, "gone", gone, 0.05, now); !counted || !errors.Is(err, ErrNotFound) {
		t.Errorf("a removed task's call: counted %v, %v", counted, err)
	}

	if err := s.SetTaskDraft(ctx, saved.Ref(), "Make it new.", "claude-sonnet-5-5", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskDraft(ctx, gone, "x", "m", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("a draft for a removed task: %v", err)
	}
	got, err := s.TaskByName(ctx, app.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got.Draft != "Make it new." || got.DraftModel != "claude-sonnet-5-5" || !got.DraftAt.Equal(now.Add(time.Minute)) || math.Abs(got.DraftSpendUSD-0.42) > 1e-9 ||
		got.Instruction != "Fix it." || got.NeedsReview || !got.UpdatedAt.Equal(saved.UpdatedAt) {
		t.Fatalf("after storing a draft: %+v", got)
	}

	// UpdateTask (task edit, pool update --accept-mined) never touches the draft.
	got.Instruction, got.NeedsReview = "Fix it, really.", false
	if err := s.UpdateTask(ctx, got, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.TaskByName(ctx, app.ID, "t"); again.Draft != "Make it new." || math.Abs(again.DraftSpendUSD-0.42) > 1e-9 {
		t.Errorf("UpdateTask touched the draft: %+v", again)
	}

	accept := got
	accept.Instruction, accept.NeedsReview = got.Draft, true
	if err := s.AcceptTaskDraft(ctx, accept, "another text", now.Add(3*time.Minute)); !errors.Is(err, ErrDraftChanged) {
		t.Errorf("accepting a draft that is not stored: %v", err)
	}
	if again, _ := s.TaskByName(ctx, app.ID, "t"); again.Instruction != "Fix it, really." || again.Draft == "" {
		t.Errorf("a refused accept wrote: %+v", again)
	}
	if err := s.AcceptTaskDraft(ctx, accept, "Make it new.", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, _ := s.TaskByName(ctx, app.ID, "t")
	if after.Instruction != "Make it new." || !after.NeedsReview || !after.FromDraft || after.Draft != "" || after.DraftModel != "" || !after.DraftAt.IsZero() ||
		math.Abs(after.DraftSpendUSD-0.42) > 1e-9 || !after.UpdatedAt.Equal(now.Add(3*time.Minute)) || string(after.Validation) != `{"status":"valid"}` {
		t.Errorf("after accepting the draft: %+v", after)
	}
	missing := accept
	missing.Name = "nope"
	if err := s.AcceptTaskDraft(ctx, missing, "Make it new.", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("accepting the draft of a missing task: %v", err)
	}
}

// A draft's text is never marked reviewed by AcceptMined, nor is a task whose instruction changed since it was read; a
// hand-written instruction clears the draft's mark.
func TestAcceptMinedLeavesDraftsAndChangedTexts(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) Task {
		saved, err := s.SaveTask(ctx, Task{ProjectID: app.ID, Name: name, Instruction: "Fix " + name + ".", Source: "commit abc", BaseCommit: "b",
			Verify: []string{"make test"}, NeedsReview: true, CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	plain, drafted, edited := mk("plain"), mk("drafted"), mk("edited")
	if err := s.SetTaskDraft(ctx, drafted.Ref(), "Draft.", "m", now); err != nil {
		t.Fatal(err)
	}
	accept := drafted
	accept.Instruction = "Draft."
	if err := s.AcceptTaskDraft(ctx, accept, "Draft.", now); err != nil {
		t.Fatal(err)
	}
	changed := edited
	changed.Instruction = "Fix it otherwise."
	if err := s.UpdateTask(ctx, changed, now); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		task Task
		want bool
	}{{plain, true}, {accept, false}, {edited, false}} {
		if ok, err := s.AcceptMined(ctx, c.task, now); err != nil || ok != c.want {
			t.Errorf("AcceptMined(%s) = %v, %v; want %v", c.task.Name, ok, err, c.want)
		}
	}
	if got, _ := s.TaskByName(ctx, app.ID, "drafted"); !got.NeedsReview || !got.FromDraft {
		t.Errorf("a draft's text after AcceptMined: %+v", got)
	}
	hand := accept
	hand.Instruction, hand.NeedsReview = "Written by hand.", false
	if err := s.UpdateTask(ctx, hand, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TaskByName(ctx, app.ID, "drafted"); got.FromDraft {
		t.Errorf("a hand-written instruction kept the draft's mark: %+v", got)
	}
}

// A removed task's id given to a new task: a draft call made for the old one charges and stores nothing on the new one.
func TestDraftWritesNeverReachAReusedID(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.SaveTask(ctx, Task{ProjectID: app.ID, Name: "old", Instruction: "x", Source: "manual", BaseCommit: "b", Verify: []string{"t"}, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, app.ID, "old"); err != nil {
		t.Fatal(err)
	}
	reused, err := s.SaveTask(ctx, Task{ProjectID: app.ID, Name: "old", Instruction: "y", Source: "manual", BaseCommit: "b", Verify: []string{"t"}, CreatedAt: now.Add(time.Hour)})
	if err != nil || reused.ID != old.ID {
		t.Fatalf("the id was not reused (%d, %d): %v", old.ID, reused.ID, err)
	}
	if counted, err := s.CountDraftCall(ctx, "c", old.Ref(), 0.3, now); !counted || !errors.Is(err, ErrNotFound) {
		t.Errorf("the old task's call: %v, %v", counted, err)
	}
	if listed, _ := s.DraftCallCounted(ctx, "c"); !listed {
		t.Error("the call is not listed")
	}
	if err := s.SetTaskDraft(ctx, old.Ref(), "Draft.", "m", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("a draft for the old task: %v", err)
	}
	if got, _ := s.TaskByName(ctx, app.ID, "old"); got.DraftSpendUSD != 0 || got.Draft != "" {
		t.Errorf("the new task was charged or given the draft: %+v", got)
	}
}

// A task removed while --accept-mined checks it, its id taken by a task of another project with the same text: the
// replacement is not marked reviewed.
func TestAcceptMinedNeverMarksAReusedID(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := s.SaveTask(ctx, Task{ProjectID: app.ID, Name: "fix", Instruction: "Fix it.", Source: "commit abc", BaseCommit: "b",
		Verify: []string{"t"}, NeedsReview: true, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, app.ID, "fix"); err != nil {
		t.Fatal(err)
	}
	reused, err := s.SaveTask(ctx, Task{ProjectID: other.ID, Name: "fix", Instruction: "Fix it.", Source: "commit abc", BaseCommit: "b",
		Verify: []string{"t"}, NeedsReview: true, CreatedAt: now.Add(time.Minute)})
	if err != nil || reused.ID != checked.ID {
		t.Fatalf("the id was not reused (%d, %d): %v", checked.ID, reused.ID, err)
	}
	if ok, err := s.AcceptMined(ctx, checked, now); err != nil || ok {
		t.Errorf("AcceptMined of the removed task = %v, %v", ok, err)
	}
	if got, _ := s.TaskByName(ctx, other.ID, "fix"); !got.NeedsReview {
		t.Errorf("the replacement was marked reviewed: %+v", got)
	}
}
