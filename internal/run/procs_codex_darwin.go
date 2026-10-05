//go:build darwin && cgo

package run

/*
#include <libproc.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/types.h>
extern const int SANDBOX_CHECK_NO_REPORT;
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);
static int ag_codex_sandboxed(int pid) { return sandbox_check(pid, NULL, 0 | SANDBOX_CHECK_NO_REPORT); }
static int ag_codex_denies(int pid, const char *operation, const char *path) {
	return sandbox_check(pid, operation, 1 | SANDBOX_CHECK_NO_REPORT, path);
}
// ag_codex_bsd gives a process's parent, start time and command name (MAXCOMLEN+1 bytes) when it belongs to uid;
// it returns 0, or -1 for another user's process, one that is gone or cannot be inspected.
static int ag_codex_bsd(int pid, unsigned int uid, int *ppid, uint64_t *sec, uint64_t *usec, char *comm) {
	struct proc_bsdinfo bsd;
	if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &bsd, sizeof(bsd)) != sizeof(bsd) || bsd.pbi_uid != uid) return -1;
	*ppid = (int)bsd.pbi_ppid;
	*sec = bsd.pbi_start_tvsec;
	*usec = bsd.pbi_start_tvusec;
	memcpy(comm, bsd.pbi_comm, MAXCOMLEN); comm[MAXCOMLEN] = 0;
	return 0;
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// startedSince reports whether p started at since or later (its start time in seconds and microseconds).
func (p process) startedSince(since time.Time) bool {
	return !time.Unix(int64(p.sec), int64(p.usec)*1000).Before(since)
}

// tableEntry is one process of the user's in a snapshot of the process table: its identity and its parent's ID.
type tableEntry struct {
	id   identity
	ppid int
}

// processTable is one snapshot of this user's processes (not Agentium's own), by process ID.
func processTable() (map[int]tableEntry, error) {
	pids, err := allPIDs()
	if err != nil {
		return nil, err
	}
	self, uid := os.Getpid(), C.uint(os.Getuid())
	table := make(map[int]tableEntry, len(pids))
	comm := make([]byte, C.MAXCOMLEN+1)
	for _, pid := range pids {
		if pid <= 1 || pid == self {
			continue
		}
		var ppid C.int
		var sec, usec C.uint64_t
		if C.ag_codex_bsd(C.int(pid), uid, &ppid, &sec, &usec, (*C.char)(unsafe.Pointer(&comm[0]))) != 0 {
			continue
		}
		name, _, _ := strings.Cut(string(comm), "\x00")
		table[pid] = tableEntry{id: identity{PID: pid, Sec: uint64(sec), Usec: uint64(usec), Command: name}, ppid: int(ppid)}
	}
	return table, nil
}

// identityNow is the process with ID pid as it is now (ok false: gone, or another user's).
func identityNow(pid int) (identity, bool) {
	p, ok := startOf(pid)
	if !ok {
		return identity{}, false
	}
	return identity{PID: pid, Sec: p.sec, Usec: p.usec, Command: p.command}, true
}

// snapshot looks at the process table once and adds to d every process whose parent chain leads to one it tracks
// (the agent, or a descendant seen before), each link a parent that started no later than its child (a parent whose
// ID was taken by a later process breaks the chain). Persisted when it grows.
func (d *descendants) snapshot() error {
	table, err := processTable()
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.tracked) == 0 {
		return nil
	}
	added := false
	for _, e := range table {
		if _, ok := d.tracked[e.id.key()]; ok {
			continue
		}
		chain := []identity{e.id}
		for cur, steps := e, 0; steps < 64; steps++ {
			parent, ok := table[cur.ppid]
			if !ok || parent.id.started().After(cur.id.started()) {
				break
			}
			if _, tracked := d.tracked[parent.id.key()]; tracked {
				for _, id := range chain {
					d.tracked[id.key()] = id
				}
				added = true
				break
			}
			chain = append(chain, parent.id)
			cur = parent
		}
	}
	if added {
		return d.persist()
	}
	return nil
}

// observe tracks the agent started with process ID pid (its identity becomes the root) and looks every poll until ctx
// ends (agent.Invocation.Observe).
func (d *descendants) observe(ctx context.Context, pid int) {
	if id, ok := identityNow(pid); ok {
		d.mu.Lock()
		d.tracked[id.key()] = id
		d.mu.Unlock()
	}
	ticker := time.NewTicker(descendantsPoll)
	defer ticker.Stop()
	for {
		_ = d.snapshot()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// stopDescendants kills every process d tracks that is still alive with exactly the identity recorded and started at
// since or later, looking again (snapshot: the tracked set's own new children) until none is left, 5 rounds at most.
// The identity is read again right before each kill. guard, when set (tests), sees every round's targets first and
// may refuse them all.
func stopDescendants(d *descendants, since time.Time, guard func(pids []int) bool) (killed []string, err error) {
	for round := 0; round < 5; round++ {
		targets, err := descendantTargets(d, since)
		if err != nil {
			return killed, err
		}
		if len(targets) == 0 {
			return killed, nil
		}
		if guard != nil {
			pids := make([]int, len(targets))
			for i, id := range targets {
				pids[i] = id.PID
			}
			if !guard(pids) {
				return killed, errGuardRefused
			}
		}
		for _, id := range targets {
			if now, ok := identityNow(id.PID); ok && now.key() == id.key() && syscall.Kill(id.PID, syscall.SIGKILL) == nil {
				killed = append(killed, fmt.Sprintf("%d %q", id.PID, id.Command))
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return killed, fmt.Errorf("the agent's descendants were still running after %d were stopped", len(killed))
}

// descendantTargets is what stopDescendants would stop now, without stopping any: after one more snapshot, every
// process d tracks that is alive with exactly the identity recorded (never one that only shares its ID) and started at
// since or later.
func descendantTargets(d *descendants, since time.Time) ([]identity, error) {
	if err := d.snapshot(); err != nil {
		return nil, err
	}
	var targets []identity
	for _, id := range d.list() {
		if now, ok := identityNow(id.PID); ok && now.key() == id.key() && !id.started().Before(since) {
			targets = append(targets, id)
		}
	}
	return targets, nil
}

// codexReports lists, without stopping any, the processes that may be what a Codex run's commands left but that
// Agentium did not see descend from its agent (started after it, s.since): in the run's own sandbox (its marker and
// checkout writable, its workspace and the workspaces folder not), or using a path under its workspace or temp root.
// They are reported, never stopped: a sandbox can grant by pattern without knowing the marker, and a path can be the
// user's editor's. Processes found both ways are joined by their whole identity (ID and start time), never by ID.
func codexReports(s codexSweep) ([]process, error) {
	under, err := processesUnder([]string{s.workspace, s.tempRoot})
	if err != nil {
		return nil, err
	}
	sandboxed, err := sandboxedOwning(s)
	if err != nil {
		return nil, err
	}
	type key [3]uint64
	byID := map[key]process{}
	order := []key{}
	add := func(p process, why string) {
		k := key{uint64(p.pid), p.sec, p.usec}
		if prev, ok := byID[k]; ok {
			prev.why += "; " + why
			byID[k] = prev
			return
		}
		p.why = why
		byID[k] = p
		order = append(order, k)
	}
	for _, p := range sandboxed {
		add(p, "in the run's sandbox")
	}
	for _, p := range under {
		if p.startedSince(s.since) {
			add(p, "using the run's folders")
		}
	}
	var out []process
	for _, k := range order {
		out = append(out, byID[k])
	}
	return out, nil
}

// sandboxedOwning is codexReports' first kind: the processes in the run's own sandbox.
func sandboxedOwning(s codexSweep) ([]process, error) {
	if s.marker == "" {
		return nil, nil
	}
	resolve := func(p string) (string, bool) {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", false
		}
		info, err := os.Lstat(p)
		return real, err == nil && info.IsDir()
	}
	marker, ok1 := resolve(s.marker)
	checkout, ok2 := resolve(filepath.Join(s.workspace, "repo"))
	workspace, ok3 := resolve(s.workspace)
	workspaces, ok4 := resolve(filepath.Dir(s.workspace))
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, nil // gone: nothing to tell its processes by
	}
	write := C.CString("file-write-data")
	defer C.free(unsafe.Pointer(write))
	cpath := func(p string) *C.char { return C.CString(p) }
	allowed, denied := []*C.char{cpath(marker), cpath(checkout)}, []*C.char{cpath(workspace), cpath(workspaces)}
	defer func() {
		for _, c := range append(allowed, denied...) {
			C.free(unsafe.Pointer(c))
		}
	}()
	pids, err := allPIDs()
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var found []process
	for _, pid := range pids {
		if pid <= 1 || pid == self {
			continue
		}
		p, ok := startOf(pid)
		if !ok || !p.startedSince(s.since) || C.ag_codex_sandboxed(C.int(pid)) != 1 {
			continue
		}
		owns := true
		for _, a := range allowed {
			owns = owns && C.ag_codex_denies(C.int(pid), write, a) == 0
		}
		for _, d := range denied {
			owns = owns && C.ag_codex_denies(C.int(pid), write, d) == 1
		}
		if owns {
			found = append(found, p)
		}
	}
	return found, nil
}

// codexReportPIDs are codexReports' process IDs.
func codexReportPIDs(s codexSweep) ([]int, error) {
	found, err := codexReports(s)
	var pids []int
	for _, p := range found {
		pids = append(pids, p.pid)
	}
	return pids, err
}

// errGuardRefused is a kill its guard refused (tests): nothing was stopped.
var errGuardRefused = errors.New("the kill's guard refused its targets")

// reportCodex is codexReports as the records keep them (leftProcess).
func reportCodex(s codexSweep) ([]leftProcess, error) {
	found, err := codexReports(s)
	var out []leftProcess
	for _, p := range found {
		out = append(out, leftProcess{PID: p.pid, Command: p.command, StartSec: p.sec, StartUsec: p.usec, Why: p.why})
	}
	return out, err
}
