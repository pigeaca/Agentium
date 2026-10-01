package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// repoState captures everything Agentium must leave untouched: files (tracked, untracked, ignored), refs, HEAD, git
// config, hooks and the index (its bytes and modification time). It reads without optional locks, so taking the state
// does not itself rewrite the index.
func repoState(t *testing.T, dir string) string {
	t.Helper()
	var state strings.Builder
	state.WriteString(gitIn(t, dir, "--no-optional-locks", "status", "--porcelain", "--ignored") + gitIn(t, dir, "for-each-ref"))
	for _, name := range []string{"config", "HEAD", "index"} {
		file := filepath.Join(dir, ".git", name)
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&state, "%s %x %s\n", name, sha256.Sum256(data), info.ModTime())
	}
	hooks, err := os.ReadDir(filepath.Join(dir, ".git", "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hook := range hooks {
		state.WriteString("hook " + hook.Name() + "\n")
	}
	return state.String()
}

func TestInitRegistersWithoutTouchingTheRepository(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	for name, body := range map[string]string{"go.mod": "module x\n", "CLAUDE.md": "# Rules\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "initial")
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\necho '2.1.281 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "agentium-home")
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": t.TempDir(), "AGENTIUM_CLAUDE": claude}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{
			Args: args, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv:   func(key string) string { return vars[key] },
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Now:      func() time.Time { return now },
		})
		return code, stdout.String(), stderr.String()
	}

	before := repoState(t, repo)
	code, stdout, stderr := run("init")
	if code != ExitOK {
		t.Fatalf("init exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"Registered ", "(project 1)", claude + " 2.1.281", "go test ./...", "about 2 tokens at session start (estimated) from 1 file(s)", "your Claude login", "was not modified"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if after := repoState(t, repo); after != before {
		t.Errorf("init modified the repository:\nbefore %s\nafter  %s", before, after)
	}

	code, stdout, _ = run("init", ".")
	if code != ExitOK || !strings.Contains(stdout, "(project 1)") {
		t.Errorf("re-running init must keep the project: exit %d\n%s", code, stdout)
	}
	db, err := store.Open(context.Background(), filepath.Join(data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(context.Background())
	if err != nil || len(projects) != 1 || !strings.Contains(string(projects[0].Discovery), `"sign_in":"login"`) {
		t.Errorf("stored projects = %+v, err %v", projects, err)
	}
}

func TestInitRefusesADataFolderInsideTheRepository(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "initial")
	before := repoState(t, repo)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), Env{
		Args: []string{"init", repo}, Stdout: &stdout, Stderr: &stderr, Dir: t.TempDir(),
		Getenv: func(key string) string {
			return map[string]string{"AGENTIUM_HOME": filepath.Join(repo, ".agentium-data")}[key]
		},
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now,
	})
	if code != ExitError || !strings.Contains(stderr.String(), "is inside the repository") || strings.Contains(stdout.String(), "was not modified") {
		t.Errorf("exit %d\nstdout %s\nstderr %s", code, stdout.String(), stderr.String())
	}
	if after := repoState(t, repo); after != before {
		t.Errorf("the repository changed:\nbefore %s\nafter  %s", before, after)
	}
}

func TestInitErrors(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := Env{Stdout: &stdout, Stderr: &stderr, Dir: t.TempDir(), Getenv: func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now}
	env.Args = []string{"init", "a", "b"}
	if code := Run(context.Background(), env); code != ExitUsage {
		t.Errorf("two paths: exit %d, want %d", code, ExitUsage)
	}
	env.Args = []string{"init"}
	if code := Run(context.Background(), env); code != ExitError || !strings.Contains(stderr.String(), "not inside a git repository") {
		t.Errorf("outside a repository: exit %d, stderr %q", code, stderr.String())
	}
	for _, help := range []string{"-h", "--help"} {
		stdout.Reset()
		env.Args = []string{"init", help}
		if code := Run(context.Background(), env); code != ExitOK || !strings.Contains(stdout.String(), "Usage: agentium init") {
			t.Errorf("%s: exit %d, stdout %q", help, code, stdout.String())
		}
	}
	stderr.Reset()
	env.Args = []string{"init", "--force"}
	if code := Run(context.Background(), env); code != ExitUsage || !strings.Contains(stderr.String(), "flag provided but not defined: -force") {
		t.Errorf("unknown flag: exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	env.Args, env.Dir = []string{"init"}, ""
	if code := Run(context.Background(), env); code != ExitError || !strings.Contains(stderr.String(), "current folder cannot be read") {
		t.Errorf("unreadable working folder: exit %d, stderr %q", code, stderr.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stderr.Reset()
	env.Args = []string{"init", t.TempDir()}
	if code := Run(ctx, env); code != ExitError || !strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("cancelled: exit %d, stderr %q", code, stderr.String())
	}
}
