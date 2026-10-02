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
	// Watermark is the default branch's head that the last complete pass scanned up to: the next pass mines only the
	// commits after it ((Watermark, head]), and the first (empty) mines the history. It moves only when a pass ends
	// having scanned its whole range and tried every candidate, so it never passes a commit no pass has handled.
	Watermark string `json:"watermark,omitempty"`
	// LastPass is when the last pass ended.
	LastPass time.Time `json:"last_pass,omitzero"`
	// Pending lists the solution commits a pass is importing, written before the import: after a kill, the next pass
	// finds which of them became tasks (Reconcile) and validates those.
	Pending []string `json:"pending,omitempty"`
	// Mined lists the tasks the pool imported. Dismissed lists solution commits never to mine again: those of mined
	// tasks the user removed.
	Mined     []Record `json:"mined,omitempty"`
	Dismissed []string `json:"dismissed,omitempty"`
	// Unreadable says why the file could not be read, when it could not ("" otherwise); the state is then empty, and
	// its next write replaces the file. It costs only the review shortcut (--accept-mined must refuse) and the
	// dismissals; an empty watermark only scans again.
	Unreadable string `json:"-"`
}

// Record is one task the pool imported, with enough to tell it from a task made later under the same name: the commit
// it solves and when it was created.
type Record struct {
	Name           string    `json:"name"`
	SolutionCommit string    `json:"solution_commit"`
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

// Load reads the state file; a missing file is the empty state, and an unreadable one too, with Unreadable set.
func Load(file string) State {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return State{}
	} else if err != nil {
		return State{Unreadable: err.Error()}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxState+1))
	if err == nil && len(data) > maxState {
		err = fmt.Errorf("larger than %d bytes", maxState)
	}
	var st State
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err != nil {
		return State{Unreadable: err.Error()}
	}
	return st
}

// Update applies change to the state on disk under an exclusive lock (a .lock file beside it, flock: the OS drops it
// when a process dies), then writes the result through a temporary file in the same folder, synced, renamed into
// place. Processes that update at once each see the other's changes. It returns the state as written. When change
// fails nothing is written.
func Update(file string, change func(*State) error) (State, error) {
	lock, err := os.OpenFile(file+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return State{}, fmt.Errorf("pool state: %w", err)
	}
	defer lock.Close() // closing releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return State{}, fmt.Errorf("lock %s: %w", file, err)
	}
	st := Load(file)
	if err := change(&st); err != nil {
		return State{}, err
	}
	if err := write(file, st); err != nil {
		return State{}, fmt.Errorf("pool state: %w", err)
	}
	st.Unreadable = ""
	return st, nil
}

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
	return os.Rename(f.Name(), file)
}

// Reconcile brings the state up to date with the project's tasks (as stored now), after imported (the tasks this pass
// just imported, as stored; nil when none) are recorded:
//   - a record whose task is gone, or whose name now belongs to another task, is dropped and its commit dismissed, so
//     the pool never mines it again (the user removed it);
//   - a pending commit that became a task no record holds is recorded as Recovered (a pass was killed after the
//     import, before recording it); the other pending commits never became tasks, and are dropped: the watermark did
//     not move past them, so the next scan finds them again.
func (st *State) Reconcile(tasks []store.Task, imported []store.Task) {
	for _, t := range imported {
		if !slices.ContainsFunc(st.Mined, func(r Record) bool { return r.Of(t) }) {
			st.Mined = append(st.Mined, Record{Name: t.Name, SolutionCommit: t.SolutionCommit, CreatedAt: t.CreatedAt.UTC()})
		}
	}
	kept := st.Mined[:0]
	for _, r := range st.Mined {
		if slices.ContainsFunc(tasks, func(t store.Task) bool { return r.Of(t) }) {
			kept = append(kept, r)
		} else if !slices.Contains(st.Dismissed, r.SolutionCommit) {
			st.Dismissed = append(st.Dismissed, r.SolutionCommit)
		}
	}
	st.Mined = kept
	for _, c := range st.Pending {
		for _, t := range tasks {
			if t.SolutionCommit == c && !slices.ContainsFunc(st.Mined, func(r Record) bool { return r.Of(t) }) {
				st.Mined = append(st.Mined, Record{Name: t.Name, SolutionCommit: t.SolutionCommit, CreatedAt: t.CreatedAt.UTC(), Recovered: true})
			}
		}
	}
	st.Pending = nil
	slices.Sort(st.Dismissed)
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
