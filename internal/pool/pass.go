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

// Scanned is what a scan found in (after, head]: the candidates, best first, and whether it read every commit of that
// range (false when it stopped at a bound, which keeps the watermark where it is).
type Scanned[C any] struct {
	Candidates []C
	Complete   bool
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
// plan gives them internal/mine and internal/task); none may start an agent.
//
// The order, and what a kill after each step leaves:
//  1. Reconcile the state with the tasks (recovering what a killed pass left).
//  2. Scan (watermark, head] for candidates; Scan must skip commits that are tasks already and the dismissed ones.
//  3. Record the candidates' commits as pending. Killed here: the next pass drops them (no task has them) and scans
//     the same range again, since the watermark has not moved.
//  4. Import up to Limit of them. Killed here: the tasks saved so far are found from pending (Recovered), and the scan
//     skips their commits, so none is imported twice.
//  5. Record the imports. Killed after: the next pass validates them (step 6), as it does any mined task without a
//     validation.
//  6. Validate the mined tasks that have no validation. Killed here: each finished validation is stored; the rest
//     are validated next time.
//  7. Maintain (re-validate stale tasks, retire dead ones), when given. Killed here: the rules find the same tasks.
//  8. End: record the time, and move the watermark to head only when the scan was complete, the import tried every
//     candidate, nothing was interrupted, and no other pass moved it meanwhile. Killed before: the next pass scans
//     the range again, which costs a scan, never a commit.
type Pass[C any] struct {
	File   string // StateFile
	Limit  int    // imports per pass; Policy.Limit
	Now    func() time.Time
	Commit func(C) string // a candidate's solution commit

	Tasks    func(ctx context.Context) ([]store.Task, error) // the project's tasks, as stored now
	Head     func(ctx context.Context) (string, error)       // the default branch's head commit
	Scan     func(ctx context.Context, after, head string, dismissed []string) (Scanned[C], error)
	Import   func(ctx context.Context, candidates []C, limit int) (Imported, error)
	Validate func(ctx context.Context, tasks []store.Task) error // stores each finished validation itself
	Maintain func(ctx context.Context) error                     // optional
}

// PassResult is what a pass did.
type PassResult struct {
	Head       string
	Candidates int
	Imported   []store.Task // this pass's imports, which alone --accept-mined may accept
	Validated  []store.Task // the tasks handed to Validate: this pass's imports and those a killed pass left
	Moved      bool         // the watermark moved to Head
	Unreadable string       // the state file was unreadable at the start (State.Unreadable)
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
	after := st.Watermark
	if res.Head, err = p.Head(ctx); err != nil {
		return res, err
	}
	scanned, err := p.Scan(ctx, after, res.Head, st.Dismissed)
	if err != nil {
		return res, err
	}
	res.Candidates = len(scanned.Candidates)
	imp := Imported{}
	if len(scanned.Candidates) > 0 {
		if _, err := Update(p.File, func(st *State) error {
			for _, c := range scanned.Candidates {
				if commit := p.Commit(c); !slices.Contains(st.Pending, commit) {
					st.Pending = append(st.Pending, commit)
				}
			}
			return nil
		}); err != nil {
			return res, err
		}
		if imp, err = p.Import(ctx, scanned.Candidates, p.Limit); err != nil {
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
	exhausted := scanned.Complete && !imp.Interrupted && imp.Tried == len(scanned.Candidates)
	_, err = Update(p.File, func(st *State) error {
		st.LastPass = p.Now().UTC()
		if exhausted && st.Watermark == after {
			st.Watermark, res.Moved = res.Head, true
		}
		return nil
	})
	return res, err
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
