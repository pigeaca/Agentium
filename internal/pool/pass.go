package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

// ErrPassRunning means another pass of the same project's pool is running.
var ErrPassRunning = errors.New("a pool pass of this project is running")

// ScanRange is the history a pass asks its scan to read.
type ScanRange struct {
	Head string // the default branch's head
	// Exclude is the watermark: commits reachable from any of these were handled. Empty: read the whole window.
	Exclude []string
	// Since is the window's start: older commits are outside the pool (a candidate's base older than the window would
	// retire at once). Zero: no window.
	Since time.Time
	// Dismissed and DismissedPatches are solution commits and patch IDs never to offer again.
	Dismissed, DismissedPatches []string
}

// Scanned is what a scan found. The scan's contract, which the watermark's guarantee rests on:
//   - It reads the commits reachable from Head and from none of the Exclude commits it knows, committed at or after
//     Since (judged commit by commit: git's own --since stops walking at the first old commits, and could miss newer
//     ones behind them), oldest first in a topological order (each commit after its parents: `git rev-list
//     --topo-order --reverse`), up to its bound.
//   - Complete: it read every commit of that range. Otherwise Through lists the tips of what it read (the commits it
//     read that no other commit it read has as a parent); as it read a prefix of a topological order, everything in the
//     range that Through reaches was read.
//   - Unknown lists the Exclude commits the repository does not have (a force-push or rebase, then gc): it ignores
//     them, which reads their part of the history again rather than failing every pass.
//   - Candidates come best first, and leave out commits that are tasks already, dismissed commits and patch IDs.
type Scanned[C any] struct {
	Candidates []C
	Complete   bool
	Through    []string
	Unknown    []string
}

// Imported is what an import did: the tasks saved (as stored), how many candidates it tried, best first, and whether
// an interrupt stopped it.
type Imported struct {
	Tasks       []store.Task
	Tried       int
	Interrupted bool
}

// Pass is one pass's mining and validation for a project: the order of its steps, and what it records between them so
// that a kill at any step is recovered by the next pass. The steps that do the work are parameters (step 2 of the
// plan gives them internal/mine and internal/task); none may start an agent. It imports up to Limit tasks per pass,
// within the Window.
//
// The order, and what a kill after each step leaves:
//  1. Reconcile the state with the tasks (recovering what a killed pass left).
//  2. Scan the history after the watermark (Scanned), and drop the candidates whose base is older than the window or
//     whose patch ID is dismissed or already mined (a rebase gives the same change a new commit).
//  3. Record the candidates as pending. Killed here: the next pass drops them (no task has them) and scans the same
//     range again, since the watermark has not moved.
//  4. Import up to Limit of them. Killed here: the tasks saved so far are found from pending (Recovered), and the scan
//     skips their commits, so none is imported twice.
//  5. Record the imports. Killed after: the next pass validates them (step 6), as it does any mined task without a
//     validation.
//  6. Validate the mined tasks that have no validation. Killed here: each finished validation is stored; the rest
//     are validated next time.
//  7. Maintain (re-validate stale tasks, retire dead ones), when given. Killed here: the rules find the same tasks.
//  8. End: record the time and, when the import tried every candidate and nothing was interrupted, move the watermark
//     to the head (a complete scan) or add the scan's Through tips (a bounded one), unless another pass moved it
//     meanwhile. Killed before: the next pass reads the range again, which costs a scan, never a commit.
type Pass[C any] struct {
	File   string        // StateFile
	Limit  int           // imports per pass; Policy.Limit
	Window time.Duration // Policy.RetireAge: commits and candidates' bases older than this are left alone; 0: none
	Now    func() time.Time
	Commit func(C) string    // a candidate's solution commit
	Patch  func(C) string    // optional: its patch ID ("" when unknown)
	Base   func(C) time.Time // optional: when its base commit was committed (zero when unknown)

	Tasks    func(ctx context.Context) ([]store.Task, error) // the project's tasks, as stored now
	Head     func(ctx context.Context) (string, error)       // the default branch's head commit
	Scan     func(ctx context.Context, r ScanRange) (Scanned[C], error)
	Import   func(ctx context.Context, candidates []C, limit int) (Imported, error)
	Validate func(ctx context.Context, tasks []store.Task) error // stores each finished validation itself
	Maintain func(ctx context.Context) error                     // optional
}

// PassResult is what a pass did.
type PassResult struct {
	Head       string
	Candidates int          // after the window and patch filters
	Imported   []store.Task // this pass's imports, which alone --accept-mined may accept
	Validated  []store.Task // the tasks handed to Validate: this pass's imports and those a killed pass left
	Moved      bool         // the watermark moved
	Unknown    []string     // watermark commits the repository no longer has: their history was read again
	Unreadable string       // the state file was unreadable at the start (State.Unreadable), and was set aside
}

