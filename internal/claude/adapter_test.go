package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
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
		gotArgs, gotEnv, err := a.Command(agent.Invocation(inv), parentEnv)
		if err != nil || !slices.Equal(gotArgs, args) || !slices.Equal(gotEnv, env) {
			t.Errorf("%s: the adapter's command differs: %v", name, err)
		}
		if !slices.Equal(a.DeniedPaths(agent.Invocation(inv), parentEnv), inv.DeniedPaths(parentEnv)) {
			t.Errorf("%s: the adapter's denied paths differ", name)
		}
	}
	// A refusal comes through too.
	bad := invocation(t, SignInAPIKey, "")
	if _, _, err := a.Command(agent.Invocation(bad), parentEnv); err == nil {
		t.Error("an API key sign-in without a key was not refused")
	}
}

// Parse, Classify and Check through the adapter are the package's own.
func TestAdapterReadsTranscripts(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "ok.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a := Adapter{}
	m, err := a.Parse(f)
	if err != nil || !m.SawResult {
		t.Fatalf("parsed %+v, %v", m, err)
	}
	if drift := a.Check(m, agent.Expect{}); a.Classify(m, false, drift) != agent.OutcomeOK || len(drift) != 0 {
		t.Errorf("drift %q, outcome %s", drift, a.Classify(m, false, drift))
	}
	if a.Classify(m, true, nil) != agent.OutcomeTimeout || a.Classify(m, false, []string{"x"}) != agent.OutcomeUnfair {
		t.Error("the outcome rules differ from Classify's")
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
