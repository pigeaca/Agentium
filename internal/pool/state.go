package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

// State is what the pool keeps between passes, in a file beside the project's repository in the data folder
// (StateFile), as start keeps start-mined.json: it is not task data, so it needs no migration. Every change goes
// through Update, which writes it whole by rename, so a kill leaves the old file or the new one.
type State struct {
	// Watermark is what earlier passes handled: every commit reachable from one of these commits was mined (or is
	// outside the pool's window), so the next pass reads only the commits reachable from the head and from none of
	// them. Empty: nothing yet, and the first pass reads the whole window. It holds the head after a pass that read its
	// whole range, and the tips of what a bounded scan read otherwise (Scanned.Through). It moves only when a pass ends
	// having tried every candidate it found, so it never covers a commit no pass has handled.
	Watermark []string `json:"watermark,omitempty"`
	// Modules holds the watermark of each monorepo module mined (Pass.Module), by its path: a pass in a module sets
	// aside every commit outside it, so a module's watermark says nothing of another module's commits, or the root's.
	// Watermark stays the root's, so a project that never mined a module keeps its file as it was.
	Modules map[string][]string `json:"module_watermarks,omitempty"`
	// LastPass is when the last pass ended.
	LastPass time.Time `json:"last_pass,omitzero"`
	// Pending lists the candidates a pass is importing, written before the import: after a kill, the next pass finds
	// which of them became tasks (Reconcile) and validates those.
	Pending []Pending `json:"pending,omitempty"`
	// Mined lists the tasks the pool imported. Dismissed and DismissedPatches are never mined again: the solution
	// commits of mined tasks the user removed, and their patch IDs (`git patch-id --stable`), which a rebase keeps when
	// it gives the same change a new commit.
	Mined            []Record `json:"mined,omitempty"`
	Dismissed        []string `json:"dismissed,omitempty"`
	DismissedPatches []string `json:"dismissed_patches,omitempty"`
	// Unreadable says why the file could not be parsed, when it could not ("" otherwise); the state is then empty, and
	// its next write sets the file aside (pool.json.corrupt-<time>) before replacing it. It costs the review shortcut
	// (--accept-mined must refuse) and the dismissals; an empty watermark only scans the window again.
	Unreadable string `json:"-"`
}

// WatermarkOf is the watermark of module's passes ("": the root's, Watermark).
func (st State) WatermarkOf(module string) []string {
	if module == "" {
		return st.Watermark
	}
	return st.Modules[module]
}

// setWatermark sets module's watermark ("": the root's).
func (st *State) setWatermark(module string, commits []string) {
	if module == "" {
		st.Watermark = commits
		return
	}
	if st.Modules == nil {
		st.Modules = map[string][]string{}
	}
	st.Modules[module] = commits
}

// Pending is a candidate a pass began importing: its solution commit, and its patch ID when known.
type Pending struct {
	Commit string `json:"commit"`
	Patch  string `json:"patch,omitempty"`
}

