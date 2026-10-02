package pool

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

// Preview reports what a pass would scan and import, and leaves no file behind: no state, no lock.
func TestPreviewWritesNothing(t *testing.T) {
	w := newWorld(t, 4, "c2", "c4")
	prev, err := w.pass(10).Preview(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range prev.Candidates {
		got = append(got, c.commit)
	}
	if prev.Head != "c4" || !slices.Equal(slices.Sorted(slices.Values(got)), []string{"c2", "c4"}) {
		t.Errorf("preview %+v", prev)
	}
	for _, f := range []string{w.file, w.file + ".lock", w.file + ".pass.lock"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s exists after a preview: %v", f, err)
		}
	}
	if tasks, _ := w.tasks(w.ctx); len(tasks) != 0 {
		t.Errorf("the preview imported %d tasks", len(tasks))
	}
}

// Plan: a task that retires is not also re-validated; a stale task in use is kept with its experiments; retired tasks
// are left alone.
func TestPlanSortsTasks(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	old := []byte(`{"status":"valid","arms":[{"name":"base"}],"stages":[],"at":"2026-08-01T00:00:00Z"}`)
	tasks := []store.Task{
		{Name: "dead", BaseCommit: "b-old", Validation: old},
		{Name: "stale", BaseCommit: "b-new", Validation: old},
		{Name: "locked", BaseCommit: "b-new", Validation: old},
		{Name: "gone", BaseCommit: "b-old", Validation: old, RetiredAt: now.Add(-Day), RetiredReason: "earlier"},
		{Name: "fresh", BaseCommit: "b-new", Validation: []byte(`{"status":"valid","arms":[{"name":"base"}],"stages":[],"at":"2026-10-01T00:00:00Z"}`)},
	}
	plan := DefaultPolicy().Plan(tasks, Facts{Now: now, InUse: map[string][]string{"locked": {"lean"}},
		BaseTime: func(c string) time.Time {
			if c == "b-old" {
				return now.Add(-300 * Day)
			}
			return now.Add(-10 * Day)
		}})
	if len(plan.Retire) != 1 || plan.Retire[0].Task.Name != "dead" {
		t.Errorf("retire %+v", plan.Retire)
	}
	if len(plan.Revalidate) != 1 || plan.Revalidate[0].Task.Name != "stale" || len(plan.Revalidate[0].Stale.Reasons) == 0 {
		t.Errorf("revalidate %+v", plan.Revalidate)
	}
	if len(plan.Kept) != 1 || plan.Kept[0].Task.Name != "locked" || !slices.Equal(plan.Kept[0].Experiments, []string{"lean"}) {
		t.Errorf("kept %+v", plan.Kept)
	}
}
