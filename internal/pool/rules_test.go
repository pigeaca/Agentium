package pool

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

var now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// validated is a task whose last validation is v.
func validated(t *testing.T, v task.Validation) store.Task {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return store.Task{Name: "fix", BaseCommit: "base", SolutionCommit: "sol", HiddenTests: []string{"p/p_test.go"},
		Reference: []string{"p/p.go"}, Validation: raw}
}

func TestStaleByAgeAtTheBoundary(t *testing.T) {
	p := DefaultPolicy()
	tools := task.Toolchain{"go": "go1.27.1"}
	for _, c := range []struct {
		age   time.Duration
		stale bool
	}{{29 * Day, false}, {30 * Day, false}, {30*Day + time.Second, true}, {31 * Day, true}} {
		v := task.Validation{Status: task.StatusValid, Arms: []task.Arm{{Name: "base"}}, At: now.Add(-c.age), Toolchain: tools}
		got := p.Stale(validated(t, v), now, tools)
		if (len(got.Reasons) > 0) != c.stale {
			t.Errorf("validated %v ago: %+v, want stale %v", c.age, got, c.stale)
		}
		if want := "validated on " + now.Add(-c.age).Format(time.DateOnly) + ", more than 30 days ago"; c.stale && got.Reasons[0] != want {
			t.Errorf("reason %q, want %q", got.Reasons[0], want)
		}
	}
}

func TestStaleByToolchain(t *testing.T) {
	p := DefaultPolicy()
	fresh := now.Add(-Day)
	recorded := task.Toolchain{"go": "go1.27.1", "java": "21.0.4"}
	v := task.Validation{Status: task.StatusValid, Arms: []task.Arm{{Name: "base"}, {Name: "lean", Snapshot: "abc"}}, At: fresh, Toolchain: recorded, Repeats: 2,
		WeakTests: &task.WeakTests{Checked: 3}}
	for _, c := range []struct {
		name    string
		current task.Toolchain
		want    string
	}{
		{"same versions", task.Toolchain{"go": "go1.27.1", "java": "21.0.4"}, ""},
		{"an unrelated tool appeared", task.Toolchain{"go": "go1.27.1", "java": "21.0.4", "cargo": "cargo 1.90"}, ""},
		{"not detected", nil, ""},
		{"a tool missing now", task.Toolchain{"go": "go1.27.1"}, ""},
		{"nothing found now", task.Toolchain{}, ""},
		{"Go upgraded", task.Toolchain{"go": "go1.28", "java": "21.0.4"}, "go is go1.28 now, go1.27.1 then"},
	} {
		got := p.Stale(validated(t, v), now, c.current)
		if strings.Join(got.Reasons, "; ") != c.want {
			t.Errorf("%s: %q, want %q", c.name, got.Reasons, c.want)
		}
		if c.want != "" && (len(got.Arms) != 2 || got.Arms[1].Snapshot != "abc" || got.Repeat != 2 || got.WeakTests == nil || got.WeakTests.Checked != 3) {
			t.Errorf("%s: the re-validation must keep the arms, repeats and weak-tests result: %+v", c.name, got)
		}
	}
	// A validation from before toolchains were recorded is never stale by toolchain.
	old := validated(t, task.Validation{Status: task.StatusValid, At: fresh})
	if got := p.Stale(old, now, task.Toolchain{"go": "go1.28"}); len(got.Reasons) != 0 {
		t.Errorf("an older validation: %+v", got)
	}
	if got := p.Stale(store.Task{Validation: []byte(`{"status":"valid","arms":[],"stages":[],"at":"2026-09-30T00:00:00Z"}`)}, now, recorded); len(got.Reasons) != 0 {
		t.Errorf("stored without a toolchain: %+v", got)
	}
}

