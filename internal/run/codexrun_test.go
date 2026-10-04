package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/codex"
	"github.com/pigeaca/agentium/internal/home"
)

// A Codex run whose Agentium process died is recovered with its spend: its session's rollout is still in the Codex
// home (the shared login home, or with an API key the run's own, in the workspace recovery removes), so recovery
// gathers it into the records first, without the account's fields, and prices it. The run is cancelled, its cost
// priced by Agentium.
func TestRecoverACodexRun(t *testing.T) {
	for _, signIn := range []string{codex.SignInLogin, codex.SignInAPIKey} {
		t.Run(signIn, func(t *testing.T) {
			data := t.TempDir()
			layout := home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
				Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records"), Cache: filepath.Join(data, "cache")}
			dir, workspace := filepath.Join(layout.Records, "r1"), filepath.Join(layout.Workspaces, "r1")
			stream, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "exec-interrupted.jsonl"))
			must(t, err)
			rollout, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "rollout-interrupted.jsonl"))
			must(t, err)
			rollout = []byte(strings.Replace(string(rollout), `"cli_version"`, `"creator_account_id": "account-private", "cli_version"`, 1))
			codexHome := codexHomeOf(layout, signIn, workspace)
			session := filepath.Join(codexHome, "sessions", "2026", "10", "04", "rollout-2026-10-04T14-56-19-00000000-0000-7000-8000-000000000001.jsonl")
			for p, body := range map[string][]byte{filepath.Join(dir, "stream.jsonl"): stream, session: rollout, filepath.Join(workspace, "repo", "a.go"): nil} {
				must(t, os.MkdirAll(filepath.Dir(p), 0o700))
				must(t, os.WriteFile(p, body, 0o600))
			}
			rec := Record{ID: "r1", Task: "fix", Arm: "A", Agent: codex.Name, SignIn: signIn, Model: "gpt-6.1-sol", RecordsDir: dir}
			must(t, (Env{}).writeStart(start{Record: rec, Workspace: workspace, AgentStarted: true}))
			orphans, err := Recover(context.Background(), layout, func(string) (bool, error) { return false, nil }, "", time.Now())
			if err != nil || len(orphans) != 1 {
				t.Fatalf("Recover = %+v, %v", orphans, err)
			}
			got := orphans[0].Record
			if got.Outcome != agent.OutcomeCancelled || got.Metrics.CostUSD < 0.036 || got.Metrics.CostUSD > 0.037 || got.Metrics.Turns != 5 ||
				got.CostSource != CostPricedByAgentium || got.PriceTable != "2026-10-04" {
				t.Errorf("recovered %s, $%.4f, %d request(s), cost source %q", got.Outcome, got.Metrics.CostUSD, got.Metrics.Turns, got.CostSource)
			}
			if _, err := os.Stat(session); err == nil {
				t.Error("the session was left in the Codex home")
			}
			if data, err := os.ReadFile(filepath.Join(dir, codex.Rollout)); err != nil || strings.Contains(string(data), "account-private") {
				t.Errorf("the gathered rollout: %v, account kept: %v", err, strings.Contains(string(data), "account-private"))
			}
			if _, err := os.Stat(workspace); err == nil {
				t.Error("the workspace was left")
			}
		})
	}
}

// Codex's home: Agentium's shared login home, which every agent's run is denied once it exists (it holds the
// sign-in), or the run's own with an API key.
func TestCodexHomeAndItsDenial(t *testing.T) {
	f := newModuleOnce(t, "", "decoy", "")
	ws := filepath.Join(f.env.Layout.Workspaces, "r1")
	if got := codexHomeOf(f.env.Layout, codex.SignInLogin, ws); got != filepath.Join(f.env.Layout.Root, "codex") {
		t.Errorf("login home %s", got)
	}
	if got := codexHomeOf(f.env.Layout, codex.SignInAPIKey, ws); got != filepath.Join(ws, "codex-home") {
		t.Errorf("API key home %s", got)
	}
	denied, err := f.env.denied(context.Background(), ws)
	must(t, err)
	for _, p := range denied {
		if p == f.env.Layout.CodexHome() {
			t.Error("a Codex home that does not exist is denied (Claude Code's runs would change)")
		}
	}
	must(t, os.MkdirAll(f.env.Layout.CodexHome(), 0o700))
	denied, err = f.env.denied(context.Background(), ws)
	must(t, err)
	found := false
	for _, p := range denied {
		found = found || p == f.env.Layout.CodexHome()
	}
	if !found {
		t.Error("the shared Codex home, with the ChatGPT sign-in, is readable to the agent (Claude Code's runs included)")
	}
}

