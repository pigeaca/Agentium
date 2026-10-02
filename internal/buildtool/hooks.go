package buildtool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// WarmSteps are the selected profiles' warm-up commands for deps, in table order; dir is the throwaway checkout.
func WarmSteps(selected []Profile, dir, deps string) []WarmStep {
	has := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	var steps []WarmStep
	for _, p := range selected {
		if p.Warm != nil {
			steps = append(steps, p.Warm(dir, deps, has)...)
		}
	}
	return steps
}

// WarmVersion is a short hash of the selected profiles' warm-up recipes (their step commands and WarmRecipe), part of
// a warm-up's stamp: a changed recipe gives a new version, so bases warmed by the old one are warmed again. A stamp
// from before versions existed has no version and never matches.
func WarmVersion(selected []Profile) string {
	h := sha256.New()
	for _, p := range selected {
		fmt.Fprintf(h, "%s\n%s\n", p.Name, p.WarmRecipe)
		if p.Warm != nil {
			// Placeholders for the folders; both wrapper variants, since the choice is the repository's.
			for _, wrapper := range []bool{false, true} {
				for _, s := range p.Warm("", "<deps>", func(string) bool { return wrapper }) {
					fmt.Fprintf(h, "%s\n%s\n", s.Command, strings.Join(s.Env, " "))
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// WarmSkippedFile is the file, in the warm-up's throwaway checkout, where a warm-up step lists what it could not warm
// (Gradle: configurations that failed to resolve), one "name<TAB>reason" per line.
const WarmSkippedFile = ".agentium-warm-skipped"

// ReadWarmSkipped reads WarmSkippedFile of the warm-up checkout dir: the names of what was skipped, and whether any
// reason looks like a network failure, which may pass: such a warm-up should be tried again, not stamped as done.
func ReadWarmSkipped(dir string) (names []string, transient bool) {
	data, err := os.ReadFile(filepath.Join(dir, WarmSkippedFile))
	if err != nil {
		return nil, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, reason, _ := strings.Cut(line, "\t")
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		names = append(names, name)
		for _, marker := range []string{"Could not GET", "Could not HEAD", "timed out", "Connection", "UnknownHost"} {
			if strings.Contains(reason, marker) {
				transient = true
			}
		}
	}
	return names, transient
}

// PrepareDeps runs the selected profiles' PrepareDeps hooks.
func PrepareDeps(selected []Profile, deps string) error {
	for _, p := range selected {
		if p.PrepareDeps != nil {
			if err := p.PrepareDeps(deps); err != nil {
				return fmt.Errorf("%s: prepare the deps folder: %w", p.Name, err)
			}
		}
	}
	return nil
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

// PrepareCommands makes the files Agentium's own commands need in the data folder's cache root (Gradle's user home
// with no daemon), for every tool: the cache is shared by every project.
func PrepareCommands(cache string) error {
	for _, p := range Profiles() {
		if p.PrepareCommands != nil {
			if err := p.PrepareCommands(cache); err != nil {
				return fmt.Errorf("%s: %w", p.Name, err)
			}
		}
	}
	return nil
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
func StopRun(ctx context.Context, selected []Profile, buildCache string, host Host) error {
	var errs []error
	for _, p := range selected {
		if p.StopRun != nil {
			if err := p.StopRun(ctx, buildCache, host); err != nil {
				errs = append(errs, fmt.Errorf("%s: stop: %w", p.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Host is how a StopRun hook sees the machine's processes; tests replace it.
type Host struct {
	// Daemons lists the process IDs of this user's Gradle daemons (command lines naming GradleDaemon).
	Daemons func(ctx context.Context) ([]int, error)
	// OpenFiles lists the files a process has open (by real path, with link counts when the machine reports them); an
	// error means it could not be told (no lsof).
	OpenFiles func(ctx context.Context, pid int) ([]OpenFile, error)
	// Signal sends a signal (0 only tests that the process exists).
	Signal func(pid int, sig syscall.Signal) error
	// Grace is how long a process gets to end after SIGTERM before SIGKILL.
	Grace time.Duration
}

// OpenFile is a file a process holds open. Links is its link count, or 0 when unknown.
type OpenFile struct {
	Name  string
	Links int
}

// SystemHost is the real machine: ps for command lines, lsof for open files.
func SystemHost() Host {
	return Host{
		Daemons: func(ctx context.Context) ([]int, error) {
			out, err := exec.CommandContext(ctx, tool("ps", "/bin/ps", "/usr/bin/ps"), "-ww", "-u", strconv.Itoa(os.Getuid()), "-o", "pid=,command=").Output()
			if err != nil {
				return nil, fmt.Errorf("ps: %w", err)
			}
			var pids []int
			for _, line := range strings.Split(string(out), "\n") {
				id, command, _ := strings.Cut(strings.TrimSpace(line), " ")
				if pid, err := strconv.Atoi(id); err == nil && pid > 1 && strings.Contains(command, "GradleDaemon") {
					pids = append(pids, pid)
				}
			}
			return pids, nil
		},
		OpenFiles: func(ctx context.Context, pid int) ([]OpenFile, error) {
			out, err := exec.CommandContext(ctx, tool("lsof", "/usr/sbin/lsof", "/usr/bin/lsof"), "-w", "+L", "-p", strconv.Itoa(pid), "-Fkn").Output()
			if err != nil && len(out) == 0 {
				return nil, fmt.Errorf("lsof -p %d: %w", pid, err)
			}
			var files []OpenFile
			links := 0
			for _, line := range strings.Split(string(out), "\n") {
				switch {
				case strings.HasPrefix(line, "f"): // a new file: its link count is unknown until a k line says
					links = 0
				case strings.HasPrefix(line, "k"):
					links, _ = strconv.Atoi(line[1:])
				case strings.HasPrefix(line, "n"):
					files = append(files, OpenFile{Name: line[1:], Links: links})
				}
			}
			return files, nil
		},
		Signal: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		Grace:  5 * time.Second,
	}
}

// tool is the first of the absolute paths that exists: ps and lsof are never taken from PATH, which the user's
// environment (or a repository's direnv) may have pointed at something else, and Agentium runs them with the user's
// rights. When none exists, the command fails with a name that says which tool is missing.
func tool(name string, paths ...string) string {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	return "/nonexistent/" + name
}

// terminate asks a process to end, then kills it after the grace period (or at once when ctx ends).
func (h Host) terminate(ctx context.Context, pid int, stillTheSame func() bool) {
	if h.Signal(pid, syscall.SIGTERM) != nil {
		return
	}
	kill := func() {
		if stillTheSame() { // the pid may belong to another process by now
			h.Signal(pid, syscall.SIGKILL)
		}
	}
	deadline := time.After(h.Grace)
	for {
		select {
		case <-deadline:
			kill()
			return
		case <-ctx.Done():
			kill()
			return
		case <-time.After(50 * time.Millisecond):
			if h.Signal(pid, 0) != nil {
				return
			}
		}
	}
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
