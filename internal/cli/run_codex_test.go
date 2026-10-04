package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCodex is a Codex CLI as a shell script: version (codex-cli VERSION), `login status` (signed in to ChatGPT, or
// not), and exec: it starts a thread, writes the session's rollout into CODEX_HOME as Codex does (the model and effort
// it was given), makes the task's change, answers a calibration's sandbox check, and completes the turn.
func fakeCodex(t *testing.T, version string, signedIn bool) string {
	t.Helper()
	login := `echo "Not logged in" >&2; exit 1` // on standard error, as Codex 0.160.0 prints it
	if signedIn {
		login = `echo "Logged in using ChatGPT" >&2; exit 0`
	}
	script := `#!/bin/sh
case "$1" in
--version) echo "codex-cli ` + version + `"; exit 0 ;;
login) ` + login + ` ;;
esac
prompt=$(cat)
model= effort= prev=
for a in "$@"; do
	[ "$prev" = "-m" ] && model=$a
	case "$a" in model_reasoning_effort=*) effort=$(printf '%s' "${a#model_reasoning_effort=}" | tr -d '"') ;; esac
	prev=$a
done
thread=00000000-0000-7000-8000-00000000c0de
dir="$CODEX_HOME/sessions/2026/10/04"
mkdir -p "$dir"
f="$dir/rollout-2026-10-04T15-00-00-$thread.jsonl"
printf '{"type":"session_meta","payload":{"id":"%s","cwd":"%s","cli_version":"0.160.0","timestamp":"2026-10-04T15:00:00.000Z"}}\n' "$thread" "$(pwd -P)" > "$f"
printf '{"type":"turn_context","payload":{"model":"%s","effort":"%s","approval_policy":"never","sandbox_policy":{"type":"workspace-write","network_access":false},"active_permission_profile":{"id":"agentium"}}}\n' "$model" "$effort" >> "$f"
printf '{"type":"token_usage_record","payload":{"usage":{"input_tokens":20000,"cached_input_tokens":10000,"cache_write_input_tokens":0,"output_tokens":500,"reasoning_output_tokens":100}}}\n' >> "$f"
printf '{"type":"thread.started","thread_id":"%s"}\n' "$thread"
case "$prompt" in
*agentium-sandbox-ok*)
	printf '{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -c \\"printf agentium-sandbox-ok\\"","aggregated_output":"agentium-sandbox-ok\\n","exit_code":0,"status":"completed"}}\n'
	printf '{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"SANDBOX=agentium-sandbox-ok CODEWORD=NONE"}}\n' ;;
*)
	printf 'new\n' > value.txt
	printf '{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"%s/value.txt","kind":"update"}],"status":"completed"}}\n' "$(pwd -P)" ;;
esac
printf '{"type":"turn.completed","usage":{"input_tokens":20000,"cached_input_tokens":10000,"cache_write_input_tokens":0,"output_tokens":500,"reasoning_output_tokens":100}}\n'
`
	cli := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli
}

// run once --agent codex: refused until Agentium's own Codex home is signed in (and never signing in itself), for a
// Codex other than the supported version, for a cap below one full-context request, and for an unknown agent; then a
// run, graded, priced by Agentium, with Codex's default model and effort.
func TestRunOnceWithCodex(t *testing.T) {
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CODEX"] = fakeCodex(t, "0.160.0", true)
	expect(t, f.run(context.Background(), "run", "once", "value", "--agent", "codex"), ExitError, "codex login", filepath.Join(f.data, "codex"))
	expect(t, f.run(context.Background(), "run", "once", "value", "--agent", "copilot"), ExitUsage, `--agent "copilot": use claude or codex`)
	if err := os.MkdirAll(filepath.Join(f.data, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.vars["AGENTIUM_CODEX"] = fakeCodex(t, "0.160.0", false)
	expect(t, f.run(context.Background(), "run", "once", "value", "--agent", "codex"), ExitError, "not signed in to ChatGPT")
	f.vars["AGENTIUM_CODEX"] = fakeCodex(t, "0.159.0", true)
	expect(t, f.run(context.Background(), "run", "once", "value", "--agent", "codex"), ExitError, "runs Codex 0.160 only")
	f.vars["AGENTIUM_CODEX"] = fakeCodex(t, "0.160.0", true)
	expect(t, f.run(context.Background(), "run", "once", "value", "--agent", "codex", "--budget", "1"), ExitError, "too small for Codex")

	result := f.run(context.Background(), "run", "once", "value", "--agent", "codex")
	expect(t, result, ExitOK, "Starting a real Codex run (gpt-6.1-sol, sign-in login, graded on the host)", "priced by Agentium",
		"outcome      ok; verification passed", "environment  Codex 0.160.0, gpt-6.1-sol, effort low, sandbox workspace-write, permission profile agentium",
		"tokens       10000 input, 10000 cached, 500 output (100 reasoning); cost priced by Agentium at the list prices of 2026-10-04")
	doc := jsonRun(t, f, ExitOK, "run", "once", "value", "--agent", "codex", "--model", "gpt-6.1-sol:high")
	for key, want := range map[string]any{"agent": "codex", "model": "gpt-6.1-sol", "effort": "high", "sign_in": "login", "outcome": "ok",
		"cost_source": "priced by Agentium", "price_table": "2026-10-04", "cli_version": "0.160.0"} {
		if got := doc.get("run", key); got != want {
			t.Errorf("run.%s = %v, want %v", key, got, want)
		}
	}
	// (20,000 − 10,000) × $2 + 10,000 × $0.20 + 500 × $10, per million
	if cost := doc.get("run", "cost_usd").(float64); cost < 0.026999 || cost > 0.027001 {
		t.Errorf("cost $%.6f", cost)
	}
	if input := doc.get("run", "tokens", "input").(float64); input != 10000 {
		t.Errorf("tokens.input %v", input)
	}
	// Its sessions left the shared home.
	if entries, _ := os.ReadDir(filepath.Join(f.data, "codex", "sessions", "2026", "10", "04")); len(entries) != 0 {
		t.Errorf("%d session(s) left in the shared home", len(entries))
	}
}

// With an API key in Agentium's environment, a Codex run uses it, needs no login, and the key is in no record.
func TestRunOnceWithCodexAndAnAPIKey(t *testing.T) {
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CODEX"] = fakeCodex(t, "0.160.0", false)
	f.vars["CODEX_API_KEY"] = "codex-key-for-the-test-not-real"
	result := f.run(context.Background(), "run", "once", "value", "--agent", "codex")
	expect(t, result, ExitOK, "sign-in api-key", "outcome      ok")
	if strings.Contains(result.stdout+result.stderr, "codex-key-for-the-test") {
		t.Error("the key was printed")
	}
	records, _ := filepath.Glob(filepath.Join(f.data, "records", "*", "*"))
	for _, p := range records {
		if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), "codex-key-for-the-test") {
			t.Errorf("the key is in %s", p)
		}
	}
}

// run calibrate --agent codex calibrates Codex (its own prompt, no large-output check), with a cap above the
// allowance; a Claude Code run is not checked against a Codex calibration, a Codex run is.
func TestRunCalibrateWithCodex(t *testing.T) {
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	f.vars["AGENTIUM_CODEX"] = fakeCodex(t, "0.160.0", true)
	if err := os.MkdirAll(filepath.Join(f.data, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(context.Background(), "run", "calibrate", "--agent", "codex"), ExitOK,
		"with real Codex runs (gpt-6.1-sol, sign-in login): up to $2.50 each", "Codex 0.160.0, gpt-6.1-sol")
	expect(t, f.run(context.Background(), "run", "once", "value", "--agent", "codex"), ExitOK, "Checking the environment against the calibration of base")
	f.vars["AGENTIUM_CLAUDE"] = scriptedAgent(t, "printf 'new\\n' > value.txt", false)
	expect(t, f.run(context.Background(), "run", "once", "value"), ExitOK, "arm base is not calibrated for this agent", "outcome      ok")
}
