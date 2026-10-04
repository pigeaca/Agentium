package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

type candidate struct {
	commit, patch string
	base          time.Time
}

// killed is what a simulated kill panics with: the pass stops where it is, as if the process died there.
type killed struct{}

// node is a commit of the fake history.
type node struct {
	parents []string
	at      time.Time
	patch   string
}

// world is a project with a history (a DAG; creation order is a topological order), some of whose commits are task
// candidates, and a pass's steps over it that follow the Scanned contract.
type world struct {
	t         *testing.T
	ctx       context.Context
	db        *store.Store
	project   int64
	file      string
	nodes     map[string]node
	order     []string
	head      string
	candidate map[string]bool
	bound     int    // a scan reads at most this many commits, oldest first (0: no bound)
	kill      string // where the next pass dies: "pending", "import:N" (after N saves), "record", "validate:N", "maintain", "end"
	ending    bool   // the pass is past Maintain: the next Now is the end's
	clock     time.Time
	maintain  func()
	reads     map[string]int // how often scans read each commit
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
	w := &world{t: t, ctx: ctx, db: db, project: p.ID, file: StateFile(filepath.Join(dir, "repo.git")), nodes: map[string]node{},
		candidate: map[string]bool{}, clock: clock, reads: map[string]int{}}
	w.commit(commits)
	for _, c := range candidates {
		w.candidate[c] = true
	}
	return w
}

// add makes a commit with these parents, at this time, without moving the head.
func (w *world) add(name string, at time.Time, parents ...string) {
	w.nodes[name] = node{parents: parents, at: at, patch: "p-" + name}
	w.order = append(w.order, name)
}

// commit adds n commits c<k> on the head, minutes apart, ten days ago.
func (w *world) commit(n int) {
	for range n {
		name := fmt.Sprintf("c%d", len(w.order)+1)
		var parents []string
		if w.head != "" {
			parents = []string{w.head}
		}
		w.add(name, w.clock.Add(-10*Day+time.Duration(len(w.order))*time.Minute), parents...)
		w.head = name
	}
}

// reach is every commit reachable from tips that the history has.
func (w *world) reach(tips []string) map[string]bool {
	seen := map[string]bool{}
	stack := slices.Clone(tips)
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := w.nodes[c]; !ok || seen[c] {
			continue
		}
		seen[c] = true
		stack = append(stack, w.nodes[c].parents...)
	}
	return seen
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

func (w *world) base(c string) time.Time {
	if ps := w.nodes[c].parents; len(ps) > 0 {
		return w.nodes[ps[0]].at
	}
	return time.Time{}
}

// scan follows the Scanned contract over the fake history.
func (w *world) scan(ctx context.Context, r ScanRange) (Scanned[candidate], error) {
	tasks, err := w.tasks(ctx)
	if err != nil {
		return Scanned[candidate]{}, err
	}
	out := Scanned[candidate]{Complete: true}
	var known []string
	for _, e := range r.Exclude {
		if _, ok := w.nodes[e]; ok {
			known = append(known, e)
		} else {
			out.Unknown = append(out.Unknown, e)
		}
	}
	excluded, reachable := w.reach(known), w.reach([]string{r.Head})
	var rng []string
	for _, c := range w.order { // oldest first, parents before children
		if reachable[c] && !excluded[c] && !w.nodes[c].at.Before(r.Since) {
			rng = append(rng, c)
		}
	}
	read := rng
	if w.bound > 0 && len(rng) > w.bound {
		read, out.Complete = rng[:w.bound], false
		for _, c := range read {
			if !slices.ContainsFunc(read, func(o string) bool { return slices.Contains(w.nodes[o].parents, c) }) {
				out.Through = append(out.Through, c)
			}
		}
	}
	for _, c := range read {
		w.reads[c]++
	}
	for i := len(read) - 1; i >= 0; i-- { // best (here: newest) first
		c := read[i]
		if w.candidate[c] && !slices.Contains(r.Dismissed, c) && !slices.Contains(r.DismissedPatches, w.nodes[c].patch) &&
			!slices.ContainsFunc(tasks, func(t store.Task) bool { return t.SolutionCommit == c }) {
			out.Candidates = append(out.Candidates, candidate{commit: c, patch: w.nodes[c].patch, base: w.base(c)})
		}
	}
	return out, nil
}

