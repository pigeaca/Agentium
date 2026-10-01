package claude

import (
	"slices"
	"strings"
	"testing"
)

func TestJudgementCommand(t *testing.T) {
	j := Judgement{CLI: "/bin/claude", Dir: "/data/judge/call-1", Model: "claude-opus-5-5", Effort: "high", SystemPrompt: "You review.",
		Schema: `{"type": "object"}`, BudgetUSD: 1, SignIn: SignInLogin, Home: "/home/u"}
	environ := []string{"PATH=/bin", "HOME=/home/u", "ANTHROPIC_API_KEY=sk-user", "GITHUB_TOKEN=gh", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1",
		"CLAUDE_CONFIG_DIR=/home/u/.claude-work", "LC_ALL=C"}
	args, env, err := j.Command(environ)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--model", "claude-opus-5-5", "--tools", "", "--system-prompt", "You review.", "--json-schema", `{"type": "object"}`,
		"--output-format", "json", "--no-session-persistence", "--setting-sources", "project", "--strict-mcp-config",
		"--settings", `{"autoMemoryEnabled":false,"disableClaudeAiConnectors":true}`, "--effort", "high",
		"--max-budget-usd", "1"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q", args)
	}
	for _, kv := range []string{"PATH=/bin", "HOME=/home/u", "LC_ALL=C", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTOUPDATER=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false", "CLAUDE_CONFIG_DIR=/home/u/.claude-work"} {
		if !slices.Contains(env, kv) {
			t.Errorf("env lacks %s: %q", kv, env)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "CLAUDE_CODE_SUBPROCESS") {
			t.Errorf("env passes %s", kv)
		}
	}
}

func TestJudgementSignIn(t *testing.T) {
	base := Judgement{CLI: "/bin/claude", Dir: "/d", Model: "m", SystemPrompt: "s", Schema: "{}", Home: "/home/u"}
	key := base
	key.SignIn, key.Secret, key.ConfigDir = SignInAPIKey, "sk-run", "/data/cfg"
	_, env, err := key.Command([]string{"ANTHROPIC_API_KEY=sk-user"})
	if err != nil || !slices.Contains(env, "ANTHROPIC_API_KEY=sk-run") || !slices.Contains(env, "CLAUDE_CONFIG_DIR=/data/cfg") || slices.Contains(env, "ANTHROPIC_API_KEY=sk-user") {
		t.Errorf("api key: %q, %v", env, err)
	}
	token := base
	token.SignIn, token.Secret, token.ConfigDir = SignInTokenFile, "tok", "/data/cfg"
	if _, env, err := token.Command(nil); err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=tok") {
		t.Errorf("token: %q, %v", env, err)
	}
	for name, bad := range map[string]Judgement{
		"login with a secret": func() Judgement { j := base; j.SignIn, j.Secret = SignInLogin, "x"; return j }(),
		"a key without one":   func() Judgement { j := base; j.SignIn, j.ConfigDir = SignInAPIKey, "/c"; return j }(),
		"an unknown mode":     func() Judgement { j := base; j.SignIn = "magic"; return j }(),
		"no schema":           func() Judgement { j := base; j.SignIn, j.Schema = SignInLogin, ""; return j }(),
		"a relative folder":   func() Judgement { j := base; j.SignIn, j.Dir = SignInLogin, "judge"; return j }(),
	} {
		if _, _, err := bad.Command(nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	login := base
	login.SignIn = SignInLogin
	if _, _, err := login.Command([]string{"CLAUDE_CONFIG_DIR=work-claude"}); err == nil {
		t.Error("a relative CLAUDE_CONFIG_DIR was accepted") // it would resolve in the empty call folder, signed out
	}
}
