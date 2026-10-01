// Package gitx runs git for Agentium. Every command disables hooks, fsmonitor, prompts and optional index writes, and
// drops inherited GIT_* variables, so reading a user's repository never changes it or runs its code, and a GIT_DIR
// inherited from a hook or alias cannot redirect Agentium's writes into the user's repository.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// Run runs git with args (for example "-C", dir or "--git-dir", bare first) and returns trimmed stdout.
func Run(ctx context.Context, args ...string) (string, error) {
	out, err := Output(ctx, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// Output runs git, feeding stdin when it is not nil, and returns raw stdout.
func Output(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return OutputEnv(ctx, nil, stdin, args...)
}

// OutputEnv is Output with extra environment variables (for example GIT_INDEX_FILE).
func OutputEnv(ctx context.Context, env []string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(Environ(os.Environ()), env...)
	// A cancelled git is asked to stop (SIGINT) so it can remove its lock files (shallow.lock, index.lock); it is killed
	// only if it has not exited after the delay.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// Environ is environ without GIT_* variables and without credentials (Agentium's git calls are all local, and a
// filter or program configured in a repository must not see an API key), plus: never prompt, ignore system-wide
// config, and never take optional locks (so `git status` does not rewrite the user's index).
func Environ(environ []string) []string {
	out := runner.EnvPolicy{DropPrefixes: []string{runner.GitPrefix}}.Filter(environ)
	return append(out, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0")
}

// InitBare creates a bare repository at dir if it does not exist yet.
func InitBare(ctx context.Context, dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	_, err := Run(ctx, "init", "--quiet", "--bare", dir)
	return err
}

// FetchCommit copies commit, without its history (depth 1), from the repository at source into the repository at
// dest, and keeps it under ref when ref is not empty (so it is never garbage-collected). Only upload-pack runs in the
// source repository: no push, no hooks, nothing written there. allowAnySHA1InWant lets a commit that no ref points at
// be fetched. dest is a --git-dir for bare repositories or a -C folder for checkouts, as given by where.
func FetchCommit(ctx context.Context, source, commit, ref string, where ...string) error {
	refspec := commit
	if ref != "" {
		refspec += ":" + ref
	}
	args := append(append([]string{}, where...), "fetch", "--quiet", "--no-tags", "--depth=1",
		"--upload-pack=git -c uploadpack.allowAnySHA1InWant=true upload-pack", "--end-of-options", source, refspec)
	_, err := Run(ctx, args...)
	return err
}

// SourceRef is where FetchCommit keeps a user's commit in Agentium's bare repository.
func SourceRef(commit string) string { return "refs/agentium/sources/" + commit }
