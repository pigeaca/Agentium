package watch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

func eligible(n int) []store.Task {
	tasks := make([]store.Task, n)
	for i := range tasks {
		tasks[i] = store.Task{ID: int64(i + 1), Name: fmt.Sprintf("task-%02d", i), BaseCommit: "base", SolutionCommit: fmt.Sprintf("sol%d", i),
			Instruction: "Fix it.", HiddenTests: []string{"a_test.go"}, Verify: []string{"go test ./..."}, Grading: "tests",
			Validation: []byte(`{"status":"valid"}`)}
	}
	return tasks
}

// The panel is fixed by its seed: the same tasks and seed draw the same eight in any input order, another seed draws
// another panel, and too few tasks start no chart.
func TestDrawPanelIsFixedBySeed(t *testing.T) {
	tasks := eligible(20)
	first, err := DrawPanel(tasks, 42)
	if err != nil || len(first) != PanelSize {
		t.Fatal(first, err)
	}
	reversed := slices.Clone(tasks)
	slices.Reverse(reversed)
	again, err := DrawPanel(reversed, 42)
	if err != nil || !slices.Equal(first, again) {
		t.Errorf("the same seed in another order drew %v, then %v", first, again)
	}
	other, _ := DrawPanel(tasks, 43)
	if slices.Equal(first, other) {
		t.Error("another seed drew the same panel")
	}
	if !slices.IsSortedFunc(first, func(a, b PanelTask) int { return strings.Compare(a.Name, b.Name) }) {
		t.Errorf("the panel is not in name order: %v", first)
	}
	if _, err := DrawPanel(tasks[:7], 42); err == nil || !strings.Contains(err.Error(), "needs 8") {
		t.Errorf("seven tasks: %v", err)
	}
}

// A chart closes when a panel task is removed, retired, edited or no longer valid; a new validation of the same task,
// or an empty list stored as nil, is not an edit.
func TestPanelChange(t *testing.T) {
	tasks := eligible(8)
	panel, err := DrawPanel(tasks, 1)
	if err != nil {
		t.Fatal(err)
	}
	revalidated := slices.Clone(tasks)
	revalidated[0].Validation, revalidated[0].UpdatedAt = []byte(`{"status":"valid","at":"2026-10-03T00:00:00Z"}`), time.Now()
	revalidated[1].Setup = []string{}
	if reason := PanelChange(panel, revalidated); reason != "" {
		t.Errorf("a revalidated task closed the chart: %s", reason)
	}
	for _, c := range []struct {
		change func(ts []store.Task) []store.Task
		want   string
	}{
		{func(ts []store.Task) []store.Task { return ts[1:] }, "task task-00 was removed"},
		{func(ts []store.Task) []store.Task {
			ts[0].RetiredAt, ts[0].RetiredReason = time.Now(), "its base is 130 days old"
			return ts
		}, "task task-00 retired (its base is 130 days old)"},
		{func(ts []store.Task) []store.Task { ts[0].Instruction = "Fix it better."; return ts }, "task task-00 was edited"},
		{func(ts []store.Task) []store.Task { ts[0].Verify = []string{"go test -run X ./..."}; return ts }, "task task-00 was edited"},
		{func(ts []store.Task) []store.Task { ts[0].Validation = []byte(`{"status":"flaky"}`); return ts }, "task task-00 is no longer valid"},
	} {
		if got := PanelChange(panel, c.change(slices.Clone(tasks))); got != c.want {
			t.Errorf("PanelChange = %q, want %q", got, c.want)
		}
	}
}

// A chart starts with its drawn panel and pinned snapshot, one per project, model and effort; a changed task closes
// it and loses its open check, and a new chart can then start.
func TestStartAndCloseChart(t *testing.T) {
	ctx := context.Background()
	s, c, app := newService(t, time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC))
	tasks := eligible(10)
	for i := range tasks {
		saved, err := s.DB.SaveTask(ctx, store.Task{ProjectID: app.ID, Name: tasks[i].Name, Instruction: tasks[i].Instruction, Source: "manual",
			BaseCommit: tasks[i].BaseCommit, SolutionCommit: tasks[i].SolutionCommit, HiddenTests: tasks[i].HiddenTests, Verify: tasks[i].Verify,
			CreatedAt: c.now, UpdatedAt: c.now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.SetTaskValidation(ctx, saved.ID, saved.Verify, saved.Setup, []byte(`{"status":"valid"}`), c.now); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := s.DB.Tasks(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	chart := Chart{ProjectID: app.ID, Model: "claude-sonnet-5", Effort: "high", Snapshot: "c0ffee", SnapshotName: "main"}
	panel, err := s.StartChart(ctx, chart, stored, 9)
	if err != nil {
		t.Fatal(err)
	}
	drawn, err := PanelTasks(panel)
	if err != nil || len(drawn) != PanelSize || panel.Seed != 9 || panel.Snapshot != "c0ffee" {
		t.Fatalf("the chart = %+v, tasks %v, %v", panel, drawn, err)
	}
	if _, err := s.StartChart(ctx, chart, stored, 10); !errors.Is(err, store.ErrExists) {
		t.Errorf("a second chart for the same model and effort: %v, want ErrExists", err)
	}
	if reason, err := s.CloseChartIfChanged(ctx, panel, stored); err != nil || reason != "" {
		t.Errorf("an unchanged panel: %q, %v", reason, err)
	}
	check, _, err := s.DB.StartDriftCheck(ctx, panel.ID, "2.1.0", c.now)
	if err != nil {
		t.Fatal(err)
	}
	victim, _ := s.DB.TaskByName(ctx, app.ID, drawn[3].Name)
	if _, err := s.DB.RetireTask(ctx, victim.ID, "its base is 130 days old", c.now); err != nil {
		t.Fatal(err)
	}
	stored, _ = s.DB.Tasks(ctx, app.ID)
	reason, err := s.CloseChartIfChanged(ctx, panel, stored)
	if err != nil || !strings.Contains(reason, drawn[3].Name+" retired") {
		t.Fatalf("after a retirement: %q, %v", reason, err)
	}
	if got, _ := s.DB.DriftCheckByID(ctx, check.ID); got.Status != store.DriftCheckLost {
		t.Errorf("the open check of the closed chart = %+v", got)
	}
	if _, err := s.StartChart(ctx, chart, stored, 11); err != nil {
		t.Errorf("a new chart after the old one closed: %v", err)
	}
}