// A Codex calibration's checks come from the shell commands its stream shows: the sandboxed command's output, and the
// codeword in the answer unless a command read the probe file or printed the codeword. There is no large-output check.
func TestCodexCalibrationChecks(t *testing.T) {
	zero, one := 0, 1
	ok := []codex.CommandResult{{Command: "/bin/zsh -c \"printf 'agentium-sandbox-ok\\n'\"", Output: "agentium-sandbox-ok\n", ExitCode: &zero}}
	answer := "SANDBOX=agentium-sandbox-ok CODEWORD=AGENTIUM-ABC123"
	for name, c := range map[string]struct {
		commands                     []codex.CommandResult
		answer, probe                string
		sandbox, large, instructions string
	}{
		"healthy":        {ok, answer, "AGENTS.md", checkOK, checkNA, checkOK},
		"no probe file":  {ok, "SANDBOX=agentium-sandbox-ok CODEWORD=NONE", "", checkOK, checkNA, checkNA},
		"a failed shell": {[]codex.CommandResult{{Command: "printf 'agentium-sandbox-ok\\n'", Output: "denied", ExitCode: &one}}, answer, "AGENTS.md", checkFailed, checkNA, checkOK},
		"read the probe": {append(ok, codex.CommandResult{Command: "cat AGENTS.md", Output: "Calibration codeword: AGENTIUM-ABC123", ExitCode: &zero}), answer, "AGENTS.md", checkOK, checkNA, checkUnverified},
		"a wrong answer": {ok, "SANDBOX=agentium-sandbox-ok CODEWORD=NONE", "AGENTS.md", checkOK, checkNA, checkFailed},
	} {
		s, l, i := codexChecks(c.commands, c.answer, "AGENTIUM-ABC123", c.probe)
		if s != c.sandbox || l != c.large || i != c.instructions {
			t.Errorf("%s: %s %s %s, want %s %s %s", name, s, l, i, c.sandbox, c.large, c.instructions)
		}
	}
	healthy := Calibration{Agent: codex.Name, Outcome: agent.OutcomeOK, Sandbox: checkOK, LargeOutput: checkNA, Instructions: checkOK}
	if !healthy.Healthy() {
		t.Error("a Codex calibration without a large-output check is not healthy")
	}
}

