// Package runner runs commands for Agentium (verification commands now, agent CLIs later). Each command runs in its own
// process group with a timeout and an environment without credentials, and the whole group is killed when the command
// ends, times out or is cancelled, so no process outlives its run. Unix only (macOS and Linux).
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Spec is one command to run.
type Spec struct {
	Dir     string
	Command string   // run with /bin/sh -c
	Args    []string // instead of Command: run Args[0] with Args[1:], no shell
	Env     []string // added to the base environment
	// Environ replaces the base environment, Environ(os.Environ()), when not nil; the caller filters it.
	Environ []string
	Timeout time.Duration // 0: no timeout beyond ctx
	// Grace, when not zero, stops the command gently on timeout or cancel: SIGINT to its process group first, SIGKILL
	// after Grace. Claude Code, for one, finishes its turn and reports a result on SIGINT.
	Grace time.Duration
	// Output receives stdout, and stderr too unless Stderr is set. Pass *os.File values: the child then writes to them
	// directly, so a background process that keeps the output open cannot hold Run past the command's end.
	Output io.Writer
	Stderr io.Writer
	// Stdin, when set, is the command's standard input (for example a prompt too long for an argument).
	Stdin io.Reader
	// Started, when set, is called with the process ID (also its process group's) once the command runs.
	Started func(pid int)
	// Stop, when set, stops the command once it is closed, the way a timeout does (gently with Grace, then SIGKILL), and
	// the result says so (Result.Stopped): a watcher's decision, such as Agentium's cost cap, not a timeout.
	Stop <-chan struct{}
	// BeforeStop, when set, is called right before the command's process group is signalled to stop (a timeout, a
	// cancellation, Stop), while the command still runs: a caller tracking its processes looks once more.
	BeforeStop func()
}

// Result is how a command ended.
type Result struct {
	ExitCode int // -1 when it timed out or was killed by a signal
	Duration time.Duration
	TimedOut bool
	// Stopped: Spec.Stop was closed while the command ran, and it was stopped for that (never with TimedOut).
	Stopped bool
}

// Passed reports whether the command exited with status 0.
func (r Result) Passed() bool { return r.ExitCode == 0 && !r.TimedOut && !r.Stopped }

// IsCredential reports whether an environment variable looks like it carries a credential: by name pattern (tokens,
// keys, secrets, passwords, credentials) or by being a known one (the ssh agent socket, which allows pushes; Docker and
// netrc settings). The value is never looked at. Credentials in files under HOME are not covered: agent runs must deny
// those paths themselves.
func IsCredential(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "SSH_AUTH_SOCK", "DOCKER_AUTH_CONFIG", "NETRC", "AWS_ACCESS_KEY_ID", "AWS_PROFILE", "GOOGLE_APPLICATION_CREDENTIALS":
		return true
	}
	for _, marker := range []string{"TOKEN", "API_KEY", "APIKEY", "SECRET", "PASSWORD", "PASSWD", "ACCESS_KEY", "PRIVATE_KEY", "CREDENTIAL", "AUTH_KEY"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// EnvPolicy is how an environment is filtered; runner.Environ, gitx.Environ and sandbox.Environ are each one policy.
// Whatever the policy, a variable that IsCredential is dropped: that is the part all three share.
type EnvPolicy struct {
	// Allowlist keeps only the variables named in Names or starting with one of Prefixes; otherwise everything not
	// dropped is kept.
	Allowlist bool
	Names     []string
	Prefixes  []string
	// DropPrefixes drops variables by name prefix (for example GIT_), even when the allowlist names them.
	DropPrefixes []string
}

// GitPrefix is the prefix of git's own variables. A hook's GIT_DIR would redirect git in a child, and an inherited
// GIT_INDEX_FILE would write the user's index.
const GitPrefix = "GIT_"

// Filter returns the variables of environ the policy keeps, in their order, unchanged. It is nil when none is kept.
func (p EnvPolicy) Filter(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if IsCredential(name) || hasAnyPrefix(name, p.DropPrefixes) {
			continue
		}
		if p.Allowlist && !slices.Contains(p.Names, name) && !hasAnyPrefix(name, p.Prefixes) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func hasAnyPrefix(name string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) })
}

// Environ is environ without credentials, GIT_* (a hook's GIT_DIR would redirect git in the child) and AGENTIUM_*.
// It is never nil: Spec.Environ nil means "use this default".
func Environ(environ []string) []string {
	out := EnvPolicy{DropPrefixes: []string{GitPrefix, "AGENTIUM_"}}.Filter(environ)
	if out == nil {
		out = []string{}
	}
	return out
}

// Run runs spec. A non-zero exit or a timeout is a Result, not an error; errors mean the command could not run, or ctx
// was cancelled (then the process group has been killed).
func Run(ctx context.Context, spec Spec) (Result, error) {
	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}
	defer cancel()
	// A stop (Spec.Stop) cancels the command's own context with errStopped as its cause, so it ends as a timeout does
	// but is told apart from one.
	var stopCancel context.CancelCauseFunc
	runCtx, stopCancel = context.WithCancelCause(runCtx)
	defer stopCancel(nil)
	if spec.Stop != nil {
		go func() {
			select {
			case <-spec.Stop:
				stopCancel(errStopped)
			case <-runCtx.Done():
			}
		}()
	}
	argv := []string{"/bin/sh", "-c", spec.Command}
	if len(spec.Args) > 0 {
		argv = spec.Args
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = spec.Dir
	base := spec.Environ
	if base == nil {
		base = Environ(os.Environ())
	}
	cmd.Env = append(append([]string{}, base...), spec.Env...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = spec.Output, spec.Output, spec.Stdin
	if spec.Stderr != nil {
		cmd.Stderr = spec.Stderr
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var escalate *time.Timer
	cmd.Cancel = func() error {
		if spec.BeforeStop != nil {
			spec.BeforeStop()
		}
		if spec.Grace <= 0 {
			return killGroup(cmd.Process.Pid)
		}
		pid := cmd.Process.Pid
		escalate = time.AfterFunc(spec.Grace, func() { killGroup(pid) })
		return interruptGroup(pid)
	}
	cmd.WaitDelay = spec.Grace + 5*time.Second
	start := time.Now()
	err := cmd.Start()
	if err == nil {
		if spec.Started != nil {
			spec.Started(cmd.Process.Pid)
		}
		err = cmd.Wait()
	}
	if escalate != nil {
		escalate.Stop()
	}
	result := Result{Duration: time.Since(start), ExitCode: -1}
	if cmd.Process != nil {
		killGroup(cmd.Process.Pid) // background children of a finished command
	}
	name := spec.Command
	if len(spec.Args) > 0 {
		name = spec.Args[0]
	}
	switch {
	case ctx.Err() != nil:
		return result, fmt.Errorf("run %q: %w", name, ctx.Err())
	case errors.Is(context.Cause(runCtx), errStopped):
		result.Stopped = true
		return result, nil
	case runCtx.Err() != nil:
		result.TimedOut = true
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("run %q: %w", name, err)
	}
	result.ExitCode = 0
	return result, nil
}

// errStopped is the cause of a command's context when Spec.Stop stopped it.
var errStopped = errors.New("stopped")

// interruptGroup sends SIGINT to the process group led by pid; a group that is already gone is not an error.
func interruptGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// killGroup kills the process group led by pid; a group that is already gone is not an error.
func killGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