// Record is one task the pool imported, with enough to tell it from a task made later under the same name: the commit
// it solves and when it was created; and the solution's patch ID when known, which a rebase keeps.
type Record struct {
	Name           string    `json:"name"`
	SolutionCommit string    `json:"solution_commit"`
	Patch          string    `json:"patch,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	// Recovered: a pass was killed before it recorded the task, and a later pass found it from Pending. It is validated
	// like the others, but never accepted without a review: it could be a task the user imported by hand meanwhile.
	Recovered bool `json:"recovered,omitempty"`
}

// Of reports whether r records t.
func (r Record) Of(t store.Task) bool {
	return r.Name == t.Name && r.SolutionCommit == t.SolutionCommit && r.CreatedAt.Equal(t.CreatedAt)
}

// StateFile is the pool's state file for the project whose Agentium repository is bare.
func StateFile(bare string) string {
	return filepath.Join(filepath.Dir(bare), "pool.json")
}

// maxState bounds the state file: larger is unreadable.
const maxState = 8 << 20

// Load reads the state file. A missing file is the empty state; one that cannot be parsed (or is larger than
// maxState) is the empty state with Unreadable set. Any other failure to read it is an error: the file may be fine,
// and replacing it would lose the dismissals and the records --accept-mined relies on.
func Load(file string) (State, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	} else if err != nil {
		return State{}, fmt.Errorf("pool state: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxState+1))
	if err != nil {
		return State{}, fmt.Errorf("pool state: read %s: %w", file, err)
	}
	if len(data) > maxState {
		return State{Unreadable: fmt.Sprintf("larger than %d bytes", maxState)}, nil
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{Unreadable: err.Error()}, nil
	}
	return st, nil
}

// Update applies change to the state on disk under an exclusive lock (a .lock file beside it, flock: the OS drops it
// when a process dies), then writes the result through a temporary file in the same folder, synced, renamed into
// place, and syncs the folder. Processes that update at once each see the other's changes. An unreadable file is
// renamed to <file>.corrupt-<time> first, so nothing is lost. It returns the state as written. When change fails, or
// the file cannot be read, nothing is written.
func Update(file string, change func(*State) error) (State, error) {
	lock, err := os.OpenFile(file+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return State{}, fmt.Errorf("pool state: %w", err)
	}
	defer lock.Close() // closing releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return State{}, fmt.Errorf("lock %s: %w", file, err)
	}
	st, err := Load(file)
	if err != nil {
		return State{}, err
	}
	if err := change(&st); err != nil {
		return State{}, err
	}
	if st.Unreadable != "" {
		aside := file + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := os.Rename(file, aside); err != nil {
			return State{}, fmt.Errorf("pool state: set the unreadable file aside: %w", err)
		}
	}
	if err := write(file, st); err != nil {
		return State{}, fmt.Errorf("pool state: %w", err)
	}
	st.Unreadable = ""
	return st, nil
}

// write replaces file with st: a synced temporary file renamed over it, then the folder synced so the rename itself
// survives a crash. (macOS's fsync does not flush the drive's cache, which F_FULLFSYNC would; a power loss can still
// lose the last write, never tear it, which costs a rescan at most.)
func write(file string, st State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), "pool-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // after a successful rename it is gone already
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), file); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Reconcile brings the state up to date with the project's tasks (as stored now), after imported (the tasks this pass
// just imported, as stored; nil when none) are recorded, with the patch IDs their pending entries carry:
//   - a record whose task is gone, or whose name now belongs to another task, is dropped and its commit and patch ID
//     dismissed, so the pool never mines that change again (the user removed it);
//   - a pending commit that became a task no record holds is recorded as Recovered (a pass was killed after the
//     import, before recording it); the other pending commits never became tasks, and are dropped: the watermark did
//     not move past them, so the next scan finds them again.
func (st *State) Reconcile(tasks []store.Task, imported []store.Task) {
	patch := func(commit string) string {
		if i := slices.IndexFunc(st.Pending, func(p Pending) bool { return p.Commit == commit }); i >= 0 {
			return st.Pending[i].Patch
		}
		return ""
	}
	for _, t := range imported {
		if !slices.ContainsFunc(st.Mined, func(r Record) bool { return r.Of(t) }) {
			st.Mined = append(st.Mined, Record{Name: t.Name, SolutionCommit: t.SolutionCommit, Patch: patch(t.SolutionCommit), CreatedAt: t.CreatedAt.UTC()})
		}
	}
	kept := st.Mined[:0]
	for _, r := range st.Mined {
		if slices.ContainsFunc(tasks, func(t store.Task) bool { return r.Of(t) }) {
			kept = append(kept, r)
			continue
		}
		st.Dismissed = appendNew(st.Dismissed, r.SolutionCommit)
		if r.Patch != "" {
			st.DismissedPatches = appendNew(st.DismissedPatches, r.Patch)
		}
	}
	st.Mined = kept
	for _, p := range st.Pending {
		for _, t := range tasks {
			if t.SolutionCommit == p.Commit && !slices.ContainsFunc(st.Mined, func(r Record) bool { return r.Of(t) }) {
				st.Mined = append(st.Mined, Record{Name: t.Name, SolutionCommit: t.SolutionCommit, Patch: p.Patch, CreatedAt: t.CreatedAt.UTC(), Recovered: true})
			}
		}
	}
	st.Pending = nil
	slices.Sort(st.Dismissed)
	slices.Sort(st.DismissedPatches)
}

// appendNew appends s to list unless it is there.
func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// Unvalidated lists the tasks the pool imported (Mined, recovered ones included) that are active and have no
// validation yet, oldest first as tasks are listed: what a pass validates, including those a killed pass left.
func (st State) Unvalidated(tasks []store.Task) []store.Task {
	var out []store.Task
	for _, t := range tasks {
		if t.Validation == nil && !t.Retired() && slices.ContainsFunc(st.Mined, func(r Record) bool { return r.Of(t) }) {
			out = append(out, t)
		}
	}
	return out
}
