package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// A read of the process table is not one instant: it read the tracked parent P (started at 10), then P ended, a
// stranger took its ID (P', at 20) and started C (at 21), and the read reached C after. Joined by IDs alone, C would
// look like P's child. It is not linked: C was born after the read began; and when a clock step makes it look older,
// its parent's ID is held by another process now, so the link is not trusted either. A real child born before the
// read, whose parent still holds its ID (or is gone), is linked, with the chain between them.
func TestSnapshotLinksOnlyProvenDescendants(t *testing.T) {
	root, p := at(100, 1, "codex"), at(200, 10, "sh")
	stranger, c := at(200, 20, "sh"), at(300, 21, "sleep")
	// By the time the read is checked, the stranger has ended too: P's ID is free, which proves nothing either way.
	f := &fakeProcs{clock: time.Unix(1_800_000_015, 0), alive: map[int]identity{300: c, 100: root}}
	f.table = map[int]tableEntry{100: entry(root, 1), 200: entry(p, 100), 300: entry(c, 200)}
	d := f.tracker("")
	d.tracked[root.key()] = root
	must(t, d.snapshot(false))
	if got := pids(d.list()); !slices.Equal(got, []int{100, 200}) {
		t.Fatalf("tracked %v after a mixed read that began at 15: want the root and P only", got)
	}
	// A clock stepped back: the read seems to begin at 25, after C's start. P's ID is the stranger's now.
	f.clock = time.Unix(1_800_000_025, 0)
	f.alive[200] = stranger
	must(t, d.snapshot(false))
	if got := pids(d.list()); slices.Contains(got, 300) {
		t.Fatalf("tracked %v: a child joined to a parent whose ID another process holds now", got)
	}
	// The next read sees the stranger under P's ID: P is gone (dropped), and C's parent is the stranger.
	f.table = map[int]tableEntry{100: entry(root, 1), 200: entry(stranger, 1), 300: entry(c, 200)}
	f.clock = time.Unix(1_800_000_030, 0)
	must(t, d.snapshot(false))
	if got := pids(d.list()); !slices.Equal(got, []int{100}) {
		t.Fatalf("tracked %v: want only the root (P ended; the stranger and its child are not the agent's)", got)
	}

	// The real case: a grandchild (born before the read), its parent alive and still holding its ID, or gone.
	child, grandchild := at(400, 3, "bash"), at(500, 4, "node")
	f.table = map[int]tableEntry{100: entry(root, 1), 400: entry(child, 100), 500: entry(grandchild, 400)}
	f.alive = map[int]identity{100: root, 500: grandchild} // the child ended after the read: gone, not replaced
	must(t, d.snapshot(false))
	if got := pids(d.list()); !slices.Equal(got, []int{100, 400, 500}) {
		t.Fatalf("tracked %v: want the root, its child and its grandchild", got)
	}
	// A parent that started after its child is not its parent (an ID reused while the child was reparented).
	late := at(600, 50, "late")
	f.table[700] = entry(at(700, 40, "orphan"), 600)
	f.table[600] = entry(late, 100)
	f.clock = time.Unix(1_800_000_060, 0)
	f.alive[600] = late
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
