package gitx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = Environ(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// snapshotDir records every file under dir with its size and modification time.
func snapshotDir(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %s %s %d\n", p, info.Mode(), info.ModTime(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestFetchCommitCopiesAnUnreferencedCommitWithoutTouchingTheSource(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	git(t, source, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, source, "add", "a.txt")
	git(t, source, "commit", "-q", "-m", "first")
	// A commit no ref points at, like a detached experiment.
	tree := git(t, source, "write-tree")
	dangling := git(t, source, "commit-tree", tree, "-p", "HEAD", "-m", "dangling")
	// Hooks in the source must never run.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	for _, hook := range []string{"reference-transaction", "post-checkout", "pre-push", "post-index-change"} {
		script := "#!/bin/sh\ntouch " + marker + "\n"
		if err := os.WriteFile(filepath.Join(source, ".git", "hooks", hook), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotDir(t, source)

	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := InitBare(ctx, bare); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := FetchCommit(ctx, bare, source, dangling); err != nil {
		t.Fatal(err)
	}
	if got, err := Run(ctx, "--git-dir", bare, "rev-parse", "refs/agentium/sources/"+dangling); err != nil || got != dangling {
		t.Errorf("fetched ref = %q, %v; want %s", got, err, dangling)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a hook ran")
	}
	if after := snapshotDir(t, source); after != before {
		t.Errorf("fetching changed the source repository:\nbefore %s\nafter  %s", before, after)
	}
	if err := FetchCommit(ctx, bare, source, strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "git --git-dir") {
		t.Errorf("missing commit: err = %v, want a contextual git error", err)
	}
}

func TestEnvironDropsInheritedGitVariables(t *testing.T) {
	got := Environ([]string{"HOME=/h", "GIT_DIR=/user/.git", "GIT_INDEX_FILE=/user/.git/index", "GIT_CONFIG_PARAMETERS='x=y'", "PATH=/bin"})
	for _, dropped := range []string{"GIT_DIR=/user/.git", "GIT_INDEX_FILE=/user/.git/index", "GIT_CONFIG_PARAMETERS='x=y'"} {
		if slices.Contains(got, dropped) {
			t.Errorf("%s was inherited: %v", dropped, got)
		}
	}
	for _, kept := range []string{"HOME=/h", "PATH=/bin", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"} {
		if !slices.Contains(got, kept) {
			t.Errorf("%s missing: %v", kept, got)
		}
	}
}

func TestInheritedGitDirCannotRedirectCommands(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	decoy := filepath.Join(t.TempDir(), "decoy.git")
	if err := InitBare(ctx, decoy); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", decoy) // as inside a git hook: without the scrub, -C repo would act on decoy
	got, err := Run(ctx, "-C", repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git")); got != want && got != filepath.Join(repo, ".git") {
		t.Errorf("git dir = %q, want %q", got, want)
	}
}
