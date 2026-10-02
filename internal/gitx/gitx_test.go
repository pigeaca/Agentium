package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/home"
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
	if err := FetchCommit(ctx, source, dangling, SourceRef(dangling), "--git-dir", bare); err != nil {
		t.Fatal(err)
	}
	if got, err := Run(ctx, "--git-dir", bare, "rev-parse", SourceRef(dangling)); err != nil || got != dangling {
		t.Errorf("fetched ref = %q, %v; want %s", got, err, dangling)
	}
	if count, _ := Run(ctx, "--git-dir", bare, "rev-list", "--count", dangling); count != "1" {
		t.Errorf("fetched %s commits, want only the commit itself (depth 1)", count)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a hook ran")
	}
	if after := snapshotDir(t, source); after != before {
		t.Errorf("fetching changed the source repository:\nbefore %s\nafter  %s", before, after)
	}
	if err := FetchCommit(ctx, source, strings.Repeat("0", 40), "", "--git-dir", bare); err == nil || !strings.Contains(err.Error(), "git --git-dir") {
		t.Errorf("missing commit: err = %v, want a contextual git error", err)
	}
}

func TestEnvironDropsInheritedGitVariables(t *testing.T) {
	got := Environ([]string{"HOME=/h", "GIT_DIR=/user/.git", "GIT_INDEX_FILE=/user/.git/index", "GIT_CONFIG_PARAMETERS='x=y'", "PATH=/bin",
		"ANTHROPIC_API_KEY=k", "GITHUB_TOKEN=t"})
	for _, dropped := range []string{"GIT_DIR=/user/.git", "GIT_INDEX_FILE=/user/.git/index", "GIT_CONFIG_PARAMETERS='x=y'", "ANTHROPIC_API_KEY=k", "GITHUB_TOKEN=t"} {
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

// TestEnvironPinned pins what Environ keeps byte for byte: credentials and GIT_* dropped (AGENTIUM_* kept, unlike
// runner.Environ), then the three variables git is always given.
func TestEnvironPinned(t *testing.T) {
	environ := []string{"PATH=/bin", "HOME=/h", "GIT_DIR=/x", "GIT_AUTHOR_NAME=a", "AGENTIUM_HOME=/a", "AGENTIUM_X=1", "ANTHROPIC_API_KEY=k",
		"GITHUB_TOKEN=t", "SSH_AUTH_SOCK=/s", "MY_PASSWORD=p", "CLAUDE_CODE_TMPDIR=/t", "LC_ALL=C", "GOFLAGS=-mod=mod", "JAVA_HOME=/j",
		"RUSTC_WRAPPER=w", "HTTPS_PROXY=http://p", "XDG_RUNTIME_DIR=/r", "FOO=bar", "NODE_OPTIONS=x", "TERM=xterm", "EMPTY=", "GOPATH=/g",
		"MAVEN_OPTS=-X", "GRADLE_USER_HOME=/gu", "CARGO_HOME=/c", "SHELL=/bin/zsh", "TMPDIR=/tmp", "AWS_PROFILE=p", "NETRC=/n", "GIT=ok", "GITHUB=ok"}
	want := []string{"PATH=/bin", "HOME=/h", "AGENTIUM_HOME=/a", "AGENTIUM_X=1", "CLAUDE_CODE_TMPDIR=/t", "LC_ALL=C", "GOFLAGS=-mod=mod",
		"JAVA_HOME=/j", "RUSTC_WRAPPER=w", "HTTPS_PROXY=http://p", "XDG_RUNTIME_DIR=/r", "FOO=bar", "NODE_OPTIONS=x", "TERM=xterm", "EMPTY=",
		"GOPATH=/g", "MAVEN_OPTS=-X", "GRADLE_USER_HOME=/gu", "CARGO_HOME=/c", "SHELL=/bin/zsh", "TMPDIR=/tmp", "GIT=ok", "GITHUB=ok",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0"}
	if got := Environ(environ); !slices.Equal(got, want) {
		t.Errorf("Environ = %q\nwant %q", got, want)
	}
	if got := Environ(nil); !slices.Equal(got, want[len(want)-3:]) {
		t.Errorf("Environ(nil) = %q", got)
	}
}

// A cancelled git is asked to stop with SIGINT (so it can remove its lock files) and the call returns at once, well
// before the kill delay.
func TestCancelStopsGitGently(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "-c", "alias.slow=!sleep 30", "slow") // a git that would run for 30 seconds
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled git succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled git kept running")
	}
}

// A repository is a partial clone when its config says so, by either signal; a plain one is not.
func TestPartialClone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, set := range map[string][]string{
		"plain":           nil,
		"extension":       {"extensions.partialClone", "origin"},
		"promisor remote": {"remote.origin.promisor", "true"},
		"promisor false":  {"remote.origin.promisor", "false"},
	} {
		dir := t.TempDir()
		git(t, dir, "init", "-q")
		if set != nil {
			git(t, dir, "config", set[0], set[1])
		}
		got, err := PartialClone(ctx, dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := name == "extension" || name == "promisor remote"; got != want {
			t.Errorf("%s: partial = %v, want %v", name, got, want)
		}
	}
}

// commits makes n commits in a new repository and returns it with their IDs, oldest first.
func commits(t *testing.T, n int) (string, []string) {
	t.Helper()
	source := t.TempDir()
	git(t, source, "init", "-q", "-b", "main")
	var ids []string
	for i := range n {
		if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte(fmt.Sprintf("%d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, source, "add", "a.txt")
		git(t, source, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
		ids = append(ids, git(t, source, "rev-parse", "HEAD"))
	}
	return source, ids
}

// Shallow fetches rewrite the bare repository's shallow file; git refuses one whose file changed under it ("shallow
// file has changed since we read it"), so concurrent imports into one project's repository used to fail at random.
func TestFetchCommitConcurrentlyIntoOneRepository(t *testing.T) {
	ctx := context.Background()
	source, ids := commits(t, 10)
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, len(ids))
	for _, id := range ids {
		go func() { errs <- FetchCommit(ctx, source, id, SourceRef(id), "--git-dir", bare) }()
	}
	for range ids {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	for _, id := range ids {
		if got, err := Run(ctx, "--git-dir", bare, "rev-parse", SourceRef(id)); err != nil || got != id {
			t.Errorf("ref for %s = %q, %v", id, got, err)
		}
	}
}

// A failed or cancelled fetch never keeps the repository's fetch lock, and a fetch waiting for the lock stops when its
// context ends.
func TestFetchCommitReleasesItsLock(t *testing.T) {
	ctx := context.Background()
	source, ids := commits(t, 1)
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(bare, FetchLock)
	free := func(when string) {
		t.Helper()
		short, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		unlock, err := home.LockFile(short, lockPath, nil)
		if err != nil {
			t.Fatalf("%s: the fetch lock is still held: %v", when, err)
		}
		unlock()
	}

	if err := FetchCommit(ctx, source, strings.Repeat("0", 40), "", "--git-dir", bare); err == nil {
		t.Fatal("fetching a missing commit succeeded")
	}
	free("after a failed fetch")

	// Cancelled before it starts: the fetch fails and leaves no lock behind (a cancel during the fetch releases it through
	// the same deferred unlock).
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := FetchCommit(cancelled, source, ids[0], SourceRef(ids[0]), "--git-dir", bare); err == nil {
		t.Fatal("a cancelled fetch succeeded")
	}
	free("after a cancelled fetch")

	// Cancelled while waiting for another holder: it gives up with the context's error, and fetches nothing.
	unlock, err := home.LockFile(ctx, lockPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := FetchCommit(waiting, source, ids[0], SourceRef(ids[0]), "--git-dir", bare); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch while the lock is held: err = %v, want the context's deadline", err)
	}
	if _, err := Run(ctx, "--git-dir", bare, "rev-parse", "--verify", "--quiet", SourceRef(ids[0])); err == nil {
		t.Error("the fetch ran without the lock")
	}
	unlock()
	if err := FetchCommit(ctx, source, ids[0], SourceRef(ids[0]), "--git-dir", bare); err != nil {
		t.Fatalf("after the holder let go: %v", err)
	}
}
