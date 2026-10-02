package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

type candidate struct{ commit string }

// killed is what a simulated kill panics with: the pass stops where it is, as if the process died there.
type killed struct{}

// world is a project with a linear history, some of whose commits are task candidates, and the pass's steps over it.
type world struct {
	t         *testing.T
	ctx       context.Context
	db        *store.Store
	project   int64
	file      string
	history   []string // oldest first; the last is the head
	candidate map[string]bool
	bound     int    // a scan reads at most this many commits, newest first (0: no bound)
	kill      string // where the next pass dies: "import:N" (after N saves), "pending", "record", "validate:N", "maintain", "end"
	clock     time.Time
	maintain  func()
}

func newWorld(t *testing.T, commits int, candidates ...string) *world {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	p, err := db.SaveProject(ctx, "/work/app", "app", []byte(`{}`), clock)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ctx: ctx, db: db, project: p.ID, file: StateFile(filepath.Join(dir, "repo.git")), candidate: map[string]bool{}, clock: clock}
	w.commit(commits)
	for _, c := range candidates {
		w.candidate[c] = true
	}
	return w
}

// commit adds n commits c<k> to the history.
func (w *world) commit(n int) {
	for range n {
		w.history = append(w.history, fmt.Sprintf("c%d", len(w.history)+1))
	}
}

func (w *world) die(point string) {
	if w.kill == point {
		w.kill = ""
		panic(killed{})
	}
}

func (w *world) now() time.Time {
	w.clock = w.clock.Add(time.Second)
	return w.clock
}

func (w *world) tasks(ctx context.Context) ([]store.Task, error) { return w.db.Tasks(ctx, w.project) }

func (w *world) pass(limit int) Pass[candidate] {
	return Pass[candidate]{
		File: w.file, Limit: limit, Commit: func(c candidate) string { return c.commit },
		Now:   func() time.Time { w.die("end"); return w.now() },
		Tasks: w.tasks,
		Head:  func(context.Context) (string, error) { return w.history[len(w.history)-1], nil },
		Scan: func(ctx context.Context, after, head string, dismissed []string) (Scanned[candidate], error) {
			tasks, err := w.tasks(ctx)
			if err != nil {
				return Scanned[candidate]{}, err
			}
			first := slices.Index(w.history, after) + 1 // (after, head]; 0 for the whole history
			last := slices.Index(w.history, head)
			out := Scanned[candidate]{Complete: true}
			for i := last; i >= first; i-- { // newest first
				if w.bound > 0 && last-i == w.bound {
					out.Complete = false
					break
				}
				c := w.history[i]
				if w.candidate[c] && !slices.Contains(dismissed, c) && !slices.ContainsFunc(tasks, func(t store.Task) bool { return t.SolutionCommit == c }) {
					out.Candidates = append(out.Candidates, candidate{c})
				}
			}
			return out, nil
		},
		Import: func(ctx context.Context, cands []candidate, limit int) (Imported, error) {
			w.die("pending")
			var imp Imported
			for _, c := range cands {
				if len(imp.Tasks) == limit {
					break
				}
				w.die(fmt.Sprintf("import:%d", len(imp.Tasks)))
				imp.Tried++
				saved, err := w.db.SaveTask(ctx, store.Task{ProjectID: w.project, Name: "t-" + c.commit, Instruction: "Do it.",
					Source: "commit " + c.commit, BaseCommit: "base", SolutionCommit: c.commit, Verify: []string{"true"}, NeedsReview: true, CreatedAt: w.now()})
				if err != nil {
					return imp, err
				}
				imp.Tasks = append(imp.Tasks, saved)
			}
			return imp, nil
		},
		Validate: func(ctx context.Context, tasks []store.Task) error {
			w.die("record")
			for i, t := range tasks {
				w.die(fmt.Sprintf("validate:%d", i))
				if _, err := w.db.SetTaskValidation(ctx, t.ID, t.Verify, t.Setup, []byte(`{"status":"valid"}`), w.now()); err != nil {
					return err
				}
			}
			return nil
		},
		Maintain: func(context.Context) error {
			w.die("maintain")
			if w.maintain != nil {
				w.maintain()
			}
			return nil
		},
	}
}

