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
	"strings"
	"syscall"
	"time"
)

// Spec is one command to run.
type Spec struct {
	Dir     string
	Command string        // run with /bin/sh -c
	Env     []string      // added to Environ(os.Environ())
	Timeout time.Duration // 0: no timeout beyond ctx
	// Output receives stdout and stderr. Pass an *os.File: the child then writes to it directly, so a background
	// process that keeps the output open cannot hold Run past the command's end.
	Output io.Writer
}

// Result is how a command ended.
type Result struct {
	ExitCode int // -1 when it timed out or was killed by a signal
	Duration time.Duration
	TimedOut bool
}

// Passed reports whether the command exited with status 0.
func (r Result) Passed() bool { return r.ExitCode == 0 && !r.TimedOut }

// credentialVars are removed from every child's environment, whatever their value.
func credentialVars() []string {
	return []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY",
		"GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN", "NPM_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS", "AZURE_OPENAI_API_KEY",
	}
}

// Environ is environ without credentials, GIT_* (a hook's GIT_DIR would redirect git in the child) and AGENTIUM_*.
func Environ(environ []string) []string {
	drop := map[string]bool{}
	for _, name := range credentialVars() {
		drop[name] = true
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] || strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "AGENTIUM_") {
			continue
		}
		out = append(out, kv)
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
	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", spec.Command)
	cmd.Dir = spec.Dir
	cmd.Env = append(Environ(os.Environ()), spec.Env...)
	cmd.Stdout, cmd.Stderr = spec.Output, spec.Output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err := cmd.Run()
	result := Result{Duration: time.Since(start), ExitCode: -1}
	if cmd.Process != nil {
		killGroup(cmd.Process.Pid) // background children of a finished command
	}
	switch {
	case ctx.Err() != nil:
		return result, fmt.Errorf("run %q: %w", spec.Command, ctx.Err())
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
		return result, fmt.Errorf("run %q: %w", spec.Command, err)
	}
	result.ExitCode = 0
	return result, nil
}

// killGroup kills the process group led by pid; a group that is already gone is not an error.
func killGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
