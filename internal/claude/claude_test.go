package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tests are named after the Phase 0 isolation requirements they cover (Req1 … Req9); see the PR for the map.

func invocation(t *testing.T, signIn, secret string) Invocation {
	t.Helper()
	inv := Invocation{CLI: "/bin/claude", Dir: "/work/runs/r1/repo", Prompt: "Fix the parser.", Model: "claude-sonnet-5",
		Effort: "medium", BudgetUSD: 3, SignIn: signIn, Secret: secret, Home: "/home/u",
		Deny: []string{"/data/projects", "/data/records", "/users/repo"}}
	if signIn != SignInLogin {
		inv.ConfigDir = "/work/runs/r1/config"
	}
	return inv
}

// parentEnv is a user's environment with credentials and settings that must not reach a run.
var parentEnv = []string{"PATH=/usr/bin:/bin", "HOME=/home/u", "GOPATH=/home/u/go", "LANG=en_US.UTF-8", "LC_ALL=C",
	"ANTHROPIC_API_KEY=sk-ant-parent", "CLAUDE_CODE_OAUTH_TOKEN=parent-token", "CLAUDE_CONFIG_DIR=/home/u/.claude-work",
	"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1", "GITHUB_TOKEN=ghp_x", "HF_TOKEN=hf_x", "GIT_DIR=/home/u/repo/.git",
	"AGENTIUM_HOME=/data", "SSH_AUTH_SOCK=/tmp/agent.sock", "AWS_SECRET_ACCESS_KEY=x", "EDITOR=vim"} // secret-scan: allow

func command(t *testing.T, inv Invocation) (args []string, env map[string]string, settings map[string]any) {
	t.Helper()
	args, list, err := inv.Command(parentEnv)
	if err != nil {
		t.Fatal(err)
	}
	env = map[string]string{}
	for _, kv := range list {
		name, value, _ := strings.Cut(kv, "=")
		env[name] = value
	}
	i := slices.Index(args, "--settings")
	if i < 0 {
		t.Fatalf("no --settings in %q", args)
	}
	if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
		t.Fatal(err)
	}
	return args, env, settings
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestReq1PersonalContextDoesNotLoad(t *testing.T) {
	for _, mode := range []string{SignInLogin, SignInTokenFile, SignInAPIKey} {
		secret := ""
		if mode != SignInLogin {
			secret = "s3cret"
		}
		args, _, _ := command(t, invocation(t, mode, secret))
		if flagValue(args, "--setting-sources") != "project" {
			t.Errorf("%s: --setting-sources = %q, want project", mode, flagValue(args, "--setting-sources"))
		}
	}
	home := t.TempDir()
	for _, dir := range []string{".claude/skills/my-review", ".claude/skills/deploy"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "commands", "ship.md"), []byte("ship"), 0o644); err != nil {
		t.Fatal(err)
	}
	personal := PersonalSkills(home)
	if !slices.Equal(personal, []string{"deploy", "my-review", "ship"}) {
		t.Fatalf("personal skills = %v", personal)
	}
	leaked := Metrics{SawInit: true, PermissionMode: PermissionMode, Skills: []string{"deploy", "product-increment", "ship"}}
	drift := Check(leaked, Expect{PersonalSkills: personal})
	if len(drift) != 1 || drift[0] != "2 personal skill(s) loaded" {
		t.Errorf("drift = %q (names must stay private)", drift)
	}
	if Classify(Metrics{SawResult: true, Result: "success"}, false, drift) != OutcomeUnfair {
		t.Error("a run with personal skills is unfair")
	}
}

func TestReq2AccountConnectorsStayOff(t *testing.T) {
	args, env, settings := command(t, invocation(t, SignInLogin, ""))
	if !slices.Contains(args, "--strict-mcp-config") || env["ENABLE_CLAUDEAI_MCP_SERVERS"] != "false" || settings["disableClaudeAiConnectors"] != true {
		t.Errorf("connectors not disabled: args %q, env %v, settings %v", args, env["ENABLE_CLAUDEAI_MCP_SERVERS"], settings["disableClaudeAiConnectors"])
	}
	for _, tool := range []string{"WebFetch", "WebSearch", "SendMessage", "PushNotification"} {
		if !strings.Contains(flagValue(args, "--disallowedTools"), tool) {
			t.Errorf("%s is not disallowed", tool)
		}
	}
	m := Metrics{SawInit: true, PermissionMode: PermissionMode, MCPTools: 2}
	if drift := Check(m, Expect{}); len(drift) != 1 || !strings.Contains(drift[0], "2 MCP or connector tool(s)") {
		t.Errorf("drift = %q", drift)
	}
}

