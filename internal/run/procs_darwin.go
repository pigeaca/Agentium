//go:build cgo

package run

/*
#include <errno.h>
#include <libproc.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/types.h>

// sandbox_check is libsystem_sandbox's (part of libSystem), declared here as WebKit and Chromium declare it: it asks
// the kernel whether a process's sandbox allows an operation, without the process. SANDBOX_CHECK_NO_REPORT keeps the
// check out of the log (a denied check would otherwise be logged under the other sandbox's tag). A path filter needs
// the path to exist.
extern const int SANDBOX_CHECK_NO_REPORT;
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);

// ag_sandboxed returns 1 when the process is sandboxed, 0 when it is not, -1 when that cannot be told.
static int ag_sandboxed(int pid) {
	return sandbox_check(pid, NULL, 0 | SANDBOX_CHECK_NO_REPORT);
}

// ag_denies returns 0 when the process's sandbox allows operation on path (which must exist), 1 when it denies it.
static int ag_denies(int pid, const char *operation, const char *path) {
	return sandbox_check(pid, operation, 1 | SANDBOX_CHECK_NO_REPORT, path);
}

// ag_listpids fills buf (n entries) with every process ID, and returns how many, or -errno.
static int ag_listpids(int *buf, int n) {
	int got = proc_listallpids(buf, n * (int)sizeof(int));
	return got < 0 ? -errno : got;
}

// ag_start gives a process's start time (seconds and microseconds) and command name (comm, MAXCOMLEN+1 bytes) when it
// belongs to uid; it returns 0, or -1 for another user's process, one that is gone or cannot be inspected.
static int ag_start(int pid, unsigned int uid, uint64_t *sec, uint64_t *usec, char *comm) {
	struct proc_bsdinfo bsd;
	if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &bsd, sizeof(bsd)) != sizeof(bsd) || bsd.pbi_uid != uid) return -1;
	*sec = bsd.pbi_start_tvsec;
	*usec = bsd.pbi_start_tvusec;
	if (comm != NULL) { memcpy(comm, bsd.pbi_comm, MAXCOMLEN); comm[MAXCOMLEN] = 0; }
	return 0;
}

// ag_paths returns the paths a process uses, NUL-separated in a buffer the caller frees (its length in *len): its
// working and root folders, its executable, and every file or folder it holds open. It returns NULL for a process of
// another user (or one that cannot be inspected, or is gone). The kernel gives real paths (/private/var/...).
static char *ag_paths(int pid, unsigned int uid, int *len) {
	struct proc_bsdinfo bsd;
	*len = 0;
	if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &bsd, sizeof(bsd)) != sizeof(bsd) || bsd.pbi_uid != uid) return NULL;
	int fdbytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
	if (fdbytes < 0) fdbytes = 0;
	struct proc_fdinfo *fds = fdbytes > 0 ? malloc(fdbytes) : NULL;
	if (fds != NULL) fdbytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, fdbytes);
	if (fdbytes < 0) fdbytes = 0;
	int nfds = fdbytes / (int)sizeof(struct proc_fdinfo);
	size_t cap = (size_t)(nfds + 3) * (MAXPATHLEN + 1);
	char *out = malloc(cap);
	if (out == NULL) { free(fds); return NULL; }
	size_t used = 0;
#define AG_ADD(p) do { size_t l = strnlen((p), MAXPATHLEN); if (l > 0 && used + l + 1 <= cap) { memcpy(out + used, (p), l); used += l; out[used++] = 0; } } while (0)
	struct proc_vnodepathinfo vp;
	if (proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &vp, sizeof(vp)) == sizeof(vp)) {
		AG_ADD(vp.pvi_cdir.vip_path);
		AG_ADD(vp.pvi_rdir.vip_path);
	}
	char exe[PROC_PIDPATHINFO_MAXSIZE];
	if (proc_pidpath(pid, exe, sizeof(exe)) > 0) AG_ADD(exe);
	for (int i = 0; i < nfds; i++) {
		if (fds[i].proc_fdtype != PROX_FDTYPE_VNODE) continue;
		struct vnode_fdinfowithpath vi;
		if (proc_pidfdinfo(pid, fds[i].proc_fd, PROC_PIDFDVNODEPATHINFO, &vi, sizeof(vi)) == sizeof(vi)) AG_ADD(vi.pvip.vip_path);
	}
#undef AG_ADD
	free(fds);
	*len = (int)used;
	return out;
}
*/
import "C"

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// process is one process of the user's: its ID, start time (which tells it from a later process with the same ID) and
// command name.
type process struct {
	pid       int
	sec, usec uint64
	command   string
}

