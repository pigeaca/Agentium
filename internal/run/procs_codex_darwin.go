//go:build darwin && cgo

package run

/*
#include <libproc.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <sys/proc_info.h>
#include <sys/sysctl.h>
#include <sys/types.h>
extern const int SANDBOX_CHECK_NO_REPORT;
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);
static int ag_codex_sandboxed(int pid) { return sandbox_check(pid, NULL, 0 | SANDBOX_CHECK_NO_REPORT); }
static int ag_codex_denies(int pid, const char *operation, const char *path) {
	return sandbox_check(pid, operation, 1 | SANDBOX_CHECK_NO_REPORT, path);
}
// ag_codex_proc is one process in a read of the process table (ag_codex_table, ag_codex_one).
typedef struct { int pid, ppid, zombie; uint64_t sec, usec; char comm[MAXCOMLEN + 1]; } ag_codex_proc;
static void ag_codex_fill(const struct kinfo_proc *p, ag_codex_proc *out) {
	out->pid = p->kp_proc.p_pid;
	out->ppid = p->kp_eproc.e_ppid;
	out->zombie = p->kp_proc.p_stat == SZOMB;
	out->sec = (uint64_t)p->kp_proc.p_starttime.tv_sec;
	out->usec = (uint64_t)p->kp_proc.p_starttime.tv_usec;
	memcpy(out->comm, p->kp_proc.p_comm, MAXCOMLEN); out->comm[MAXCOMLEN] = 0;
}
// ag_codex_table reads the whole process table in one sysctl (kern.proc.all, zombies included) and keeps uid's
// processes in *out (malloc'd; the caller frees it). It returns their count, or -1.
static int ag_codex_table(unsigned int uid, ag_codex_proc **out) {
	int mib[3] = {CTL_KERN, KERN_PROC, KERN_PROC_ALL};
	struct kinfo_proc *procs = NULL;
	size_t size = 0;
	for (int tries = 0; tries < 8 && procs == NULL; tries++) {
		if (sysctl(mib, 3, NULL, &size, NULL, 0) != 0) return -1;
		size += size / 4 + 64 * sizeof(struct kinfo_proc); // room for the processes started meanwhile
		if ((procs = malloc(size)) == NULL) return -1;
		if (sysctl(mib, 3, procs, &size, NULL, 0) != 0) {
			free(procs);
			procs = NULL;
			if (errno != ENOMEM) return -1;
		}
	}
	if (procs == NULL) return -1;
	int n = (int)(size / sizeof(struct kinfo_proc)), k = 0;
	ag_codex_proc *res = malloc((n > 0 ? n : 1) * sizeof(ag_codex_proc));
	if (res == NULL) { free(procs); return -1; }
	for (int i = 0; i < n; i++) {
		if (procs[i].kp_eproc.e_ucred.cr_uid == uid) ag_codex_fill(&procs[i], &res[k++]);
	}
	free(procs);
	*out = res;
	return k;
}
// ag_codex_one reads one process (kern.proc.pid, a zombie too) into *out when it belongs to uid; it returns 0, or -1
// for another user's process or one that is gone.
static int ag_codex_one(int pid, unsigned int uid, ag_codex_proc *out) {
	int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, pid};
	struct kinfo_proc p;
	size_t size = sizeof(p);
	if (sysctl(mib, 4, &p, &size, NULL, 0) != 0 || size != sizeof(p) || p.kp_proc.p_pid != pid || p.kp_eproc.e_ucred.cr_uid != uid) return -1;
	ag_codex_fill(&p, out);
	return 0;
}
*/
import "C"

import (
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

// tracksDescendants: whether descendants can read this system's processes.
const tracksDescendants = true

// procIdentity is a process read by ag_codex_table or ag_codex_one.
func procIdentity(p *C.ag_codex_proc) tableEntry {
	comm := C.GoStringN(&p.comm[0], C.MAXCOMLEN)
	name, _, _ := strings.Cut(comm, "\x00")
	return tableEntry{id: identity{PID: int(p.pid), Sec: uint64(p.sec), Usec: uint64(p.usec), Command: name}, ppid: int(p.ppid),
		zombie: p.zombie != 0}
}

// processTable is one read of this user's processes (not Agentium's own), by process ID: one sysctl, which lists the
// IDs at once but reads each process after (descendants.snapshot proves descent despite that).
func processTable() (map[int]tableEntry, error) {
	var procs *C.ag_codex_proc
	n := C.ag_codex_table(C.uint(os.Getuid()), &procs)
	if n < 0 {
		return nil, errors.New("the process table could not be read")
	}
	defer C.free(unsafe.Pointer(procs))
	self := os.Getpid()
	table := make(map[int]tableEntry, int(n))
	for _, p := range unsafe.Slice(procs, int(n)) {
		e := procIdentity(&p)
		if e.id.PID > 1 && e.id.PID != self {
			table[e.id.PID] = e
		}
	}
	return table, nil
}

// identityNow is the process with ID pid as it is now, a zombie too (ok false: gone, or another user's).
func identityNow(pid int) (identity, bool) {
	e, ok := readOne(pid)
	return e.id, ok
}

// runningNow is identityNow without zombies: a process that can still be stopped.
func runningNow(pid int) (identity, bool) {
	e, ok := readOne(pid)
	return e.id, ok && !e.zombie
}

func readOne(pid int) (tableEntry, bool) {
	var p C.ag_codex_proc
	if pid <= 0 || C.ag_codex_one(C.int(pid), C.uint(os.Getuid()), &p) != 0 {
		return tableEntry{}, false
	}
	return procIdentity(&p), true
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
			if now, ok := runningNow(id.PID); ok && now.key() == id.key() && syscall.Kill(id.PID, syscall.SIGKILL) == nil {
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
	if err := d.snapshot(true); err != nil {
		return nil, err
	}
	var targets []identity
	for _, id := range d.list() {
		if now, ok := runningNow(id.PID); ok && now.key() == id.key() && !id.started().Before(since) {
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
