package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claudectx"
)

// lintRepoFixture is a committed repository whose context has a broken import.
func lintRepoFixture(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "CLAUDE.md", "# Project\n@docs/missing.md\nRun the tests.\n")
	writeFile(t, repo, ".claude/rules/go.md", "Always gofmt.\n")
	writeFile(t, repo, "main.go", "package main\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "initial")
	return repo
}

// hookIn runs `context lint --hook` with payload on stdin from dir.
func hookIn(t *testing.T, dir, data, payload string) cliResult {
	t.Helper()
	return hookInReader(t, dir, data, strings.NewReader(payload))
}

func hookInReader(t *testing.T, dir, data string, stdin io.Reader) cliResult {
	t.Helper()
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": t.TempDir(), "FORCE_COLOR": "1"}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), Env{
		Args: []string{"context", "lint", "--hook"}, Stdin: stdin, Stdout: &stdout, Stderr: &stderr, Dir: dir,
		Getenv: func(key string) string { return vars[key] },
		Now:    func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) },
	})
	return cliResult{code, stdout.String(), stderr.String()}
}

func editPayload(tool, cwd, file string) string {
	payload, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": tool,
		"tool_input": map[string]any{"file_path": file}, "tool_response": map[string]any{"filePath": file}, "tool_use_id": "toolu_1"})
	return string(payload)
}

// message decodes a hook's stdout, which must be one JSON object with a systemMessage.
func message(t *testing.T, r cliResult) string {
	t.Helper()
	var out struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out.SystemMessage == "" {
		t.Fatalf("hook output is not a systemMessage object: %q (%v)", r.stdout, err)
	}
	return out.SystemMessage
}

