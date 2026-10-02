package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

// A validation records the toolchain it was given, as a copy, and an older validation without one still loads.
func TestValidationRecordsTheToolchain(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	v, _ := validator(t, f.bare)
	tools := Toolchain{"go": "go1.27.1", "java": `openjdk version "21.0.4"`}
	v.Toolchain = tools
	got, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{"true"}}, []Arm{{Name: "base"}})
	if err != nil {
		t.Fatal(err)
	}
	tools["go"] = "go1.28" // the caller's map changing later must not change the record
	if got.Toolchain["go"] != "go1.27.1" || got.Toolchain["java"] != `openjdk version "21.0.4"` {
		t.Errorf("Toolchain = %v", got.Toolchain)
	}
	raw, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(raw), `"toolchain":{"go":"go1.27.1"`) {
		t.Errorf("stored form: %s, %v", raw, err)
	}

	v.Toolchain = nil
	none, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{"true"}}, []Arm{{Name: "base"}})
	if raw, _ := json.Marshal(none); err != nil || strings.Contains(string(raw), "toolchain") {
		t.Errorf("without a toolchain the stored form gains a field: %s, %v", raw, err)
	}
	old := store.Task{Validation: []byte(`{"status":"valid","arms":[{"name":"base"}],"stages":[],"at":"2026-01-02T03:04:05Z"}`)}
	if loaded := ValidationOf(old); loaded.Status != StatusValid || loaded.Toolchain != nil || !loaded.At.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("an older validation = %+v", loaded)
	}
}

// With SkipInUse (the pool's re-validations), a finished validation of a task that a locked, unfinished experiment uses
// is not stored and the batch says why; task validate (no SkipInUse) stores it as before.
func TestSkipInUseLeavesLockedTasksAlone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	p, err := db.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	var tasks []store.Task
	for _, name := range []string{"locked", "free"} {
		saved, err := db.SaveTask(ctx, store.Task{ProjectID: p.ID, Name: name, Instruction: "Do it.", Source: "manual", BaseCommit: f.base,
			Verify: []string{"true"}, Validation: []byte(`{"status":"invalid"}`), CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, saved)
	}
	e, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: p.ID, Name: "lean", Template: "context-ab", Design: []byte(`{}`), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.LockExperiment(ctx, e.ID, []byte(`{"tasks":[{"name":"locked"}]}`)); err != nil {
		t.Fatal(err)
	}
	v := Validating{DB: db, Bare: f.bare, Artifacts: t.TempDir(), Now: func() time.Time { return now }, SkipInUse: true,
		Toolchain: Toolchain{"go": "go1.27.1"}}
	var out bytes.Buffer
	results := v.Batch(ctx, BatchOutput{Out: &out, Show: func(func() string) {}}, tasks, ValidateOptions{Arms: []Arm{{Name: "base"}}, Timeout: 30 * time.Second}, 1)
	locked, free := results[0], results[1]
	if locked.Validated || locked.Stopped || !errors.Is(locked.Err, store.ErrTaskInUse) || !strings.Contains(locked.Problem(), "not stored") ||
		!strings.Contains(locked.Problem(), "lean") {
		t.Errorf("locked: %+v, problem %q", locked, locked.Problem())
	}
	if got, _ := db.TaskByName(ctx, p.ID, "locked"); string(got.Validation) != `{"status":"invalid"}` {
		t.Errorf("a locked task's validation changed: %s", got.Validation)
	}
	if !free.Validated || ValidationOf(free.Task).Toolchain["go"] != "go1.27.1" {
		t.Errorf("free: %+v", free)
	}
	if !strings.Contains(out.String(), "locked  not stored") {
		t.Errorf("batch output:\n%s", out.String())
	}

	v.SkipInUse = false
	result, err := v.Quiet(ctx, tasks[0], ValidateOptions{Arms: []Arm{{Name: "base"}}, Timeout: 30 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := v.StoreValidation(ctx, tasks[0], result, now); err != nil || ValidationOf(stored).Status != StatusUnchecked {
		t.Errorf("task validate's store of a locked task = %s, %v", stored.Validation, err)
	}
}

// A re-validation with KeepWeakTests (the pool's, which never runs the weak-tests check) keeps the task's last
// weak-tests result; without it, as task validate --all, the result is dropped as before.
func TestBatchKeepsWeakTestsWhenAsked(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	p, err := db.SaveProject(ctx, "/work/app", "app", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	last := `{"status":"valid","arms":[{"name":"base"}],"stages":[],"at":"2026-08-01T00:00:00Z","weak_tests":{"checked":2,"untested":[{"file":"lib.go","start":3,"end":4}]}}`
	saved, err := db.SaveTask(ctx, store.Task{ProjectID: p.ID, Name: "weak", Instruction: "Do it.", Source: "manual", BaseCommit: f.base,
		Verify: []string{"true"}, Validation: []byte(last), CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	v := Validating{DB: db, Bare: f.bare, Artifacts: t.TempDir(), Now: func() time.Time { return now }}
	for _, keep := range []bool{true, false} {
		o := ValidateOptions{Arms: []Arm{{Name: "base"}}, Timeout: 30 * time.Second, KeepWeakTests: keep}
		got := v.Batch(ctx, BatchOutput{Out: &bytes.Buffer{}, Show: func(func() string) {}}, []store.Task{saved}, o, 1)[0]
		weak := ValidationOf(got.Task).WeakTests
		if !got.Validated || (keep && (weak == nil || len(weak.Untested) != 1 || weak.Untested[0].File != "lib.go")) || (!keep && weak != nil) {
			t.Errorf("keep %v: validated %v, weak tests %+v", keep, got.Validated, weak)
		}
		saved = got.Task
	}
}
