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

	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/task"
)

// A sandboxed validation grades each stage in a folder of its own, <artifacts>/tasks/<id>/<time>/grading/<label>
// (task.Validator), removed when the grade ends; a validation that dies leaves it behind, with its copy (hidden tests
// included), its cache and its temp root, and may leave its lock file (<label>.lock) without a folder. Recovery does
// not know these folders (validations take no run lock and write no start file), so cleanup removes them, under its
// rules:
//   - never one in use: the validation holds the folder's lock (task.GradeLock) from before the folder exists until it
//     is gone, and the system drops a dead process's lock; and a folder some process still uses (one of its grade's,
//     a validation from before the lock, or the user's own shell or editor there) is kept, never stopped
//     (gradeInUse). A dry run only probes the lock, with a shared lock it releases at once, and creates nothing;
//   - nothing changed within CleanGrace (the folder's own modification time: a grade writes its entries at its start;
//     a lock file's, which it gets when taken);
//   - the removal is a grade's own (removeOrQuarantine: never following a link, what resists removal goes to the
//     quarantine), under the lock taken exclusively, once nothing uses the folder.
//
// Not cleaned: a dead validation's checkouts (<time>/checkouts/<label>, where a stage's hidden tests may lie before and
// after its grade). Nothing tells them from the ones `task validate --keep` keeps for the user, and host-mode
// validations take no lock.

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
				c.add(it, c.gradeVerdict(it, id.Name(), "the grade folder of a validation of task "+id.Name()+" that stopped"))
			}
			// Lock files without their folder: a validation stopped between taking the lock and making the folder.
			entries, err := os.ReadDir(grading)
			if err != nil {
				return fmt.Errorf("clean: %w", err)
			}
			for _, e := range entries {
				label, ok := strings.CutSuffix(e.Name(), ".lock")
				if !ok || !e.Type().IsRegular() || lexists(filepath.Join(grading, label)) {
					continue
				}
				path := filepath.Join(grading, e.Name())
				it := CleanItem{Kind: CleanValidations, Path: path, LastUsed: modTime(path), lock: path}
				it.recheck = func() time.Time { return modTime(path) }
				c.add(it, c.gradeVerdict(it, id.Name(), "the lock of a grade a stopped validation of task "+id.Name()+" began"))
			}
		}
	}
	return nil
}

// gradeVerdict decides for a validation's grade folder or stray lock: kept while its lock is held, while a process uses
// the folder, or within CleanGrace of its last change; otherwise it goes, as gone says.
func (c *planner) gradeVerdict(it CleanItem, id, gone string) verdict {
	age := c.in.Now.Sub(it.LastUsed)
	switch {
	case gradeLockHeld(it.lock):
		return verdict{reason: CleanKeptRunning, detail: "a validation of task " + id + " is grading in it"}
	case age < CleanGrace:
		return verdict{reason: CleanKeptRecent, detail: "changed " + ago(age) + " ago"}
	}
	if it.Path != it.lock {
		if using, _ := gradeInUse(it.Path); len(using) > 0 {
			return verdict{reason: CleanKeptRunning, detail: inUseDetail(using)}
		}
	}
	return verdict{gone: true, reason: CleanStoppedValidation, detail: gone}
}

// gradeInUse lists, without stopping any, the processes that run in the grade's sandbox (one that left the folder
// included) or use its folders: "<pid> <command>" each.
func gradeInUse(root string) ([]string, error) {
	return usingGrade(filepath.Join(root, "tmp"), filepath.Dir(root), []string{root})
}

// inUseDetail says which processes keep a grade folder: their IDs only (a grade picks its processes' names).
func inUseDetail(using []string) string {
	ids := make([]string, len(using))
	for i, u := range using {
		ids[i], _, _ = strings.Cut(u, " ")
	}
	return fmt.Sprintf("in use by %d process(es) (%s): a validation from an older Agentium, what its grade left, or your own shell there; kept, never stopped",
		len(using), strings.Join(ids, ", "))
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

// errValidationGrading is RemoveClean's error for a grade folder a validation holds the lock of, or some process uses:
// kept, and not a failure (errors.Is(err, ErrCleanBusy)).
type errValidationGrading struct{ detail string }

func (e errValidationGrading) Error() string {
	if e.detail != "" {
		return e.detail
	}
	return "a validation is grading in it: kept; try again later"
}
func (errValidationGrading) Is(target error) bool { return target == ErrCleanBusy }

// removeValidationGrade removes a grade folder a stopped validation left, or a stray lock file of one (the item's Path
// is then its lock), under the lock (taken exclusively, so no validation can be grading there), unless it changed
// since the plan or some process uses the folder: cleanup stops no process for a validation, since it cannot tell a
// grade's from the user's own shell there. The lock file goes with the folder.
func removeValidationGrade(ctx context.Context, layout home.Layout, it CleanItem) error {
	stray := it.Path == it.lock
	if err := gradeCleanable(layout, it.Path, stray); err != nil {
		return err
	}
	if !stray && it.lock != task.GradeLock(it.Path) {
		return fmt.Errorf("refused: %s is not the lock of %s", it.lock, it.Path)
	}
	unlock, err := lockGrade(ctx, it.lock)
	if err != nil {
		return err
	}
	defer unlock()
	if stray {
		if label, _ := strings.CutSuffix(it.Path, ".lock"); lexists(label) {
			return ErrCleanUsed // its folder is there now
		}
		if it.recheck != nil && it.recheck().After(it.LastUsed) {
			return ErrCleanUsed
		}
		return os.Remove(it.Path)
	}
	if _, err := os.Lstat(it.Path); errors.Is(err, fs.ErrNotExist) {
		os.Remove(it.lock)
		return nil // its validation finished meanwhile and removed it
	}
	if it.recheck != nil && it.recheck().After(it.LastUsed) {
		return ErrCleanUsed
	}
	using, err := gradeInUse(it.Path)
	if err != nil {
		return fmt.Errorf("what uses it could not be listed, so it was kept: %w", err)
	}
	if len(using) > 0 {
		return errValidationGrading{detail: inUseDetail(using)}
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

// lexists reports whether anything (a link too) is at p.
func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// lockGrade takes a grade's lock file exclusively, as its validation does (home.LockFile), waiting cleanLockWait at
// most (then errValidationGrading). A lock path that is there but not a regular file (a link) is refused, as the
// probe (gradeLockHeld) never follows one.
func lockGrade(ctx context.Context, path string) (unlock func(), err error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refused: the lock %s is not a regular file", path)
	}
	waitCtx, cancel := context.WithTimeout(ctx, cleanLockWait)
	unlock, err = home.LockFile(waitCtx, path, nil)
	cancel()
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return nil, errValidationGrading{}
	case err != nil:
		return nil, fmt.Errorf("lock: %w", err)
	}
	return unlock, nil
}

// gradeCleanable refuses any path but a validation's grade folder in the data folder's artifacts:
// <artifacts>/tasks/<id>/<time>/grading/<label>, each folder from the artifacts down a real folder (a link there would
// lead the removal elsewhere), and the grade folder itself a real folder, never a link; with lock, the path is a grade's
// lock file there (<label>.lock), a regular file.
func gradeCleanable(layout home.Layout, path string, lock bool) error {
	if layout.Artifacts == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || lock != strings.HasSuffix(path, ".lock") {
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
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return nil // gone: nothing to remove
	case lock && !info.Mode().IsRegular():
		return fmt.Errorf("refused: %s is not a regular file", path)
	case !lock && !info.IsDir():
		return fmt.Errorf("refused: %s is not a real folder", path)
	}
	return nil
}
