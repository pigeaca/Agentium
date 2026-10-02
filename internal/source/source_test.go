package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func write(t *testing.T, root, p, body string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// fixture is a repository whose HEAD has CLAUDE.md, an executable hook, a symlink to a file inside the repository and
// one pointing outside it.
func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "CLAUDE.md", "@docs/rules.md\n", 0o644)
	write(t, root, "docs/rules.md", "rules\n", 0o644)
	write(t, root, ".claude/hooks/check.sh", "#!/bin/sh\n", 0o755)
	write(t, root, ".gitignore", "CLAUDE.local.md\n", 0o644)
	if err := os.Symlink("docs/rules.md", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside.md", filepath.Join(root, "docs", "escape.md")); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "initial")
	return root, git(t, root, "rev-parse", "HEAD")
}

func TestCommitReadsModesAndFollowsSymlinksInsideTheRepository(t *testing.T) {
	root, head := fixture(t)
	src, err := Commit(context.Background(), head, "-C", root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".claude/hooks/check.sh", ".gitignore", "AGENTS.md", "CLAUDE.md", "docs/escape.md", "docs/rules.md"}
	if !slices.Equal(src.Paths(), want) {
		t.Errorf("paths = %v, want %v", src.Paths(), want)
	}
	if data, err := src.ReadFile("AGENTS.md"); err != nil || string(data) != "rules\n" {
		t.Errorf("symlink read = %q, %v; want the target's content", data, err)
	}
	if _, err := src.ReadFile("docs/escape.md"); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("escaping symlink: err = %v", err)
	}
	if _, err := src.ReadFile("missing.md"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v, want ErrNotExist", err)
	}
	if !src.Executable(".claude/hooks/check.sh") || src.Executable("CLAUDE.md") {
		t.Error("executable bits not taken from the tree modes")
	}
	if !strings.HasPrefix(src.Describe(), "commit "+head[:12]) || !Has(src, "CLAUDE.md") || Has(src, "CLAUDE") {
		t.Errorf("describe %q / Has mismatch", src.Describe())
	}
}

func TestWorkingTreeSeesUncommittedEditsButNotIgnoredOrDeletedFiles(t *testing.T) {
	root, head := fixture(t)
	write(t, root, "CLAUDE.md", "edited\n", 0o644)                             // modified
	write(t, root, ".claude/rules/new.md", "new rule\n", 0o644)                // untracked
	write(t, root, "CLAUDE.local.md", "personal\n", 0o644)                     // ignored
	if err := os.Remove(filepath.Join(root, "docs", "rules.md")); err != nil { // deleted (AGENTS.md now dangles)
		t.Fatal(err)
	}
	src, err := WorkingTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".claude/hooks/check.sh", ".claude/rules/new.md", ".gitignore", "AGENTS.md", "CLAUDE.md", "docs/escape.md"}
	if !slices.Equal(src.Paths(), want) {
		t.Errorf("paths = %v, want %v", src.Paths(), want)
	}
	if _, err := src.ReadFile("AGENTS.md"); err == nil {
		t.Error("a dangling symlink must not read")
	}
	if data, err := src.ReadFile("CLAUDE.md"); err != nil || string(data) != "edited\n" {
		t.Errorf("working tree read = %q, %v", data, err)
	}
	if !src.Executable(".claude/hooks/check.sh") || src.Executable("CLAUDE.md") {
		t.Error("executable bits not taken from disk")
	}
	// The commit view is unaffected by working-tree edits.
	committed, err := Commit(context.Background(), head, "-C", root)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := committed.ReadFile("CLAUDE.md"); string(data) != "@docs/rules.md\n" {
		t.Errorf("commit read = %q", data)
	}
}

func TestCommitSkipsSubmodules(t *testing.T) {
	root, _ := fixture(t)
	sub := git(t, root, "rev-parse", "HEAD") // any commit id serves as a gitlink target
	git(t, root, "update-index", "--add", "--cacheinfo", "160000,"+sub+",vendor/lib")
	git(t, root, "commit", "-q", "-m", "add gitlink")
	src, err := Commit(context.Background(), "HEAD", "-C", root)
	if err != nil {
		t.Fatal(err)
	}
	if Has(src, "vendor/lib") {
		t.Error("a submodule is not a file")
	}
}

func TestWorkingTreeSymlinksNeverReachPersonalFiles(t *testing.T) {
	root, _ := fixture(t)
	personal := filepath.Join(t.TempDir(), "personal-CLAUDE.md")
	write(t, filepath.Dir(personal), filepath.Base(personal), "my private rules\n", 0o644)
	write(t, root, "CLAUDE.local.md", "ignored personal rules\n", 0o644)
	if err := os.Remove(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(personal, filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("CLAUDE.local.md", filepath.Join(root, "RULES.md")); err != nil {
		t.Fatal(err)
	}
	src, err := WorkingTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"CLAUDE.md", "RULES.md"} {
		if data, err := src.ReadFile(p); err == nil || !strings.Contains(err.Error(), "outside the repository") {
			t.Errorf("%s read %q, err %v; want a refusal", p, data, err)
		}
	}
	if data, err := src.ReadFile("AGENTS.md"); err != nil || string(data) != "rules\n" {
		t.Errorf("an in-repository symlink should read: %q, %v", data, err)
	}
	if _, err := src.ReadFile("CLAUDE.local.md"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ignored file: err = %v, want ErrNotExist", err)
	}
}

func TestLinkReportsStoredTargetsWithoutFollowingThem(t *testing.T) {
	root, head := fixture(t)
	committed, err := Commit(context.Background(), head, "-C", root)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := WorkingTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []Source{committed, tree} {
		cases := []struct {
			path, target string
			link         bool
		}{
			{"AGENTS.md", "docs/rules.md", true},
			{"docs/escape.md", "../../outside.md", true}, // reported as stored, never opened
			{"CLAUDE.md", "", false},
			{"docs", "", false},       // a folder is not a file of the source
			{"missing.md", "", false}, // nor is a missing path
		}
		for _, c := range cases {
			target, ok, err := Link(src, c.path)
			if err != nil || ok != c.link || target != c.target {
				t.Errorf("%s: Link(%s) = %q, %v, %v; want %q, %v", src.Describe(), c.path, target, ok, err, c.target, c.link)
			}
		}
	}
}