// run runs a pass, reporting whether a simulated kill stopped it.
func (w *world) run(limit int) (res PassResult, died bool, err error) {
	w.t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(killed); !ok {
				panic(r)
			}
			died = true
		}
	}()
	res, err = w.pass(limit).Run(w.ctx)
	return res, false, err
}

// check asserts that every candidate commit in the history is exactly one task, validated, and the watermark is the
// head.
func (w *world) check(label string) {
	w.t.Helper()
	tasks, err := w.tasks(w.ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	seen := map[string]int{}
	for _, t := range tasks {
		seen[t.SolutionCommit]++
		if t.Validation == nil {
			w.t.Errorf("%s: %s is not validated", label, t.Name)
		}
	}
	for _, c := range w.history {
		if w.candidate[c] && seen[c] != 1 {
			w.t.Errorf("%s: %s is %d tasks, want 1", label, c, seen[c])
		}
	}
	if st := Load(w.file); st.Watermark != w.history[len(w.history)-1] || len(st.Pending) != 0 {
		w.t.Errorf("%s: state %+v, want the watermark at the head and nothing pending", label, st)
	}
}

// A kill at every step of a pass is recovered by the next pass: no candidate is lost or imported twice, every
// imported task is validated, and the watermark ends at the head. A new commit after that is mined by the pass after.
func TestPassRecoversFromAKillAtEveryStep(t *testing.T) {
	for _, point := range []string{"pending", "import:0", "import:1", "import:3", "record", "validate:0", "validate:2", "maintain", "end"} {
		t.Run(point, func(t *testing.T) {
			w := newWorld(t, 6, "c2", "c3", "c5", "c6")
			w.kill = point
			if _, died, err := w.run(10); !died || err != nil {
				t.Fatalf("the killed pass: died %v, %v", died, err)
			}
			if st := Load(w.file); st.Watermark != "" {
				t.Fatalf("a killed pass moved the watermark: %+v", st)
			}
			res, died, err := w.run(10)
			if died || err != nil || !res.Moved {
				t.Fatalf("the next pass: %+v, died %v, %v", res, died, err)
			}
			w.check("after the next pass")
			w.candidate["c8"] = true
			w.commit(3)
			if res, _, err := w.run(10); err != nil || len(res.Imported) != 1 || res.Imported[0].SolutionCommit != "c8" {
				t.Errorf("after a pull: %+v, %v", res, err)
			}
			w.check("after a pull")
		})
	}
}

// Tasks a killed pass saved before recording them are found from its pending commits: validated, but recorded as
// recovered, so --accept-mined (which takes only PassResult.Imported) never accepts them.
func TestTasksSavedBeforeAKillAreRecoveredNotAccepted(t *testing.T) {
	w := newWorld(t, 6, "c2", "c3", "c5", "c6")
	w.kill = "import:2"
	if _, died, _ := w.run(10); !died {
		t.Fatal("not killed")
	}
	res, _, err := w.run(10)
	if err != nil {
		t.Fatal(err)
	}
	var imported, validated []string
	for _, t := range res.Imported {
		imported = append(imported, t.SolutionCommit)
	}
	for _, t := range res.Validated {
		validated = append(validated, t.SolutionCommit)
	}
	if !slices.Equal(imported, []string{"c3", "c2"}) || !slices.Equal(validated, []string{"c6", "c5", "c3", "c2"}) {
		t.Errorf("imported %v, validated %v", imported, validated)
	}
	for _, r := range Load(w.file).Mined {
		if want := r.SolutionCommit == "c6" || r.SolutionCommit == "c5"; r.Recovered != want {
			t.Errorf("record %+v: recovered should be %v", r, want)
		}
	}
}

// With more candidates than the limit, the watermark stays until a pass has tried them all, so commits pulled in the
// meantime and the older candidates are all mined.
func TestWatermarkWaitsForEveryCandidate(t *testing.T) {
	w := newWorld(t, 6, "c2", "c3", "c5", "c6")
	for i, want := range []string{"c6", "c5", "c3"} {
		res, _, err := w.run(1)
		if err != nil || len(res.Imported) != 1 || res.Imported[0].SolutionCommit != want || res.Moved || Load(w.file).Watermark != "" {
			t.Fatalf("pass %d: %+v, %v", i+1, res, err)
		}
	}
	w.candidate["c7"] = true
	w.commit(1)
	for i, want := range []string{"c7", "c2"} {
		if res, _, err := w.run(1); err != nil || len(res.Imported) != 1 || res.Imported[0].SolutionCommit != want {
			t.Fatalf("after the pull, pass %d: %+v, %v", i+1, res, err)
		}
	}
	w.check("after every candidate")
}

// A task the user removed is dismissed: its commit is not mined again, though the watermark has not passed it.
func TestRemovedTasksAreNotMinedAgain(t *testing.T) {
	w := newWorld(t, 6, "c2", "c3", "c5", "c6")
	if _, _, err := w.run(1); err != nil {
		t.Fatal(err)
	}
	if err := w.db.DeleteTask(w.ctx, w.project, "t-c6"); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, _, err := w.run(1); err != nil {
			t.Fatal(err)
		}
	}
	tasks, _ := w.tasks(w.ctx)
	var commits []string
	for _, t := range tasks {
		commits = append(commits, t.SolutionCommit)
	}
	if st := Load(w.file); !slices.Equal(commits, []string{"c5", "c3", "c2"}) || !slices.Equal(st.Dismissed, []string{"c6"}) || st.Watermark != "c6" {
		t.Errorf("tasks %v, state %+v", commits, st)
	}
}

