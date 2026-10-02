package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The user's own entries in /tmp are resolved through like any path, a link included: AGENTIUM_HOME=/tmp/ag with
// /tmp/ag a link the user made must be denied where it really lies, or the sandbox, which matches real paths, denies
// nothing. Below them, the missing rest is appended; a missing name follows only /tmp itself.
func TestFormsResolveTheUsersOwnEntriesInTmp(t *testing.T) {
	target := t.TempDir()
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join("/tmp", fmt.Sprintf("agentium-forms-own-%d", os.Getpid()))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create %s: %v", link, err)
	}
	t.Cleanup(func() { os.Remove(link) })
	for _, rest := range []string{"", "inside", "inside/missing", "missing/deeper"} {
		p := filepath.Join(link, rest)
		if got, want := Forms(p), []string{p, filepath.Join(targetReal, rest)}; !slices.Equal(got, want) {
			t.Errorf("Forms(%s) = %q, want %q", p, got, want)
		}
	}

	tmp, err := filepath.EvalSymlinks("/tmp") // the system's own link, /private/tmp on macOS
	if err != nil {
		t.Fatal(err)
	}
	withReal := func(p, real string) []string {
		if real == p {
			return []string{p}
		}
		return []string{p, real}
	}
	missing := filepath.Join("/tmp", fmt.Sprintf("agentium-forms-missing-%d", os.Getpid()), "x")
	if got, want := Forms(missing), withReal(missing, filepath.Join(tmp, filepath.Base(filepath.Dir(missing)), "x")); !slices.Equal(got, want) {
		t.Errorf("Forms(%s) = %q, want %q", missing, got, want)
	}
	dir := filepath.Join("/tmp", fmt.Sprintf("agentium-forms-real-%d", os.Getpid()))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "missing", "x")
	if got, want := Forms(p), withReal(p, filepath.Join(tmp, filepath.Base(dir), "missing", "x")); !slices.Equal(got, want) {
		t.Errorf("Forms(%s) = %q, want %q", p, got, want)
	}
	if outside := t.TempDir(); !slices.Equal(Forms(outside), formsResolved(outside)) {
		t.Errorf("a path outside /tmp is not resolved as before")
	}
}

// Another user's entry in /tmp, a link or a folder, is never resolved through: only /tmp itself is, and the rest is
// kept as written, so their link cannot make a run deny (and refuse to start over) a folder of their choosing, nor can
// they swap their folder for such a link between two looks. Root's entries and the user's own are resolved. A test
// cannot make another user's entry, so the decision is checked with the owner given.
func TestTmpFormFollowsOnlyTheUsersOwnAndRootsEntries(t *testing.T) {
	target := t.TempDir()
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("agentium-forms-other-%d", os.Getpid())
	link := filepath.Join("/tmp", name)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create %s: %v", link, err)
	}
	t.Cleanup(func() { os.Remove(link) })
	owner, exists := entryOwner(link)
	if !exists || int64(owner) != int64(os.Getuid()) {
		t.Fatalf("entryOwner(%s) = %d %v, want this user's %d", link, owner, exists, os.Getuid())
	}
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	other := uint32(uid + 1)
	for _, rest := range []string{"inside", "inside/missing", "missing/deeper"} {
		p := filepath.Join(link, rest)
		if got, want := tmpForm(p, "/tmp", name, rest, true, other, uid), filepath.Join(tmp, name, rest); got != want {
			t.Errorf("another user's entry: tmpForm(%s) = %q, want %q (never through it to %s)", p, got, want, target)
		}
		if got, want := tmpForm(p, "/tmp", name, rest, false, 0, uid), filepath.Join(tmp, name, rest); got != want {
			t.Errorf("a missing entry: tmpForm(%s) = %q, want %q", p, got, want)
		}
		for _, trusted := range []uint32{uint32(uid), 0} {
			if got, want := tmpForm(p, "/tmp", name, rest, true, trusted, uid), filepath.Join(targetReal, rest); got != want {
				t.Errorf("an entry of uid %d: tmpForm(%s) = %q, want %q", trusted, p, got, want)
			}
		}
	}
	// The /private/tmp spelling of another user's entry stays there.
	p := filepath.Join("/private/tmp", name, "inside")
	if got, want := tmpForm(p, "/private/tmp", name, "inside", true, other, uid), filepath.Join(resolvedPrefix("/private/tmp"), name, "inside"); got != want {
		t.Errorf("tmpForm(%s) = %q, want %q", p, got, want)
	}
}

// formsResolved is the plain rule outside /tmp: the path and, when different, its resolved form.
func formsResolved(p string) []string {
	out := []string{filepath.Clean(p)}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != out[0] {
		out = append(out, r)
	}
	return out
}
