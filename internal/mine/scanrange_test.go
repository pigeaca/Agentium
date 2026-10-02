package mine

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/store"
)

// feature commits a candidate: a function and its test in a file pair of their own.
func (f *fixture) feature(name string) string {
	f.t.Helper()
	return f.commit("Add "+name+" to the parser\n\nThe "+name+" helper reads one more unit of the duration syntax.",
		map[string]string{name + ".go": "package parse\n" + lines("// "+name, 20), name + "_test.go": "package parse\n" + lines("// test "+name, 15)})
}

// day is the fixture's date of a day of January 2026 (gitAt's).
func day(n int) time.Time { return time.Date(2026, 1, n, 12, 0, 0, 0, time.UTC) }

// passes scans the range after the watermark again and again as pool passes do (the watermark becomes Through, or the
// head once a scan is complete), and returns every commit each scan read inside the window, in order.
func passes(t *testing.T, f *fixture, r pool.ScanRange, bound int) (reads [][]string, last RangeResult) {
	t.Helper()
	for range 20 {
		res, err := ScanRange(context.Background(), RangeInput{Root: f.root, Bare: filepath.Join(f.root, ".git"), Range: r, Options: Options{MaxCommits: bound}})
		if err != nil {
			t.Fatal(err)
		}
		var read []string
		for _, c := range res.Result.Candidates {
			read = append(read, c.Hash)
		}
		for _, rej := range res.Result.Rejected {
			read = append(read, rej.Hash)
		}
		if len(read) != res.Result.Scanned {
			t.Fatalf("scanned %d, classified %d", res.Result.Scanned, len(read))
		}
		reads, last = append(reads, read), res
		if res.Scanned.Complete {
			return reads, last
		}
		if len(res.Scanned.Through) == 0 {
			t.Fatal("a bounded scan without Through tips")
		}
		r.Exclude = append(r.Exclude, res.Scanned.Through...)
	}
	t.Fatal("the scans never completed")
	return nil, last
}

// A bounded scan reads a prefix, oldest first; its Through tips, as the next watermark, make the next scan read the
// rest, so every commit (merges and the branch behind them included) is read exactly once.
func TestScanRangeBoundedReadsEveryCommitOnceThroughMerges(t *testing.T) {
	f := newFixture(t)
	all := []string{f.commit("Initial commit", map[string]string{"parse.go": "package parse\n", "parse_test.go": "package parse\n"})}
	all = append(all, f.feature("hours"), f.feature("minutes"))
	f.git("checkout", "-q", "-b", "side")
	all = append(all, f.feature("weeks"), f.feature("days"))
	f.git("checkout", "-q", "main")
	all = append(all, f.feature("seconds"))
	f.day++
	f.git("merge", "-q", "--no-ff", "-m", "Merge branch 'side'", "side")
	all = append(all, f.git("rev-parse", "HEAD"), f.feature("millis"))
	head := f.git("rev-parse", "HEAD")

	reads, last := passes(t, f, pool.ScanRange{Head: head}, 2)
	var seen []string
	for i, read := range reads {
		if len(read) > 2 {
			t.Errorf("scan %d read %d commits past its bound of 2", i, len(read))
		}
		seen = append(seen, read...)
	}
	if len(reads) < 4 {
		t.Errorf("%d scans for %d commits with a bound of 2", len(reads), len(all))
	}
	slices.Sort(seen)
	want := slices.Sorted(slices.Values(all))
	if !slices.Equal(seen, want) {
		t.Errorf("read %v,\nwant each of %v once", seen, want)
	}
	if !last.Scanned.Complete || len(last.Scanned.Unknown) != 0 {
		t.Errorf("last scan: %+v", last.Scanned)
	}
	// Oldest first: the first scan read the root and its child, never the head.
	if first := slices.Sorted(slices.Values(reads[0])); !slices.Equal(first, slices.Sorted(slices.Values(all[:2]))) {
		t.Errorf("first scan read %v, want the two oldest %v", reads[0], all[:2])
	}
}

