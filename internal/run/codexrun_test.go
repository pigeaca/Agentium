package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/codex"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/judge"
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

// Codex's home: Agentium's shared login home, which every agent's run is denied (it holds the sign-in), even before it
// exists (the user may sign in while a run is going), or the run's own with an API key.
func TestCodexHomeAndItsDenial(t *testing.T) {
	f := newModuleOnce(t, "", "decoy", "")
	ws := filepath.Join(f.env.Layout.Workspaces, "r1")
	if got := codexHomeOf(f.env.Layout, codex.SignInLogin, ws); got != filepath.Join(f.env.Layout.Root, "codex") {
		t.Errorf("login home %s", got)
	}
	if got := codexHomeOf(f.env.Layout, codex.SignInAPIKey, ws); got != filepath.Join(ws, "codex-home") {
		t.Errorf("API key home %s", got)
	}
	for _, exists := range []bool{false, true} {
		if exists {
			must(t, os.MkdirAll(f.env.Layout.CodexHome(), 0o700))
		}
		denied, err := f.env.denied(context.Background(), ws)
		must(t, err)
		if !slices.Contains(denied, f.env.Layout.CodexHome()) {
			t.Errorf("the shared Codex home (existing: %v), with the ChatGPT sign-in, is readable to the agent (Claude Code's runs included)", exists)
		}
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
	if interrupted.Spend().AgentUSD != codex.Bound("gpt-6.1-sol", 3) || !interrupted.CostEstimated || !strings.Contains(strings.Join(interrupted.Notes, " "), "the cap and one more request") {
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
	if got := orphans[0].Record; got.Spend().AgentUSD != codex.Bound("gpt-6.1-sol", 2.5) || !got.CostEstimated {
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
	orphans, err := RecoverWarn(context.Background(), layout, func(string) (bool, error) { return false, nil },
		[]string{"claude-token-value-2", "codex-key-value-1"}, time.Now(), nil)
	if err != nil || len(orphans) != 1 {
		t.Fatalf("%+v, %v", orphans, err)
	}
	// The returned record is what is stored: it holds neither secret.
	stored, err := json.Marshal(orphans[0].Record)
	must(t, err)
	if strings.Contains(string(stored), "value-1") || strings.Contains(string(stored), "value-2") || !strings.Contains(orphans[0].Record.Metrics.ResultExcerpt, "[REDACTED]") {
		t.Errorf("the recovered record keeps a secret: %s", stored)
	}
	data2, err := os.ReadFile(filepath.Join(dir, "stream.jsonl"))
	must(t, err)
	if strings.Contains(string(data2), "value-1") || strings.Contains(string(data2), "value-2") {
		t.Errorf("a secret is left in the records: %s", data2)
	}
}

// redactRecord removes secrets from every text of a record, through pointers, slices and maps, without changing the
// record it was given.
func TestRedactRecord(t *testing.T) {
	rec := Record{Notes: []string{"a note with s3cr3t-value"}, Drift: []string{"drift s3cr3t-value"},
		Metrics: agent.Metrics{ResultExcerpt: "done; key s3cr3t-value", Commands: []string{"echo s3cr3t-value"},
			// Keys from the transcript: two that redact to one merge (counts add up, lists join, in the keys' order).
			ToolUses:       map[string]int{"tool s3cr3t-value": 2, "tool sk-proj-abcdefghijklmnopqrstuvwx": 3, "x": 1},         // secret-scan: allow
			SubagentModels: map[string][]string{"role s3cr3t-value": {"m1"}, "role sk-proj-abcdefghijklmnopqrstuvwx": {"m2"}}}, // secret-scan: allow
		ContextUse: &ContextUse{}, Judge: &judge.Verdict{Reasons: []string{"because s3cr3t-value"}}}
	clean := redactRecord(rec, "", "s3cr3t-value")
	data, err := json.Marshal(clean)
	must(t, err)
	if strings.Contains(string(data), "s3cr3t") || strings.Contains(string(data), "sk-proj") || strings.Contains(strings.Join(clean.Metrics.Commands, " "), "s3cr3t") {
		t.Errorf("a secret is left: %s", data)
	}
	if clean.Metrics.ToolUses["tool [REDACTED]"] != 5 || clean.Metrics.ToolUses["x"] != 1 ||
		!slices.Equal(clean.Metrics.SubagentModels["role [REDACTED]"], []string{"m1", "m2"}) {
		t.Errorf("map keys: tools %v, subagent models %v", clean.Metrics.ToolUses, clean.Metrics.SubagentModels)
	}
	if !strings.Contains(rec.Judge.Reasons[0], "s3cr3t") || !strings.Contains(rec.Notes[0], "s3cr3t") || rec.Metrics.ToolUses["x"] != 1 {
		t.Error("the record given was changed in place")
	}
}

// A collection left incomplete (a rollout that could not be moved) or a rollout cut short counts what was read, and
// never less than the cap: budgets only go up. Gather leaves the rollout it could not move where it was, and the
// records say the collection is incomplete; a later gather that completes it clears that.
func TestCodexSpendWithAPartialCollection(t *testing.T) {
	records := recordsWith(t, filepath.Join("..", "codex", "testdata", "exec-ok.jsonl"))
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "10", "04")
	must(t, os.MkdirAll(day, 0o700))
	main, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "rollout-ok.jsonl"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(day, "rollout-2026-10-04T14-58-55-00000000-0000-7000-8000-000000000001.jsonl"), main, 0o600))
	sub := strings.Replace(string(main), `"id":"00000000-0000-7000-8000-000000000001"`, `"id":"00000000-0000-7000-8000-0000000000aa"`, 1)
	subFile := filepath.Join(day, "rollout-2026-10-04T14-59-00-00000000-0000-7000-8000-0000000000aa.jsonl")
	must(t, os.WriteFile(subFile, []byte(sub), 0o600))
	// The subagents' folder is a file: the subagent's rollout cannot be moved.
	must(t, os.WriteFile(filepath.Join(records, codex.Subagents), nil, 0o600))
	if err := (codex.Adapter{}).Gather(home, records); err == nil {
		t.Fatal("a partial collection was not reported")
	}
	if _, err := os.Stat(subFile); err != nil {
		t.Errorf("the rollout left behind was deleted: %v", err)
	}
	rec := Record{Agent: codex.Name, Model: "gpt-6.1-sol", CapUSD: 3}
	rec.Metrics, _ = parseRecords(codex.Adapter{}, records)
	codexSpendFallback(&rec)
	if !rec.Metrics.RolloutsIncomplete || rec.Spend().AgentUSD != codex.Bound("gpt-6.1-sol", 3) || !rec.CostEstimated {
		t.Errorf("a partial collection: incomplete %v, $%.3f, estimated %v, notes %q", rec.Metrics.RolloutsIncomplete, rec.Spend().AgentUSD, rec.CostEstimated, rec.Notes)
	}
	// Completed later (the subagents' folder made right): the mark goes, and the spend is what was read.
	must(t, os.Remove(filepath.Join(records, codex.Subagents)))
	must(t, (codex.Adapter{}).Gather(home, records))
	whole := Record{Agent: codex.Name, Model: "gpt-6.1-sol", CapUSD: 3}
	whole.Metrics, _ = parseRecords(codex.Adapter{}, records)
	codexSpendFallback(&whole)
	if whole.Metrics.RolloutsIncomplete || whole.CostEstimated || whole.Metrics.Rollouts != 2 {
		t.Errorf("a completed collection: %+v", whole.Metrics)
	}

	// A rollout cut short (a line too long to read) keeps what was read before it, and the run counts its cap.
	cut := recordsWith(t, filepath.Join("..", "codex", "testdata", "exec-ok.jsonl"))
	must(t, os.WriteFile(filepath.Join(cut, codex.Rollout), append(main, []byte(strings.Repeat("x", 65<<20))...), 0o600))
	m, err := parseRecords(codex.Adapter{}, cut)
	if err == nil || !m.RolloutsIncomplete || m.CostUSD <= 0 {
		t.Fatalf("a rollout cut short: %v, incomplete %v, $%.4f read", err, m.RolloutsIncomplete, m.CostUSD)
	}
	short := Record{Agent: codex.Name, Model: "gpt-6.1-sol", CapUSD: 3, Metrics: m}
	codexSpendFallback(&short)
	if short.Spend().AgentUSD != codex.Bound("gpt-6.1-sol", 3) {
		t.Errorf("a rollout cut short counts $%.3f", short.Spend().AgentUSD)
	}
}

