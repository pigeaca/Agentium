package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// leftovers lists what recovery (RecoverWarn) would remove of runs whose Agentium process died, without changing
// anything: it reads the start files as Recover does and follows its decisions, so that a dry run can show them.
//   - A stored run: its leftover grading copy, grade folder and pair judge's folder (cleanGrade).
//   - A run without a start file: its records folder (nothing was prepared yet), unless an earlier recovery set its
//     unreadable start file aside.
//   - A run with a start file: kept while its process group exists (it may still be running); otherwise its workspace,
//     temp root, grading copy and grade folder, and the judges' folders of a finished run, or its whole records folder
//     when its agent never started. A run whose agent started keeps its records: recovery stores it as cancelled.
//   - A run whose start file cannot be read is kept: recovery decides when it is safe (recoverUnreadable).
//
// Call it holding the run lock, or knowing that no run is in progress: a live run between two commands looks dead.
func (c *planner) leftovers(ctx context.Context) error {
	layout := c.in.Layout
	entries, err := realDirs(layout.Records)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, dir := e.Name(), filepath.Join(layout.Records, e.Name())
		known, err := c.in.Stored(id)
		if err != nil {
			return err
		}
		it := CleanItem{Kind: CleanLeftovers, Path: dir, LastUsed: modTime(dir)}
		gone := verdict{gone: true, reason: CleanStoppedRun}
		if known {
			it.parts = existingParts(filepath.Join(dir, "verify"), filepath.Join(dir, gradingFolder), filepath.Join(dir, pairJudgeFolder))
			if len(it.parts) > 0 {
				gone.detail = "grading folders of a stored run"
				c.addLeftover(it, gone)
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, startFile))
		if errors.Is(err, fs.ErrNotExist) {
			if _, err := os.Lstat(filepath.Join(dir, startFile+corruptSuffix)); err == nil {
				continue // set aside by an earlier recovery, and kept for the user
			}
			it.parts = existingParts(dir)
			gone.detail = "a run that stopped before it was prepared"
			c.addLeftover(it, gone)
			continue
		}
		var s start
		if err == nil {
			err = json.Unmarshal(data, &s)
		}
		workspace := realPath(s.Workspace)
		if err == nil && (!within(workspace, realPath(layout.Workspaces)) || workspace == realPath(layout.Workspaces)) {
			err = fmt.Errorf("its start file names a workspace outside %s", layout.Workspaces)
		}
		if err != nil {
			c.addLeftover(it, verdict{reason: CleanKeptUnreadable, detail: "its start file cannot be read: recovery decides"})
			continue
		}
		if s.PGID > 0 && !s.Finished && groupExists(s.PGID) {
			c.addLeftover(it, verdict{reason: CleanKeptRunning, detail: fmt.Sprintf("its process group %d still exists", s.PGID)})
			continue
		}
		paths := []string{workspace, filepath.Join(dir, "verify"), filepath.Join(dir, gradingFolder)}
		if temp := layout.RunTemp(filepath.Base(s.Workspace)); temp != "" && ownFolder(temp) {
			paths = append(paths, temp)
		}
		switch {
		case s.Finished && s.Record.Outcome != "":
			paths = append(paths, filepath.Join(dir, "judge"), filepath.Join(dir, pairJudgeFolder))
			gone.detail = "a finished run that was not stored: recovery stores it"
		case !s.AgentStarted || s.Finished:
			paths = append(paths, dir)
			gone.detail = "a run that stopped before its agent started"
		default:
			gone.detail = "a run that stopped: recovery stores it as cancelled"
		}
		it.parts = existingParts(paths...)
		c.addLeftover(it, gone)
	}
	return nil
}

// addLeftover files a run's leftovers, sized by their parts.
func (c *planner) addLeftover(it CleanItem, v verdict) {
	for _, p := range it.parts {
		it.Bytes += p.bytes
	}
	c.add(it, v)
}

// existingParts sizes the paths that exist; a path inside an earlier one is counted with it, not again.
func existingParts(paths ...string) []cleanPart {
	var parts []cleanPart
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		parts = append(parts, cleanPart{path: p, bytes: treeSize(p)})
	}
	// The records folder holds the grading copy and grade folder: when it goes whole, it alone is counted.
	for i, outer := range parts {
		for j := range parts {
			if i != j && within(parts[j].path, outer.path) && parts[j].path != outer.path {
				parts[j].bytes = 0
			}
		}
	}
	return parts
}
