package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// at is a process identity started at second sec of a fixed day.
func at(pid int, sec uint64, command string) identity {
	return identity{PID: pid, Sec: 1_800_000_000 + sec, Usec: 0, Command: command}
}

// fakeProcs is a process table the test sets (read, identify) and a clock it moves (now).
type fakeProcs struct {
	table map[int]tableEntry // what descendants.snapshot's read returns
	alive map[int]identity   // what identify returns now (missing: gone)
	clock time.Time
	fail  error // the read's error
}

func (f *fakeProcs) tracker(file string) *descendants {
	d := loadDescendants(file)
	d.read = func() (map[int]tableEntry, error) { return f.table, f.fail }
	d.identify = func(pid int) (identity, bool) { id, ok := f.alive[pid]; return id, ok }
	d.now = func() time.Time { return f.clock }
	return d
}

func entry(id identity, ppid int) tableEntry { return tableEntry{id: id, ppid: ppid} }

func pids(ids []identity) []int {
	out := []int{}
	for _, id := range ids {
		out = append(out, id.PID)
	}
	return out
}

// A read of the process table is not one instant: it read the tracked parent P (started at 10); then P ended, a
// stranger took its ID, started C (at 21) and ended too, before the read was checked; the read reached C after. Joined
// by IDs alone, C would look like P's child, whatever the clock says. It is not linked: its parent's ID, checked after
// the read, no longer holds P (gone, or held by the stranger). A child whose parent was tracked before the read and
// still holds its ID after it is linked.
func TestSnapshotLinksOnlyProvenDescendants(t *testing.T) {
	root, p := at(100, 1, "codex"), at(200, 10, "sh")
	stranger, c := at(200, 20, "sh"), at(300, 21, "sleep")
	f := &fakeProcs{clock: time.Unix(1_800_000_025, 0), alive: map[int]identity{100: root, 300: c}} // P's ID free again
	f.table = map[int]tableEntry{100: entry(root, 1), 200: entry(p, 100), 300: entry(c, 200)}
	d := f.tracker("")
	d.tracked[root.key()] = root
	d.tracked[p.key()] = p
	must(t, d.snapshot(false))
	if got := pids(d.list()); slices.Contains(got, 300) {
		t.Fatalf("tracked %v: a child joined to a parent that was gone when the read was checked", got)
	}
	f.alive[200] = stranger // the stranger still runs when the read is checked
	must(t, d.snapshot(false))
	if got := pids(d.list()); slices.Contains(got, 300) {
		t.Fatalf("tracked %v: a child joined to a parent whose ID another process holds now", got)
	}
	f.alive[200] = p // P runs throughout: C is its child
	must(t, d.snapshot(false))
	if got := pids(d.list()); !slices.Equal(got, []int{100, 200, 300}) {
		t.Fatalf("tracked %v: want the root, P and its child C", got)
	}
	// A parent that started after its child is not its parent (an ID reused while the child was reparented).
	late := at(600, 50, "late")
	f.table[600], f.alive[600] = entry(late, 100), late
	f.table[700] = entry(at(700, 40, "orphan"), 600)
	must(t, d.snapshot(false))
	must(t, d.snapshot(false))
	if got := pids(d.list()); slices.Contains(got, 700) || !slices.Contains(got, 600) {
		t.Fatalf("tracked %v: the root's late child is, the older process under it is not", got)
	}
	// A process that exited and is not yet reaped (a zombie) can no longer be stopped: dropped, and never linked.
	f.table[600] = tableEntry{id: late, ppid: 100, zombie: true}
	f.table[800] = tableEntry{id: at(800, 55, "exited"), ppid: 100, zombie: true}
	must(t, d.snapshot(false))
	if got := pids(d.list()); slices.Contains(got, 600) || slices.Contains(got, 800) {
		t.Fatalf("tracked %v: zombies kept", got)
	}
}

// Each snapshot links one generation (a parent must be tracked before its read began): a three-level chain under the
// agent is linked over three snapshots, a level each.
func TestSnapshotLinksAGenerationAtATime(t *testing.T) {
	root, a, b, c := at(100, 1, "codex"), at(200, 2, "bash"), at(300, 3, "make"), at(400, 4, "cc")
	f := &fakeProcs{alive: map[int]identity{100: root, 200: a, 300: b, 400: c}}
	f.table = map[int]tableEntry{100: entry(root, 1), 200: entry(a, 100), 300: entry(b, 200), 400: entry(c, 300)}
	d := f.tracker("")
	d.observe(100)
	for i, want := range [][]int{{100, 200}, {100, 200, 300}, {100, 200, 300, 400}} {
		must(t, d.snapshot(false))
		if got := pids(d.list()); !slices.Equal(got, want) {
			t.Fatalf("after snapshot %d: tracked %v, want %v", i+1, got, want)
		}
	}
}

