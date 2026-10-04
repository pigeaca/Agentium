//go:build darwin && cgo

package run

/*
#include <stdlib.h>
#include <sys/types.h>
extern const int SANDBOX_CHECK_NO_REPORT;
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);
static int ag_codex_sandboxed(int pid) { return sandbox_check(pid, NULL, 0 | SANDBOX_CHECK_NO_REPORT); }
static int ag_codex_denies(int pid, const char *operation, const char *path) {
	return sandbox_check(pid, operation, 1 | SANDBOX_CHECK_NO_REPORT, path);
}
*/
import "C"

import (
	"os"
	"path/filepath"
	"time"
	"unsafe"
)

// startedSince reports whether p started at since or later (its start time in seconds and microseconds).
func (p process) startedSince(since time.Time) bool {
	return !time.Unix(int64(p.sec), int64(p.usec)*1000).Before(since)
}

// codexLeftovers lists, without stopping any, the processes a Codex run's commands left (sweepCodex), each one shown to
// be the run's own, and started no earlier than its agent (s.since):
//   - in the run's own sandbox: sandboxed, allowed to write the run's marker folder (a random name in its workspace,
//     which only its profile lists: no grant for workspaces/*/repo, another run's or the system's, can cover it) and
//     its checkout, and denied its workspace folder and the workspaces folder above it. A sandbox that grants several
//     runs' checkouts while denying their workspaces fails the marker; the system's agents, which may write /tmp or the
//     user's folders, fail a denial or the start time;
//   - or using a path under the run's workspace or temp root (processesUnder).
//
// Without a marker (a run recovered without one) only the second applies.
func codexLeftovers(s codexSweep) ([]process, error) {
	under, err := processesUnder([]string{s.workspace, s.tempRoot})
	if err != nil {
		return nil, err
	}
	var found []process
	seen := map[int]bool{}
	for _, p := range under {
		if p.startedSince(s.since) && !seen[p.pid] {
			seen[p.pid] = true
			found = append(found, p)
		}
	}
	sandboxed, err := sandboxedOwning(s)
	if err != nil {
		return found, err
	}
	for _, p := range sandboxed {
		if !seen[p.pid] {
			seen[p.pid] = true
			found = append(found, p)
		}
	}
	return found, nil
}

// sandboxedOwning is codexLeftovers' first kind: the processes in the run's own sandbox.
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

// codexLeftoverPIDs are codexLeftovers' process IDs.
func codexLeftoverPIDs(s codexSweep) ([]int, error) {
	found, err := codexLeftovers(s)
	pids := make([]int, len(found))
	for i, p := range found {
		pids[i] = p.pid
	}
	return pids, err
}

// stopCodexLeftovers kills what codexLeftovers finds, and looks again until none is left (stopFound).
func stopCodexLeftovers(s codexSweep) ([]string, error) {
	return stopFound(func() ([]process, error) { return codexLeftovers(s) }, "left by the Codex run")
}