// A Codex run's probe goes into the AGENTS.md Codex loads, not the CLAUDE.md Claude Code would.
func TestCodexProbeGoesIntoAgentsMD(t *testing.T) {
	repo := t.TempDir()
	must(t, os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("claude\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("agents\n"), 0o600))
	env := Env{Agent: codex.Adapter{}}
	file, err := env.appendProbe(context.Background(), repo, "Calibration codeword: X")
	if err != nil || file != "AGENTS.md" {
		t.Fatalf("probe file %q, %v", file, err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "AGENTS.md")); string(data) != "agents\n\nCalibration codeword: X\n" {
		t.Errorf("AGENTS.md: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md")); string(data) != "claude\n" {
		t.Errorf("CLAUDE.md changed: %q", data)
	}
}

// A Codex run whose rollout is lost never counts as free: the stream's whole-turn tokens priced at the requested
// model's list prices when the turn completed, else (an interrupted run, whose stream holds no usage) its cap; both
// marked estimated. A run that never started a session spent nothing, and a rollout that was read stands.
func TestCodexSpendWithoutItsRollout(t *testing.T) {
	read := func(fixture string) Record {
		t.Helper()
		rec := Record{Agent: codex.Name, Model: "gpt-6.1-sol", CapUSD: 3}
		m, err := parseRecords(codex.Adapter{}, recordsWith(t, filepath.Join("..", "codex", "testdata", fixture)))
		must(t, err)
		rec.Metrics = m
		codexSpendFallback(&rec)
		return rec
	}
	completed := read("exec-ok.jsonl") // 44,416 input (21,248 cached), 316 output
	if want := (float64(44416-21248)*2 + 21248*0.2 + 316*10) / 1e6; completed.Spend().AgentUSD < want-1e-12 || completed.Spend().AgentUSD > want+1e-12 ||
		!completed.CostEstimated || completed.Metrics.UnpricedRequests != 0 || len(completed.Notes) != 1 || !strings.Contains(completed.Notes[0], "long-context") {
		t.Errorf("a completed turn without its rollout: $%.6f (want $%.6f), estimated %v, notes %q", completed.Spend().AgentUSD, want, completed.CostEstimated, completed.Notes)
	}
	interrupted := read("exec-interrupted.jsonl")
	if interrupted.Spend().AgentUSD != 3 || !interrupted.CostEstimated || !strings.Contains(strings.Join(interrupted.Notes, " "), "$3.00 cap") {
		t.Errorf("an interrupted run without its rollout: $%.2f, estimated %v, notes %q", interrupted.Spend().AgentUSD, interrupted.CostEstimated, interrupted.Notes)
	}
	never := Record{Agent: codex.Name, Model: "gpt-6.1-sol", CapUSD: 3}
	codexSpendFallback(&never)
	if never.Spend().AgentUSD != 0 || never.CostEstimated {
		t.Errorf("a run without a session: $%.2f", never.Spend().AgentUSD)
	}
	gathered := Record{Agent: codex.Name, Model: "gpt-6.1-sol", CapUSD: 3, Metrics: agent.Metrics{SawInit: true, Rollouts: 1}}
	codexSpendFallback(&gathered)
	if gathered.Spend().AgentUSD != 0 || gathered.CostEstimated {
		t.Errorf("a run whose rollout was read, without requests: $%.2f", gathered.Spend().AgentUSD)
	}
}

// Recovering a Codex run whose rollout is gone counts its cap: budgets only ever go up.
func TestRecoverACodexRunWithoutItsRollout(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
		Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records"), Cache: filepath.Join(data, "cache")}
	dir, workspace := filepath.Join(layout.Records, "r1"), filepath.Join(layout.Workspaces, "r1")
	stream, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "exec-interrupted.jsonl"))
	must(t, err)
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.MkdirAll(workspace, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "stream.jsonl"), stream, 0o600))
	rec := Record{ID: "r1", Task: "fix", Arm: "A", Agent: codex.Name, SignIn: codex.SignInAPIKey, Model: "gpt-6.1-sol", CapUSD: 2.5, RecordsDir: dir}
	must(t, (Env{}).writeStart(start{Record: rec, Workspace: workspace, AgentStarted: true}))
	orphans, err := Recover(context.Background(), layout, func(string) (bool, error) { return false, nil }, "", time.Now())
	if err != nil || len(orphans) != 1 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	if got := orphans[0].Record; got.Spend().AgentUSD != 2.5 || !got.CostEstimated {
		t.Errorf("recovered $%.2f, estimated %v, notes %q", got.Spend().AgentUSD, got.CostEstimated, got.Notes)
	}
}

// Recovery redacts a dead run's records of every sign-in secret it is given, Claude Code's and Codex's key alike.
func TestRecoveryRedactsEveryAgentsSecret(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
		Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records"), Cache: filepath.Join(data, "cache")}
	dir, workspace := filepath.Join(layout.Records, "r1"), filepath.Join(layout.Workspaces, "r1")
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.MkdirAll(workspace, 0o700))
	stream := `{"type":"thread.started","thread_id":"00000000-0000-7000-8000-000000000001"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"codex-key-value-1 and claude-token-value-2"}}` + "\n"
	must(t, os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(stream), 0o600))
	rec := Record{ID: "r1", Task: "fix", Arm: "A", Agent: codex.Name, SignIn: codex.SignInAPIKey, Model: "gpt-6.1-sol", CapUSD: 3, RecordsDir: dir}
	must(t, (Env{}).writeStart(start{Record: rec, Workspace: workspace, AgentStarted: true}))
	if _, err := RecoverWarn(context.Background(), layout, func(string) (bool, error) { return false, nil },
		[]string{"claude-token-value-2", "codex-key-value-1"}, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(filepath.Join(dir, "stream.jsonl"))
	must(t, err)
	if strings.Contains(string(data2), "value-1") || strings.Contains(string(data2), "value-2") {
		t.Errorf("a secret is left in the records: %s", data2)
	}
}
