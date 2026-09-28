package cli

import (
	"bytes"
	"context"
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

// repoState captures everything init must leave untouched: files (tracked, untracked, ignored), refs and git config.
func repoState(t *testing.T, dir string) string {
	t.Helper()
	config, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	return gitIn(t, dir, "status", "--porcelain", "--ignored") + gitIn(t, dir, "for-each-ref") + string(config)
}

func TestInitRegistersWithoutTouchingTheRepository(t *testing.T) {
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
	for _, want := range []string{"Registered ", "(project 1)", claude + " 2.1.281", "go test ./...", "CLAUDE.md (8 B)", "your Claude login", "was not modified"} {
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

func TestInitErrors(t *testing.T) {
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
}
