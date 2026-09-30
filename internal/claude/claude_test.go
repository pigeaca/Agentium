package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
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
	personal := PersonalSkills(UserConfigDir(nil, home))
	if !slices.Equal(personal, []string{"deploy", "my-review", "ship"}) {
		t.Fatalf("personal skills = %v", personal)
	}
	leaked := Metrics{SawInit: true, PermissionMode: PermissionMode, Skills: []string{"deploy", "my-review", "product-increment"},
		SlashCommands: []string{"compact", "ship"}}
	drift := Check(leaked, Expect{PersonalSkills: personal})
	if len(drift) != 1 || drift[0] != "2 personal skill(s) loaded" {
		t.Errorf("drift = %q (names must stay private)", drift)
	}
	// A personal command shows up among slash commands, next to Claude Code's own: an exact set from a calibration run
	// catches it; a personal skill named like a built-in command is not a leak.
	if drift := Check(leaked, Expect{SlashCommands: []string{"compact", "review"}}); len(drift) != 1 || drift[0] != "slash commands differ (1 added, 1 missing)" {
		t.Errorf("slash command drift = %q", drift)
	}
	builtin := Metrics{SawInit: true, PermissionMode: PermissionMode, SlashCommands: []string{"compact", "review"}}
	if drift := Check(builtin, Expect{PersonalSkills: []string{"review"}}); len(drift) != 0 {
		t.Errorf("a personal skill named like a built-in command: %q", drift)
	}
	// A bundled skill that a calibration run also had is not a leak either.
	bundled := Metrics{SawInit: true, PermissionMode: PermissionMode, Skills: []string{"review"}}
	if drift := Check(bundled, Expect{PersonalSkills: []string{"review"}, Skills: []string{"review"}}); len(drift) != 0 {
		t.Errorf("a bundled skill named like a personal one: %q", drift)
	}
	// A project skill that shares a personal skill's name is the arm's own, not a leak.
	own := Metrics{SawInit: true, PermissionMode: PermissionMode, Skills: []string{"deploy"}}
	if drift := Check(own, Expect{PersonalSkills: personal, ProjectSkills: []string{"deploy"}}); len(drift) != 0 {
		t.Errorf("a project skill named like a personal one: %q", drift)
	}
	// With a lock's exact skill set, any other set is drift, reported by count.
	if drift := Check(own, Expect{Skills: []string{"deploy", "review"}}); len(drift) != 1 || drift[0] != "skills differ (0 added, 1 missing)" {
		t.Errorf("skill set drift = %q", drift)
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
	// Login mode uses the user's own login: their CLAUDE_CONFIG_DIR when they have one, and no secret.
	_, login, _ := command(t, invocation(t, SignInLogin, ""))
	if login["CLAUDE_CONFIG_DIR"] != "/home/u/.claude-work" || login["ANTHROPIC_API_KEY"] != "" || login["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
		t.Errorf("login mode env = %v", login)
	}
	_, plain, err := invocation(t, SignInLogin, "").Command([]string{"PATH=/usr/bin", "HOME=/home/u"})
	if err != nil || strings.Contains(strings.Join(plain, " "), "CLAUDE_CONFIG_DIR") {
		t.Errorf("login mode without a custom config folder: %v, %v", plain, err)
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
		// Claude Code's history and credential stores are denied in every mode, to the shell and to the Read tool. The
		// active config folder keeps its working files (the shell snapshot the Bash tool sources); others go whole.
		active, other := "/home/u/.claude-work", "/home/u/.claude"
		if mode == SignInAPIKey {
			active, other = "/work/runs/r1/config", "/home/u/.claude-work"
		}
		denied := []string{other, "/home/u/.claude.json", active + "/file-history", active + "/history.jsonl", active + "/.credentials.json",
			"/home/u/.ssh", "/home/u/.config/gh", "/home/u/.aws"}
		for _, p := range denied {
			if !slices.Contains(toStrings(settings["permissions"].(map[string]any)["deny"]), "Read(/"+p+"/**)") || !strings.Contains(denyRead, p) {
				t.Errorf("%s: %s is not denied: %s", mode, p, rules)
			}
		}
		if slices.Contains(toStrings(sandbox["filesystem"].(map[string]any)["denyRead"]), active) {
			t.Errorf("%s: the whole active config folder %s is denied; the Bash tool needs its working files", mode, active)
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

func TestReq5DeniedPathsAreCheckedAndResolved(t *testing.T) {
	inv := invocation(t, SignInLogin, "")
	inv.Deny = []string{"relative/path"}
	if _, _, err := inv.Command(parentEnv); err == nil {
		t.Error("a relative denied path must be refused")
	}
	inv.Deny = nil
	if _, _, err := inv.Command([]string{"CLAUDE_CONFIG_DIR=relative-config"}); err == nil {
		t.Error("a relative CLAUDE_CONFIG_DIR must be refused")
	}
	token := invocation(t, SignInTokenFile, "tok")
	token.TokenFile = "token.txt"
	if _, _, err := token.Command(parentEnv); err == nil {
		t.Error("a relative token file must be refused")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	inv.Deny = []string{link + "/"}
	realResolved, _ := filepath.EvalSymlinks(real)
	_, _, settings := command(t, inv)
	rules := strings.Join(toStrings(settings["permissions"].(map[string]any)["deny"]), " ")
	if !strings.Contains(rules, "Read(/"+link+"/**)") || !strings.Contains(rules, "Read(/"+realResolved+"/**)") {
		t.Errorf("both forms of a symlinked path must be denied: %s", rules)
	}
	token = invocation(t, SignInTokenFile, "tok")
	token.TokenFile = "/home/u/.secrets/claude-token"
	_, _, settings = command(t, token)
	if rules := strings.Join(toStrings(settings["permissions"].(map[string]any)["deny"]), " "); !strings.Contains(rules, "Read(//home/u/.secrets/**)") {
		t.Errorf("the token file's folder is readable: %s", rules)
	}
}

func TestReq5EnvironmentAllowlistEdges(t *testing.T) {
	env := strings.Join(Environ([]string{"GOOGLE_CLOUD_PROJECT=p", "GOAUTH=netrc", "GOPATH=/g", "GOFLAGS=-mod=mod",
		"HTTPS_PROXY=http://proxy:3128", "NO_PROXY=localhost", "SSL_CERT_FILE=/etc/ca.pem", "CGO_ENABLED=1", "NODE_OPTIONS=--x"}), " ")
	if env != "GOPATH=/g GOFLAGS=-mod=mod HTTPS_PROXY=http://proxy:3128 NO_PROXY=localhost SSL_CERT_FILE=/etc/ca.pem CGO_ENABLED=1 NODE_OPTIONS=--x" {
		t.Errorf("kept %s", env)
	}
}

func TestParseToleratesOddFieldsAndZeroFirstRequest(t *testing.T) {
	stream := `{"type":"system","subtype":"init","permissionMode":"acceptEdits","tools":["Bash"]}
{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":0,"service_tier":"standard"},"content":[]}}
{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":900},"content":"not a list"}}
{"type":"result","subtype":"success","is_error":false,"result":"ok","total_cost_usd":0.5,"modelUsage":{"a":{"inputTokens":"lots"},"b":{"inputTokens":7,"outputTokens":3,"costUSD":0.1}}}
`
	m, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if m.FirstRequest != 0 || !m.SawResult || m.InputTokens != 7 || m.OutputTokens != 3 || m.CostUSD != 0.5 {
		t.Errorf("metrics = %+v (the first request is the first one, even at 0; a bad model entry is skipped, not the result)", m)
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

func TestReq5PastSessionsAreDeniedButNotTheRunsOwn(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{".claude/projects/-work-old-project", ".claude/projects/-work-other"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inv := invocation(t, SignInLogin, "")
	inv.Home = home
	args, _, err := inv.Command([]string{"PATH=/usr/bin", "HOME=" + home}) // the default config folder, ~/.claude
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(flagValue(args, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	rules := toStrings(settings["permissions"].(map[string]any)["deny"])
	for _, past := range []string{"-work-old-project", "-work-other"} {
		if !slices.Contains(rules, "Read(/"+filepath.Join(home, ".claude", "projects", past)+"/**)") {
			t.Errorf("past session %s is readable: %v", past, rules)
		}
	}
	// The projects folder itself stays readable: the run's own session folder, created later, holds its large outputs.
	if slices.Contains(rules, "Read(/"+filepath.Join(home, ".claude", "projects")+"/**)") {
		t.Errorf("all of projects/ is denied, so large outputs cannot be read back: %v", rules)
	}
}

func TestToolCallsPairResultsWithTheirUse(t *testing.T) {
	stream := `{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"seq 1 40000"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"<persisted-output>\nOutput too large. Full output saved to: /c/projects/-w/tool-results/b1.txt\n</persisted-output>"}]}}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"b","name":"Read","input":{"file_path":"/c/projects/-w/tool-results/b1.txt","offset":20000}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","content":[{"type":"text","text":"20000\t20000"}],"is_error":false}]}}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"/secret"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":"Permission denied","is_error":true}]}}
`
	calls, err := ToolCalls(strings.NewReader(stream))
	if err != nil || len(calls) != 3 {
		t.Fatalf("calls = %+v, %v", calls, err)
	}
	if !strings.Contains(calls[0].Result, "saved to: /c/projects/-w/tool-results/b1.txt") || calls[1].Result != "20000\t20000" || !calls[2].IsError {
		t.Errorf("calls = %+v", calls)
	}
}

func TestSessionFolder(t *testing.T) {
	if got := SessionFolder("/home/u/.claude", "/private/tmp/work/ws_1.2/repo"); got != "/home/u/.claude/projects/-private-tmp-work-ws-1-2-repo" {
		t.Errorf("session folder = %s", got)
	}
}

// A transcript cut off before its result (Agentium was killed) is priced from its requests: each message's usage is
// counted once however many events repeat it, subagent requests at their own model's prices, and output from the
// content's size where the stream reports less.
func TestParseEstimatesCostWithoutAResult(t *testing.T) {
	text := `{"type":"text","text":"` + strings.Repeat("x", 400) + `"}`
	tool := `{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}`
	usage := `{"input_tokens":2,"cache_creation_input_tokens":11199,"cache_read_input_tokens":16754,"output_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":11199}}`
	transcript := strings.Join([]string{
		`{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5","permissionMode":"acceptEdits","tools":["Bash"]}`,
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-sonnet-5","usage":` + usage + `,"content":[` + text + `]}}`,
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-sonnet-5","usage":` + usage + `,"content":[` + tool + `]}}`,
		`{"type":"assistant","parent_tool_use_id":"t1","message":{"id":"m2","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":100,"cache_creation_input_tokens":1000,"output_tokens":5000},"content":[]}}`,
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m3","model":"somebody-elses-model","usage":{"input_tokens":100},"content":[]}}`,
	}, "\n")
	m, err := Parse(strings.NewReader(transcript))
	if err != nil {
		t.Fatal(err)
	}
	sonnet := (2*2 + 11199*4 + 16754*0.2 + float64((len(text)+len(tool))/4)*10) / 1e6 // output: 125 tokens of content, not 3
	haiku := (100*1 + 1000*2 + 5000*5) / 1e6                                          // no split: the one-hour write rate
	if m.SawResult || m.CostUSD != 0 || math.Abs(m.EstimatedCostUSD-(sonnet+haiku)) > 1e-9 || m.UnpricedRequests != 1 {
		t.Errorf("metrics = cost %v, estimated %.7f (want %.7f), unpriced %d", m.CostUSD, m.EstimatedCostUSD, sonnet+haiku, m.UnpricedRequests)
	}
}

// Req5: the user's Go build caches hold compiled hidden tests (validation and grading compile them); runs are denied
// them and write their own cache instead, which the sandbox lets them.
func TestReq5BuildCaches(t *testing.T) {
	inv := invocation(t, SignInLogin, "")
	environ := append(slices.Clone(parentEnv), "GOCACHE=/home/u/gocache", "XDG_CACHE_HOME=/home/u/xdg")
	own := inv
	own.BuildCache = "/work/runs/r1/go-build"
	args, list, err := own.Command(environ)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(flagValue(args, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	fs := settings["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	if allow, _ := fs["allowWrite"].([]any); len(allow) != 1 || allow[0] != "/work/runs/r1/go-build" {
		t.Errorf("allowWrite = %v", fs["allowWrite"])
	}
	denied := own.DeniedPaths(environ)
	for _, p := range []string{"/home/u/Library/Caches/go-build", "/home/u/.cache/go-build", "/home/u/gocache", "/home/u/xdg/go-build"} {
		if !slices.Contains(denied, p) {
			t.Errorf("%s is not denied", p)
		}
	}
	var gocache []string
	for _, kv := range list {
		if v, ok := strings.CutPrefix(kv, "GOCACHE="); ok {
			gocache = append(gocache, v)
		}
	}
	if !slices.Equal(gocache, []string{"/work/runs/r1/go-build"}) {
		t.Errorf("GOCACHE in the run's environment: %v; want only the run's own", gocache)
	}
	// Without a cache of its own, the sandbox allows no extra writes.
	_, _, plain := command(t, inv)
	if _, ok := plain["sandbox"].(map[string]any)["filesystem"].(map[string]any)["allowWrite"]; ok {
		t.Error("allowWrite without a build cache")
	}
	own.BuildCache = "go-build"
	if _, _, err := own.Command(environ); err == nil {
		t.Error("a relative build cache is refused")
	}

	// A cache set with `go env -w` is denied; values Go ignores are skipped.
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte("GOPRIVATE=example.com\nGOCACHE=/home/u/written\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	denied = inv.DeniedPaths(append(slices.Clone(parentEnv), "GOENV="+envFile, "GOCACHE=off", "XDG_CACHE_HOME=relative"))
	if !slices.Contains(denied, "/home/u/written") {
		t.Errorf("the cache in Go's env file is not denied: %v", denied)
	}
	for _, p := range denied {
		if !filepath.IsAbs(p) {
			t.Errorf("denied %q, which Go would not use as a cache", p)
		}
	}
	// The sandbox matches real paths: a symlinked data folder is allowed by both spellings.
	target := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(target, "go-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	own.BuildCache = filepath.Join(dir, "link", "go-build")
	_, _, linked := command(t, own)
	allow, _ := linked["sandbox"].(map[string]any)["filesystem"].(map[string]any)["allowWrite"].([]any)
	resolved, _ := filepath.EvalSymlinks(filepath.Join(target, "go-build"))
	if len(allow) != 2 || allow[0] != own.BuildCache || allow[1] != resolved {
		t.Errorf("allowWrite = %v, want %s and %s", allow, own.BuildCache, resolved)
	}
}

// A subscription's usage readings are kept, first and last, and each subagent type's models are told apart from the
// run's own: a role's model alias can move to a newer model while --model stays pinned.
func TestParseUsageReadingsAndSubagentModels(t *testing.T) {
	limit := func(five, seven float64, status string) string {
		return fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":%q,"resetsAt":1790716800,"unifiedWindows":`+
			`{"five_hour":{"utilization":%v,"resetsAt":1790716800},"seven_day":{"utilization":%v,"resetsAt":1791025200}}}}`, status, five, seven)
	}
	agent := func(id, kind string) string {
		input := `{"prompt":"look"}`
		if kind != "" {
			input = `{"prompt":"look","subagent_type":"` + kind + `"}`
		}
		return `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m-` + id + `","model":"claude-sonnet-5","content":[{"type":"tool_use","id":"` + id + `","name":"Agent","input":` + input + `}]}}`
	}
	child := func(parent, model string) string {
		return `{"type":"assistant","parent_tool_use_id":"` + parent + `","message":{"id":"c-` + parent + model + `","model":"` + model + `","content":[]}}`
	}
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5","permissionMode":"acceptEdits","tools":["Agent","Bash"]}`,
		limit(0.30, 0.07, "allowed"),
		agent("a1", "investigator"), child("a1", "claude-sonnet-5-5"), child("a1", "claude-sonnet-5-5"),
		agent("a2", ""), child("a2", "claude-sonnet-5"),
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`, // no windows: skipped
		limit(0.36, 0.08, "allowed_warning"),
		`{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.5}`,
	}, "\n")
	m, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	resets := time.Unix(1790716800, 0).UTC()
	if m.UsageFirst == nil || m.UsageLast == nil || m.UsageFirst.FiveHour != 0.30 || m.UsageLast.FiveHour != 0.36 || m.UsageLast.SevenDay != 0.08 ||
		!m.UsageLast.FiveHourResets.Equal(resets) || m.UsageLast.Status != "allowed_warning" {
		t.Errorf("usage readings: first %+v, last %+v", m.UsageFirst, m.UsageLast)
	}
	want := map[string][]string{"investigator": {"claude-sonnet-5-5"}, "general-purpose": {"claude-sonnet-5"}}
	if !reflect.DeepEqual(m.SubagentModels, want) {
		t.Errorf("subagent models = %v, want %v", m.SubagentModels, want)
	}
	if got := m.UsageLast.FiveHourAt(resets.Add(-time.Minute)); got != 0.36 {
		t.Errorf("before the reset: %v", got)
	}
	if got := m.UsageLast.FiveHourAt(resets); got != 0 {
		t.Errorf("once the window resets, nothing of it is used: %v", got)
	}
	if !m.UsageLast.Newer(*m.UsageFirst) || m.UsageFirst.Newer(*m.UsageLast) || !(UsageReading{FiveHour: 0.1, FiveHourResets: resets.Add(time.Hour)}).Newer(*m.UsageLast) {
		t.Error("a later reading is one further into the same window, or in a later window")
	}

	// An API-key run reports no readings and no subagents.
	plain, err := Parse(strings.NewReader(`{"type":"result","subtype":"success","total_cost_usd":0.1}`))
	if err != nil || plain.UsageFirst != nil || plain.UsageLast != nil || plain.SubagentModels != nil {
		t.Errorf("a run without readings: %+v, %v", plain, err)
	}

	// Across runs: a type that changed models is reported; one used for the first time is not.
	seen := map[string][]string{"investigator": {"claude-sonnet-5"}}
	if got := SubagentModelChanges(seen, m.SubagentModels); len(got) != 1 || !strings.Contains(got[0], "subagent investigator ran on claude-sonnet-5-5; earlier runs used claude-sonnet-5") {
		t.Errorf("changes = %q", got)
	}
	if got := SubagentModelChanges(map[string][]string{"investigator": {"claude-sonnet-5-5"}}, m.SubagentModels); len(got) != 0 {
		t.Errorf("no change: %q", got)
	}
}

// The sandbox denies writes to the module cache, where Go would record the main module's VCS-stamped version: agents
// build with -buildvcs=false, next to the user's own GOFLAGS; Environ itself leaves GOFLAGS as the user set it.
func TestAgentGoflagsDisableVCSStamping(t *testing.T) {
	goflags := func(parent []string) []string {
		_, env, err := invocation(t, SignInLogin, "").Command(parent)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, kv := range env {
			if strings.HasPrefix(kv, "GOFLAGS=") {
				got = append(got, kv)
			}
		}
		return got
	}
	if got := goflags([]string{"PATH=/usr/bin"}); len(got) != 1 || got[0] != "GOFLAGS=-buildvcs=false" {
		t.Errorf("without user flags: %v", got)
	}
	if got := goflags([]string{"PATH=/usr/bin", "GOFLAGS=-mod=mod -race"}); len(got) != 1 || got[0] != "GOFLAGS=-mod=mod -race -buildvcs=false" {
		t.Errorf("with user flags: %v", got)
	}
	if got := Environ([]string{"GOFLAGS=-mod=mod"}); len(got) != 1 || got[0] != "GOFLAGS=-mod=mod" {
		t.Errorf("Environ changed the user's flags: %v", got)
	}
}

// GOFLAGS saved with `go env -w` reach the agent too: an environment GOFLAGS would otherwise shadow them.
func TestAgentGoflagsKeepSavedFlags(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("GOFLAGS=-tags=integration\nGOPROXY=off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, env, err := invocation(t, SignInLogin, "").Command([]string{"PATH=/usr/bin", "GOENV=" + envFile})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains("\n"+strings.Join(env, "\n")+"\n", "\nGOFLAGS=-tags=integration -buildvcs=false\n") {
		t.Errorf("saved flags lost: %v", env)
	}
}