// startOf reads a process's start time and command name; ok is false for another user's process or one that is gone.
func startOf(pid int) (process, bool) {
	var sec, usec C.uint64_t
	comm := make([]byte, C.MAXCOMLEN+1)
	if C.ag_start(C.int(pid), C.uint(os.Getuid()), &sec, &usec, (*C.char)(unsafe.Pointer(&comm[0]))) != 0 {
		return process{}, false
	}
	name, _, _ := strings.Cut(string(comm), "\x00")
	return process{pid: pid, sec: uint64(sec), usec: uint64(usec), command: name}, true
}

// sweepRoots are the forms of folders the kernel's paths are compared with: each as given, and with its parent folder
// resolved (/var to /private/var) and its own name kept. The folder itself is never resolved: a grade may have swapped
// it for a link, which must not widen the sweep to wherever the link points.
func sweepRoots(folders []string) []string {
	var roots []string
	for _, f := range folders {
		if f == "" {
			continue
		}
		roots = append(roots, filepath.Clean(f))
		if parent, err := filepath.EvalSymlinks(filepath.Dir(f)); err == nil {
			roots = append(roots, filepath.Join(parent, filepath.Base(f)))
		}
	}
	return roots
}

// processesUnder lists this user's processes (not Agentium's own) that use a path under one of folders: their working
// or root folder, their executable, or a file or folder they hold open (libproc, as lsof reads it, without starting a
// tool). folders are compared in the forms sweepRoots gives, as the kernel reports real paths.
//
// What it cannot see: a process that uses nothing under the folders at the moment it looks (one that changed its
// working folder away and closed every file there, a sleeping `setsid` child, say). Such a process can still act on
// the folders by path later; removal never follows a link it plants (removeTreeAt), and once the grade's folder is
// gone the grading sandbox lets it create nothing in its place (the folder's parent is not writable to it).
func processesUnder(folders []string) ([]process, error) {
	roots := sweepRoots(folders)
	if len(roots) == 0 {
		return nil, nil
	}
	buf := make([]C.int, 1<<16)
	n := C.ag_listpids(&buf[0], C.int(len(buf)))
	if n < 0 {
		return nil, fmt.Errorf("list processes: %w", syscall.Errno(-n))
	}
	self, uid := os.Getpid(), C.uint(os.Getuid())
	var found []process
	for _, pid := range buf[:min(int(n), len(buf))] {
		if int(pid) <= 1 || int(pid) == self {
			continue
		}
		before, ok := startOf(int(pid))
		if !ok {
			continue
		}
		var length C.int
		paths := C.ag_paths(pid, uid, &length)
		if paths == nil {
			continue
		}
		used := C.GoStringN(paths, length)
		C.free(unsafe.Pointer(paths))
		for _, p := range strings.Split(used, "\x00") {
			if p != "" && underAny(p, roots) {
				found = append(found, before)
				break
			}
		}
	}
	return found, nil
}

func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if within(p, r) {
			return true
		}
	}
	return false
}