func TestStaleFlakyRetriedWeeklyWithRepeats(t *testing.T) {
	p := DefaultPolicy()
	for _, c := range []struct {
		age   time.Duration
		stale bool
	}{{6 * Day, false}, {7 * Day, false}, {8 * Day, true}} {
		got := p.Stale(validated(t, task.Validation{Status: task.StatusFlaky, At: now.Add(-c.age)}), now, nil)
		if (len(got.Reasons) > 0) != c.stale {
			t.Errorf("flaky, tried %v ago: %+v, want stale %v", c.age, got, c.stale)
		}
		if c.stale && (got.Repeat != 3 || got.Reasons[0] != "flaky, last tried on 2026-09-24 (retried every 7 days)" || len(got.Arms) != 1 || got.Arms[0].Name != "base") {
			t.Errorf("flaky retry: %+v", got)
		}
	}
	// A flaky task stale for another reason is tried with repeats too; a valid one keeps a single run.
	got := p.Stale(validated(t, task.Validation{Status: task.StatusFlaky, At: now.Add(-Day), Toolchain: task.Toolchain{"go": "a"}}), now, task.Toolchain{"go": "b"})
	if got.Repeat != 3 {
		t.Errorf("flaky by toolchain: %+v", got)
	}
	if got := p.Stale(validated(t, task.Validation{Status: task.StatusValid, At: now.Add(-40 * Day)}), now, nil); got.Repeat != 1 {
		t.Errorf("valid: %+v", got)
	}
}

func TestStaleSkipsNewAndRetiredTasksAndFlagsUnreadable(t *testing.T) {
	p := DefaultPolicy()
	if got := p.Stale(store.Task{Name: "new"}, now, nil); len(got.Reasons) != 0 {
		t.Errorf("never validated: %+v", got)
	}
	retired := validated(t, task.Validation{Status: task.StatusValid, At: now.Add(-100 * Day)})
	retired.RetiredAt, retired.RetiredReason = now, "old"
	if got := p.Stale(retired, now, nil); len(got.Reasons) != 0 {
		t.Errorf("retired: %+v", got)
	}
	if got := p.Stale(store.Task{Validation: []byte("{not json")}, now, nil); len(got.Reasons) == 0 || got.Reasons[0] != "its stored validation is unreadable" {
		t.Errorf("unreadable: %+v", got)
	}
}

func TestRetireByBaseAgeAtTheBoundary(t *testing.T) {
	p := DefaultPolicy()
	tk := store.Task{Name: "fix", SolutionCommit: "sol", Reference: []string{"p/p.go"}}
	for _, c := range []struct {
		age  time.Duration
		want string
	}{
		{269 * Day, ""},
		{270*Day - time.Second, ""},
		{270 * Day, "its base commit is 270 days old (270 or more retire)"},
		{271 * Day, "its base commit is 271 days old (270 or more retire)"},
	} {
		if got := p.Retire(tk, now, now.Add(-c.age), Head{}); got != c.want {
			t.Errorf("base %v old: %q, want %q", c.age, got, c.want)
		}
	}
	if got := p.Retire(tk, now, time.Time{}, Head{}); got != "" {
		t.Errorf("unknown base time: %q", got)
	}
	tk.RetiredAt, tk.RetiredReason = now, "old"
	if got := p.Retire(tk, now, now.Add(-400*Day), Head{}); got != "" {
		t.Errorf("retired already: %q", got)
	}
}

func TestRetireWhenANamedFileIsGone(t *testing.T) {
	p := DefaultPolicy()
	tk := store.Task{Name: "fix", SolutionCommit: "sol", HiddenTests: []string{"p/p_test.go"}, Reference: []string{"p/p.go", "p/q.go"}}
	files := map[string]bool{"p/p_test.go": true, "p/p.go": true, "p/q.go": true}
	solution := map[string]bool{"p/p_test.go": true, "p/p.go": true, "p/q.go": true}
	merged := func(c string) bool { return c == "sol" }
	head := Head{Has: func(f string) bool { return files[f] }, Contains: merged,
		InCommit: func(c, f string) bool { return c == "sol" && solution[f] }}
	recent := now.Add(-10 * Day)
	if got := p.Retire(tk, now, recent, head); got != "" {
		t.Errorf("every file present: %q", got)
	}
	delete(files, "p/q.go")
	if got := p.Retire(tk, now, recent, head); got != "p/q.go is gone from the default branch" {
		t.Errorf("a deleted reference file: %q", got)
	}
	delete(files, "p/p_test.go")
	if got := p.Retire(tk, now, recent, head); got != "p/p_test.go and 1 more files it names are gone from the default branch" {
		t.Errorf("a deleted hidden test too: %q", got)
	}
	// A task from a branch the default branch never merged: its files were never there.
	unmerged := head
	unmerged.Contains = func(string) bool { return false }
	if got := p.Retire(tk, now, recent, unmerged); got != "" {
		t.Errorf("an unmerged solution: %q", got)
	}
	if got := p.Retire(store.Task{Name: "manual"}, now, recent, Head{Has: func(string) bool { return false }, Contains: merged,
		InCommit: func(string, string) bool { return true }}); got != "" {
		t.Errorf("a task without a solution: %q", got)
	}
}

