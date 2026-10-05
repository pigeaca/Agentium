package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TaskGrades returns only graded runs of kind task of tasks that still exist, oldest first, from every version; the
// outcome is left for the caller to judge.
func TestTaskGrades(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "agentium.db"))
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	app, err := s.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveProject(ctx, "/work/other", "other", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	save := func(p Project, name string) Task {
		task, err := s.SaveTask(ctx, Task{ProjectID: p.ID, Name: name, Instruction: "Fix.", Source: "manual", BaseCommit: "b", Verify: []string{"true"}, CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	fix, gone, foreign := save(app, "fix"), save(app, "gone"), save(other, "fix")
	yes, no := true, false
	n := 0
	record := `{"secret":"record"}`
	add := func(p Project, task Task, kind, outcome string, passed *bool, minutes int) {
		n++
		run := Run{ID: "20261001T1000Z-" + string(rune('a'+n)), ProjectID: p.ID, TaskID: task.ID, TaskName: task.Name, Kind: kind, Arm: "base",
			Outcome: outcome, Passed: passed, Record: []byte(record), Started: now.Add(time.Duration(minutes) * time.Minute), Finished: now.Add(time.Duration(minutes+1) * time.Minute)}
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	add(app, fix, "task", "ok", &no, 30)         // third by time, saved first
	add(app, fix, "task", "capped", &yes, 10)    // first
	add(app, fix, "calibration", "ok", &yes, 11) // not a task run
	add(app, fix, "task", "infra", &yes, 12)     // kept: the caller filters outcomes
	add(app, fix, "task", "ok", nil, 13)         // no grade
	add(app, fix, "task", "timeout", &no, 20)    // second
	add(app, gone, "task", "ok", &yes, 14)       // its task is removed below
	add(other, foreign, "task", "ok", &yes, 15)  // another project's
	record = `{"behavior":{"config_changed":["jest.config.js"]}}`
	add(app, fix, "task", "ok", &yes, 40) // passed, but changed the test runner's configuration
	record = `{"behavior":{"config_changed":[]}}`
	add(app, fix, "task", "ok", &yes, 50)
	record = `not json`
	add(app, fix, "task", "ok", &yes, 60)
	if err := s.DeleteTask(ctx, app.ID, "gone"); err != nil {
		t.Fatal(err)
	}
	got, err := s.TaskGrades(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []TaskGrade{
		{fix.ID, "capped", true, now.Add(10 * time.Minute), false},
		{fix.ID, "infra", true, now.Add(12 * time.Minute), false},
		{fix.ID, "timeout", false, now.Add(20 * time.Minute), false},
		{fix.ID, "ok", false, now.Add(30 * time.Minute), false},
		{fix.ID, "ok", true, now.Add(40 * time.Minute), true},
		{fix.ID, "ok", true, now.Add(50 * time.Minute), false},
		{fix.ID, "ok", true, now.Add(60 * time.Minute), false},
	}
	if len(got) != len(want) {
		t.Fatalf("grades %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("grade %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if none, err := s.TaskGrades(ctx, 999); err != nil || len(none) != 0 {
		t.Errorf("an unknown project: %v, %v", none, err)
	}
}