// A crash between recovery removing an API key run's workspace (its Codex home, with a rollout the first gather left
// behind) and recording the run: the next recovery finds no source, and that is no collection. The records' list of
// missing rollouts stays, and the run counts its bound, never what was read alone.
func TestRecoveryKeepsAnIncompleteMarkWithoutTheSources(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
		Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records"), Cache: filepath.Join(data, "cache")}
	dir, workspace := filepath.Join(layout.Records, "r1"), filepath.Join(layout.Workspaces, "r1") // the workspace is gone
	stream, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "exec-ok.jsonl"))
	must(t, err)
	rollout, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "rollout-ok.jsonl"))
	must(t, err)
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "stream.jsonl"), stream, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, codex.Rollout), rollout, 0o600))
	missing := "# Codex rollouts of this run that are not in these records\nrollouts/rollout-2026-10-04T14-59-00-00000000-0000-7000-8000-0000000000aa.jsonl\n"
	must(t, os.WriteFile(filepath.Join(dir, codex.Incomplete), []byte(missing), 0o600))
	rec := Record{ID: "r1", Task: "fix", Arm: "A", Agent: codex.Name, SignIn: codex.SignInAPIKey, Model: "gpt-6.1-sol", CapUSD: 3, RecordsDir: dir}
	must(t, (Env{}).writeStart(start{Record: rec, Workspace: workspace, AgentStarted: true}))
	orphans, err := Recover(context.Background(), layout, func(string) (bool, error) { return false, nil }, "", time.Now())
	if err != nil || len(orphans) != 1 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, codex.Incomplete)); err != nil || !strings.Contains(string(data), "0000000000aa") {
		t.Errorf("the list of missing rollouts was cleared without them: %q, %v", data, err)
	}
	bound := codex.Bound("gpt-6.1-sol", 3)
	if got := orphans[0].Record; got.Spend().AgentUSD != bound || !got.CostEstimated {
		t.Errorf("recovered $%.2f (want the bound $%.2f), estimated %v, notes %q", got.Spend().AgentUSD, bound, got.CostEstimated, got.Notes)
	}
}

