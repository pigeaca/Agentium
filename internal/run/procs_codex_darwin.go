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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

// startedSince reports whether p started at since or later (its start time in seconds and microseconds).
func (p process) startedSince(since time.Time) bool {
	return !time.Unix(int64(p.sec), int64(p.usec)*1000).Before(since)
}

// codexLeftovers lists, without stopping any, the processes a Codex run's commands may have left (sweepCodex), each
// started no earlier than its agent (s.since):
//   - kill: those shown to be the run's on both counts. In the run's own sandbox: sandboxed, allowed to write the run's
//     marker folder (a random name in its workspace, which only its profile lists) and its checkout, and denied its
//     workspace folder and the workspaces folder above it. And using a path under the run's workspace or temp root.
//   - report: those that pass only one. A sandbox can grant paths by pattern without knowing the marker (a regex over
//     workspaces/*/*), so the sandbox alone proves nothing; a path alone can be the user's editor, or Spotlight's
//     mdworker, opened in the workspace. A detached child of the run (cd /, every descriptor closed) passes only the
//     first: reported, never killed; its sandbox can write only the run's own folders, which go with the run.
//
// Without a marker (a run recovered without one) nothing is in the run's sandbox: only reports.
func codexLeftovers(s codexSweep) (kill, report []process, err error) {
	under, err := processesUnder([]string{s.workspace, s.tempRoot})
	if err != nil {
		return nil, nil, err
	}
	usesFolders := map[int]process{}
	for _, p := range under {
		if p.startedSince(s.since) {
			usesFolders[p.pid] = p
		}
	}
	sandboxed, err := sandboxedOwning(s)
	if err != nil {
		return nil, nil, err
	}
	inSandbox := map[int]bool{}
	for _, p := range sandboxed {
		inSandbox[p.pid] = true
		if _, both := usesFolders[p.pid]; both {
			kill = append(kill, p)
		} else {
			report = append(report, p.reported("in the run's sandbox, but holding none of its folders"))
		}
	}
	for _, p := range under {
		if _, ok := usesFolders[p.pid]; ok && !inSandbox[p.pid] {
			report = append(report, p.reported("using the run's folders, but not in its sandbox"))
			delete(usesFolders, p.pid) // once
		}
	}
	return kill, report, nil
}

// reported is p with why it was reported, not stopped.
func (p process) reported(why string) process {
	p.why = why
	return p
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

// codexLeftoverPIDs are codexLeftovers' process IDs: those it would stop, and those it would report.
func codexLeftoverPIDs(s codexSweep) (kill, report []int, err error) {
	k, r, err := codexLeftovers(s)
	for _, p := range k {
		kill = append(kill, p.pid)
	}
	for _, p := range r {
		report = append(report, p.pid)
	}
	return kill, report, err
}

// errGuardRefused is a sweep its guard refused (tests): nothing was stopped.
var errGuardRefused = errors.New("the sweep's guard refused its targets")

// stopCodexLeftovers kills what codexLeftovers would stop, looking again until none is left (stopFound), and returns
// what it would only report. guard, when set (tests), sees the targets of every round before any is stopped, and may
// refuse them all.
func stopCodexLeftovers(s codexSweep, guard func(pids []int) bool) (killed []string, reported []leftProcess, err error) {
	var report []process
	killed, err = stopFound(func() ([]process, error) {
		kill, rep, err := codexLeftovers(s)
		report = rep
		if err == nil && guard != nil && len(kill) > 0 {
			pids := make([]int, len(kill))
			for i, p := range kill {
				pids[i] = p.pid
			}
			if !guard(pids) {
				return nil, errGuardRefused
			}
		}
		return kill, err
	}, "left by the Codex run")
	for _, p := range report {
		reported = append(reported, leftProcess{PID: p.pid, Command: p.command, StartSec: p.sec, StartUsec: p.usec, Why: p.why})
	}
	return killed, reported, err
}

// leftAlive reports whether the process left is still the one recorded (its ID and start time), not a later one that
// took its ID.
func leftAlive(p leftProcess) bool {
	now, ok := startOf(p.PID)
	return ok && now.sec == p.StartSec && now.usec == p.StartUsec
}

// stopLeft kills a process left by a run, once it is checked to be still the one recorded (leftAlive).
func stopLeft(p leftProcess) error {
	if !leftAlive(p) {
		return nil // gone, or another process now
	}
	if err := syscall.Kill(p.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop process %d: %w", p.PID, err)
	}
	return nil
}