// A scan that stopped at its bound keeps the watermark: the commits it did not read are scanned next time.
func TestAnIncompleteScanKeepsTheWatermark(t *testing.T) {
	w := newWorld(t, 6, "c2", "c6")
	w.bound = 3
	res, _, err := w.run(10)
	if err != nil || res.Moved || len(res.Imported) != 1 || Load(w.file).Watermark != "" {
		t.Errorf("bounded scan: %+v, %v", res, err)
	}
}

// A pass does not move a watermark that another process moved while it ran; a second pass at once is refused.
func TestWatermarkMovesOnlyFromWhereThePassStarted(t *testing.T) {
	w := newWorld(t, 3, "c2")
	w.maintain = func() {
		if _, err := Update(w.file, func(st *State) error { st.Watermark = "elsewhere"; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := w.pass(10).Run(w.ctx); !errors.Is(err, ErrPassRunning) {
			t.Errorf("a second pass at once: %v", err)
		}
	}
	res, _, err := w.run(10)
	if st := Load(w.file); err != nil || res.Moved || st.Watermark != "elsewhere" || st.LastPass.IsZero() {
		t.Errorf("result %+v, %v, state %+v", res, err, st)
	}
}

// An unreadable state file costs a rescan, not a duplicate: the tasks it recorded are skipped by commit.
func TestAnUnreadableStateIsRebuilt(t *testing.T) {
	w := newWorld(t, 4, "c2", "c4")
	if _, _, err := w.run(10); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.file, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _, err := w.run(10)
	if err != nil || res.Unreadable == "" || len(res.Imported) != 0 || !res.Moved {
		t.Errorf("after corruption: %+v, %v", res, err)
	}
	w.check("rebuilt")
}

// A failing step (not a kill) returns its error and leaves the state as a kill there would.
func TestAFailingStepLeavesRecoverableState(t *testing.T) {
	w := newWorld(t, 4, "c2", "c4")
	p := w.pass(10)
	p.Validate = func(context.Context, []store.Task) error { return errors.New("disk full") }
	if _, err := p.Run(w.ctx); err == nil || err.Error() != "disk full" {
		t.Fatalf("Run = %v", err)
	}
	if st := Load(w.file); st.Watermark != "" || len(st.Mined) != 2 {
		t.Errorf("state after a failure: %+v", st)
	}
	if _, _, err := w.run(10); err != nil {
		t.Fatal(err)
	}
	w.check("after the failure")
}
