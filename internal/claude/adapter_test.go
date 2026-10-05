package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/sandbox"
)

// The adapter is Claude Code's own code behind the seam: the same command, environment and denied paths as Invocation's
// methods, for every sign-in mode and a module run, and its records name Claude Code.
func TestAdapterIsInvocationsOwn(t *testing.T) {
	a := Adapter{}
	if a.Name() != agent.ClaudeCode {
		t.Errorf("name %q", a.Name())
	}
	module := invocation(t, SignInLogin, "")
	module.Repo, module.Dir, module.TempRoot, module.UID = "/work/runs/r1/repo", "/work/runs/r1/repo/svc", "/t/ag-1", 501
	for name, inv := range map[string]Invocation{"login": invocation(t, SignInLogin, ""), "api key": invocation(t, SignInAPIKey, "k"),
		"token file": invocation(t, SignInTokenFile, "tok"), "module": module} {
		args, env, err := inv.Command(parentEnv)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := a.Command(agent.Invocation(inv), parentEnv)
		if err != nil || !slices.Equal(got.Args, args) || !slices.Equal(got.Env, env) || got.Stdin != "" || got.Dirs != nil || got.Exclusive != "" || got.Watch != nil {
			t.Errorf("%s: the adapter's command differs: %v", name, err)
		}
		if !slices.Equal(a.DeniedPaths(agent.Invocation(inv), parentEnv), inv.DeniedPaths(parentEnv)) {
			t.Errorf("%s: the adapter's denied paths differ", name)
		}
	}
	// A refusal comes through too.
	bad := invocation(t, SignInAPIKey, "")
	if _, err := a.Command(agent.Invocation(bad), parentEnv); err == nil {
		t.Error("an API key sign-in without a key was not refused")
	}
}

// Parse (of the records' stream.jsonl), Classify and Check through the adapter are the package's own, and Gather has
// nothing to gather.
func TestAdapterReadsTranscripts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "ok.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	records := t.TempDir()
	if err := os.WriteFile(filepath.Join(records, agent.Transcript), data, 0o600); err != nil {
		t.Fatal(err)
	}
	a := Adapter{}
	if err := a.Gather(t.TempDir(), records); err != nil {
		t.Fatal(err)
	}
	m, err := a.Parse(records)
	if err != nil || !m.SawResult {
		t.Fatalf("parsed %+v, %v", m, err)
	}
	if drift := a.Check(m, agent.Expect{}); a.Classify(m, agent.StopNone, drift) != agent.OutcomeOK || len(drift) != 0 {
		t.Errorf("drift %q, outcome %s", drift, a.Classify(m, agent.StopNone, drift))
	}
	if a.Classify(m, agent.StopTimeout, nil) != agent.OutcomeTimeout || a.Classify(m, agent.StopNone, []string{"x"}) != agent.OutcomeUnfair {
		t.Error("the outcome rules differ from Classify's")
	}
	if _, err := a.Parse(t.TempDir()); err == nil {
		t.Error("records without a transcript were read")
	}
}

func TestAdapterVersion(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho '2.1.285 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if v, err := (Adapter{}).Version(context.Background(), cli); err != nil || v != "2.1.285" {
		t.Errorf("version %q, %v", v, err)
	}
	if _, err := (Adapter{}).Version(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a missing CLI: %v", err)
	}
}

// The log folders every Claude Code session of the user shares are denied, for reading and writing, as the sign-in
// decides: npm's logs and ~/.claude/debug always, and the user's own CLAUDE_CONFIG_DIR's debug folder only with the
// user's login (a run with its own config folder writes its own; the user's folder is then denied whole instead).
func TestSharedLogFoldersBySignIn(t *testing.T) {
	environ := append(slices.Clone(parentEnv), "CLAUDE_CONFIG_DIR=/home/u/.claude-work")
	base := []string{"/home/u/.npm/_logs", "/home/u/.claude/debug"}
	for _, c := range []struct {
		name string
		inv  Invocation
		want []string
	}{
		{"api key", invocation(t, SignInAPIKey, "k"), base},
		{"token file", func() Invocation {
			inv := invocation(t, SignInTokenFile, "tok")
			inv.TokenFile = "/home/u/tokens/claude-oauth-token"
			return inv
		}(), base},
		{"login", invocation(t, SignInLogin, ""), append(slices.Clone(base), "/home/u/.claude-work/debug")},
	} {
		args, _, err := c.inv.Command(environ)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var settings struct {
			Sandbox struct {
				Filesystem struct {
					DenyRead  []string `json:"denyRead"`
					DenyWrite []string `json:"denyWrite"`
				} `json:"filesystem"`
			} `json:"sandbox"`
		}
		if err := json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &settings); err != nil {
			t.Fatal(err)
		}
		fs := settings.Sandbox.Filesystem
		// The log folders end both lists, each in every form the sandbox matches (/home is a link on macOS).
		want := sandbox.WithForms(c.want)
		for list, got := range map[string][]string{"denyRead": fs.DenyRead, "denyWrite": fs.DenyWrite} {
			if len(got) < len(want) || !slices.Equal(got[len(got)-len(want):], want) {
				t.Errorf("%s: %s ends %q, want the log folders %q", c.name, list, got[max(0, len(got)-len(want)-1):], want)
			}
			if c.name != "login" && slices.Contains(got, "/home/u/.claude-work/debug") {
				t.Errorf("%s: %s lists the user's config folder's debug folder", c.name, list)
			}
		}
		if c.name != "login" && !slices.Contains(fs.DenyRead, "/home/u/.claude-work") {
			t.Errorf("%s: the user's own config folder is not denied whole", c.name)
		}
	}
}