func TestReq3PermissionModeIsFixedAndChecked(t *testing.T) {
	args, env, _ := command(t, invocation(t, SignInLogin, ""))
	if flagValue(args, "--permission-mode") != PermissionMode || flagValue(args, "--permission-prompts") != "none" {
		t.Errorf("args = %q", args)
	}
	if _, ok := env["CLAUDE_CODE_SUBPROCESS_ENV_SCRUB"]; ok {
		t.Error("CLAUDE_CODE_SUBPROCESS_ENV_SCRUB forces the default permission mode and must never reach a run")
	}
	for mode, unfair := range map[string]bool{PermissionMode: false, "default": true, "bypassPermissions": true, "": true} {
		drift := Check(Metrics{SawInit: true, PermissionMode: mode}, Expect{})
		if (len(drift) > 0) != unfair {
			t.Errorf("mode %q: drift %q", mode, drift)
		}
	}
}

func TestReq4SignInModesAndFreshConfig(t *testing.T) {
	_, login, _ := command(t, invocation(t, SignInLogin, ""))
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := login[name]; ok {
			t.Errorf("login mode passed %s (the user's own login and config folder are used)", name)
		}
	}
	_, token, _ := command(t, invocation(t, SignInTokenFile, "tok-123"))
	if token["CLAUDE_CONFIG_DIR"] != "/work/runs/r1/config" || token["CLAUDE_CODE_OAUTH_TOKEN"] != "tok-123" || token["ANTHROPIC_API_KEY"] != "" {
		t.Errorf("token mode env = %v", token)
	}
	_, key, _ := command(t, invocation(t, SignInAPIKey, "sk-ant-run"))                                                                          // secret-scan: allow
	if key["CLAUDE_CONFIG_DIR"] != "/work/runs/r1/config" || key["ANTHROPIC_API_KEY"] != "sk-ant-run" || key["CLAUDE_CODE_OAUTH_TOKEN"] != "" { // secret-scan: allow
		t.Errorf("API key mode env = %v", key)
	}
	for _, bad := range []Invocation{invocation(t, SignInTokenFile, ""), invocation(t, SignInLogin, "x"), invocation(t, "magic", "")} {
		if _, _, err := bad.Command(parentEnv); err == nil {
			t.Errorf("sign-in %q with secret %q accepted", bad.SignIn, bad.Secret)
		}
	}
	noConfig := invocation(t, SignInAPIKey, "k")
	noConfig.ConfigDir = ""
	if _, _, err := noConfig.Command(parentEnv); err == nil {
		t.Error("an API key run needs a fresh config folder")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	for body, ok := range map[string]bool{"tok-abc\n": true, "": false, "two words": false} {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadToken(file); (err == nil) != ok || (ok && got != "tok-abc") {
			t.Errorf("token %q: %q, %v", body, got, err)
		}
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(file); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a token readable by others: %v", err)
	}
}

func TestReq5HiddenPathsAndCredentialsAreDenied(t *testing.T) {
	for _, mode := range []string{SignInLogin, SignInAPIKey} {
		secret := ""
		if mode == SignInAPIKey {
			secret = "k"
		}
		_, env, settings := command(t, invocation(t, mode, secret))
		sandbox := settings["sandbox"].(map[string]any)
		denyRead := strings.Join(toStrings(sandbox["filesystem"].(map[string]any)["denyRead"]), " ")
		rules := strings.Join(toStrings(settings["permissions"].(map[string]any)["deny"]), " ")
		for _, p := range []string{"/data/projects", "/data/records", "/users/repo"} {
			if !strings.Contains(denyRead, p) || !strings.Contains(rules, "Read(/"+p+"/**)") {
				t.Errorf("%s: %s not denied: %s | %s", mode, p, denyRead, rules)
			}
		}
		transcripts := "/home/u/.claude/projects"
		if strings.Contains(denyRead, transcripts) != (mode == SignInLogin) {
			t.Errorf("%s: session transcripts denied = %v; want only in login mode", mode, strings.Contains(denyRead, transcripts))
		}
		if sandbox["enabled"] != true || sandbox["allowUnsandboxedCommands"] != false || sandbox["failIfUnavailable"] != true {
			t.Errorf("sandbox = %v", sandbox)
		}
		network := sandbox["network"].(map[string]any)
		if network["strictAllowlist"] != true || len(toStrings(network["allowedDomains"])) != 0 {
			t.Errorf("network = %v", network)
		}
		credentials, _ := json.Marshal(sandbox["credentials"])
		for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "/home/u/.ssh", "/home/u/.config/gh", "/home/u/.git-credentials"} {
			if !strings.Contains(string(credentials), want) {
				t.Errorf("credentials do not deny %s: %s", want, credentials)
			}
		}
		// The environment is an allowlist: toolchain settings pass, credentials and repository pointers do not.
		for _, gone := range []string{"GITHUB_TOKEN", "HF_TOKEN", "GIT_DIR", "AGENTIUM_HOME", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "EDITOR"} {
			if _, ok := env[gone]; ok {
				t.Errorf("%s: %s reached the run", mode, gone)
			}
		}
		for _, kept := range []string{"PATH", "HOME", "GOPATH", "LANG", "LC_ALL"} {
			if _, ok := env[kept]; !ok {
				t.Errorf("%s: %s is missing", mode, kept)
			}
		}
	}
}

