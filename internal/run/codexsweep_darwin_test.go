//go:build darwin && cgo

package run

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		kill, report, err := codexLeftovers(s)
		if err != nil || len(kill) != 0 || len(report) != 0 {
			t.Errorf("a run in %s: the sweep would stop %v and report %v (%v)", base, kill, report, err)
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
// in dir (/: holding nothing of any run), and waits until its sandbox applies. An empty profile: no sandbox.
func sandboxed(t *testing.T, profile, dir string) int {
	t.Helper()
	c := exec.Command("/usr/bin/sandbox-exec", "-p", profile, "/bin/sleep", "60")
	if profile == "" {
		c = exec.Command("/bin/sleep", "60")
	}
	c.Dir, c.SysProcAttr = dir, &syscall.SysProcAttr{Setsid: true}
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

// The matcher kills only on combined proof, and reports the rest (codexLeftovers; nothing is stopped here, every
// process is the test's own). Killed: the run's own child, in its sandbox and holding its cwd in the checkout. Reported,
// never killed: a sandbox that grants by pattern (workspaces/[^/]+/[^/]+, which covers the marker without knowing it)
// and denies the workspaces, started from /; a user's unsandboxed process holding its cwd in the workspace; the run's
// own detached child (in its sandbox, holding nothing). Neither: a concurrent run's child, and a process in the run's
// own profile that started before its agent.
func TestCodexSweepNeedsCombinedProof(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	runA, runB := fakeRun(t, base, "a"), fakeRun(t, base, "b")
	root := filepath.Join(base, "workspaces")
	ownA := grants(root, filepath.Join(runA.workspace, "repo"), runA.marker)
	early := sandboxed(t, ownA, filepath.Join(runA.workspace, "repo"))
	runA.since, runB.since = time.Now(), time.Now()
	time.Sleep(10 * time.Millisecond)
	pattern := sandboxed(t, fmt.Sprintf(`(version 1)(allow default)(deny file-write* (subpath %q))(allow file-write* (regex #"^%s/[^/]+/[^/]+(/.*)?$"))`,
		root, regexp.QuoteMeta(root)), "/")
	user := sandboxed(t, "", filepath.Join(runA.workspace, "repo"))
	child := sandboxed(t, ownA, filepath.Join(runA.workspace, "repo"))
	detached := sandboxed(t, ownA, "/")
	other := sandboxed(t, grants(root, filepath.Join(runB.workspace, "repo"), runB.marker), filepath.Join(runB.workspace, "repo"))

	kill, report, err := codexLeftoverPIDs(runA)
	must(t, err)
	slices.Sort(report)
	wantReport := []int{pattern, user, detached}
	slices.Sort(wantReport)
	if !slices.Equal(kill, []int{child}) || !slices.Equal(report, wantReport) {
		t.Errorf("run A's sweep would stop %v (want only its child %d) and report %v (want %v: pattern %d, user %d, detached %d); early %d, run B's %d",
			kill, child, report, wantReport, pattern, user, detached, early, other)
	}
	if kill, _, err := codexLeftoverPIDs(runB); err != nil || !slices.Equal(kill, []int{other}) {
		t.Errorf("run B's sweep would stop %v (want only %d), %v", kill, other, err)
	}
}