// The window is judged commit by commit, by committer date: an old commit between new ones is read past (counted as
// Old, never mined), and the new commits behind it are still read, which git's own --since could stop short of.
func TestScanRangeJudgesTheWindowPerCommit(t *testing.T) {
	f := newFixture(t)
	f.day = 9
	f.commit("Initial commit", map[string]string{"parse.go": "package parse\n", "parse_test.go": "package parse\n"})
	newer := f.feature("hours")
	f.day = 1 // the next commit is on day 2: committed "earlier" than its parent, as a rebase with an old committer date leaves it
	old := f.feature("minutes")
	f.day = 19
	newest := f.feature("seconds")
	head := f.git("rev-parse", "HEAD")

	res, err := ScanRange(context.Background(), RangeInput{Root: f.root, Bare: filepath.Join(f.root, ".git"),
		Range: pool.ScanRange{Head: head, Since: day(5)}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range res.Result.Candidates {
		got = append(got, c.Hash)
		if c.BaseDate.IsZero() || c.Patch == "" {
			t.Errorf("%s: base date %v, patch %q", c.Hash, c.BaseDate, c.Patch)
		}
	}
	slices.Sort(got)
	if want := slices.Sorted(slices.Values([]string{newer, newest})); !slices.Equal(got, want) || slices.Contains(got, old) {
		t.Errorf("candidates %v, want %v (not the old %s)", got, want, old)
	}
	if res.Old != 1 || !res.Scanned.Complete {
		t.Errorf("old %d, complete %v", res.Old, res.Scanned.Complete)
	}
	if i := slices.IndexFunc(res.Result.Candidates, func(c Candidate) bool { return c.Hash == newest }); i < 0 || !res.Result.Candidates[i].BaseDate.Equal(day(2)) {
		t.Errorf("the newest candidate's base date should be its parent's committer date, %v", day(2))
	}
}

// A watermark commit the repository no longer has (a force-push, then gc) is reported and ignored, so its part of
// the history is read again rather than every scan failing.
func TestScanRangeReportsAnUnknownWatermark(t *testing.T) {
	f := newFixture(t)
	f.commit("Initial commit", map[string]string{"parse.go": "package parse\n", "parse_test.go": "package parse\n"})
	first := f.feature("hours")
	second := f.feature("minutes")
	gone := "0123456789abcdef0123456789abcdef01234567"
	res, err := ScanRange(context.Background(), RangeInput{Root: f.root, Bare: filepath.Join(f.root, ".git"),
		Range: pool.ScanRange{Head: second, Exclude: []string{gone, first, "not-a-hash"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Scanned.Unknown, []string{gone, "not-a-hash"}) { // in the order given
		t.Errorf("unknown %v", res.Scanned.Unknown)
	}
	if res.Result.Scanned != 1 || len(res.Result.Candidates) != 1 || res.Result.Candidates[0].Hash != second {
		t.Errorf("read %d, candidates %+v; want only %s", res.Result.Scanned, res.Result.Candidates, second)
	}
	// Only unknown commits: the whole history is read.
	res, err = ScanRange(context.Background(), RangeInput{Root: f.root, Bare: filepath.Join(f.root, ".git"),
		Range: pool.ScanRange{Head: second, Exclude: []string{gone}}})
	if err != nil || res.Result.Scanned != 3 {
		t.Errorf("read %d, %v; want all 3", res.Result.Scanned, err)
	}
}

// A rebased or cherry-picked copy of any task's change (by patch ID, read from Agentium's repository) is set aside, as
// are dismissed commits and copies of dismissed changes.
func TestScanRangeSkipsCopiesByPatchID(t *testing.T) {
	f := newFixture(t)
	root := f.commit("Initial commit", map[string]string{"parse.go": "package parse\n", "parse_test.go": "package parse\n"})
	f.git("checkout", "-q", "-b", "old")
	original := f.feature("hours")
	dropped := f.feature("weeks")
	f.git("checkout", "-q", "main")
	f.commit("Note the unit table", map[string]string{"units.md": "# Units\n"})
	f.day++
	f.git("cherry-pick", original)
	copied := f.git("rev-parse", "HEAD")
	f.day++
	f.git("cherry-pick", dropped)
	droppedCopy := f.git("rev-parse", "HEAD")
	fresh := f.feature("minutes")
	dismissed := f.feature("seconds")
	head := f.git("rev-parse", "HEAD")
	if copied == original {
		t.Fatal("the cherry-pick kept the commit")
	}

	bare := filepath.Join(f.root, ".git") // holds the tasks' commits, as Agentium's repository does
	tasks := []store.Task{{Name: "hand-imported", BaseCommit: root, SolutionCommit: original}}
	ids, err := PatchIDs(context.Background(), [][2]string{{dropped, original}}, "--git-dir", bare)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ScanRange(context.Background(), RangeInput{Root: f.root, Bare: bare, Tasks: tasks,
		Range: pool.ScanRange{Head: head, Dismissed: []string{dismissed}, DismissedPatches: []string{ids[dropped]}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Result.Candidates) != 1 || res.Result.Candidates[0].Hash != fresh {
		t.Errorf("candidates %+v, want only %s", res.Result.Candidates, fresh)
	}
	reasons := map[string]Reason{}
	for _, r := range res.Result.Rejected {
		reasons[r.Hash] = r.Reason
	}
	for hash, want := range map[string]Reason{copied: ReasonCopy, droppedCopy: ReasonDismissed, dismissed: ReasonDismissed} {
		if reasons[hash] != want {
			t.Errorf("%s: %q, want %q", hash, reasons[hash], want)
		}
	}
}

func TestCommitTimesAndTreeHas(t *testing.T) {
	f := newFixture(t)
	f.day = 3
	c := f.commit("Initial commit", map[string]string{"a b.go": "package a\n", "*.go": "package glob\n"})
	times, err := CommitTimes(context.Background(), []string{c, "0123456789abcdef0123456789abcdef01234567"}, "-C", f.root)
	if err != nil || len(times) != 1 || !times[c].Equal(day(4)) {
		t.Errorf("times %v, %v", times, err)
	}
	has, err := TreeHas(context.Background(), c, []string{"a b.go", "*.go", "gone.go"}, "-C", f.root)
	if err != nil || fmt.Sprint(has) != "map[*.go:true a b.go:true]" {
		t.Errorf("has %v, %v (a literal *.go must not match a b.go twice or gone.go)", has, err)
	}
}