func (w *world) pass(limit int) Pass[candidate] {
	return Pass[candidate]{
		File: w.file, Limit: limit, Window: DefaultPolicy().RetireAge,
		Commit: func(c candidate) string { return c.commit },
		Patch:  func(c candidate) string { return c.patch },
		Base:   func(c candidate) time.Time { return c.base },
		Now: func() time.Time {
			if w.ending {
				w.die("end")
			}
			return w.now()
		},
		Tasks: w.tasks,
		Head:  func(context.Context) (string, error) { return w.head, nil },
		Scan:  w.scan,
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
			w.ending = true
			return nil
		},
	}
}

// run runs a pass, reporting whether a simulated kill stopped it.
func (w *world) run(limit int) (res PassResult, died bool, err error) {
	w.t.Helper()
	w.ending = false
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

func (w *world) state() State {
	w.t.Helper()
	st, err := Load(w.file)
	if err != nil {
		w.t.Fatal(err)
	}
	return st
}

// taskCommits counts the tasks per solution commit.
func (w *world) taskCommits() map[string]int {
	w.t.Helper()
	tasks, err := w.tasks(w.ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	seen := map[string]int{}
	for _, t := range tasks {
		seen[t.SolutionCommit]++
	}
	return seen
}

// neverSkipped asserts the watermark's guarantee: every candidate it covers is a task (or the user dismissed it).
func (w *world) neverSkipped(label string) {
	w.t.Helper()
	st, seen := w.state(), w.taskCommits()
	for c := range w.reach(st.Watermark) {
		if w.candidate[c] && seen[c] == 0 && !slices.Contains(st.Dismissed, c) {
			w.t.Errorf("%s: the watermark %v covers %s, which is no task", label, st.Watermark, c)
		}
	}
}

// check asserts that every candidate of the head's history is exactly one task, validated, and the watermark is the
// head.
func (w *world) check(label string) {
	w.t.Helper()
	tasks, err := w.tasks(w.ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, t := range tasks {
		if t.Validation == nil {
			w.t.Errorf("%s: %s is not validated", label, t.Name)
		}
	}
	seen := w.taskCommits()
	for c := range w.reach([]string{w.head}) {
		if w.candidate[c] && seen[c] != 1 {
			w.t.Errorf("%s: %s is %d tasks, want 1", label, c, seen[c])
		}
	}
	if st := w.state(); !slices.Equal(st.Watermark, []string{w.head}) || len(st.Pending) != 0 {
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
			if st := w.state(); len(st.Watermark) != 0 {
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
	for _, r := range w.state().Mined {
		if want := r.SolutionCommit == "c6" || r.SolutionCommit == "c5"; r.Recovered != want || r.Patch != "p-"+r.SolutionCommit {
			t.Errorf("record %+v: recovered should be %v, with the commit's patch ID", r, want)
		}
	}
}

// With more candidates than the limit, the watermark stays until a pass has tried them all, so commits pulled in the
// meantime and the older candidates are all mined.
func TestWatermarkWaitsForEveryCandidate(t *testing.T) {
	w := newWorld(t, 6, "c2", "c3", "c5", "c6")
	for i, want := range []string{"c6", "c5", "c3"} {
		res, _, err := w.run(1)
		if err != nil || len(res.Imported) != 1 || res.Imported[0].SolutionCommit != want || res.Moved || len(w.state().Watermark) != 0 {
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

// A task the user removed is dismissed, with its patch ID: its commit is not mined again, though the watermark has
// not passed it.
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
	st := w.state()
	if !slices.Equal(commits, []string{"c5", "c3", "c2"}) || !slices.Equal(st.Dismissed, []string{"c6"}) ||
		!slices.Equal(st.DismissedPatches, []string{"p-c6"}) || !slices.Equal(st.Watermark, []string{"c6"}) {
		t.Errorf("tasks %v, state %+v", commits, st)
	}
}

// A scan bounded to fewer commits than the range reads it oldest first, a window per pass, and the watermark moves to
// the tips of each window it finished: every candidate is mined in the end, none twice, and the watermark never covers
// one that is no task. (The review's case: c1..c6, bound 3, candidates c2 and c6.)
func TestABoundedScanReachesEveryCommit(t *testing.T) {
	w := newWorld(t, 6, "c2", "c6")
	w.bound = 3
	res, _, err := w.run(10)
	if err != nil || !res.Moved || len(res.Imported) != 1 || res.Imported[0].SolutionCommit != "c2" || !slices.Equal(w.state().Watermark, []string{"c3"}) {
		t.Fatalf("first window: %+v, %v, state %+v", res, err, w.state())
	}
	w.neverSkipped("first window")
	if res, _, err = w.run(10); err != nil || !res.Moved || len(res.Imported) != 1 || res.Imported[0].SolutionCommit != "c6" {
		t.Fatalf("second window: %+v, %v", res, err)
	}
	w.check("bounded")
}

// merges is a history with a branch and a merge, and a candidate in most commits:
//
//	c1 - c2 - c4 ------- m6 - c7 - c8
//	       \            /
//	        b3 - b5 ----
func merges(t *testing.T) *world {
	w := newWorld(t, 2)
	at := w.clock.Add(-5 * Day)
	w.add("b3", at, "c2")
	w.add("c4", at.Add(time.Minute), "c2")
	w.add("b5", at.Add(2*time.Minute), "b3")
	w.add("m6", at.Add(3*time.Minute), "c4", "b5")
	w.add("c7", at.Add(4*time.Minute), "m6")
	w.add("c8", at.Add(5*time.Minute), "c7")
	w.head = "c8"
	for _, c := range []string{"c1", "b3", "c4", "b5", "c7", "c8"} {
		w.candidate[c] = true
	}
	return w
}

// passes runs passes of limit imports until the watermark is the head, asserting after each that it covers no
// candidate that is no task.
func (w *world) passes(limit int) {
	w.t.Helper()
	for pass := 1; ; pass++ {
		if pass > 20 {
			w.t.Fatalf("no end after 20 passes: state %+v", w.state())
		}
		if _, _, err := w.run(limit); err != nil {
			w.t.Fatal(err)
		}
		w.neverSkipped(fmt.Sprintf("pass %d", pass))
		if slices.Equal(w.state().Watermark, []string{w.head}) {
			return
		}
	}
}

// The same over merges and a limit of one import per pass: whatever the windows' tips, the watermark never covers a
// candidate that is no task, and every candidate is mined once.
func TestABoundedScanOverMerges(t *testing.T) {
	w := merges(t)
	w.bound = 2
	w.passes(1)
	w.check("over merges")
}

// When every window's candidates fit the limit, the windows move on without overlap: each commit is read once. (A
// watermark that dropped a tip would read its history again.)
func TestABoundedScanReadsEachCommitOnce(t *testing.T) {
	w := merges(t)
	w.bound = 2
	w.passes(10)
	w.check("read once")
	for _, c := range w.order {
		if w.reads[c] != 1 {
			t.Errorf("%s was read %d times: %v", c, w.reads[c], w.reads)
		}
	}
}

// The pool reads only the window (Policy.RetireAge): commits older than 270 days are not read, and a candidate whose
// base is older is not imported, as it would retire at once. A first pass that reaches the window's start is complete.
func TestFirstPassStopsAtTheRetireWindow(t *testing.T) {
	w := newWorld(t, 0)
	day := func(n int) time.Time { return w.clock.Add(-time.Duration(n) * Day) }
	w.add("c1", day(300))
	w.add("c2", day(280), "c1") // older than the window: not read
	w.add("c3", day(269), "c2") // in the window, but its base (c2) is not: not imported
	w.add("c4", day(100), "c3") // in the window, base c3 too
	w.add("c5", day(1), "c4")
	w.head = "c5"
	for _, c := range []string{"c2", "c3", "c4", "c5"} {
		w.candidate[c] = true
	}
	res, _, err := w.run(10)
	if err != nil || !res.Moved || res.Candidates != 2 {
		t.Fatalf("first pass: %+v, %v", res, err)
	}
	seen := w.taskCommits()
	if seen["c2"] != 0 || seen["c3"] != 0 || seen["c4"] != 1 || seen["c5"] != 1 || !slices.Equal(w.state().Watermark, []string{"c5"}) {
		t.Errorf("tasks %v, state %+v", seen, w.state())
	}
}

// A candidate whose base would retire within the margin is not imported: its import and validation would cost a build
// for a task the next passes retire.
func TestNoImportOfATaskThatWouldRetireSoon(t *testing.T) {
	w := newWorld(t, 0)
	day := func(n int) time.Time { return w.clock.Add(-time.Duration(n) * Day) }
	w.add("c1", day(265))
	w.add("c2", day(250), "c1") // base c1: 265 days, retires within 30 days
	w.add("c3", day(200), "c2") // base c2: 250 days, also within the margin
	w.add("c4", day(10), "c3")  // base c3: 200 days, kept
	w.head = "c4"
	for _, c := range []string{"c2", "c3", "c4"} {
		w.candidate[c] = true
	}
	p := w.pass(10)
	p.Margin = DefaultPolicy().StaleAfter
	if _, err := p.Run(w.ctx); err != nil {
		t.Fatal(err)
	}
	if seen := w.taskCommits(); seen["c2"] != 0 || seen["c3"] != 0 || seen["c4"] != 1 {
		t.Errorf("tasks %v", seen)
	}
}

// After a force-push (or a rebase, then gc) the watermark names commits the repository no longer has: the pass reads
// the window again and says so, instead of failing every time. The rebased copies of mined and dismissed changes keep
// their patch IDs and are not imported again; a genuinely new commit is.
func TestAnUnknownWatermarkIsReadAgain(t *testing.T) {
	w := newWorld(t, 4, "c2", "c3", "c4")
	if _, _, err := w.run(10); err != nil {
		t.Fatal(err)
	}
	if err := w.db.DeleteTask(w.ctx, w.project, "t-c3"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.run(10); err != nil { // reconciles: c3's commit and patch are dismissed
		t.Fatal(err)
	}
	// The history is rewritten: c2..c4 become r2..r4 (same patches), on top of c1; r5 is new. The old commits are gone.
	for _, old := range []string{"c2", "c3", "c4"} {
		delete(w.nodes, old)
	}
	w.order = slices.DeleteFunc(w.order, func(c string) bool { _, ok := w.nodes[c]; return !ok })
	prev := "c1"
	for i, name := range []string{"r2", "r3", "r4", "r5"} {
		w.add(name, w.clock.Add(-Day+time.Duration(i)*time.Minute), prev)
		w.candidate[name] = true
		prev = name
	}
	for _, c := range []string{"r2", "r3", "r4"} {
		n := w.nodes[c]
		n.patch = "p-c" + c[1:]
		w.nodes[c] = n
	}
	w.head = "r5"
	res, _, err := w.run(10)
	if err != nil || !slices.Equal(res.Unknown, []string{"c4"}) || !res.Moved || len(res.Imported) != 1 || res.Imported[0].SolutionCommit != "r5" {
		t.Fatalf("after the rewrite: %+v, %v", res, err)
	}
	if st := w.state(); !slices.Equal(st.Watermark, []string{"r5"}) {
		t.Errorf("state %+v", st)
	}
	if seen := w.taskCommits(); seen["r2"]+seen["r3"]+seen["r4"] != 0 {
		t.Errorf("rebased copies were imported: %v", seen)
	}
}

// Unknown watermark commits are dropped even when the pass could not finish its candidates, so later passes do not
// read their history again and again; that only widens what is read.
func TestUnknownWatermarkCommitsAreDropped(t *testing.T) {
	w := newWorld(t, 4, "c2", "c3")
	if _, err := Update(w.file, func(st *State) error { st.Watermark = []string{"gone", "c1"}; return nil }); err != nil {
		t.Fatal(err)
	}
	res, _, err := w.run(1) // two candidates, one import: not finished
	if err != nil || !slices.Equal(res.Unknown, []string{"gone"}) || !res.Moved || !slices.Equal(w.state().Watermark, []string{"c1"}) {
		t.Errorf("result %+v, %v, state %+v", res, err, w.state())
	}
}

// A pass does not move a watermark that another process moved while it ran; a second pass at once is refused.
func TestWatermarkMovesOnlyFromWhereThePassStarted(t *testing.T) {
	w := newWorld(t, 3, "c2")
	w.maintain = func() {
		if _, err := Update(w.file, func(st *State) error { st.Watermark = []string{"elsewhere"}; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := w.pass(10).Run(w.ctx); !errors.Is(err, ErrPassRunning) {
			t.Errorf("a second pass at once: %v", err)
		}
	}
	res, _, err := w.run(10)
	if st := w.state(); err != nil || res.Moved || !slices.Equal(st.Watermark, []string{"elsewhere"}) || st.LastPass.IsZero() {
		t.Errorf("result %+v, %v, state %+v", res, err, st)
	}
}

// An unreadable state file is set aside and costs a rescan, not a duplicate: the tasks it recorded are skipped by
// commit.
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
	if aside, _ := filepath.Glob(w.file + ".corrupt-*"); len(aside) != 1 {
		t.Errorf("the corrupt file set aside: %v", aside)
	}
}

// A failing step (not a kill) returns its error and leaves the state as a kill there would.
func TestAFailingStepLeavesRecoverableState(t *testing.T) {
	w := newWorld(t, 4, "c2", "c4")
	p := w.pass(10)
	p.Validate = func(context.Context, []store.Task) error { return errors.New("disk full") }
	if _, err := p.Run(w.ctx); err == nil || err.Error() != "disk full" {
		t.Fatalf("Run = %v", err)
	}
	if st := w.state(); len(st.Watermark) != 0 || len(st.Mined) != 2 {
		t.Errorf("state after a failure: %+v", st)
	}
	if _, _, err := w.run(10); err != nil {
		t.Fatal(err)
	}
	w.check("after the failure")
}

// A re-scan (Pass.Since) reads the commits from Since on whatever the watermark says, so it reaches a commit an earlier
// pass set aside (here: not a candidate then, one now, as after a settings change); it imports it once, leaves the
// watermark where it was, and keeps candidates whose base is older than Since (the window alone bounds bases).
func TestARescanReachesSetAsideCommitsAndKeepsTheWatermark(t *testing.T) {
	w := newWorld(t, 6, "c5")
	if res, _, err := w.run(10); err != nil || !res.Moved || len(res.Imported) != 1 {
		t.Fatalf("the first pass: %+v, %v", res, err)
	}
	w.candidate["c2"] = true
	if res, _, err := w.run(10); err != nil || len(res.Imported) != 0 {
		t.Fatalf("a plain pass reads only after the watermark: %+v, %v", res, err)
	}
	rescan := func(since time.Time) Pass[candidate] {
		p := w.pass(10)
		p.Since = since
		w.ending = false
		return p
	}
	run := func(since time.Time) PassResult {
		t.Helper()
		res, err := rescan(since).Run(w.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if prev, err := rescan(w.nodes["c2"].at).Preview(w.ctx); err != nil || len(prev.Candidates) != 1 || prev.Candidates[0].commit != "c2" {
		t.Errorf("the re-scan's preview: %+v, %v", prev, err)
	}
	if res := run(w.nodes["c3"].at); len(res.Imported) != 0 || res.Moved {
		t.Errorf("a re-scan from after c2: %+v", res)
	}
	res := run(w.nodes["c2"].at)
	if len(res.Imported) != 1 || res.Imported[0].SolutionCommit != "c2" || res.Moved || !slices.Equal(w.state().Watermark, []string{"c6"}) {
		t.Errorf("the re-scan: %+v, state %+v", res, w.state())
	}
	if res := run(w.clock.Add(-400 * Day)); len(res.Imported) != 0 { // before the window: clamped to it; c2 is a task now
		t.Errorf("a re-scan from before the window: %+v", res)
	}
	w.check("after the re-scans")
}

// A re-scan killed mid-import is recovered like any pass: the next re-scan imports the rest, none twice, every task
// is validated, and neither moves the watermark.
func TestAKilledRescanIsRecovered(t *testing.T) {
	w := newWorld(t, 6, "c6")
	if _, _, err := w.run(10); err != nil {
		t.Fatal(err)
	}
	w.candidate["c2"], w.candidate["c3"], w.candidate["c4"] = true, true, true
	rescan := func() (res PassResult, died bool, err error) {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(killed); !ok {
					panic(r)
				}
				died = true
			}
		}()
		p := w.pass(10)
		p.Since, w.ending = w.nodes["c1"].at, false
		res, err = p.Run(w.ctx)
		return res, false, err
	}
	w.kill = "import:1"
	if _, died, err := rescan(); !died || err != nil {
		t.Fatalf("the killed re-scan: died %v, %v", died, err)
	}
	if res, died, err := rescan(); died || err != nil || res.Moved || len(res.Imported) != 2 {
		t.Fatalf("the next re-scan: %+v, died %v, %v", res, died, err)
	}
	w.check("after a killed re-scan")
}

// A re-scan whose date falls after commits the watermark has not reached yet must not move the watermark past them,
// complete or bounded: otherwise the commits between the watermark and Since would never be read. (The watermark is at
// c3, the head at c7, and c4, a candidate, is older than Since.)
func TestARescanNeverMovesTheWatermarkPastUnreadCommits(t *testing.T) {
	for _, bound := range []int{0, 1} {
		t.Run(fmt.Sprintf("bound %d", bound), func(t *testing.T) {
			w := newWorld(t, 3)
			if res, _, err := w.run(10); err != nil || !res.Moved || !slices.Equal(w.state().Watermark, []string{"c3"}) {
				t.Fatalf("the first pass: %+v, %v, state %+v", res, err, w.state())
			}
			w.commit(4)
			w.candidate["c4"], w.candidate["c6"] = true, true
			w.bound = bound
			p := w.pass(10)
			p.Since, w.ending = w.nodes["c5"].at, false
			res, err := p.Run(w.ctx)
			if err != nil || res.Moved || !slices.Equal(w.state().Watermark, []string{"c3"}) {
				t.Fatalf("the re-scan: %+v, %v, state %+v", res, err, w.state())
			}
			if bound == 0 && (len(res.Imported) != 1 || res.Imported[0].SolutionCommit != "c6") {
				t.Errorf("the re-scan imported %+v, want c6", res.Imported)
			}
			w.bound = 0
			res, _, err = w.run(10)
			if err != nil || !slices.ContainsFunc(res.Imported, func(t store.Task) bool { return t.SolutionCommit == "c4" }) {
				t.Fatalf("the next plain pass: %+v, %v", res, err)
			}
			w.check("after the plain pass")
		})
	}
}

// A pass in a module keeps its own watermark: it never moves the root's (or another module's), so a pass at the root
// after one in a module still reads the commits the module's pass set aside, and the module's next pass reads only
// new ones. A project that never mined a module keeps its state file without the field.
func TestPassWatermarkPerModule(t *testing.T) {
	w := newWorld(t, 4, "c2")
	if _, died, err := w.run(5); died || err != nil {
		t.Fatal(died, err)
	}
	data, err := os.ReadFile(w.file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "module_watermarks") {
		t.Errorf("a root-only state names module watermarks: %s", data)
	}
	root := slices.Clone(w.state().Watermark)

	w.commit(2) // c5, c6
	pass := w.pass(5)
	pass.Module = "svc"
	if _, err := pass.Run(w.ctx); err != nil {
		t.Fatal(err)
	}
	st := w.state()
	if !slices.Equal(st.Watermark, root) || !slices.Equal(st.WatermarkOf("svc"), []string{"c6"}) || st.WatermarkOf("other") != nil {
		t.Fatalf("after the module's pass: root %v (was %v), svc %v", st.Watermark, root, st.Modules)
	}
	if w.reads["c1"] != 2 {
		t.Errorf("the module's first pass read c1 %d time(s), want 2 (once at the root, once for the module)", w.reads["c1"])
	}
	if _, died, err := w.run(5); died || err != nil { // the root again: only what it has not read
		t.Fatal(died, err)
	}
	if w.reads["c1"] != 2 || w.reads["c5"] != 2 {
		t.Errorf("the root's second pass read c1 %d and c5 %d time(s)", w.reads["c1"], w.reads["c5"])
	}
	if st := w.state(); !slices.Equal(st.Watermark, []string{"c6"}) || !slices.Equal(st.WatermarkOf("svc"), []string{"c6"}) {
		t.Errorf("watermarks: root %v, modules %v", st.Watermark, st.Modules)
	}
}
