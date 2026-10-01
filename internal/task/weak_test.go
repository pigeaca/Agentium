package task

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A reference with one tested hunk (value.txt), one untested edit (a comment in notes.txt) and one new file nothing
// uses (extra.txt).
func weakFixture(t *testing.T) (Spec, Validator, *strings.Builder) {
	t.Helper()
	bare, base, ids := history(t, map[string]string{
		"run_tests.sh": "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n",
		"value.txt":    "old\n",
		"notes.txt":    "a\nb\nc\n",
	}, map[string]string{
		"tests/value_test.sh": "grep -q new value.txt\n",
		"value.txt":           "new\n",
		"notes.txt":           "a\nB\nc\n",
		"extra.txt":           "hello\n",
	})
	v, _ := validator(t, bare)
	var progress strings.Builder
	v.Progress = &progress
	v.WeakTests = true
	spec := Spec{Base: base, Solution: ids[0], HiddenTests: []string{"tests/value_test.sh"},
		Reference: []string{"extra.txt", "notes.txt", "value.txt"}, Verify: []string{"sh run_tests.sh"}}
	return spec, v, &progress
}

func TestWeakTestsListsUntestedHunks(t *testing.T) {
	spec, v, progress := weakFixture(t)
	got, err := v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
	if err != nil {
		t.Fatal(err)
	}
	w := got.WeakTests
	if got.Status != StatusValid || w == nil || w.Checked != 3 || w.Skipped != 0 || len(w.Untested) != 2 {
		t.Fatalf("status %s, weak tests %+v", got.Status, w)
	}
	if w.Untested[0].String() != "extra.txt:1" || w.Untested[1].String() != "notes.txt:2" { // a new file is one hunk
		t.Errorf("untested = %v", w.Untested)
	}
	if !strings.Contains(progress.String(), "NOT TESTED") || !strings.Contains(progress.String(), "value.txt:1") {
		t.Errorf("progress:\n%s", progress)
	}
	if entries, _ := os.ReadDir(v.WorkDir); len(entries) != 0 {
		t.Errorf("checkouts left behind: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(v.LogDir, "base-weak-3.log")); err != nil {
		t.Error(err)
	}
	raw, _ := json.Marshal(got)
	var back Validation
	if err := json.Unmarshal(raw, &back); err != nil || len(back.WeakTests.Untested) != 2 || back.WeakTests.Untested[1].File != "notes.txt" {
		t.Errorf("round trip: %v %+v", err, back.WeakTests)
	}
}

func TestWeakTestsCapSkipsAndCounts(t *testing.T) {
	spec, v, _ := weakFixture(t)
	v.MaxHunks = 2
	got, err := v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
	if err != nil || got.WeakTests.Checked != 2 || got.WeakTests.Skipped != 1 || len(got.WeakTests.Untested) != 2 {
		t.Errorf("%+v, %v", got.WeakTests, err)
	}
}

func TestWeakTestsTimeoutCountsAsTested(t *testing.T) {
	spec, v, progress := weakFixture(t)
	v.Timeout = 500 * time.Millisecond
	// Hangs only when notes.txt is reverted while value.txt is the reference's.
	spec.Verify = []string{"grep -q new value.txt && ! grep -q B notes.txt && sleep 5; sh run_tests.sh"}
	got, err := v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusValid || got.WeakTests.TimedOut != 1 || len(got.WeakTests.Untested) != 1 ||
		!strings.Contains(progress.String(), "tested (timed out)") {
		t.Errorf("%+v, %v\n%s", got.WeakTests, err, progress)
	}
}

func TestWeakTestsNeedsSolutionAndPassingReference(t *testing.T) {
	spec, v, _ := weakFixture(t)
	_, err := v.Validate(context.Background(), Spec{Base: spec.Base, Verify: spec.Verify}, []Arm{{Name: "base"}})
	if !errors.Is(err, ErrNoSolution) {
		t.Errorf("no solution: %v", err)
	}
	spec.Verify = []string{"false"} // the hidden tests fail as wanted, but the reference never passes
	got, err := v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusInvalid || got.WeakTests == nil || got.WeakTests.Reason == "" || got.WeakTests.Checked != 0 {
		t.Errorf("%+v, %v", got.WeakTests, err)
	}
}

func TestWeakTestsStopOnCancel(t *testing.T) {
	spec, v, _ := weakFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v.Started = func(arm, stage string) {
		if stage == "weak tests 2/3" {
			cancel()
		}
	}
	got, err := v.Validate(ctx, spec, []Arm{{Name: "base"}})
	if !errors.Is(err, context.Canceled) || got.WeakTests == nil || got.WeakTests.Checked != 1 {
		t.Errorf("%v, %+v", err, got.WeakTests)
	}
	if entries, _ := os.ReadDir(v.WorkDir); len(entries) != 0 {
		t.Errorf("checkouts left behind: %v", entries)
	}
}

func TestStoredValidationWithoutWeakTestsMeansNotChecked(t *testing.T) {
	var v Validation
	if err := json.Unmarshal([]byte(`{"status":"valid","arms":[],"stages":[],"at":"2026-09-28T12:00:00Z"}`), &v); err != nil || v.WeakTests != nil {
		t.Errorf("%v %+v", err, v.WeakTests)
	}
}

func TestHunkWithoutRestoresOnlyItsLines(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	sol := "a\nB\nc\nd\nx\ny\ne\n" // line 2 changed, lines 5-6 added
	change := Hunk{newStart: 2, newCount: 1, baseStart: 2, baseCount: 1}
	insert := Hunk{newStart: 5, newCount: 2, baseStart: 4, baseCount: 0}
	if got := change.without(sol, base); got != "a\nb\nc\nd\nx\ny\ne\n" {
		t.Errorf("change undone: %q", got)
	}
	if got := insert.without(sol, base); got != "a\nB\nc\nd\ne\n" {
		t.Errorf("insertion undone: %q", got)
	}
	deleted := Hunk{newStart: 1, newCount: 0, baseStart: 2, baseCount: 2} // the solution dropped b and c after line 1
	if got := deleted.without("a\nd\ne\n", base); got != "a\nb\nc\nd\ne\n" {
		t.Errorf("deletion undone: %q", got)
	}
}
