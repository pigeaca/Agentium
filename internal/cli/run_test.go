package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgent writes a stand-in for Claude Code. It records what it saw (its folder, whether the hidden test was
// present, its config folder), optionally edits the checkout, and prints a stream-json transcript whose result text
// includes the sign-in token, so redaction can be checked.
func fakeAgent(t *testing.T, edit, permissionMode, extraTool string) (cli, seen string) {
	t.Helper()
	dir := t.TempDir()
	seen = filepath.Join(dir, "seen")
	script := `#!/bin/sh
{ pwd; test -e tests/value_test.sh && echo hidden-present || echo hidden-absent; echo "config=$CLAUDE_CONFIG_DIR"; } > ` + seen + `
` + edit + `
cat <<EOF
{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5","permissionMode":"` + permissionMode + `","tools":["Bash","Edit","Read"],"skills":[],"slash_commands":["compact"]}
{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":25000,"cache_read_input_tokens":0,"service_tier":"standard"},"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sh run_tests.sh"}},{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"$(pwd)/value.txt"}}` + extraTool + `]}}
{"type":"result","subtype":"success","is_error":false,"result":"Done. (token: ${CLAUDE_CODE_OAUTH_TOKEN:-none})","total_cost_usd":0.25,"num_turns":4,"duration_ms":30000,"permission_denials":[],"modelUsage":{"claude-sonnet-5":{"inputTokens":10,"outputTokens":500,"cacheReadInputTokens":100,"cacheCreationInputTokens":25000,"costUSD":0.25}}}
EOF
`
	cli = filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli, seen
}

func TestRunOnceGradesWithHiddenTestsAndIsolation(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "run_tests.sh", "for f in tests/*.sh; do [ -e \"$f\" ] || continue; sh \"$f\" || exit 1; done\n")
	writeFile(t, repo, "CLAUDE.md", "# Rules\n")
	writeFile(t, repo, "value.txt", "old\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	writeFile(t, repo, "tests/value_test.sh", "grep -q new value.txt\n")
	writeFile(t, repo, "value.txt", "new\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "Make the value new")

	home := t.TempDir()
	data := filepath.Join(t.TempDir(), "data")
	tokenFile := filepath.Join(home, "token")
	writeFile(t, home, "token", "tok-secret-1234567890\n")
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"AGENTIUM_HOME": data, "HOME": home, "AGENTIUM_CLAUDE_TOKEN_FILE": tokenFile}
	run := func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{
			Args: args, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv: func(key string) string { return vars[key] },
			Environ: func() []string {
				return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GITHUB_TOKEN=ghp_notforthechild"}
			},
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Now:      func() time.Time { return time.Now() },
		})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	expect(t, run("init"), ExitOK)
	expect(t, run("task", "import", "--commit", "HEAD", "--name", "value", "--verify", "sh run_tests.sh"), ExitOK)
	before := repoState(t, repo)

	solver, seen := fakeAgent(t, "printf 'new\\n' > value.txt", "acceptEdits", "")
	vars["AGENTIUM_CLAUDE"] = solver
	solved := run("run", "once", "value")
	expect(t, solved, ExitOK, "Starting a real Claude Code run", "sign-in token-file", "outcome      ok; verification passed",
		"changes      1 file(s), +1 -1, 0 commit(s)", "ran the checks: true", "first request 25005 tokens")
	saw, _ := os.ReadFile(seen)
	if !strings.Contains(string(saw), "hidden-absent") || !strings.Contains(string(saw), filepath.Join("workspaces")) ||
		!strings.Contains(string(saw), "config="+filepath.Join(data, "workspaces")) {
		t.Errorf("the agent saw:\n%s", saw)
	}
	records := filepath.Join(data, "records")
	entries, _ := os.ReadDir(records)
	if len(entries) != 1 {
		t.Fatalf("records = %v", entries)
	}
	runDir := filepath.Join(records, entries[0].Name())
	transcript, _ := os.ReadFile(filepath.Join(runDir, "stream.jsonl"))
	if strings.Contains(string(transcript), "tok-secret-1234567890") || !strings.Contains(string(transcript), "[REDACTED]") {
		t.Errorf("the token must be redacted from the transcript:\n%s", transcript)
	}
	diff, _ := os.ReadFile(filepath.Join(runDir, "agent.diff"))
	if !strings.Contains(string(diff), "+new") || strings.Contains(string(diff), "value_test.sh") {
		t.Errorf("the agent's diff (hidden tests are not the agent's work):\n%s", diff)
	}
	for _, gone := range []string{filepath.Join(runDir, "verify"), filepath.Join(data, "workspaces", entries[0].Name())} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s should be removed after the run", gone)
		}
	}

	idle, _ := fakeAgent(t, "", "acceptEdits", "")
	vars["AGENTIUM_CLAUDE"] = idle
	expect(t, run("run", "once", "value"), ExitOK, "outcome      ok; verification failed", "changes      0 file(s)")

	drifted, _ := fakeAgent(t, "", "default", "")
	vars["AGENTIUM_CLAUDE"] = drifted
	expect(t, run("run", "once", "value"), ExitOK, "outcome      unfair", `unfair: permission mode "default", not "acceptEdits"`)

	peek := `,{"type":"tool_use","id":"t3","name":"Read","input":{"file_path":"` + filepath.Join(data, "agentium.db") + `"}}`
	peeker, _ := fakeAgent(t, "", "acceptEdits", peek)
	vars["AGENTIUM_CLAUDE"] = peeker
	expect(t, run("run", "once", "value"), ExitOK, "outcome      unfair", "1 file tool call(s) reached Agentium's data")

	list := run("run", "list")
	expect(t, list, ExitOK, "value", "yes", "no", "unfair", "$0.25")
	id := strings.Fields(strings.Split(list.stdout, "\n")[1])[0]
	expect(t, run("run", "show", id), ExitOK, "Run "+id, "files        agent.diff", "stream.jsonl", "verify.log")
	expect(t, run("run", "show", "nope"), ExitError, "not found")

	expect(t, run("task", "add", "broken-setup", "--base", "HEAD~1", "--instruction", "Anything.", "--setup", "exit 3", "--verify", "true"), ExitOK)
	expect(t, run("run", "once", "broken-setup"), ExitOK, "outcome      infra; verification not run", "note: setup failed")
	expect(t, run("run", "once", "value", "--snapshot", "missing"), ExitError, `snapshot "missing"`)
	expect(t, run("run", "once"), ExitUsage)

	if after := repoState(t, repo); after != before {
		t.Errorf("runs modified the repository:\nbefore %s\nafter  %s", before, after)
	}
}

func TestRunOnceRefusesInstructionFilesAboveTheWorkspace(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "a.txt", "a\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	outer := t.TempDir()
	writeFile(t, outer, "CLAUDE.md", "personal notes\n")
	vars := map[string]string{"AGENTIUM_HOME": filepath.Join(outer, "data"), "HOME": t.TempDir(), "AGENTIUM_CLAUDE": "/bin/false"}
	run := func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{Args: args, Stdout: &stdout, Stderr: &stderr, Dir: repo,
			Getenv: func(key string) string { return vars[key] }, Environ: func() []string { return nil },
			LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	expect(t, run("init"), ExitOK)
	expect(t, run("task", "add", "t", "--base", "HEAD", "--instruction", "Do it.", "--verify", "true"), ExitOK)
	expect(t, run("run", "once", "t"), ExitError, filepath.Join(outer, "CLAUDE.md"), "would load it into every run")
}
