package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/task"
)

// validationGrade makes a grade folder a validation of task id left at <artifacts>/tasks/<id>/<at>/grading/<label>,
// as a dead sandboxed grade leaves it: its copy (with a hidden test), cache and temp root, the folder read-only
// (grading.lock), last changed ago before now.
func validationGrade(t *testing.T, l home.Layout, id, at, label string, now time.Time, ago time.Duration) string {
	t.Helper()
	root := filepath.Join(l.Artifacts, "tasks", id, at, "grading", label)
	fill(t, filepath.Join(root, "copy", "tests"), 100)
	fill(t, filepath.Join(root, "cache"), 100)
	must(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o700))
	must(t, os.Chmod(root, 0o500))
	usedAgo(t, root, now, ago)
	return root
}

// A dry run lists the grade folders stopped validations left, keeps the one a validation is grading in (its lock is
// held) and a recent one, never lists a link or what else a validation keeps, and writes nothing (no lock file).
func TestCleanPlansValidationGrades(t *testing.T) {
	t.Parallel()
	l := cleanLayout(t)
	now := time.Now()
	at := "20261001T120000Z"
	stopped := validationGrade(t, l, "7", at, "base-hidden-tests", now, 3*time.Hour)
	recent := validationGrade(t, l, "7", at, "base-reference", now, 10*time.Minute)
	grading := validationGrade(t, l, "8", at, "base-reference", now, 5*time.Hour)
	unlock, err := home.LockFile(context.Background(), task.GradeLock(grading), nil) // a validation grading there
	must(t, err)
	defer unlock()
	// Not grade folders: the validation's checkouts and logs, a link in a grade's place, a task folder that is a link.
	fill(t, filepath.Join(l.Artifacts, "tasks", "7", at, "checkouts", "base-reference"), 10)
	fill(t, filepath.Join(l.Artifacts, "tasks", "7", at, "logs"), 10)
	outside := t.TempDir()
	fill(t, filepath.Join(outside, "victim"), 10)
	must(t, os.Symlink(filepath.Join(outside, "victim"), filepath.Join(l.Artifacts, "tasks", "7", at, "grading", "linked")))
	must(t, os.MkdirAll(filepath.Join(outside, at, "grading", "x"), 0o700))
	usedAgo(t, filepath.Join(outside, at, "grading", "x"), now, 5*time.Hour)
	must(t, os.Symlink(outside, filepath.Join(l.Artifacts, "tasks", "9")))
	// Lock files without their folder: an old one goes, a recent one stays, a link in a lock's place is not listed.
	strayOld, strayNew := filepath.Join(l.Artifacts, "tasks", "7", at, "grading", "arm-reference.lock"), filepath.Join(l.Artifacts, "tasks", "7", at, "grading", "arm-hidden-tests.lock")
	for _, p := range []string{strayOld, strayNew} {
		must(t, os.WriteFile(p, nil, 0o600))
	}
	usedAgo(t, strayOld, now, 2*time.Hour)
	must(t, os.Symlink(filepath.Join(outside, "victim"), filepath.Join(l.Artifacts, "tasks", "7", at, "grading", "gone.lock")))
	items := 5
	var inUse string
	if runtime.GOOS == "darwin" { // a process in an old folder (a validation from before the lock, or your shell) keeps it
		inUse = validationGrade(t, l, "8", at, "base-hidden-tests", now, 5*time.Hour)
		leftover(t, filepath.Join(inUse, "copy"), "")
		items++
	}
	before := snapshotTree(t, l.Artifacts)

	plan, err := PlanClean(context.Background(), CleanInput{Layout: l, Now: now, OlderThan: CleanDefaultAge})
	if err != nil {
		t.Fatal(err)
	}
	if it, ok := byPath(plan.Remove, stopped); !ok || it.Kind != CleanValidations || it.Reason != CleanStoppedValidation || it.Bytes < 200 ||
		!strings.Contains(it.Detail, "task 7") {
		t.Errorf("the stopped validation's grade: %+v, %v", it, ok)
	}
	if it, ok := byPath(plan.Keep, recent); !ok || it.Reason != CleanKeptRecent {
		t.Errorf("the recent grade: %+v, %v", it, ok)
	}
	if it, ok := byPath(plan.Keep, grading); !ok || it.Reason != CleanKeptRunning {
		t.Errorf("the grade in progress: %+v, %v", it, ok)
	}
	if it, ok := byPath(plan.Remove, strayOld); !ok || it.Reason != CleanStoppedValidation || it.lock != strayOld {
		t.Errorf("an old stray lock: %+v, %v", it, ok)
	}
	if it, ok := byPath(plan.Keep, strayNew); !ok || it.Reason != CleanKeptRecent {
		t.Errorf("a recent stray lock: %+v, %v", it, ok)
	}
	if it, ok := byPath(plan.Keep, inUse); inUse != "" && (!ok || it.Reason != CleanKeptRunning || !strings.Contains(it.Detail, "in use by 1 process(es)")) {
		t.Errorf("a folder in use: %+v, %v", it, ok)
	}
	if n := len(plan.Remove) + len(plan.Keep); n != items {
		t.Errorf("%d items, want %d: %+v %+v", n, items, plan.Remove, plan.Keep)
	}
	if after := snapshotTree(t, l.Artifacts); after != before {
		t.Errorf("a dry run wrote the artifacts:\n%s\nwas\n%s", after, before)
	}
}

