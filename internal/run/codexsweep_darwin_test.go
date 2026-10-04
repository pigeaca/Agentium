//go:build darwin && cgo

package run

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// The sweep's matcher (codexLeftovers, which stops nothing) on this machine, with a fresh run's workspace, marker and
// temp root, in the user's temp folder, /tmp and the home folder: nothing matches, even with no start-time limit (the
// system's own sandboxed agents, which may write /tmp and the user's folders, among them). A first sweep, keyed on the
// temp root alone, matched cfprefsd and sharingd.
func TestCodexSweepMatchesNothingElse(t *testing.T) {
	home, err := os.UserHomeDir()
	must(t, err)
	probe, err := os.MkdirTemp(home, ".agentium-sweep-probe-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(probe) })
	short := shortTemp(t)
	t.Cleanup(func() { os.RemoveAll(short) })
	for _, base := range []string{t.TempDir(), short, probe} {
		s := fakeRun(t, base, "ws")
		s.since = time.Time{}
		found, err := codexLeftovers(s)
		if err != nil || len(found) != 0 {
			t.Errorf("a run in %s: the sweep would stop %v (%v)", base, found, err)
		}
	}
}

// fakeRun is a run's folders under base/workspaces/name: its checkout, its marker and its temp root; since is now.
func fakeRun(t *testing.T, base, name string) codexSweep {
	t.Helper()
	ws := filepath.Join(base, "workspaces", name)
	must(t, os.MkdirAll(filepath.Join(ws, "repo"), 0o700))
	marker, err := newMarker(ws)
	must(t, err)
	tempRoot := filepath.Join(base, "tmp-"+name)
	must(t, os.MkdirAll(tempRoot, 0o700))
	return codexSweep{workspace: ws, tempRoot: tempRoot, marker: marker, since: time.Now().Add(-time.Second)}
}

// sandboxed starts /bin/sleep in its own session under a Seatbelt profile (the test's own process: cleaned up here),
// from /, holding no descriptor of any run, and waits until its sandbox applies.
func sandboxed(t *testing.T, profile string) int {
	t.Helper()
	c := exec.Command("/usr/bin/sandbox-exec", "-p", profile, "/bin/sleep", "60")
	c.Dir, c.SysProcAttr = "/", &syscall.SysProcAttr{Setsid: true}
	must(t, c.Start())
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	time.Sleep(300 * time.Millisecond)
	return c.Process.Pid
}

// grants is a profile that lets a process write each folder and nothing else under root.
func grants(root string, writable ...string) string {
	profile := fmt.Sprintf(`(version 1)(allow default)(deny file-write* (subpath %q))`, realOf(root))
	for _, w := range writable {
		profile += fmt.Sprintf(`(allow file-write* (subpath %q))`, realOf(w))
	}
	return profile
}

// The matcher tells a run's processes by positive ownership. Not matched: an unrelated sandbox that grants several
// runs' checkouts (workspaces/*/repo) and denies their workspaces (which a sweep by the checkout alone matched); a
// concurrent second run's process; a process in the run's own profile that started before its agent. Matched: a
// process in the run's own profile, started after.
func TestCodexSweepNeedsOwnership(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	base := t.TempDir()
	runA, runB := fakeRun(t, base, "a"), fakeRun(t, base, "b")
	root := filepath.Join(base, "workspaces")
	early := sandboxed(t, grants(root, filepath.Join(runA.workspace, "repo"), runA.marker))
	runA.since, runB.since = time.Now(), time.Now()
	time.Sleep(10 * time.Millisecond)
	wildcard := sandboxed(t, grants(root, filepath.Join(runA.workspace, "repo"), filepath.Join(runB.workspace, "repo")))
	other := sandboxed(t, grants(root, filepath.Join(runB.workspace, "repo"), runB.marker))
	own := sandboxed(t, grants(root, filepath.Join(runA.workspace, "repo"), runA.marker))

	byCheckout, err := sandboxedIn(filepath.Join(runA.workspace, "repo"), runA.workspace)
	must(t, err)
	if !slices.ContainsFunc(byCheckout, func(p process) bool { return p.pid == wildcard }) {
		t.Fatalf("the wildcard sandbox does not overlap: the test proves nothing (%v)", byCheckout)
	}
	pidsOf := func(s codexSweep) []int {
		pids, err := codexLeftoverPIDs(s)
		must(t, err)
		return pids
	}
	if got := pidsOf(runA); !slices.Equal(got, []int{own}) {
		t.Errorf("run A's sweep would stop %v, want only its own process %d (wildcard %d, run B's %d, started early %d)", got, own, wildcard, other, early)
	}
	if got := pidsOf(runB); !slices.Equal(got, []int{other}) {
		t.Errorf("run B's sweep would stop %v, want only %d", got, other)
	}
}