// The watcher's mark of lost accounting is in the records before anything else: recovery after a crash counts the
// run's bound even though the rollout it finds parses whole.
func TestRecoveryKeepsLostAccounting(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Database: filepath.Join(data, "agentium.db"), Artifacts: filepath.Join(data, "artifacts"),
		Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records"), Cache: filepath.Join(data, "cache")}
	dir, workspace := filepath.Join(layout.Records, "r1"), filepath.Join(layout.Workspaces, "r1")
	stream, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "exec-interrupted.jsonl"))
	must(t, err)
	rollout, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "rollout-interrupted.jsonl"))
	must(t, err)
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.MkdirAll(workspace, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "stream.jsonl"), stream, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, codex.Rollout), rollout, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, codex.AccountingLost), []byte("the run's rollout went away\n"), 0o600))
	rec := Record{ID: "r1", Task: "fix", Arm: "A", Agent: codex.Name, SignIn: codex.SignInAPIKey, Model: "gpt-6.1-sol", CapUSD: 3, RecordsDir: dir}
	must(t, (Env{}).writeStart(start{Record: rec, Workspace: workspace, AgentStarted: true}))
	orphans, err := Recover(context.Background(), layout, func(string) (bool, error) { return false, nil }, "", time.Now())
	if err != nil || len(orphans) != 1 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	if got, want := orphans[0].Record.Spend().AgentUSD, codex.Bound("gpt-6.1-sol", 3); got != want || !orphans[0].Record.CostEstimated {
		t.Errorf("recovered $%.2f, want the bound $%.2f", got, want)
	}
}

// A list of missing rollouts left empty (an older Agentium cut short while writing it in place) is kept, and the run
// counts its bound: what it listed is unknown. The list is written whole or not at all (a temp file renamed).
func TestAnEmptyMissingListIsKept(t *testing.T) {
	records := recordsWith(t, filepath.Join("..", "codex", "testdata", "exec-ok.jsonl"))
	main, err := os.ReadFile(filepath.Join("..", "codex", "testdata", "rollout-ok.jsonl"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(records, codex.Rollout), main, 0o600))
	must(t, os.WriteFile(filepath.Join(records, codex.Incomplete), nil, 0o600)) // the crash point: truncated, not yet written
	if err := (codex.Adapter{}).Gather(t.TempDir(), records); err == nil {
		t.Error("an empty list was taken as a complete collection")
	}
	if data, err := os.ReadFile(filepath.Join(records, codex.Incomplete)); err != nil || len(data) == 0 {
		t.Errorf("the list: %q, %v", data, err)
	}
	rec := Record{Agent: codex.Name, Model: "gpt-6.1-sol", CapUSD: 3}
	rec.Metrics, _ = parseRecords(codex.Adapter{}, records)
	codexSpendFallback(&rec)
	if rec.Spend().AgentUSD != codex.Bound("gpt-6.1-sol", 3) {
		t.Errorf("$%.3f", rec.Spend().AgentUSD)
	}
	if strays, _ := filepath.Glob(filepath.Join(records, "."+codex.Incomplete+".tmp-*")); len(strays) != 0 {
		t.Errorf("temp files left: %v", strays)
	}
}