// Run runs the pass. Only one pass of a project runs at a time (ErrPassRunning otherwise): a second could reconcile
// away the first's pending commits while it imports them. It writes nothing on its way out after a failure: what
// it left is what a kill there would leave, which the next pass recovers.
func (p Pass[C]) Run(ctx context.Context) (PassResult, error) {
	var res PassResult
	release, err := lockPass(p.File + ".pass.lock")
	if err != nil {
		return res, err
	}
	defer release()

	tasks, err := p.Tasks(ctx)
	if err != nil {
		return res, err
	}
	st, err := Update(p.File, func(st *State) error {
		res.Unreadable = st.Unreadable
		st.Reconcile(tasks, nil)
		return nil
	})
	if err != nil {
		return res, err
	}
	start := slices.Clone(st.Watermark)
	if res.Head, err = p.Head(ctx); err != nil {
		return res, err
	}
	r := ScanRange{Head: res.Head, Exclude: start, Dismissed: st.Dismissed, DismissedPatches: st.DismissedPatches}
	if p.Window > 0 {
		r.Since = p.Now().Add(-p.Window)
	}
	scanned, err := p.Scan(ctx, r)
	if err != nil {
		return res, err
	}
	res.Unknown = scanned.Unknown
	candidates := p.keep(scanned.Candidates, r.Since, st)
	res.Candidates = len(candidates)
	imp := Imported{}
	if len(candidates) > 0 {
		if _, err := Update(p.File, func(st *State) error {
			for _, c := range candidates {
				if commit := p.Commit(c); !slices.ContainsFunc(st.Pending, func(q Pending) bool { return q.Commit == commit }) {
					st.Pending = append(st.Pending, Pending{Commit: commit, Patch: p.patch(c)})
				}
			}
			return nil
		}); err != nil {
			return res, err
		}
		if imp, err = p.Import(ctx, candidates, p.Limit); err != nil {
			return res, err
		}
		res.Imported = imp.Tasks
	}
	if tasks, err = p.Tasks(ctx); err != nil {
		return res, err
	}
	if st, err = Update(p.File, func(st *State) error { st.Reconcile(tasks, imp.Tasks); return nil }); err != nil {
		return res, err
	}
	if res.Validated = st.Unvalidated(tasks); len(res.Validated) > 0 {
		if err := p.Validate(ctx, res.Validated); err != nil {
			return res, err
		}
	}
	if p.Maintain != nil {
		if err := p.Maintain(ctx); err != nil {
			return res, err
		}
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("pool pass: %w", err)
	}
	// The watermark the scan really used: the commits it knew. Dropping the unknown ones is safe at any time (fewer
	// exclusions only read more), and keeps later passes from reading their history again and again.
	known := slices.DeleteFunc(slices.Clone(start), func(c string) bool { return slices.Contains(scanned.Unknown, c) })
	next := known
	if !imp.Interrupted && imp.Tried == len(candidates) {
		if scanned.Complete {
			next = []string{res.Head}
		} else {
			for _, tip := range scanned.Through {
				next = appendNew(next, tip)
			}
		}
	}
	_, err = Update(p.File, func(st *State) error {
		st.LastPass = p.Now().UTC()
		if slices.Equal(st.Watermark, start) && !slices.Equal(next, start) {
			st.Watermark, res.Moved = next, true
		}
		return nil
	})
	return res, err
}

// keep drops the candidates the pool leaves alone: those whose base is older than since (they would retire at once),
// and those whose patch ID is dismissed or already a mined task's (the same change, given a new commit by a rebase).
func (p Pass[C]) keep(candidates []C, since time.Time, st State) []C {
	return slices.DeleteFunc(slices.Clone(candidates), func(c C) bool {
		if p.Base != nil && !since.IsZero() {
			if base := p.Base(c); !base.IsZero() && base.Before(since) {
				return true
			}
		}
		patch := p.patch(c)
		return patch != "" && (slices.Contains(st.DismissedPatches, patch) || slices.ContainsFunc(st.Mined, func(r Record) bool { return r.Patch == patch }))
	})
}

func (p Pass[C]) patch(c C) string {
	if p.Patch == nil {
		return ""
	}
	return p.Patch(c)
}

// lockPass takes the pass lock without waiting; release drops it (as the OS does when the process dies).
func lockPass(file string) (release func(), err error) {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pool pass lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrPassRunning
		}
		return nil, fmt.Errorf("pool pass lock: %w", err)
	}
	return func() { f.Close() }, nil
}
