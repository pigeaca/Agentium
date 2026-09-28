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

// Environ is environ without GIT_* variables, plus: never prompt, ignore system-wide config, and never take optional
// locks (so `git status` does not rewrite the user's index).
func Environ(environ []string) []string {
	out := make([]string, 0, len(environ)+3)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
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

// FetchCommit copies commit (and its history) from the repository at source into the bare repository, under
// refs/agentium/sources/<commit> so it is never garbage-collected. Only upload-pack runs in the source repository: no
// push, no hooks, nothing written there. allowAnySHA1InWant lets a commit that no ref points at be fetched.
func FetchCommit(ctx context.Context, bare, source, commit string) error {
	_, err := Run(ctx, "--git-dir", bare, "fetch", "--quiet", "--no-tags",
		"--upload-pack=git -c uploadpack.allowAnySHA1InWant=true upload-pack",
		source, commit+":refs/agentium/sources/"+commit)
	return err
}