func TestReq7DenialsAreCounted(t *testing.T) {
	m := parseFixture(t)
	if m.Denials != 1 {
		t.Errorf("permission denials = %d, want 1", m.Denials)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func parseFixture(t *testing.T) Metrics {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "ok.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParse(t *testing.T) {
	m := parseFixture(t)
	if m.CLIVersion != "2.1.281" || m.Model != "claude-sonnet-5" || m.PermissionMode != PermissionMode || m.SkillCount != 2 ||
		!slices.Equal(m.Tools, []string{"Bash", "Edit", "Read", "Task", "ToolSearch", "Write"}) || m.MCPTools != 0 {
		t.Errorf("init = %+v", m)
	}
	if m.FirstRequest != 28785 { // the main agent's first request, not the subagent's
		t.Errorf("first request = %d, want 28785", m.FirstRequest)
	}
	if m.ToolUses["Bash"] != 1 || m.ToolUses["Edit"] != 1 || m.ToolUses["Read"] != 1 || !slices.Equal(m.Commands, []string{"go test ./..."}) {
		t.Errorf("tool uses %v, commands %q (a repeated tool_use id counts once)", m.ToolUses, m.Commands)
	}
	if !slices.Equal(m.FilePaths, []string{"/work/run/repo/README.md", "/work/run/repo/x.go"}) {
		t.Errorf("file paths = %q", m.FilePaths)
	}
	if m.CostUSD != 0.3135 || m.Turns != 18 || m.InputTokens != 36 || m.OutputTokens != 7570 || m.CacheReadTokens != 646204 ||
		m.CacheWriteTokens != 27132 || m.APIRetries != 1 || m.Result != "success" || m.ResultExcerpt != "Changed x.go; tests pass." {
		t.Errorf("result = %+v", m)
	}
	stored, _ := json.Marshal(m)
	if strings.Contains(string(stored), "product-increment") || strings.Contains(string(stored), "go test") {
		t.Errorf("skill names and commands must not be stored: %s", stored)
	}
}

func TestClassify(t *testing.T) {
	result := func(subtype string, isError bool, text string) Metrics {
		return Metrics{SawInit: true, SawResult: true, Result: subtype, ResultIsError: isError, ResultExcerpt: text}
	}
	for name, c := range map[string]struct {
		m        Metrics
		timedOut bool
		drift    []string
		want     string
	}{
		"ok":             {result("success", false, "done"), false, nil, OutcomeOK},
		"agent error":    {result("success", true, "I could not find the file"), false, nil, OutcomeOK},
		"turn cap":       {result("error_max_turns", true, ""), false, nil, OutcomeCapped},
		"budget cap":     {result("error_max_budget_usd", true, ""), false, nil, OutcomeCapped},
		"crash":          {result("error_during_execution", true, ""), false, nil, OutcomeInfra},
		"rate limit":     {result("success", true, "API Error: rate limit reached"), false, nil, OutcomeInfra},
		"not logged in":  {result("success", true, "Not logged in · Please run /login"), false, nil, OutcomeInfra},
		"no result":      {Metrics{SawInit: true}, false, nil, OutcomeInfra},
		"timeout":        {result("success", false, ""), true, nil, OutcomeTimeout},
		"drift wins":     {result("success", false, "done"), true, []string{"x"}, OutcomeUnfair},
		"server error":   {result("success", true, "API Error: 529 Overloaded"), false, nil, OutcomeInfra},
		"an agent's 500": {result("success", false, "Fixed the 500 error handler"), false, nil, OutcomeOK},
	} {
		if got := Classify(c.m, c.timedOut, c.drift); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

func TestCheckVersionModelToolsAndMissingInit(t *testing.T) {
	m := Metrics{SawInit: true, PermissionMode: PermissionMode, CLIVersion: "2.1.290", Model: "claude-sonnet-5", Tools: []string{"Bash", "Edit", "Monitor"}}
	drift := Check(m, Expect{CLIVersion: "2.1.281", Model: "claude-sonnet-5", Tools: []string{"Edit", "Bash", "Read"}})
	if len(drift) != 2 || drift[0] != "Claude Code 2.1.290, not 2.1.281" || drift[1] != "tools differ (added Monitor; missing Read)" {
		t.Errorf("drift = %q", drift)
	}
	if drift := Check(Metrics{SawResult: true}, Expect{}); len(drift) != 1 {
		t.Errorf("a result without an init event: %q", drift)
	}
	if drift := Check(Metrics{}, Expect{}); drift != nil {
		t.Errorf("nothing ran: %q (Classify reports infra)", drift)
	}
}

// fakeClaude writes a stand-in for the claude CLI: it records its arguments, environment and folder, prints a
// transcript, and with hang set waits until interrupted, then reports a result as Claude Code does.
func fakeClaude(t *testing.T, transcript string, hang bool) (cli, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	if err := os.Mkdir(record, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n"
	if hang { // the trap comes first, so an early interrupt still gets a result
		script += "trap 'echo \"{\\\"type\\\":\\\"result\\\",\\\"subtype\\\":\\\"success\\\",\\\"is_error\\\":false,\\\"result\\\":\\\"interrupted\\\"}\"; exit 130' INT\n"
	}
	script += "printf '%s\\n' \"$@\" > " + record + "/args\nenv > " + record + "/env\npwd > " + record + "/pwd\n"
	if hang {
		script += "head -1 " + transcript + "\nwhile :; do sleep 0.05; done\n"
	} else {
		script += "cat " + transcript + "\n"
	}
	cli = filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli, record
}

func TestRunWithAFakeClaude(t *testing.T) {
	fixture, _ := filepath.Abs(filepath.Join("testdata", "ok.jsonl"))
	cli, record := fakeClaude(t, fixture, false)
	work := t.TempDir()
	inv := invocation(t, SignInTokenFile, "tok-run")
	inv.CLI, inv.Dir, inv.ConfigDir = cli, work, filepath.Join(work, "config")
	transcript, err := os.Create(filepath.Join(t.TempDir(), "stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr.txt"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), inv, parentEnv, transcript, stderr, time.Minute, 5*time.Second)
	if err != nil || !result.Passed() {
		t.Fatalf("%+v, %v", result, err)
	}
	transcript.Seek(0, 0)
	m, err := Parse(transcript)
	if err != nil || m.Result != "success" || Classify(m, result.TimedOut, Check(m, Expect{})) != OutcomeOK {
		t.Errorf("parsed %+v, %v", m, err)
	}
	args, _ := os.ReadFile(filepath.Join(record, "args"))
	env, _ := os.ReadFile(filepath.Join(record, "env"))
	pwd, _ := os.ReadFile(filepath.Join(record, "pwd"))
	realWork, _ := filepath.EvalSymlinks(work)
	if !strings.Contains(string(args), "--setting-sources\nproject\n") || strings.TrimSpace(string(pwd)) != realWork {
		t.Errorf("args %q, pwd %q", args, pwd)
	}
	if !strings.Contains(string(env), "CLAUDE_CODE_OAUTH_TOKEN=tok-run") || strings.Contains(string(env), "sk-ant-parent") ||
		strings.Contains(string(env), "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB") {
		t.Errorf("the child's environment:\n%s", env)
	}
}

func TestRunTimeoutInterruptsFirst(t *testing.T) {
	fixture, _ := filepath.Abs(filepath.Join("testdata", "ok.jsonl"))
	cli, _ := fakeClaude(t, fixture, true)
	inv := invocation(t, SignInLogin, "")
	inv.CLI, inv.Dir = cli, t.TempDir()
	transcript, _ := os.Create(filepath.Join(t.TempDir(), "stream.jsonl"))
	stderr, _ := os.Create(filepath.Join(t.TempDir(), "stderr.txt"))
	result, err := Run(context.Background(), inv, parentEnv, transcript, stderr, 2*time.Second, 5*time.Second)
	if err != nil || !result.TimedOut {
		t.Fatalf("%+v, %v", result, err)
	}
	transcript.Seek(0, 0)
	m, _ := Parse(transcript)
	if !m.SawInit || !m.SawResult || m.ResultExcerpt != "interrupted" || Classify(m, true, Check(m, Expect{})) != OutcomeTimeout {
		t.Errorf("an interrupted run should still report its result: %+v", m)
	}
}
