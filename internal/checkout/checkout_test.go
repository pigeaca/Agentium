package checkout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/gitx"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = gitx.Environ(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type memSource map[string]string

func (m memSource) Paths() []string {
	var paths []string
	for p := range m {
		paths = append(paths, strings.TrimSuffix(p, "*"))
	}
	sort.Strings(paths)
	return paths
}
func (m memSource) ReadFile(p string) ([]byte, error) {
	for _, key := range []string{p, p + "*"} {
		if data, ok := m[key]; ok {
			return []byte(data), nil
		}
	}
	return nil, os.ErrNotExist
}
func (m memSource) Executable(p string) bool { _, ok := m[p+"*"]; return ok }
func (m memSource) Describe() string         { return "memory" }

func TestNewHoldsOnlyTheBaseCommit(t *testing.T) {
	ctx := context.Background()
	user := t.TempDir()
	git(t, user, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(user, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, user, "add", "-A")
	git(t, user, "commit", "-q", "-m", "base")
	base := git(t, user, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(user, "a_test.go"), []byte("package a // hidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, user, "add", "-A")
	git(t, user, "commit", "-q", "-m", "solution")
	solution := git(t, user, "rev-parse", "HEAD")

	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	for _, commit := range []string{base, solution} {
		if err := gitx.FetchCommit(ctx, user, commit, gitx.SourceRef(commit), "--git-dir", bare); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "run")
	if err := New(ctx, bare, base, dir); err != nil {
		t.Fatal(err)
	}
	if head := git(t, dir, "rev-parse", "HEAD"); head != base {
		t.Errorf("HEAD = %s, want %s", head, base)
	}
	if _, err := os.Stat(filepath.Join(dir, "a_test.go")); err == nil {
		t.Error("the solution's file is in the checkout")
	}
	if _, err := gitx.Run(ctx, "-C", dir, "cat-file", "-e", solution); err == nil {
		t.Error("the solution commit is reachable from the checkout")
	}
	config, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if _, err := os.Stat(filepath.Join(dir, ".git", "FETCH_HEAD")); err == nil || strings.Contains(string(config), bare) {
		t.Error("the checkout records where the bare repository is")
	}
	if hooks, _ := os.ReadDir(filepath.Join(dir, ".git", "hooks")); len(hooks) != 0 {
		t.Errorf("the checkout has template hooks: %v", hooks)
	}
	if err := New(ctx, bare, base, dir); err == nil {
		t.Error("New must refuse an existing folder")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := New(ctx, bare, strings.Repeat("0", 40), missing); err == nil {
		t.Error("an unknown commit must fail")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("a failed checkout left its folder behind")
	}
}

func TestWriteAddsReplacesAndDeletes(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"keep.txt": "keep\n", "old.go": "old\n", "gone.go": "bye\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink(outside, filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	src := memSource{"old.go": "new\n", "pkg/new_test.go": "package pkg\n", "run.sh*": "#!/bin/sh\n", "link.go": "replaced\n"}
	if err := Write(dir, src, []string{"old.go", "pkg/new_test.go", "run.sh", "gone.go", "link.go"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"keep.txt": "keep\n", "old.go": "new\n", "pkg/new_test.go": "package pkg\n", "link.go": "replaced\n"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", name, data, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.go")); err == nil {
		t.Error("gone.go should be deleted")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("writing followed a symlink out of the checkout")
	}
	if info, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh should be executable: %v %v", info, err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, memSource{"linked/x_test.go": "x"}, []string{"linked/x_test.go"}); err == nil {
		t.Error("wrote through a symlinked folder")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("a file landed outside the checkout: %v", entries)
	}
	for _, bad := range []string{"../escape", "/abs", "a/../../b", ".git/config", "sub/.GIT/hooks/x", ""} {
		if err := Write(dir, memSource{bad: "x"}, []string{bad}); err == nil {
			t.Errorf("unsafe path %q accepted", bad)
		}
	}
}
