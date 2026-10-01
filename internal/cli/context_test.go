package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cliResult struct {
	code           int
	stdout, stderr string
}

// cliIn returns a runner for Agentium commands in dir, with data in its own folder.
func cliIn(t *testing.T, dir, data string) func(args ...string) cliResult {
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": t.TempDir(), "AGENTIUM_CLAUDE": filepath.Join(t.TempDir(), "no-claude")}
	return func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{
			Args: args, Stdout: &stdout, Stderr: &stderr, Dir: dir,
			Getenv:   func(key string) string { return vars[key] },
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Now:      func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) },
		})
		return cliResult{code, stdout.String(), stderr.String()}
	}
}

func writeFile(t *testing.T, root, p, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func expect(t *testing.T, r cliResult, code int, fragments ...string) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit %d, want %d\nstdout %s\nstderr %s", r.code, code, r.stdout, r.stderr)
	}
	for _, fragment := range fragments {
		if !strings.Contains(r.stdout+r.stderr, fragment) {
			t.Errorf("output lacks %q:\nstdout %s\nstderr %s", fragment, r.stdout, r.stderr)
		}
	}
}

func TestContextSnapshotListDiffWithoutTouchingTheRepository(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	writeFile(t, outer, "CLAUDE.md", "personal notes above the repository\n")
	repo := filepath.Join(outer, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "CLAUDE.md", "# Project\n@AGENTS.md\nSee [testing](docs/testing.md).\n")
	writeFile(t, repo, "docs/testing.md", "Run go test.\n")
	writeFile(t, repo, "AGENTS.md", "Run the tests before finishing.\n")
	writeFile(t, repo, ".claude/rules/go.md", "Always gofmt.\n")
	writeFile(t, repo, "go.mod", "module x\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "initial")
	data := filepath.Join(t.TempDir(), "data")
	run := cliIn(t, repo, data)

	expect(t, run("context", "list"), ExitError, "run `agentium init` first")
	expect(t, run("init"), ExitOK)
	show := run("context", "show")
	expect(t, show, ExitOK, "(working tree)", "AGENTS.md", "via CLAUDE.md", ".claude/rules/go.md", "tokens",
		filepath.Join(outer, "CLAUDE.md")+" is above the repository")
	if strings.Contains(show.stdout, "personal notes") {
		t.Error("files above the repository must not be read")
	}
	expect(t, run("context", "snapshot", "full"), ExitOK, "Saved snapshot full from HEAD", "3 file(s)",
		"1 file(s) linked from the context are not in this snapshot (e.g. docs/testing.md)")
	expect(t, run("context", "snapshot", "--include", "docs/testing.md", "with-docs"), ExitOK, "4 file(s)")
	linked := run("context", "snapshot", "all-linked", "--include-linked")
	expect(t, linked, ExitOK, "4 file(s)")
	if strings.Contains(linked.stdout, "linked from the context are not in this snapshot") {
		t.Errorf("--include-linked left linked files out:\n%s", linked.stdout)
	}
	expect(t, run("context", "snapshot", "--include", "go.mod", "bad-include"), ExitError, "only documents (Markdown")
	if bare := filepath.Join(data, "projects", "1", "repo.git"); strings.Contains(gitIn(t, bare, "for-each-ref"), "refs/agentium/sources") {
		t.Error("show and snapshot must read commits in place, not copy the repository's history")
	}

	// A minimal version, edited in the working tree and never committed.
	writeFile(t, repo, "CLAUDE.md", "# Project\n")
	writeFile(t, repo, "main.go", "package main\n")
	future := time.Now().Add(time.Hour) // restat an unchanged tracked file: a plain `git status` would rewrite the index
	if err := os.Chtimes(filepath.Join(repo, "go.mod"), future, future); err != nil {
		t.Fatal(err)
	}
	before := repoState(t, repo)
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	indexInfo, err := os.Stat(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	future = future.Add(time.Hour) // repoState refreshed the index; make go.mod stale again
	if err := os.Chtimes(filepath.Join(repo, "go.mod"), future, future); err != nil {
		t.Fatal(err)
	}

	expect(t, run("context", "snapshot", "--working-tree", "minimal"), ExitOK,
		"Saved snapshot minimal from working tree", "not in this snapshot (e.g. main.go)", "AGENTS.md is not loaded")
	expect(t, run("context", "snapshot", "full"), ExitError, `snapshot "full" already exists`)
	expect(t, run("context", "snapshot", "Bad Name"), ExitUsage, "must be lowercase")
	expect(t, run("context", "snapshot", "a..b"), ExitUsage, "must be lowercase")
	expect(t, run("context", "show", "--ref", "--output=escape.txt"), ExitError, "is not a commit")
	if _, err := os.Stat(filepath.Join(repo, "escape.txt")); err == nil {
		t.Error("a ref was taken as a git option")
	}
	expect(t, run("context", "snapshot", "-h"), ExitOK, "--include PATH")
	expect(t, run("context", "diff", "--bogus"), ExitUsage, "flag provided but not defined")
	expect(t, run("context", "snapshot", "x", "--ref", "HEAD", "--working-tree"), ExitUsage)
	expect(t, run("context", "snapshot", "x", "--ref", "no-such-branch"), ExitError, `"no-such-branch" is not a commit`)
	expect(t, run("context", "show", "--ref", "HEAD"), ExitOK, "(commit ", "via CLAUDE.md")
	list := run("context", "list")
	expect(t, list, ExitOK, "full", "with-docs", "all-linked", "minimal", "working tree")
	if strings.Index(list.stdout, "full") > strings.Index(list.stdout, "minimal") {
		t.Errorf("list is not oldest first:\n%s", list.stdout)
	}
	diff := run("context", "diff", "full", "minimal", "--patch")
	expect(t, diff, ExitOK, "full -> minimal", "AGENTS.md", "CLAUDE.md", "-@AGENTS.md", "-Run the tests before finishing.")
	if !strings.Contains(diff.stdout, "(-") {
		t.Errorf("the minimal context should be smaller:\n%s", diff.stdout)
	}
	expect(t, run("context", "diff", "full", "full"), ExitOK, "No differences.")
	expect(t, run("context", "diff", "full", "missing"), ExitError, `snapshot "missing"`)
	expect(t, run("context"), ExitUsage, "agentium context show")
	expect(t, run("context", "bogus"), ExitUsage, `unknown subcommand "bogus"`)

	if after, _ := os.ReadFile(filepath.Join(repo, ".git", "index")); !bytes.Equal(after, index) {
		t.Error("Agentium rewrote the repository's index")
	}
	if info, _ := os.Stat(filepath.Join(repo, ".git", "index")); !info.ModTime().Equal(indexInfo.ModTime()) {
		t.Error("Agentium touched the repository's index")
	}
	if after := repoState(t, repo); after != before {
		t.Errorf("context commands modified the repository:\nbefore %s\nafter  %s", before, after)
	}
}
