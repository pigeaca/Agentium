package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI is a Codex CLI that writes its environment and arguments to a file beside it, prints out and exits with
// code. Like Codex 0.160.0, it prints `login status`'s answer on standard error, the rest on standard output.
func fakeCLI(t *testing.T, out string, code int) (cli, record string) {
	t.Helper()
	dir := t.TempDir()
	cli, record = filepath.Join(dir, "codex"), filepath.Join(dir, "record")
	script := "#!/bin/sh\n{ env; echo \"ARGS=$*\"; echo \"PWD=$(pwd -P)\"; } > " + record + "\nif [ \"$1\" = login ]; then exec 1>&2; fi\nprintf '%s\\n' '" + out + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli, record
}

// The version is read with a Codex home and a home that do not exist, outside any repository: the user's ~/.codex
// is never touched, and no credential of the user's environment reaches the CLI.
func TestVersion(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "parent-openai")
	cli, record := fakeCLI(t, "codex-cli 0.160.0", 0)
	v, err := Version(context.Background(), cli)
	if err != nil || v != "0.160.0" {
		t.Fatalf("version %q, %v", v, err)
	}
	data, _ := os.ReadFile(record)
	env := string(data)
	for _, line := range strings.Split(env, "\n") {
		name, value, _ := strings.Cut(line, "=")
		switch name {
		case "CODEX_HOME", "HOME":
			if _, err := os.Stat(value); err == nil || !strings.Contains(value, "absent") {
				t.Errorf("%s=%s exists", name, value)
			}
		case "OPENAI_API_KEY":
			t.Error("the parent's key reached the CLI")
		}
	}
	if _, err := Version(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing CLI has a version")
	}
	bad, _ := fakeCLI(t, "no version here", 0)
	if _, err := Version(context.Background(), bad); err == nil {
		t.Error("output without a version was read")
	}
}

// Only the minor version the recipe was verified with runs.
func TestCheckVersion(t *testing.T) {
	for v, ok := range map[string]bool{"0.160.0": true, "0.160.3": true, "0.159.9": false, "0.161.0": false, "1.160.0": false, "garbage": false} {
		if err := CheckVersion(v); (err == nil) != ok {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// The login is asked of the CLI (codex login status) in Agentium's own Codex home; a missing home, no login or an API
// key's login refuse, and say how to sign in. What the CLI prints is never repeated.
func TestLoginStatus(t *testing.T) {
	home := t.TempDir()
	cli, record := fakeCLI(t, "Logged in using ChatGPT", 0)
	if err := LoginStatus(context.Background(), cli, home); err != nil {
		t.Fatalf("signed in: %v", err)
	}
	data, _ := os.ReadFile(record)
	if !strings.Contains(string(data), "CODEX_HOME="+home+"\n") || !strings.Contains(string(data), "ARGS=login status\n") {
		t.Errorf("the CLI was asked:\n%s", data)
	}
	for name, c := range map[string]struct {
		out  string
		code int
		home string
	}{
		"not signed in":   {"Not logged in", 1, home},
		"an API key":      {"Logged in using an API key - sk-proj-***abc", 0, home},
		"no Codex home":   {"Logged in using ChatGPT", 0, filepath.Join(home, "missing")},
		"a failing login": {"Logged in using ChatGPT", 2, home},
	} {
		cli, _ := fakeCLI(t, c.out, c.code)
		err := LoginStatus(context.Background(), cli, c.home)
		if !errors.Is(err, ErrNotSignedIn) || !strings.Contains(err.Error(), "codex login") || strings.Contains(err.Error(), "sk-proj") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The calibration's probe goes into the first file of Codex's AGENTS.md chain, from the root down to the start folder,
// where a folder's AGENTS.override.md replaces its AGENTS.md; a link is not followed, and none means "".
func TestProbeFile(t *testing.T) {
	repo := t.TempDir()
	if got := ProbeFile(repo, "svc"); got != "" {
		t.Errorf("no instructions: %q", got)
	}
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a/svc/AGENTS.md")
	if got := ProbeFile(repo, "a/svc"); got != "a/svc/AGENTS.md" {
		t.Errorf("the module's: %q", got)
	}
	if got := ProbeFile(repo, ""); got != "" {
		t.Errorf("at the root, a module's file does not load: %q", got)
	}
	write("AGENTS.md")
	write("AGENTS.override.md")
	if got := ProbeFile(repo, "a/svc"); got != "AGENTS.override.md" {
		t.Errorf("the root's override: %q", got)
	}
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(repo, "AGENTS.md"), filepath.Join(link, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if got := ProbeFile(link, ""); got != "" {
		t.Errorf("a link: %q", got)
	}
}