// The task's lists name the files its solution deleted and the old path of each rename (git diff --no-renames): they
// are absent from the solution too, so their absence at the head retires nothing. A file the solution has and the head
// lost still does.
func TestRetireIgnoresFilesTheSolutionDeletedOrRenamed(t *testing.T) {
	p := DefaultPolicy()
	tk := store.Task{Name: "fix", SolutionCommit: "sol",
		HiddenTests: []string{"p/old_test.go", "p/new_test.go"}, // the solution renamed old_test.go to new_test.go
		Reference:   []string{"p/p.go", "p/legacy.go"}}          // and deleted legacy.go
	solution := map[string]bool{"p/new_test.go": true, "p/p.go": true}
	files := map[string]bool{"p/new_test.go": true, "p/p.go": true}
	head := Head{Has: func(f string) bool { return files[f] }, Contains: func(string) bool { return true },
		InCommit: func(c, f string) bool { return c == "sol" && solution[f] }}
	if got := p.Retire(tk, now, now.Add(-Day), head); got != "" {
		t.Errorf("a deleted and a renamed file: %q, want no retirement", got)
	}
	delete(files, "p/new_test.go")
	if got := p.Retire(tk, now, now.Add(-Day), head); got != "p/new_test.go is gone from the default branch" {
		t.Errorf("the renamed test, then deleted at the head: %q", got)
	}
	head.InCommit = nil
	if got := p.Retire(tk, now, now.Add(-Day), head); got != "" {
		t.Errorf("without the solution's tree the file rule is off: %q", got)
	}
}

func TestHealthCounts(t *testing.T) {
	enc := func(v task.Validation) []byte {
		raw, _ := json.Marshal(v)
		return raw
	}
	bases := map[string]time.Time{"b1": now.Add(-50 * Day), "b2": now.Add(-90 * Day), "b3": now.Add(-300 * Day)}
	tasks := []store.Task{
		{Name: "valid", BaseCommit: "b1", Validation: enc(task.Validation{Status: task.StatusValid})},
		{Name: "weak", BaseCommit: "b2", NeedsReview: true, Validation: enc(task.Validation{Status: task.StatusValid,
			WeakTests: &task.WeakTests{Checked: 2, Untested: []task.Hunk{{File: "a.go", Start: 1, End: 2}}}})},
		{Name: "checked-not-weak", BaseCommit: "unknown", Validation: enc(task.Validation{Status: task.StatusValid, WeakTests: &task.WeakTests{Checked: 2}})},
		{Name: "flaky", BaseCommit: "b1", Validation: enc(task.Validation{Status: task.StatusFlaky})},
		{Name: "invalid", BaseCommit: "b1", Validation: enc(task.Validation{Status: task.StatusInvalid})},
		{Name: "unreadable", BaseCommit: "b1", Validation: []byte("{")},
		{Name: "unchecked", BaseCommit: "b1", Validation: enc(task.Validation{Status: task.StatusUnchecked})},
		{Name: "new", BaseCommit: "b1", NeedsReview: true},
		{Name: "retired", BaseCommit: "b3", NeedsReview: true, RetiredAt: now, RetiredReason: "old", Validation: enc(task.Validation{Status: task.StatusValid})},
	}
	h := HealthOf(tasks, func(c string) time.Time { return bases[c] }, now.Add(-time.Hour))
	want := Health{Total: 9, Valid: 3, Weak: 1, Flaky: 1, Invalid: 2, Unchecked: 1, Unvalidated: 1, AwaitingReview: 2, Retired: 1,
		LastPass: now.Add(-time.Hour), OldestValidBase: bases["b2"]}
	if h != want {
		t.Errorf("HealthOf = %+v\nwant       %+v", h, want)
	}
	if h.Valid+h.Flaky+h.Invalid+h.Unchecked+h.Unvalidated+h.Retired != h.Total {
		t.Errorf("the statuses do not partition the tasks: %+v", h)
	}
	empty := HealthOf(nil, func(string) time.Time { return time.Time{} }, time.Time{})
	raw, err := json.Marshal(empty)
	if err != nil || string(raw) != `{"total":0,"valid":0,"weak":0,"flaky":0,"invalid":0,"unchecked":0,"unvalidated":0,"awaiting_review":0,"retired":0}` {
		t.Errorf("empty health JSON: %s, %v", raw, err)
	}
}
