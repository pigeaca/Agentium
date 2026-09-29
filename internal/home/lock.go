package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrBusy is returned when another process holds the run lock.
var ErrBusy = errors.New("another Agentium process is running agents with this data folder")

// LockRuns takes the data folder's run lock, held while a command starts agents: one process at a time, so runs can
// predict everything that may overlap them, and a run found without a stored record belongs to a process that died.
// The operating system releases the lock when the process ends, however it ends; agents never inherit it.
func (l Layout) LockRuns() (release func(), err error) {
	path := filepath.Join(l.Root, "runs.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // close-on-exec, as Go opens every file
	if err != nil {
		return nil, fmt.Errorf("run lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