func TestContextLintReportsBrokenImportsAndCodexsLimit(t *testing.T) {
	t.Parallel()
	repo := lintRepoFixture(t)
	data := filepath.Join(t.TempDir(), "data")
	run := cliIn(t, repo, data)

	r := run("context", "lint")
	expect(t, r, ExitOK, "(working tree)", "warning: CLAUDE.md imports docs/missing.md, which does not exist.",
		"not registered with Agentium")
	if strings.Contains(r.stdout, "Codex") {
		t.Errorf("a small context is under Codex's limit:\n%s", r.stdout)
	}
	if _, err := os.Stat(data); err == nil {
		t.Error("lint on an unregistered repository must not create the data folder")
	}

	expect(t, run("init"), ExitOK)
	expect(t, run("context", "lint"), ExitOK, "no snapshot yet")
	expect(t, run("context", "snapshot", "first"), ExitOK)
	writeFile(t, repo, ".claude/rules/big.md", strings.Repeat("A long rule line that grows the context.\n", 1000))
	writeFile(t, repo, "AGENTS.md", strings.Repeat("Codex instruction line.\n", 2000))
	expect(t, run("context", "lint", "--ref", "HEAD"), ExitOK, "(commit ", "+0 tokens against snapshot first") // HEAD still equals the snapshot
	r = run("context", "lint")
	expect(t, r, ExitOK, "(working tree)", "tokens against snapshot first", "the AGENTS.md files Codex loads for the repository root total 46.9 KiB; Codex reads only the first 32 KiB (AGENTS.md)", "does not exist")
	if strings.Count(r.stdout, "docs/missing.md") != 1 {
		t.Errorf("a broken import is reported once:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "+0 tokens") {
		t.Errorf("the growth is not shown:\n%s", r.stdout)
	}
	expect(t, run("context", "lint", "--ref", "no-such-ref"), ExitError, "is not a commit")
	expect(t, run("context", "lint", "--hook", "--print-hook"), ExitUsage)
	expect(t, run("context", "lint", "--hook", "--ref", "HEAD"), ExitUsage)
	expect(t, run("context", "lint", "extra"), ExitUsage)
}

func TestContextLintHook(t *testing.T) {
	t.Parallel()
	repo := lintRepoFixture(t)
	data := filepath.Join(t.TempDir(), "data")
	elsewhere := t.TempDir() // the hook runs in another folder than the repository
	claude := filepath.Join(repo, "CLAUDE.md")

	// An unregistered repository: lint without the comparison, and say so in one note.
	got := message(t, hookIn(t, elsewhere, data, editPayload("Edit", repo, claude)))
	for _, want := range []string{"CLAUDE.md changed", "At session start", "docs/missing.md", "not registered with Agentium"} {
		if !strings.Contains(got, want) {
			t.Errorf("unregistered: message lacks %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(data); err == nil {
		t.Error("the hook must not create the data folder")
	}

	run := cliIn(t, repo, data)
	expect(t, run("init"), ExitOK)
	got = message(t, hookIn(t, elsewhere, data, editPayload("Write", repo, claude)))
	if !strings.Contains(got, "no snapshot yet") {
		t.Errorf("no snapshot: message lacks the note:\n%s", got)
	}

	expect(t, run("context", "snapshot", "first"), ExitOK)
	writeFile(t, repo, "CLAUDE.md", "# Project\n"+strings.Repeat("More guidance for the agent.\n", 100))
	got = message(t, hookIn(t, elsewhere, data, editPayload("MultiEdit", repo, claude)))
	if !strings.Contains(got, "against snapshot first") || strings.Contains(got, "+0 tokens") {
		t.Errorf("message lacks the size change:\n%s", got)
	}
	// A relative path resolves against the payload's cwd; a rule and an unsaved new rule are context too.
	got = message(t, hookIn(t, elsewhere, data, editPayload("Edit", repo, "CLAUDE.md")))
	if strings.Contains(got, "\x1b") {
		t.Errorf("hook text must be plain even under FORCE_COLOR:\n%q", got)
	}
	if !strings.Contains(got, "CLAUDE.md changed") {
		t.Errorf("relative path:\n%s", got)
	}
	writeFile(t, repo, ".claude/rules/new.md", "A new rule.\n")
	message(t, hookIn(t, repo, data, editPayload("Write", repo, filepath.Join(repo, ".claude/rules/new.md"))))

	// An imported document is context; an unimported one is not.
	writeFile(t, repo, "CLAUDE.md", "# Project\n@docs/guide.md\n")
	writeFile(t, repo, "docs/guide.md", "A guide.\n")
	writeFile(t, repo, "docs/other.md", "Unrelated.\n")
	message(t, hookIn(t, repo, data, editPayload("Edit", repo, filepath.Join(repo, "docs/guide.md"))))
	// The import is reached by extension, even from a folder IsDocument treats as test data.
	writeFile(t, repo, "CLAUDE.md", "# Project\n@test/README.md\n")
	writeFile(t, repo, "test/README.md", "Testing notes.\n")
	message(t, hookIn(t, repo, data, editPayload("Edit", repo, filepath.Join(repo, "test/README.md"))))
	writeFile(t, repo, "CLAUDE.md", "# Project\n@docs/guide.md\n")

	silent := map[string]string{
		"a document that is not imported": editPayload("Edit", repo, filepath.Join(repo, "docs/other.md")),
		"not a context file":              editPayload("Edit", repo, filepath.Join(repo, "main.go")),
		"a tool that is not an edit":      editPayload("Bash", repo, claude),
		"a file outside any repository":   editPayload("Write", elsewhere, filepath.Join(elsewhere, "CLAUDE.md")),
		"a file that no longer exists":    editPayload("Write", repo, filepath.Join(repo, "gone", "CLAUDE.md")),
		"no file":                         `{"tool_name":"Edit","tool_input":{}}`,
	}
	for name, payload := range silent {
		if r := hookIn(t, repo, data, payload); r.code != ExitOK || r.stdout != "" || r.stderr != "" {
			t.Errorf("%s: want silence and exit 0, got %+v", name, r)
		}
	}

	// A huge payload (a big Write of a source file) is silent, and its stdin is read to the end.
	big := strings.NewReader(`{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(repo, "main.go") + `","content":"` +
		strings.Repeat("x", claudectx.MaxHookPayload+10) + `"}}`)
	if r := hookInReader(t, repo, data, big); r.code != ExitOK || r.stdout != "" || big.Len() != 0 {
		t.Errorf("oversize payload: %+v, %d bytes left unread", r, big.Len())
	}

	for _, bad := range []string{"", "not json", `{"tool_name":`, "[1,2]"} {
		r := hookIn(t, repo, data, bad)
		if r.code != ExitOK {
			t.Errorf("malformed input %q must exit 0, got %d", bad, r.code)
		}
		if note := message(t, r); strings.Contains(note, "\n") || !strings.Contains(note, "skipped") {
			t.Errorf("malformed input %q: want a one-line note, got %q", bad, note)
		}
	}
}

func TestContextLintHookIsFastAndReadOnly(t *testing.T) {
	t.Parallel()
	repo := lintRepoFixture(t)
	data := filepath.Join(t.TempDir(), "data")
	run := cliIn(t, repo, data)
	expect(t, run("init"), ExitOK)
	expect(t, run("context", "snapshot", "first"), ExitOK)
	database := filepath.Join(data, "agentium.db")
	before, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(database + side); err == nil {
			t.Fatalf("precondition: %s exists after the commands ended", side)
		}
	}
	payload := editPayload("Edit", repo, filepath.Join(repo, "CLAUDE.md"))
	// The goal is under a second; the margin is generous so a loaded CI machine does not flake.
	start := time.Now()
	got := message(t, hookIn(t, t.TempDir(), data, payload))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the hook took %v", elapsed)
	}
	if !strings.Contains(got, "against snapshot first") {
		t.Errorf("no comparison:\n%s", got)
	}
	after, err := os.ReadFile(database)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("the hook changed the database file (%v)", err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(database + side); err == nil {
			t.Errorf("the hook left %s behind", side)
		}
	}
}

// Another Agentium command can hold the write lock for a while (a run records results); the hook must not wait for it.
func TestContextLintHookDoesNotWaitForTheWriteLock(t *testing.T) {
	t.Parallel()
	repo := lintRepoFixture(t)
	data := filepath.Join(t.TempDir(), "data")
	run := cliIn(t, repo, data)
	expect(t, run("init"), ExitOK)
	expect(t, run("context", "snapshot", "first"), ExitOK)
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(data, "agentium.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := message(t, hookIn(t, repo, data, editPayload("Edit", repo, filepath.Join(repo, "CLAUDE.md"))))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the hook waited %v for the write lock", elapsed)
	}
	if !strings.Contains(got, "against snapshot first") {
		t.Errorf("the comparison should still work while another process writes:\n%s", got)
	}
}

func TestContextLintHookFollowsASymlinkedInstructionFile(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "docs/real.md", "Shared rules.\n")
	writeFile(t, repo, "AGENTS.md", "Instructions.\n")
	writeFile(t, repo, ".claude/rules/shared.md", "")
	if err := os.Remove(filepath.Join(repo, ".claude/rules/shared.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../docs/real.md", filepath.Join(repo, ".claude/rules/shared.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "initial")
	for _, file := range []string{"AGENTS.md", "docs/real.md"} {
		got := message(t, hookIn(t, t.TempDir(), filepath.Join(t.TempDir(), "data"), editPayload("Edit", repo, filepath.Join(repo, file))))
		if !strings.Contains(got, file+" changed") {
			t.Errorf("editing %s, the target of a symbolic link in the context, was silent or wrong:\n%s", file, got)
		}
	}
}

func TestContextLintPrintHook(t *testing.T) {
	t.Parallel()
	r := cliIn(t, t.TempDir(), t.TempDir())("context", "lint", "--print-hook")
	expect(t, r, ExitOK, "Merge this object", "never writes your settings", "project settings only", "never fires inside them")
	var settings struct {
		Hooks struct {
			PostToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"PostToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &settings); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}
	entries := settings.Hooks.PostToolUse
	if len(entries) != 1 || entries[0].Matcher != "Edit|Write|MultiEdit" || len(entries[0].Hooks) != 1 ||
		entries[0].Hooks[0].Type != "command" || entries[0].Hooks[0].Timeout != 5 ||
		!filepath.IsAbs(strings.Trim(strings.TrimSuffix(entries[0].Hooks[0].Command, " context lint --hook"), "'")) ||
		!strings.HasSuffix(entries[0].Hooks[0].Command, " context lint --hook") {
		t.Errorf("unexpected hook settings: %+v", entries)
	}
}
