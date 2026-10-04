package run

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/task"
)

// A sandboxed validation grades each stage in a folder of its own, <artifacts>/tasks/<id>/<time>/grading/<label>
// (task.Validator), removed when the grade ends; a validation that dies leaves it behind, with its copy (hidden tests
// included), its cache and its temp root. Recovery does not know these folders (validations take no run lock and
// write no start file), so cleanup removes them, under its rules:
//   - never one in use: the validation holds the folder's lock (task.GradeLock) from before the folder exists until it
//     is gone, and the system drops a dead process's lock; a dry run only probes the lock, with a shared lock it
//     releases at once, and creates nothing (a folder of a validation from before the lock has none: it is not in use);
//   - nothing changed within CleanGrace (the folder's own modification time: a grade writes its entries at its start);
//   - the removal is a grade's own (grading.stop, then removeOrQuarantine), under the lock taken exclusively: the
//     grade's sandbox first, while its folder may still be read-only (grading.lock), then what uses its folders; then
//     the folder is made writable and removed, never following a link, and what resists removal goes to the quarantine.

// gradingTasks is the artifacts' folder of the tasks' validations; validationGrading the folder of a validation's
// grade folders in its own folder.
const (
	gradingTasks      = "tasks"
	validationGrading = "grading"
)

// validations plans the grade folders that validations left: each <artifacts>/tasks/<id>/<time>/grading/<label>
// that is a real folder (links are never listed), kept while its lock is held or within CleanGrace of its last change.
func (c *planner) validations(ctx context.Context) error {
	artifacts := c.in.Layout.Artifacts
	tasks := filepath.Join(artifacts, gradingTasks)
	if artifacts == "" || !realFolder(artifacts) || !realFolder(tasks) {
		return nil
	}
	ids, err := realDirs(tasks)
	if err != nil {
		return err
	}
	for _, id := range ids {
		times, err := realDirs(filepath.Join(tasks, id.Name()))
		if err != nil {
			return err
		}
		for _, at := range times {
			grading := filepath.Join(tasks, id.Name(), at.Name(), validationGrading)
			if !realFolder(grading) {
				continue
			}
			roots, err := realDirs(grading)
			if err != nil {
				return err
			}
			for _, r := range roots {
				if err := ctx.Err(); err != nil {
					return err
				}
				path := filepath.Join(grading, r.Name())
				it := CleanItem{Kind: CleanValidations, Path: path, Bytes: treeSize(path), LastUsed: modTime(path), lock: task.GradeLock(path)}
				it.recheck = func() time.Time { return modTime(path) }
				age := c.in.Now.Sub(it.LastUsed)
				switch {
				case gradeLockHeld(it.lock):
					c.add(it, verdict{reason: CleanKeptRunning, detail: "a validation of task " + id.Name() + " is grading in it"})
				case age < CleanGrace:
					c.add(it, verdict{reason: CleanKeptRecent, detail: "changed " + ago(age) + " ago"})
				default:
					c.add(it, verdict{gone: true, reason: CleanStoppedValidation, detail: "the grade folder of a validation of task " + id.Name() + " that stopped"})
				}
			}
		}
	}
	return nil
}

// gradeLockHeld reports whether a validation holds the lock file at path (task.GradeLock). It probes with a shared
// lock, released at once, which a validation waiting for its exclusive lock outlasts, and creates nothing: a missing
// lock is a free one.
func gradeLockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// errValidationGrading is RemoveClean's error for a grade folder whose validation holds its lock: kept, and not a
// failure (errors.Is(err, ErrCleanBusy)).
type errValidationGrading struct{}

func (errValidationGrading) Error() string {
	return "a validation is grading in it: kept; try again later"
}
func (errValidationGrading) Is(target error) bool { return target == ErrCleanBusy }

// removeValidationGrade removes a grade folder a stopped validation left, under its lock (taken exclusively, so no
// validation can be grading in it), unless it changed since the plan: what the grade left running is stopped first, as
// grading.stop does (the grade's sandbox, then what uses its folders), and an error there keeps the folder. The lock
// file goes with the folder.
func removeValidationGrade(ctx context.Context, layout home.Layout, it CleanItem) error {
	if err := gradeCleanable(layout, it.Path); err != nil {
		return err
	}
	if it.lock != task.GradeLock(it.Path) {
		return fmt.Errorf("refused: %s is not the lock of %s", it.lock, it.Path)
	}
	waitCtx, cancel := context.WithTimeout(ctx, cleanLockWait)
	unlock, err := home.LockFile(waitCtx, it.lock, nil)
	cancel()
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return errValidationGrading{}
	case err != nil:
		return fmt.Errorf("lock: %w", err)
	}
	defer unlock()
	if _, err := os.Lstat(it.Path); errors.Is(err, fs.ErrNotExist) {
		os.Remove(it.lock)
		return nil // its validation finished meanwhile and removed it
	}
	if it.recheck != nil && it.recheck().After(it.LastUsed) {
		return ErrCleanUsed
	}
	if _, err := stopSandboxed(filepath.Join(it.Path, "tmp"), filepath.Dir(it.Path)); err != nil {
		return fmt.Errorf("stop what the grade left running: %w", err)
	}
	if _, err := stopUsing(buildtool.Profiles(), filepath.Join(it.Path, "cache"), it.Path); err != nil {
		return fmt.Errorf("stop what the grade left running: %w", err)
	}
	if info, err := os.Lstat(it.Path); err == nil && info.IsDir() {
		if err := os.Chmod(it.Path, 0o700); err != nil { // a dead grade's folder may still be read-only (grading.lock)
			return err
		}
	}
	warning, err := removeOrQuarantine(it.Path, quarantine(layout))
	if err != nil {
		return err
	}
	os.Remove(it.lock)
	if warning != "" {
		return errors.New(warning) // out of the artifacts, but not freed: the quarantine holds it
	}
	return nil
}

// gradeCleanable refuses any path but a validation's grade folder in the data folder's artifacts:
// <artifacts>/tasks/<id>/<time>/grading/<label>, each folder from the artifacts down a real folder (a link there would
// lead the removal elsewhere), and the grade folder itself a real folder, never a link.
func gradeCleanable(layout home.Layout, path string) error {
	if layout.Artifacts == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("refused: %s is not a validation's grade folder", path)
	}
	root := filepath.Clean(layout.Artifacts)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("refused: %s is not a validation's grade folder", path)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 5 || parts[0] != gradingTasks || parts[3] != validationGrading {
		return fmt.Errorf("refused: %s is not a validation's grade folder", path)
	}
	dir := root
	for _, part := range parts {
		if part == "." || part == ".." || part == "" {
			return fmt.Errorf("refused: %s is not a validation's grade folder", path)
		}
		if !realFolder(dir) {
			return fmt.Errorf("refused: %s is not a real folder (a link, or gone), so %s is not removed", dir, path)
		}
		dir = filepath.Join(dir, part)
	}
	if info, err := os.Lstat(path); err == nil && !info.IsDir() {
		return fmt.Errorf("refused: %s is not a real folder", path)
	}
	return nil
}
