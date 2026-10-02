package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrBusy is returned when another process holds the run lock.
var ErrBusy = errors.New("another Agentium process is running agents with this data folder")

// RunsBusy reports whether a process holds the run lock now (its runs are in progress). It probes with a shared lock
// on a file of its own, which never changes the lock file; LockRuns waits out such a probe. It creates nothing: a
// missing lock file means no run ever held it, so none is in progress.
func (l Layout) RunsBusy() bool {
	f, err := os.OpenFile(filepath.Join(l.Root, "runs.lock"), os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// LockRuns takes the data folder's run lock, held while a command starts agents: one process at a time, so runs can
// predict everything that may overlap them, and a run found without a stored record belongs to a process that died.
// The operating system releases the lock when the process ends, however it ends; agents never inherit it.
func (l Layout) LockRuns() (release func(), err error) {
	path := filepath.Join(l.Root, "runs.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // close-on-exec, as Go opens every file
	if err != nil {
		return nil, fmt.Errorf("run lock: %w", err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	for wait := 0; errors.Is(err, syscall.EWOULDBLOCK) && wait < 40; wait++ { // a RunsBusy probe lasts an instant
		time.Sleep(50 * time.Millisecond)
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		holder, _ := os.ReadFile(path)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid := strings.TrimSpace(string(holder)); pid != "" {
				return nil, fmt.Errorf("%w (pid %s)", ErrBusy, pid)
			}
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("run lock: %w", err)
	}
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return func() {
		f.Truncate(0)
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
