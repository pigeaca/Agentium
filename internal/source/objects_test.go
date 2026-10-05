package source

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pigeaca/agentium/internal/gitx/gitxtest"
)

// twoCommits is fixture's repository with a second commit that changes CLAUDE.md alone, so the two commits share
// every other blob.
func twoCommits(t *testing.T) (root, first, second string) {
	t.Helper()
	root, first = fixture(t)
	write(t, root, "CLAUDE.md", "@docs/rules.md\nMore.\n", 0o644)
	git(t, root, "commit", "-q", "-am", "second")
	return root, first, git(t, root, "rev-parse", "HEAD")
}

// readAll reads every file of src: its content, or its error's text.
func readAll(src Source) map[string]string {
	out := map[string]string{}
	for _, p := range src.Paths() {
		data, err := src.ReadFile(p)
		if err != nil {
			out[p] = "error: " + err.Error()
			continue
		}
		out[p] = string(data)
	}
	return out
}

// An Objects' sources answer as Commit's do (contents, symbolic links followed or refused, modes, links), while git
// lists each commit once and reads each blob once: the second commit costs its listing and its one new blob.
func TestObjectsReadEachObjectOnce(t *testing.T) {
	root, first, second := twoCommits(t)
	ctx := context.Background()
	want := map[string]map[string]string{}
	for _, commit := range []string{first, second} {
		plain, err := Commit(ctx, commit, "-C", root)
		if err != nil {
			t.Fatal(err)
		}
		want[commit] = readAll(plain)
	}
	calls := gitxtest.Calls(t)
	objects := NewObjects("-C", root)
	for range 3 {
		for _, commit := range []string{first, second} {
			src, err := objects.Commit(ctx, commit)
			if err != nil {
				t.Fatal(err)
			}
			if got := readAll(src); !mapsEqual(got, want[commit]) {
				t.Errorf("%s: read %q, want %q", src.Describe(), got, want[commit])
			}
			if !src.Executable(".claude/hooks/check.sh") || src.Executable("CLAUDE.md") {
				t.Errorf("%s: modes not kept", src.Describe())
			}
			if target, ok, err := Link(src, "AGENTS.md"); err != nil || !ok || target != "docs/rules.md" {
				t.Errorf("%s: Link = %q, %v, %v", src.Describe(), target, ok, err)
			}
			if _, err := src.ReadFile("missing.md"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: missing file: %v", src.Describe(), err)
			}
		}
	}
	made := calls()
	// Blobs: .claude/hooks/check.sh, .gitignore, docs/rules.md, the two links' targets as stored, and CLAUDE.md twice.
	if lists, reads := gitxtest.Count(made, "ls-tree"), gitxtest.Count(made, "cat-file"); lists != 2 || reads != 7 || len(made) != 9 {
		t.Errorf("%d listings and %d reads in %d git calls, want 2 and 7 in 9:\n%v", lists, reads, len(made), made)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// A branch can move, so a commit named by one is listed every time, and sees the move; its blobs are still read once.
func TestObjectsListAMovingNameEachTime(t *testing.T) {
	root, _ := fixture(t)
	ctx := context.Background()
	calls := gitxtest.Calls(t)
	objects := NewObjects("-C", root)
	before, err := objects.Commit(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := before.ReadFile("CLAUDE.md"); err != nil || string(data) != "@docs/rules.md\n" {
		t.Fatalf("before: %q, %v", data, err)
	}
	if _, err := before.ReadFile("docs/rules.md"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "CLAUDE.md", "moved\n", 0o644)
	git(t, root, "commit", "-q", "-am", "move main")
	calls() // the test's own git calls
	after, err := objects.Commit(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := after.ReadFile("CLAUDE.md"); err != nil || string(data) != "moved\n" {
		t.Errorf("after the branch moved: %q, %v; want the new content", data, err)
	}
	if data, err := after.ReadFile("docs/rules.md"); err != nil || string(data) != "rules\n" {
		t.Errorf("unchanged file: %q, %v", data, err)
	}
	if made := calls(); gitxtest.Count(made, "ls-tree") != 1 || gitxtest.Count(made, "cat-file") != 1 {
		t.Errorf("after the move: %v, want one listing and one read (the changed file)", made)
	}
	for _, name := range []string{"main", "HEAD", strings.Repeat("A", 40), strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("g", 40)} {
		if fullID(name) {
			t.Errorf("fullID(%q) = true", name)
		}
	}
	if !fullID(strings.Repeat("0a", 20)) || !fullID(strings.Repeat("f9", 32)) {
		t.Error("fullID refuses a SHA-1 or SHA-256 ID")
	}
}

// A read that fails keeps nothing (the next one asks git again), and a cancelled context is given nothing kept: its
// listing and its reads fail as a source of Commit's would.
func TestObjectsKeepNothingOfAFailedOrCancelledRead(t *testing.T) {
	root, head := fixture(t)
	ctx := context.Background()
	objects := NewObjects("-C", root)
	src, err := objects.Commit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	blob := git(t, root, "rev-parse", head+":CLAUDE.md")
	object := filepath.Join(root, ".git", "objects", blob[:2], blob[2:])
	if err := os.Rename(object, object+".away"); err != nil {
		t.Fatal(err)
	}
	plain, err := Commit(ctx, head, "-C", root)
	if err != nil {
		t.Fatal(err)
	}
	_, wantErr := plain.ReadFile("CLAUDE.md")
	if _, err := src.ReadFile("CLAUDE.md"); err == nil || wantErr == nil || err.Error() != wantErr.Error() {
		t.Fatalf("a blob git cannot read: %v, want Commit's error %v", err, wantErr)
	}
	if err := os.Rename(object+".away", object); err != nil {
		t.Fatal(err)
	}
	if data, err := src.ReadFile("CLAUDE.md"); err != nil || string(data) != "@docs/rules.md\n" {
		t.Errorf("after the failure: %q, %v; want the content", data, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := objects.Commit(cancelled, head); err == nil {
		t.Error("a cancelled context got a kept listing")
	}
	stopped, stop := context.WithCancel(ctx)
	late, err := objects.Commit(stopped, head)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := late.ReadFile("CLAUDE.md"); err == nil {
		t.Error("a cancelled context got a kept blob")
	}
	// Neither failure cost what was kept.
	calls := gitxtest.Calls(t)
	again, err := objects.Commit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := again.ReadFile("CLAUDE.md"); err != nil || string(data) != "@docs/rules.md\n" {
		t.Errorf("after the cancelled reads: %q, %v", data, err)
	}
	if made := calls(); len(made) != 0 {
		t.Errorf("git calls for what was kept: %v", made)
	}
}

// Several goroutines read the same commits at once: each object is still read once, since the others wait for the
// first, and every reader gets the right content.
func TestObjectsServeSeveralGoroutines(t *testing.T) {
	root, first, second := twoCommits(t)
	ctx := context.Background()
	calls := gitxtest.Calls(t)
	objects := NewObjects("-C", root)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			commit, want := first, "@docs/rules.md\n"
			if i%2 == 1 {
				commit, want = second, "@docs/rules.md\nMore.\n"
			}
			src, err := objects.Commit(ctx, commit)
			if err != nil {
				t.Error(err)
				return
			}
			for _, p := range []string{"CLAUDE.md", "docs/rules.md", "AGENTS.md", ".gitignore"} {
				data, err := src.ReadFile(p)
				if err != nil {
					t.Errorf("%s: %v", p, err)
				} else if p == "CLAUDE.md" && string(data) != want {
					t.Errorf("%s of %s = %q, want %q", p, commit, data, want)
				}
			}
		}()
	}
	wg.Wait()
	// Blobs: CLAUDE.md twice, docs/rules.md, the AGENTS.md link and .gitignore.
	if made := calls(); gitxtest.Count(made, "ls-tree") != 2 || gitxtest.Count(made, "cat-file") != 5 {
		t.Errorf("%d listings and %d reads, want 2 and 5:\n%v", gitxtest.Count(made, "ls-tree"), gitxtest.Count(made, "cat-file"), made)
	}
}

// What an Objects keeps is bounded: a blob that does not fit is read from git every time, and still read right.
func TestObjectsKeepBlobsUpToTheirLimit(t *testing.T) {
	root, _ := fixture(t)
	big := strings.Repeat("x", 100)
	write(t, root, "big.txt", big, 0o644)
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "big")
	head := git(t, root, "rev-parse", "HEAD")
	calls := gitxtest.Calls(t)
	objects := NewObjects("-C", root)
	objects.limit = 50
	src, err := objects.Commit(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if data, err := src.ReadFile("big.txt"); err != nil || string(data) != big {
			t.Fatalf("big.txt: %d bytes, %v", len(data), err)
		}
		if data, err := src.ReadFile("docs/rules.md"); err != nil || string(data) != "rules\n" {
			t.Fatalf("docs/rules.md: %q, %v", data, err)
		}
	}
	if made := calls(); gitxtest.Count(made, "cat-file") != 4 {
		t.Errorf("%d reads, want 4 (the big blob three times, the small one once):\n%v", gitxtest.Count(made, "cat-file"), made)
	}
	if objects.kept != len("rules\n") {
		t.Errorf("kept %d bytes, want %d", objects.kept, len("rules\n"))
	}
}

// What a caller is given is its own: changing a file's content or a source's paths changes no later answer.
func TestObjectsGiveEachCallerItsOwnCopy(t *testing.T) {
	root, head := fixture(t)
	ctx := context.Background()
	objects := NewObjects("-C", root)
	src, err := objects.Commit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // the first read comes from git, the second from what is kept
		data, err := src.ReadFile("docs/rules.md")
		if err != nil || string(data) != "rules\n" {
			t.Fatalf("docs/rules.md: %q, %v", data, err)
		}
		copy(data, bytes.ToUpper(data))
	}
	paths := src.Paths()
	want := slices.Clone(paths)
	slices.Reverse(paths)
	other, err := objects.Commit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(other.Paths(), want) {
		t.Errorf("another source's paths = %v, want %v", other.Paths(), want)
	}
	if data, err := other.ReadFile("docs/rules.md"); err != nil || string(data) != "rules\n" {
		t.Errorf("docs/rules.md after a caller changed its copy: %q, %v", data, err)
	}
}