// Two snapshots that overlap (the poll's, whose read is slow, and BeforeStop's) run one after the other: the poll's
// older read, without C, never drops C, which the later read found.
func TestSnapshotsDoNotInterleave(t *testing.T) {
	root, c := at(100, 1, "codex"), at(300, 3, "sleep")
	old := map[int]tableEntry{100: entry(root, 1)}
	fresh := map[int]tableEntry{100: entry(root, 1), 300: entry(c, 100)}
	reading, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f := &fakeProcs{alive: map[int]identity{100: root, 300: c}}
	d := f.tracker("")
	d.tracked[root.key()] = root
	d.read = func() (map[int]tableEntry, error) {
		if calls.Add(1) == 1 { // the poll's read: taken before C started, returned late
			close(reading)
			<-release
			return old, nil
		}
		return fresh, nil
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = d.snapshot(false) }()
	<-reading
	go func() { defer wg.Done(); _ = d.snapshot(true) }()
	time.Sleep(50 * time.Millisecond) // were they not serialized, BeforeStop's snapshot would be done by now
	close(release)
	wg.Wait()
	if got := pids(d.list()); !slices.Equal(got, []int{100, 300}) {
		t.Fatalf("tracked %v: the older read dropped what the newer one found", got)
	}
}

// What the tracker sees is saved: at once when the agent starts (its root) and when forced (before a stop, at the
// sweep), otherwise at most every descendantsSave; a save that fails is kept dirty, retried, and told in the notes;
// processes no longer alive are dropped.
func TestDescendantsSaves(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gone", AgentProcesses) // its folder is missing: saves fail
	root, child := at(100, 1, "codex"), at(200, 2, "sh")
	f := &fakeProcs{clock: time.Unix(1_800_000_010, 0), alive: map[int]identity{100: root, 200: child}}
	f.table = map[int]tableEntry{100: entry(root, 1)}
	d := f.tracker(file)
	poll := d.observe(100)
	if poll == nil || !slices.Equal(pids(d.list()), []int{100}) {
		t.Fatalf("the root was not tracked when the agent started: %v", d.list())
	}
	if note := d.note(); !strings.Contains(note, "could not be saved") || !strings.Contains(note, "would not stop") {
		t.Fatalf("a failed save is not told: %q", note)
	}
	must(t, os.MkdirAll(filepath.Dir(file), 0o700))
	saved := func() []int {
		var list []identity
		data, err := os.ReadFile(file)
		if err != nil {
			return nil
		}
		must(t, json.Unmarshal(data, &list))
		return pids(list)
	}
	f.clock = f.clock.Add(100 * time.Millisecond)
	must(t, d.snapshot(false)) // within the second since the last attempt: not yet
	if saved() != nil {
		t.Fatal("saved before the rate limit allowed")
	}
	f.clock = f.clock.Add(time.Second)
	must(t, d.snapshot(false)) // the failed save is retried
	if got := saved(); !slices.Equal(got, []int{100}) {
		t.Fatalf("saved %v: the dirty root was not saved again", got)
	}
	if note := d.note(); !strings.Contains(note, "a later save succeeded") {
		t.Errorf("note %q", note)
	}
	f.table[200] = entry(child, 100)
	f.clock = f.clock.Add(100 * time.Millisecond)
	must(t, d.snapshot(false))
	if got := saved(); !slices.Equal(got, []int{100}) {
		t.Fatalf("saved %v within the rate limit", got)
	}
	must(t, d.snapshot(true)) // before a stop
	if got := saved(); !slices.Equal(got, []int{100, 200}) {
		t.Fatalf("saved %v: a forced save waited", got)
	}
	delete(f.table, 100) // the agent ended and was reaped
	must(t, d.snapshot(true))
	if got := saved(); !slices.Equal(got, []int{200}) {
		t.Fatalf("saved %v: the ended agent is still listed", got)
	}
	// A read that fails changes nothing, and still saves what is dirty.
	f.fail = errors.New("no table")
	if err := d.snapshot(true); err == nil || !slices.Equal(pids(d.list()), []int{200}) {
		t.Fatalf("a failed read: %v, %v", err, d.list())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	poll(ctx) // ends with its context
}

// An agent whose own process cannot be read leaves nothing tracked, and the notes say so.
func TestDescendantsWithoutTheRoot(t *testing.T) {
	f := &fakeProcs{alive: map[int]identity{}}
	d := f.tracker("")
	d.observe(100)
	if len(d.list()) != 0 || !strings.Contains(d.note(), "could not be read") {
		t.Errorf("tracked %v, note %q", d.list(), d.note())
	}
}
