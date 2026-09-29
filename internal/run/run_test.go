package run

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

func TestOnlyTheRunsOwnSessionIsExempt(t *testing.T) {
	config := t.TempDir()
	own := filepath.Join(config, "projects", "-data-workspaces-r1-repo")
	paths := []string{
		filepath.Join(own, "tool-results", "out.txt"),                                 // the run's own saved output
		filepath.Join(config, "projects", "-work-old", "transcript.jsonl"),            // a past session
		filepath.Join(config, "projects", "-data-workspaces-r2-repo", "tool-results"), // another run, created meanwhile
	}
	kept := ownSessionExcluded(paths, own)
	if len(kept) != 2 || kept[0] != paths[1] || kept[1] != paths[2] {
		t.Errorf("kept = %v: only the run's own session folder is exempt", kept)
	}
	if got := union([]string{"b", "a"}, []string{"a", "c"}); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("union = %v", got)
	}
}

func TestRecover(t *testing.T) {
	layout, err := home.Resolve(func(key string) string {
		return map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data")}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	started := func(id string, agent bool, pgid int, workspace string) {
		env := Env{Meta: []byte(`{"slot":4}`)}
		rec := Record{ID: id, Task: "fix", Arm: "B", RecordsDir: filepath.Join(layout.Records, id)}
		if err := os.MkdirAll(rec.RecordsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := env.writeStart(rec, workspace, agent, pgid); err != nil {
			t.Fatal(err)
		}
	}
	// r1: records but no start file. r2: prepared, the agent never started. r3: started, killed before its result.
	// r4: stored already.
	write(filepath.Join(layout.Records, "r1", "setup.log"), "")
	started("r2", false, 0, filepath.Join(layout.Workspaces, "r2"))
	write(filepath.Join(layout.Workspaces, "r2", "repo", "a.txt"), "")
	started("r3", true, 999999, filepath.Join(layout.Workspaces, "e1-s4-t1"))
	write(filepath.Join(layout.Workspaces, "e1-s4-t1", "repo", "a.txt"), "")
	write(filepath.Join(layout.Records, "r3", "verify", "a.txt"), "")
	write(filepath.Join(layout.Records, "r3", "stream.jsonl"), `{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5"}
{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":1000,"cache_creation_input_tokens":20000},"content":[]}}
`)
	started("r4", true, 0, filepath.Join(layout.Workspaces, "r4"))
	stored := func(id string) (bool, error) { return id == "r4", nil }

	orphans, err := Recover(layout, stored, "", now)
	if err != nil || len(orphans) != 1 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	o := orphans[0].Record
	if o.ID != "r3" || o.Outcome != "cancelled" || o.Passed != nil || o.Metrics.CostUSD != 0.082 || string(orphans[0].Meta) != `{"slot":4}` ||
		!strings.Contains(strings.Join(o.Notes, "; "), "estimated from the transcript's requests") {
		t.Errorf("orphan = %+v (meta %s)", o, orphans[0].Meta)
	}
	for _, gone := range []string{filepath.Join(layout.Records, "r1"), filepath.Join(layout.Records, "r2"), filepath.Join(layout.Workspaces, "r2"),
		filepath.Join(layout.Workspaces, "e1-s4-t1"), filepath.Join(layout.Records, "r3", "verify")} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s was left behind", gone)
		}
	}
	for _, kept := range []string{filepath.Join(layout.Records, "r3", "stream.jsonl"), filepath.Join(layout.Records, "r4")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s: %v", kept, err)
		}
	}

	// A start file naming a folder outside the workspaces is not trusted with a removal.
	started("r5", true, 0, layout.Root)
	if _, err := Recover(layout, func(id string) (bool, error) { return id != "r5", nil }, "", now); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("a workspace outside: %v", err)
	}
	os.RemoveAll(filepath.Join(layout.Records, "r5"))

	// An agent whose process group lives on is left alone.
	sleeper := exec.Command("sleep", "30")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-sleeper.Process.Pid, syscall.SIGKILL); sleeper.Wait() }()
	started("r6", true, sleeper.Process.Pid, filepath.Join(layout.Workspaces, "r6"))
	var alive *AliveError
	if _, err := Recover(layout, func(id string) (bool, error) { return id != "r6", nil }, "", now); !errors.As(err, &alive) || len(alive.Runs) != 1 {
		t.Errorf("a live agent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layout.Records, "r6", startFile)); err != nil {
		t.Errorf("a live agent's run was touched: %v", err)
	}
}

func TestPredictedFolders(t *testing.T) {
	layout, _ := home.Resolve(func(key string) string { return map[string]string{"AGENTIUM_HOME": "/data"}[key] })
	login := Env{Layout: layout, SignIn: "login", Home: "/home/u", Environ: []string{"CLAUDE_CONFIG_DIR=/cfg"}}
	if got := login.Predicted("e1-s2-t1"); len(got) != 2 || got[0] != "/data/workspaces/e1-s2-t1" || got[1] != "/cfg/projects/-data-workspaces-e1-s2-t1-repo" {
		t.Errorf("login: %v", got)
	}
	key := Env{Layout: layout, SignIn: "api-key", Home: "/home/u"}
	if got := key.Predicted("e1-s2-t1"); len(got) != 1 {
		t.Errorf("with a key, the session lives in the workspace's own config: %v", got)
	}
}
