package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// repo creates a git repository with the given files committed.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fakeClaude writes an executable that prints the given version output.
func fakeClaude(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+output+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEnv(values map[string]string, claude string) Env {
	return Env{
		Getenv: func(key string) string { return values[key] },
		LookPath: func(name string) (string, error) {
			if claude == "" {
				return "", errors.New("not found")
			}
			return claude, nil
		},
	}
}

func TestDiscoverFindsCommandsInstructionsAndClaude(t *testing.T) {
	dir := repo(t, map[string]string{
		"go.mod":                          "module x\n",
		"package.json":                    `{"scripts":{"test":"vitest run"}}`,
		"pnpm-lock.yaml":                  "lockfileVersion: 9\n",
		"pyproject.toml":                  "[tool.pytest.ini_options]\n",
		"Makefile":                        "build:\n\techo\ntest:\n\tgo test ./...\n",
		"scripts/harness.py":              "",
		"CLAUDE.md":                       "@AGENTS.md\n",
		"AGENTS.md":                       "# Rules\n",
		"CLAUDE.local.md":                 "personal\n",
		".claude/skills/review/SKILL.md":  "---\nname: review\n---\n",
		".claude/rules/go.md":             "rule\n",
		".claude/skills/notaskill/README": "",
	})
	claude := fakeClaude(t, "2.1.281 (Claude Code)")
	info, err := Discover(context.Background(), filepath.Join(dir, "scripts"), testEnv(map[string]string{"HOME": t.TempDir()}, claude))
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := filepath.EvalSymlinks(dir)
	if info.Root != wantRoot || len(info.Head) != 40 {
		t.Errorf("root/head = %q/%q", info.Root, info.Head)
	}
	wantCommands := []string{"go test ./...", "pnpm test", "python3 -m pytest", "make test", "python3 scripts/harness.py check ci"}
	if !reflect.DeepEqual(info.TestCommands, wantCommands) {
		t.Errorf("test commands = %q, want %q", info.TestCommands, wantCommands)
	}
	wantFiles := []File{{Path: "CLAUDE.md", Bytes: 11}, {Path: "AGENTS.md", Bytes: 8}}
	if !reflect.DeepEqual(info.Instructions, wantFiles) || info.Skills != 1 || info.Rules != 1 {
		t.Errorf("instructions = %+v, skills %d, rules %d", info.Instructions, info.Skills, info.Rules)
	}
	if info.Claude != (ClaudeInfo{Path: claude, Version: "2.1.281", SignIn: SignInLogin}) {
		t.Errorf("claude = %+v", info.Claude)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("unexpected warnings %q", info.Warnings)
	}
}

func TestSignInModesUsePresenceOnly(t *testing.T) {
	dir := repo(t, map[string]string{"CLAUDE.md": "x\n"})
	claude := fakeClaude(t, "2.1.281 (Claude Code)")
	home := t.TempDir()
	info, err := Discover(context.Background(), dir, testEnv(map[string]string{"HOME": home, "ANTHROPIC_API_KEY": "sk-test-value"}, claude))
	if err != nil {
		t.Fatal(err)
	}
	if info.Claude.SignIn != SignInAPIKey {
		t.Errorf("sign-in = %q, want api-key", info.Claude.SignIn)
	}
	stored, _ := info.JSON()
	if strings.Contains(string(stored), "sk-test-value") {
		t.Error("the API key value leaked into the stored discovery")
	}
	tokenFile := filepath.Join(home, ".config", "agentium", "claude-oauth-token")
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("secret-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ = Discover(context.Background(), dir, testEnv(map[string]string{"HOME": home}, claude))
	if info.Claude.SignIn != SignInTokenFile {
		t.Errorf("sign-in = %q, want token-file", info.Claude.SignIn)
	}
}

func TestWarnings(t *testing.T) {
	dir := repo(t, nil)
	info, err := Discover(context.Background(), dir, testEnv(map[string]string{"HOME": t.TempDir()}, ""))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(info.Warnings, "\n")
	for _, want := range []string{"Claude Code not found", "No test command", "No CLAUDE.md or AGENTS.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q lack %q", info.Warnings, want)
		}
	}
	old := fakeClaude(t, "2.1.132 (Claude Code)")
	info, _ = Discover(context.Background(), dir, testEnv(map[string]string{"HOME": t.TempDir(), "AGENTIUM_CLAUDE": old}, ""))
	if info.Claude.Path != old || !strings.Contains(strings.Join(info.Warnings, "\n"), "older than "+MinClaudeVersion) {
		t.Errorf("old version not flagged: %+v %q", info.Claude, info.Warnings)
	}
}

func TestDiscoverRejectsNonRepositoriesAndEmptyRepos(t *testing.T) {
	if _, err := Discover(context.Background(), t.TempDir(), testEnv(nil, "")); err == nil || !strings.Contains(err.Error(), "not inside a git repository") {
		t.Errorf("err = %v", err)
	}
	empty := t.TempDir()
	git(t, empty, "init", "-q")
	if _, err := Discover(context.Background(), empty, testEnv(nil, "")); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Errorf("err = %v", err)
	}
}

func TestOlderThan(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{{"2.1.258", "2.1.259", true}, {"2.1.259", "2.1.259", false}, {"2.2.0", "2.1.259", false}, {"2.1", "2.1.0", true}} {
		if got := olderThan(tt.a, tt.b); got != tt.want {
			t.Errorf("olderThan(%s, %s) = %v", tt.a, tt.b, got)
		}
	}
}

func TestNpmPlaceholderIsNotATestCommand(t *testing.T) {
	dir := repo(t, map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`})
	if commands := testCommands(dir); len(commands) != 0 {
		t.Errorf("commands = %q", commands)
	}
}
