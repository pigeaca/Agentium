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

	"github.com/pigeaca/agentium/internal/gitx"
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

// weakRepo builds a base commit and a solution commit by running edit in a user repository, and fetches both.
// edit makes the solution commit. prepare, when set, edits the working tree first and its result is part of the base.
func weakRepo(t *testing.T, baseFiles map[string]string, prepare, edit func(t *testing.T, user string)) (bare, base, solution string) {
	t.Helper()
	ctx := context.Background()
	user := t.TempDir()
	git(t, user, "init", "-q", "-b", "main")
	base = commit(t, user, baseFiles, "base")
	if prepare != nil {
		prepare(t, user)
		git(t, user, "add", "-A")
		git(t, user, "commit", "-q", "-m", "prepared base")
		base = git(t, user, "rev-parse", "HEAD")
	}
	edit(t, user) // commits the solution itself
	solution = git(t, user, "rev-parse", "HEAD")
	bare = filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{base, solution} {
		if err := gitx.FetchCommit(ctx, user, c, gitx.SourceRef(c), "--git-dir", bare); err != nil {
			t.Fatal(err)
		}
	}
	return bare, base, solution
}

func weakValidator(t *testing.T, bare string) Validator {
	v, _ := validator(t, bare)
	v.WeakTests = true
	return v
}

var runTests = "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n"

// A deleted executable file comes back executable, so a verification that runs it still passes: the hunk is untested.
func TestWeakTestsRestoresDeletedExecutable(t *testing.T) {
	bare, base, solution := weakRepo(t, map[string]string{"run_tests.sh": runTests, "value.txt": "old\n", "tool.sh": "#!/bin/sh\nexit 0\n"},
		func(t *testing.T, user string) {
			if err := os.Chmod(filepath.Join(user, "tool.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		func(t *testing.T, user string) {
			os.Remove(filepath.Join(user, "tool.sh"))
			commit(t, user, map[string]string{"tests/value_test.sh": "grep -q new value.txt\n", "value.txt": "new\n"}, "tests")
		})
	v := weakValidator(t, bare)
	spec := Spec{Base: base, Solution: solution, HiddenTests: []string{"tests/value_test.sh"}, Reference: []string{"tool.sh", "value.txt"},
		Verify: []string{"sh run_tests.sh && { [ ! -e tool.sh ] || ./tool.sh; }"}}
	got, err := v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusValid || got.WeakTests == nil || got.WeakTests.Checked != 2 {
		t.Fatalf("%s, %+v, %v", got.Status, got.WeakTests, err)
	}
	if u := got.WeakTests.Untested; len(u) != 1 || u[0].File != "tool.sh" || !u[0].Deleted {
		t.Errorf("untested = %+v (a restored executable must still run)", u)
	}
}

func TestWeakTestsReasonWhenNoTextHunks(t *testing.T) {
	bare, base, solution := weakRepo(t, map[string]string{"run_tests.sh": runTests, "value.txt": "old\n"}, nil, func(t *testing.T, user string) {
		commit(t, user, map[string]string{"tests/blob_test.sh": "test -f blob.bin\n", "blob.bin": "\x00\x01\x02binary"}, "binary")
	})
	v := weakValidator(t, bare)
	got, err := v.Validate(context.Background(), Spec{Base: base, Solution: solution, HiddenTests: []string{"tests/blob_test.sh"},
		Reference: []string{"blob.bin"}, Verify: []string{"sh run_tests.sh"}}, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusValid || got.WeakTests == nil || !strings.Contains(got.WeakTests.Reason, "no text hunks") || got.WeakTests.Checked != 0 {
		t.Errorf("%+v, %v", got.WeakTests, err)
	}
}

func TestHunksSkipSymlinksAndDoNotMerge(t *testing.T) {
	bare, base, solution := weakRepo(t, map[string]string{"value.txt": "1\n2\n3\n4\n5\n", "other.txt": "x\n"}, nil, func(t *testing.T, user string) {
		if err := os.Symlink("value.txt", filepath.Join(user, "link")); err != nil {
			t.Fatal(err)
		}
		commit(t, user, map[string]string{"value.txt": "1\nTWO\n3\nFOUR\n5\n"}, "edit") // two hunks two lines apart
	})
	hunks, err := Hunks(context.Background(), base, solution, []string{"link", "value.txt"}, "--git-dir", bare)
	if err != nil || len(hunks) != 2 || hunks[0].File != "value.txt" || hunks[0].Start != 2 || hunks[1].Start != 4 {
		t.Errorf("%v, %v", hunks, err)
	}
}

func TestWeakTestsSkipReasonNamesTheStage(t *testing.T) {
	spec, v, _ := weakFixture(t)
	spec.Verify = []string{"true"} // the hidden tests pass on the base: that stage fails and the reference never runs
	got, err := v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
	if err != nil || got.WeakTests == nil || !strings.Contains(got.WeakTests.Reason, "hidden-tests stage") {
		t.Errorf("%+v, %v", got.WeakTests, err)
	}
	spec.Verify = []string{"false"}
	got, _ = v.Validate(context.Background(), spec, []Arm{{Name: "base"}})
	if got.WeakTests == nil || !strings.Contains(got.WeakTests.Reason, "reference stage") {
		t.Errorf("%+v", got.WeakTests)
	}
}

// A path that turns into a symlink, or back, is two diffs (a delete and an add): neither is a text hunk to take out.
func TestHunksSkipTypeChangesBetweenFileAndSymlink(t *testing.T) {
	bare, base, solution := weakRepo(t, map[string]string{"a.txt": "1\n2\n", "b.txt": "x\n", "keep.txt": "k\n"}, func(t *testing.T, user string) {
		if err := os.Symlink("keep.txt", filepath.Join(user, "l")); err != nil {
			t.Fatal(err)
		}
	}, func(t *testing.T, user string) {
		os.Remove(filepath.Join(user, "a.txt"))
		os.Remove(filepath.Join(user, "l"))
		if err := os.Symlink("keep.txt", filepath.Join(user, "a.txt")); err != nil { // file -> link
			t.Fatal(err)
		}
		commit(t, user, map[string]string{"l": "now a file\n", "b.txt": "y\n"}, "type changes") // link -> file
	})
	hunks, err := Hunks(context.Background(), base, solution, []string{"a.txt", "b.txt", "l"}, "--git-dir", bare)
	if err != nil || len(hunks) != 1 || hunks[0].File != "b.txt" {
		t.Errorf("%v, %v", hunks, err)
	}
}
