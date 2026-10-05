//go:build darwin && cgo

package run

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The reports' matcher (codexReports, which stops nothing) on this machine, for a fresh run's workspace, marker and
// temp root in the user's temp folder, /tmp and the home folder: nothing matches, even with no start-time limit. A
// first sweep, keyed on the temp root alone, matched cfprefsd and sharingd.
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
		if found, err := codexReportPIDs(s); err != nil || len(found) != 0 {
			t.Errorf("a run in %s: the sweep would report %v (%v)", base, found, err)
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

// started runs argv in its own session in dir under a Seatbelt profile ("": none), as the test's own process, waits
// until its sandbox applies, and stops it at the end only while it is still that process (identity checked: killOwn).
func started(t *testing.T, profile, dir string, argv ...string) int {
	t.Helper()
	return startedCmd(t, profile, dir, argv...).Process.Pid
}

// startedCmd is started's command.
func startedCmd(t *testing.T, profile, dir string, argv ...string) *exec.Cmd {
	t.Helper()
	if profile != "" {
		argv = append([]string{"/usr/bin/sandbox-exec", "-p", profile}, argv...)
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir, c.SysProcAttr = dir, &syscall.SysProcAttr{Setsid: true}
	must(t, c.Start())
	t.Cleanup(func() { c.Process.Kill(); c.Wait() }) // the test's own child, not yet reaped: its ID is not reused
	time.Sleep(300 * time.Millisecond)
	return c
}

// grants is a profile that lets a process write each folder and nothing else under root.
func grants(root string, writable ...string) string {
	profile := fmt.Sprintf(`(version 1)(allow default)(deny file-write* (subpath %q))`, realOf(root))
	for _, w := range writable {
		profile += fmt.Sprintf(`(allow file-write* (subpath %q))`, realOf(w))
	}
	return profile
}

// Only what Agentium saw descend from the agent is ever a kill target (descendantTargets; nothing is stopped here, and
// every process is the test's own). Targets: the agent's child, and the child it left when it ended (reparented: the
// detached case), both seen while the agent ran. Never targets, only reported: a sandbox that grants by pattern
// (workspaces/[^/]+/[^/]+) with its cwd in the checkout, which passes both the old sandbox and path rules; a user's
// unsandboxed process in the checkout. Not a target either: a recorded identity whose ID a different process holds now.
func TestCodexSweepKillsOnlyDescendants(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	run := fakeRun(t, base, "a")
	root := filepath.Join(base, "workspaces")
	checkout := filepath.Join(run.workspace, "repo")
	pattern := started(t, fmt.Sprintf(`(version 1)(allow default)(deny file-write* (subpath %q))(allow file-write* (regex #"^%s/[^/]+/[^/]+(/.*)?$"))`,
		root, regexp.QuoteMeta(root)), checkout, "/bin/sleep", "60")
	user := started(t, "", checkout, "/bin/sleep", "60")
	// The "agent": a shell with a child that will outlive it, in its own session, in /.
	agent := startedCmd(t, grants(root, checkout, run.marker), "/", "/bin/sh", "-c", "/bin/sleep 61 & exec /bin/sleep 60")
	agentPID := agent.Process.Pid
	d := loadDescendants(filepath.Join(t.TempDir(), AgentProcesses))
	if id, ok := identityNow(agentPID); ok {
		d.tracked[id.key()] = id
	}
	must(t, d.snapshot(true)) // while the agent runs: its child is seen
	var child int
	for _, id := range d.list() {
		if id.PID != agentPID {
			child = id.PID
		}
	}
	if child == 0 {
		t.Fatalf("the agent's child was not seen: %v", d.list())
	}
	killOwn(t, child)
	agent.Process.Kill() // the agent ends (and is reaped, as the runner reaps it); its child is reparented
	agent.Wait()
	targets, err := descendantTargets(d, run.since)
	must(t, err)
	pids := []int{}
	for _, id := range targets {
		pids = append(pids, id.PID)
	}
	if !slices.Equal(pids, []int{child}) {
		t.Errorf("targets %v, want only the agent's detached child %d (pattern %d, user %d)", pids, child, pattern, user)
	}
	reports, err := codexReportPIDs(run)
	must(t, err)
	if !slices.Contains(reports, pattern) || !slices.Contains(reports, user) {
		t.Errorf("reported %v: want the pattern sandbox %d and the user's process %d", reports, pattern, user)
	}
	// A recorded identity whose ID is held by another process (here: the user's, recorded with another start time).
	mixed := loadDescendants(filepath.Join(t.TempDir(), AgentProcesses))
	if id, ok := identityNow(user); ok {
		id.Usec++
		mixed.tracked[id.key()] = id
	}
	if targets, err := descendantTargets(mixed, run.since); err != nil || len(targets) != 0 {
		t.Errorf("a recorded identity matched by ID alone: %v, %v", targets, err)
	}
}

// Cleanup never stops a process: a reported one (in the run's sandbox, or using its folders) is listed as kept, with
// how to stop it if it is the user's to stop, --yes or not.
func TestCleanNeverStopsAProcess(t *testing.T) {
	layout := cleanLayout(t)
	must(t, os.MkdirAll(layout.Records, 0o700))
	run := fakeRun(t, t.TempDir(), "r1")
	user := started(t, "", filepath.Join(run.workspace, "repo"), "/bin/sleep", "60")
	id, ok := identityNow(user)
	if !ok {
		t.Fatal("the test's process is gone")
	}
	records := filepath.Join(layout.Records, "r1")
	must(t, os.MkdirAll(records, 0o700))
	must(t, recordLeftovers(records, []leftProcess{{PID: user, Command: id.Command, StartSec: id.Sec, StartUsec: id.Usec, Why: "using the run's folders"}}))
	plan, err := PlanClean(t.Context(), CleanInput{Layout: layout, Now: time.Now()})
	must(t, err)
	if slices.ContainsFunc(plan.Remove, func(it CleanItem) bool { return it.Kind == CleanProcesses }) {
		t.Fatalf("cleanup would stop a process: %+v", plan.Remove)
	}
	i := slices.IndexFunc(plan.Keep, func(it CleanItem) bool { return it.Kind == CleanProcesses && it.PID() == user })
	if i < 0 || plan.Keep[i].Reason != CleanLeftByRun || !strings.Contains(plan.Keep[i].Detail, fmt.Sprintf("kill %d", user)) {
		t.Fatalf("the reported process is not listed as kept: %+v", plan.Keep)
	}
	if errs := RemoveClean(t.Context(), layout, []CleanItem{plan.Keep[i]}); errs[0] == nil {
		t.Error("cleanup agreed to remove a process")
	}
	if now, ok := identityNow(user); !ok || now.key() != id.key() {
		t.Error("the process was stopped")
	}
}

// The agent's own process is read and saved when it starts (descendants.observe, from agent.Run's Started), before
// the runner reaps it: even an agent that has already exited, a zombie, is the root, with its real identity.
func TestObserveReadsAnExitedAgent(t *testing.T) {
	c := exec.Command("/usr/bin/true")
	must(t, c.Start())
	t.Cleanup(func() { c.Wait() }) // the test's own child: reaped only here
	zombie := false
	for i := 0; i < 200 && !zombie; i++ {
		stat, _ := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(c.Process.Pid)).Output()
		zombie = strings.Contains(string(stat), "Z")
		time.Sleep(10 * time.Millisecond)
	}
	if !zombie {
		t.Fatal("the child did not become a zombie")
	}
	file := filepath.Join(t.TempDir(), AgentProcesses)
	d := loadDescendants(file)
	d.observe(c.Process.Pid)
	list := loadDescendants(file).list()
	if len(list) != 1 || list[0].PID != c.Process.Pid || time.Since(list[0].started()) > time.Minute || d.note() != "" {
		t.Fatalf("saved %+v (note %q): want the exited agent as the root", list, d.note())
	}
	if table, err := processTable(); err != nil || table[c.Process.Pid].id.key() != list[0].key() {
		t.Errorf("the table's read of the agent %+v differs from its own (%v)", table[c.Process.Pid], err)
	}
}
