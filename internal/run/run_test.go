package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
)

func TestRedact(t *testing.T) {
	in := "key sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA and ghp_" + strings.Repeat("a", 36) + " and the run's tok-1234 and " + // secret-scan: allow
		"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\nkeep this" // secret-scan: allow (a fake key for the redaction test)
	out := string(Redact([]byte(in), "tok-1234"))
	for _, gone := range []string{"sk-ant-api03", "ghp_aaa", "tok-1234", "abc"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q survived: %s", gone, out)
		}
	}
	if !strings.Contains(out, "keep this") || strings.Count(out, "[REDACTED]") != 4 {
		t.Errorf("redacted = %s", out)
	}
	if string(Redact([]byte("plain"), "")) != "plain" {
		t.Error("an empty secret must not redact everything")
	}
}

func TestMeasureAndBehavior(t *testing.T) {
	var b Behavior
	paths := measure("3\t1\tsrc/a.go\x00-\t-\tlogo.png\x0010\t0\tsrc/a_test.go\x00", &b)
	if b.FilesChanged != 3 || b.LinesAdded != 13 || b.LinesRemoved != 1 || !slices.Equal(paths, []string{"src/a.go", "logo.png", "src/a_test.go"}) {
		t.Errorf("behavior %+v, paths %v", b, paths)
	}
	if !ranTests([]string{"cd x && go test ./pkg/...", "ls"}) || ranTests([]string{"ls", "cat go.test"}) {
		t.Error("test runner detection")
	}
	for _, c := range []string{"python3 -m pytest -q", "npm run test", "pnpm test", "cargo test", "python3 scripts/harness.py check go"} {
		if !ranTests([]string{c}) {
			t.Errorf("%q runs tests", c)
		}
	}
	if !ranChecks([]string{"sh run_tests.sh && echo ok"}, []string{"sh run_tests.sh"}) || ranChecks([]string{"sh other.sh"}, []string{"sh run_tests.sh"}) {
		t.Error("check detection")
	}
}

func TestOutsideReads(t *testing.T) {
	data := t.TempDir()
	workspace := filepath.Join(data, "workspaces", "r1")
	repo := filepath.Join(workspace, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	userRepo := t.TempDir()
	paths := []string{
		filepath.Join(repo, "main.go"), "relative/file.go", "/tmp/scratch.txt", // the run's own, or not watched
		filepath.Join(data, "agentium.db"), filepath.Join(data, "workspaces", "r2", "repo", "x.go"), filepath.Join(userRepo, "secret.go"),
	}
	if n := outsideReads(paths, repo, workspace, []string{data, userRepo}); n != 3 {
		t.Errorf("outside reads = %d, want 3 (the database, another run, the user's repository)", n)
	}
}

func TestCopyTreeKeepsModesAndLinks(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin/run.sh", filepath.Join(src, "run")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dst, "bin", "run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("mode: %v %v", info, err)
	}
	if link, err := os.Readlink(filepath.Join(dst, "run")); err != nil || link != "bin/run.sh" {
		t.Errorf("link = %q, %v", link, err)
	}
	if err := copyTree(src, dst); err == nil {
		t.Error("copying onto an existing folder must fail")
	}
}

func TestDeniedPathsCoverDataRepositoryAndOtherRuns(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
		Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records")}
	for _, dir := range []string{"workspaces/r1", "workspaces/r2"} {
		if err := os.MkdirAll(filepath.Join(data, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The user's repository is a linked worktree: its shared git data lives in the main checkout.
	main := t.TempDir()
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = gitx.Environ(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(main, "init", "-q", "-b", "main")
	git(main, "commit", "-q", "--allow-empty", "-m", "c")
	linked := filepath.Join(t.TempDir(), "linked")
	git(main, "worktree", "add", "-q", linked)
	env := Env{Layout: layout, ProjectRoot: linked, Now: time.Now}
	denied := strings.Join(env.denied(context.Background(), filepath.Join(data, "workspaces", "r1")), "\n")
	mainGit, _ := filepath.EvalSymlinks(filepath.Join(main, ".git"))
	mainCheckout, _ := filepath.EvalSymlinks(main) // another worktree: it can sit at a commit holding the solution
	for _, want := range []string{filepath.Join(data, "projects"), layout.Records, layout.Artifacts, layout.Database + "-wal",
		filepath.Join(data, "workspaces", "r2"), linked, mainGit, mainCheckout + "\n"} {
		if !strings.Contains(denied, want) {
			t.Errorf("%s is not denied:\n%s", want, denied)
		}
	}
	if strings.Contains(denied, filepath.Join(data, "workspaces", "r1")+"\n") || strings.HasSuffix(denied, filepath.Join(data, "workspaces", "r1")) {
		t.Errorf("the run's own workspace is denied:\n%s", denied)
	}
}

func TestInstructionFilesAbove(t *testing.T) {
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	found := instructionFilesAbove(filepath.Join(outer, "data", "workspaces", "r1", "repo"))
	if !slices.Contains(found, filepath.Join(outer, "AGENTS.md")) {
		t.Errorf("found = %v", found)
	}
}

func TestNewIDSortsByTime(t *testing.T) {
	a, err := NewID(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewID(time.Date(2026, 9, 28, 10, 0, 1, 0, time.UTC))
	if !strings.HasPrefix(a, "20260928T100000Z-") || len(a) != len("20260928T100000Z-")+6 || a >= b {
		t.Errorf("ids %q, %q", a, b)
	}
}