// --yes removes a stopped validation's grade folder (read-only, as a dead grade leaves it) and its lock file, and stops
// what the grade left running there first; it keeps a folder a validation is grading in, or one changed since the plan,
// without a failure; it refuses anything that is not a grade folder in its place.
func TestCleanRemovesValidationGrades(t *testing.T) {
	l := cleanLayout(t)
	now := time.Now()
	at := "20261001T120000Z"
	stopped := validationGrade(t, l, "7", at, "base-hidden-tests", now, 3*time.Hour)
	must(t, os.WriteFile(task.GradeLock(stopped), nil, 0o600)) // its dead validation's, free
	changed := validationGrade(t, l, "7", at, "base-reference", now, 3*time.Hour)
	grading := validationGrade(t, l, "8", at, "base-reference", now, 3*time.Hour)
	inUse := validationGrade(t, l, "8", at, "base-hidden-tests", now, 3*time.Hour)
	stray := filepath.Join(l.Artifacts, "tasks", "8", at, "grading", "arm-reference.lock")
	must(t, os.WriteFile(stray, nil, 0o600))
	usedAgo(t, stray, now, 2*time.Hour)
	plan, err := PlanClean(context.Background(), CleanInput{Layout: l, Now: now, OlderThan: CleanDefaultAge})
	must(t, err)
	usedAgo(t, changed, time.Now(), 0) // used since the plan
	unlock, err := home.LockFile(context.Background(), task.GradeLock(grading), nil)
	must(t, err)
	defer unlock()
	var ended <-chan struct{}
	if runtime.GOOS == "darwin" { // a process starts using the folder after the plan: kept, never stopped
		_, ended = leftover(t, filepath.Join(inUse, "copy"), "")
	}

	var items []CleanItem
	for _, p := range []string{stopped, changed, grading, inUse, stray} {
		it, ok := byPath(plan.Remove, p)
		if !ok {
			t.Fatalf("%s is not planned", p)
		}
		items = append(items, it)
	}
	errs := RemoveClean(context.Background(), l, items)
	if errs[0] != nil || exists(stopped) || exists(task.GradeLock(stopped)) {
		t.Errorf("the stopped grade: %v, still there %v, its lock %v", errs[0], exists(stopped), exists(task.GradeLock(stopped)))
	}
	if !errors.Is(errs[1], ErrCleanUsed) || !exists(changed) {
		t.Errorf("changed since the plan: %v", errs[1])
	}
	if !errors.Is(errs[2], ErrCleanBusy) || !strings.Contains(errs[2].Error(), "validation") || !exists(filepath.Join(grading, "copy", "tests", "data")) {
		t.Errorf("a grade in progress: %v", errs[2])
	}
	if ended != nil {
		select {
		case <-ended:
			t.Error("cleanup stopped a process that used the grade folder")
		default:
		}
		if !errors.Is(errs[3], ErrCleanBusy) || !strings.Contains(errs[3].Error(), "in use by 1 process(es)") || !exists(filepath.Join(inUse, "copy", "tests", "data")) {
			t.Errorf("a folder in use: %v", errs[3])
		}
	}
	if errs[4] != nil || exists(stray) {
		t.Errorf("a stray lock: %v, still there %v", errs[4], exists(stray))
	}
	// A lock that is a link is refused, never followed (the probe uses O_NOFOLLOW, the removal checks first).
	victim := filepath.Join(t.TempDir(), "victim")
	must(t, os.WriteFile(victim, []byte("yours"), 0o600))
	linkedRoot := validationGrade(t, l, "10", at, "base-reference", now, 3*time.Hour)
	must(t, os.Symlink(victim, task.GradeLock(linkedRoot)))
	linkedLock := filepath.Join(l.Artifacts, "tasks", "10", at, "grading", "x.lock")
	must(t, os.Symlink(victim, linkedLock))
	for _, it := range []CleanItem{{Kind: CleanValidations, Path: linkedRoot, lock: task.GradeLock(linkedRoot)}, {Kind: CleanValidations, Path: linkedLock, lock: linkedLock}} {
		if errs := RemoveClean(context.Background(), l, []CleanItem{it}); errs[0] == nil || !strings.Contains(errs[0].Error(), "refused") {
			t.Errorf("a linked lock %s: %v", it.lock, errs[0])
		}
	}
	if data, _ := os.ReadFile(victim); string(data) != "yours" || !exists(linkedRoot) {
		t.Errorf("a linked lock: the file it points to %q, the folder kept %v", data, exists(linkedRoot))
	}

	// Refused whatever a plan says: elsewhere in the artifacts, the grading folder itself, a grade reached through a link.
	outside := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(outside, at, "grading", "x"), 0o700))
	must(t, os.Symlink(outside, filepath.Join(l.Artifacts, "tasks", "9")))
	fill(t, filepath.Join(l.Artifacts, "tasks", "7", at, "checkouts"), 10)
	for _, p := range []string{filepath.Join(l.Artifacts, "tasks", "7", at, "checkouts"), filepath.Join(l.Artifacts, "tasks", "7", at, "grading"),
		filepath.Join(l.Artifacts, "tasks", "9", at, "grading", "x"), filepath.Join(l.Artifacts, "tasks", "7", at, "logs", "x"),
		filepath.Join(l.Artifacts, "tasks", "7", at, "grading", "..", "checkouts"), filepath.Join(l.Records, "r1", "grading"), outside} {
		errs := RemoveClean(context.Background(), l, []CleanItem{{Kind: CleanValidations, Path: p, lock: task.GradeLock(p)}})
		if errs[0] == nil || !strings.Contains(errs[0].Error(), "refused") {
			t.Errorf("RemoveClean(%s): %v", p, errs[0])
		}
	}
	if !exists(filepath.Join(outside, at, "grading", "x")) || !exists(filepath.Join(l.Artifacts, "tasks", "7", at, "checkouts")) {
		t.Error("a refused removal removed something")
	}
}

// snapshotTree lists every path under dir with its mode and modification time, never following a link.
func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	must(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		lines = append(lines, p+" "+info.Mode().String()+" "+info.ModTime().String())
		return nil
	}))
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}