// sandboxedIn lists this user's processes (not Agentium's own) that run in a grade's own sandbox: sandboxed, with a
// sandbox that may write own (the grade's temp root, which must exist) and may not write outside (the folder above the
// grade's). Only that grade's profile allows both: another grade's or an agent's sandbox cannot write this grade's
// folders, and a sandbox that could write both (one allowing everything) is not a grade's. Unlike processesUnder, this
// finds a process however little it holds (a `setsid` child that moved away and closed everything), as long as own
// is still the folder the profile names: a grade cannot rename or replace it while its folder is locked
// (grading.lock).
func sandboxedIn(own, outside string) ([]process, error) {
	if own == "" || outside == "" {
		return nil, nil
	}
	ownReal, err := filepath.EvalSymlinks(own)
	if err != nil {
		return nil, nil // gone: nothing to tell processes by
	}
	if info, err := os.Lstat(own); err != nil || !info.IsDir() {
		return nil, nil
	}
	outsideReal, err := filepath.EvalSymlinks(outside)
	if err != nil {
		return nil, nil
	}
	buf := make([]C.int, 1<<16)
	n := C.ag_listpids(&buf[0], C.int(len(buf)))
	if n < 0 {
		return nil, fmt.Errorf("list processes: %w", syscall.Errno(-n))
	}
	write := C.CString("file-write-data")
	defer C.free(unsafe.Pointer(write))
	ownC, outsideC := C.CString(ownReal), C.CString(outsideReal)
	defer C.free(unsafe.Pointer(ownC))
	defer C.free(unsafe.Pointer(outsideC))
	self := os.Getpid()
	var found []process
	for _, pid := range buf[:min(int(n), len(buf))] {
		if int(pid) <= 1 || int(pid) == self {
			continue
		}
		p, ok := startOf(int(pid))
		if !ok {
			continue
		}
		if C.ag_sandboxed(pid) == 1 && C.ag_denies(pid, write, ownC) == 0 && C.ag_denies(pid, write, outsideC) == 1 {
			found = append(found, p)
		}
	}
	return found, nil
}

// stopSandboxed kills (SIGKILL) the processes of a grade's own sandbox (sandboxedIn), and looks again until none is
// left, as stopProcessesUnder does, and returns what it killed.
func stopSandboxed(own, outside string) ([]string, error) {
	return stopFound(func() ([]process, error) { return sandboxedIn(own, outside) }, "run in the grade's sandbox")
}

// stopProcessesUnder kills (SIGKILL) this user's processes that use the folders (processesUnder), and looks again
// until none is left, a few rounds at most: what a grade left running (a daemon in its own session, a background
// server) must not outlive it, write its folders while they are removed, or hold their files. Before each kill it
// checks that the process ID still belongs to the process it found (the same start time), so a process that ended
// meanwhile never costs an unrelated one that took its ID. It returns what it killed ("<pid> <command>"), and an error
// when processes were still there after the last round or could not be listed.
func stopProcessesUnder(folders []string) ([]string, error) {
	return stopFound(func() ([]process, error) { return processesUnder(folders) }, "use "+strings.Join(folders, ", "))
}

// stopFound kills what find finds, and looks again until nothing is left (5 rounds at most). Before each kill it checks
// that the process ID still belongs to the process found (the same start time). what says what the processes do, for
// the error when some are left.
func stopFound(find func() ([]process, error), what string) ([]string, error) {
	var killed []string
	for round := 0; round < 5; round++ {
		found, err := find()
		if err != nil {
			return killed, err
		}
		if len(found) == 0 {
			return killed, nil
		}
		for _, p := range found {
			if now, ok := startOf(p.pid); !ok || now.sec != p.sec || now.usec != p.usec {
				continue // gone, or another process now
			}
			if syscall.Kill(p.pid, syscall.SIGKILL) == nil {
				killed = append(killed, fmt.Sprintf("%d %q", p.pid, p.command)) // the grade picks the name: quoted
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if found, err := find(); err != nil || len(found) > 0 {
		return killed, fmt.Errorf("%d process(es) still %s after %d were stopped (%v)", len(found), what, len(killed), err)
	}
	return killed, nil
}
