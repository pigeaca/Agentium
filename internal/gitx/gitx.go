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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/home"
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
	// A cancelled git is asked to stop (SIGINT) so it can remove its lock files (shallow.lock, index.lock), as Ctrl-C in
	// a terminal would: git runs in its own process group and the whole group gets the signal, so its children (a shell
	// alias, a remote helper) stop too and release the output pipes. Anything still running after the delay is killed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
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

// PartialClone reports whether the repository at dir is a partial clone (cloned with --filter): its config names a
// promisor remote or sets extensions.partialClone. Some of its objects are then missing locally, and reading history
// offline fails or makes git try to fetch them. Only the repository's own config is read.
func PartialClone(ctx context.Context, dir string) (bool, error) {
	out, err := Run(ctx, "-C", dir, "config", "--local", "--list")
	if err != nil {
		return false, fmt.Errorf("check for a partial clone: %w", err)
	}
	for _, line := range strings.Split(out, "\n") { // git prints keys lowercased: key=value
		key, value, _ := strings.Cut(line, "=")
		switch {
		case key == "extensions.partialclone":
			return true, nil
		case strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".promisor") && strings.EqualFold(value, "true"):
			return true, nil
		}
	}
	return false, nil
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
//
// Fetches into one repository run one at a time, across goroutines and processes: a shallow fetch rewrites the
// repository's shallow file, and git fails one whose file changed while it ran ("shallow file has changed since we
// read it", or shallow.lock exists), so concurrent imports into a project's repository used to lose tasks at random.
// The lock is FetchLock in the repository's git folder, held for the fetch only and waited for until ctx ends.
func FetchCommit(ctx context.Context, source, commit, ref string, where ...string) error {
	gitDir, err := Run(ctx, append(append([]string{}, where...), "rev-parse", "--absolute-git-dir")...)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", commit, err)
	}
	unlock, err := home.LockFile(ctx, filepath.Join(gitDir, FetchLock), nil)
	if err != nil {
		return fmt.Errorf("fetch %s: lock %s: %w", commit, gitDir, err)
	}
	defer unlock()
	refspec := commit
	if ref != "" {
		refspec += ":" + ref
	}
	args := append(append([]string{}, where...), "fetch", "--quiet", "--no-tags", "--depth=1",
		"--upload-pack=git -c uploadpack.allowAnySHA1InWant=true upload-pack", "--end-of-options", source, refspec)
	_, err = Run(ctx, args...)
	return err
}

// FetchLock is the file, in a repository's git folder, that FetchCommit locks while it fetches into that repository.
const FetchLock = "agentium-fetch.lock"

// SourceRef is where FetchCommit keeps a user's commit in Agentium's bare repository.
func SourceRef(commit string) string { return "refs/agentium/sources/" + commit }
