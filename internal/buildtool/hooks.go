package buildtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// WarmSteps are the selected profiles' warm-up commands for deps, in table order.
func WarmSteps(selected []Profile, has func(name string) bool, deps string) []WarmStep {
	var steps []WarmStep
	for _, p := range selected {
		if p.Warm != nil {
			steps = append(steps, p.Warm(has, deps)...)
		}
	}
	return steps
}

// NeedsWarming names the selected profiles that fetch dependencies (one stamp each, see run's warm-up).
func NeedsWarming(selected []Profile) []string {
	var names []string
	for _, p := range selected {
		if p.Warm != nil {
			names = append(names, p.Name)
		}
	}
	return names
}

// PrepareRun runs the selected profiles' PrepareRun hooks.
func PrepareRun(ctx context.Context, selected []Profile, deps, buildCache string) error {
	for _, p := range selected {
		if p.PrepareRun != nil {
			if err := p.PrepareRun(ctx, deps, buildCache); err != nil {
				return fmt.Errorf("%s: prepare the run: %w", p.Name, err)
			}
		}
	}
	return nil
}

// StopRun runs the selected profiles' StopRun hooks and returns the first failure after trying all of them.
func StopRun(selected []Profile, buildCache string, host Host) error {
	var errs []error
	for _, p := range selected {
		if p.StopRun != nil {
			if err := p.StopRun(buildCache, host); err != nil {
				errs = append(errs, fmt.Errorf("%s: stop: %w", p.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Host is how a StopRun hook sees the machine's processes; tests replace it.
type Host struct {
	// Command is a process's command line, or "" when it does not exist.
	Command func(pid int) string
	// Signal sends a signal (0 only tests that the process exists).
	Signal func(pid int, sig syscall.Signal) error
	// Grace is how long a process gets to end after SIGTERM before SIGKILL.
	Grace time.Duration
}

// SystemHost is the real machine.
func SystemHost() Host {
	return Host{
		Command: func(pid int) string {
			out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		},
		Signal: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		Grace:  5 * time.Second,
	}
}

// terminate asks a process to end, then kills it after the grace period.
func (h Host) terminate(pid int) {
	if h.Signal(pid, syscall.SIGTERM) != nil {
		return
	}
	deadline := time.Now().Add(h.Grace)
	for time.Now().Before(deadline) {
		if h.Signal(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.Signal(pid, syscall.SIGKILL)
}

// cloneTree copies src to dst (which must not exist), as a copy-on-write clone where the file system can (macOS
// clonefile, Linux reflinks), so a distribution of hundreds of megabytes costs neither time nor space; the copy is
// independent: writing in it never changes src.
func cloneTree(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	var attempts [][]string
	switch runtime.GOOS {
	case "darwin":
		attempts = append(attempts, []string{"-Rc", src, dst})
	case "linux":
		attempts = append(attempts, []string{"-R", "--reflink=auto", src, dst})
	}
	attempts = append(attempts, []string{"-R", src, dst})
	var last error
	for _, args := range attempts {
		os.RemoveAll(dst) // a failed attempt may leave a part
		out, err := exec.CommandContext(ctx, "cp", args...).CombinedOutput()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("cp %s: %w: %s", strings.Join(args[:len(args)-2], " "), err, strings.TrimSpace(string(out)))
		if ctx.Err() != nil {
			break
		}
	}
	return last
}

// userHome returns the folder named by environ's variable name when it is absolute, else def.
func userHome(environ []string, name, def string) string {
	if v := vars(environ)[name]; filepath.IsAbs(v) {
		return v
	}
	return def
}
