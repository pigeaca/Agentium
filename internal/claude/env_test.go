package claude

import (
	"slices"
	"testing"
)

// TestSignInEnvironmentPinned pins the variables after the allowlist that runs (Invocation) and judge calls (Judgement)
// set for each sign-in mode: the same sign-in tail, whichever builds it.
func TestSignInEnvironmentPinned(t *testing.T) {
	fixed := []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTOUPDATER=1", "ENABLE_CLAUDEAI_MCP_SERVERS=false"}
	for _, c := range []struct {
		name, mode, secret, configDir string
		environ                       []string
		tail                          []string
	}{
		{"login, default folder", SignInLogin, "", "", []string{"PATH=/bin"}, nil},
		{"login, the default folder named", SignInLogin, "", "", []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/home/u/.claude"}, nil},
		{"login, the user's own folder", SignInLogin, "", "", []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/cfgdir"}, []string{"CLAUDE_CONFIG_DIR=/cfgdir"}},
		{"api key", SignInAPIKey, "S", "/data/cfg", []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/cfgdir"}, []string{"CLAUDE_CONFIG_DIR=/data/cfg", "ANTHROPIC_API_KEY=S"}},
		{"token file", SignInTokenFile, "S", "/data/cfg", []string{"PATH=/bin"}, []string{"CLAUDE_CONFIG_DIR=/data/cfg", "CLAUDE_CODE_OAUTH_TOKEN=S"}},
	} {
		_, judge, err := Judgement{CLI: "/c", Dir: "/d", Model: "m", SystemPrompt: "s", Schema: "{}", Home: "/home/u", SignIn: c.mode,
			Secret: c.secret, ConfigDir: c.configDir}.Command(c.environ)
		if err != nil {
			t.Fatalf("%s: judge: %v", c.name, err)
		}
		wantJudge := append(append([]string{"PATH=/bin"}, fixed...), c.tail...)
		if !slices.Equal(judge, wantJudge) {
			t.Errorf("%s: judge env = %q, want %q", c.name, judge, wantJudge)
		}
		_, run, err := Invocation{CLI: "/c", Dir: "/d", Prompt: "p", Model: "m", Home: "/home/u", SignIn: c.mode, Secret: c.secret,
			ConfigDir: c.configDir}.Command(c.environ)
		if err != nil {
			t.Fatalf("%s: run: %v", c.name, err)
		}
		wantRun := append(append([]string{"PATH=/bin", "GOFLAGS=-buildvcs=false"}, fixed...), c.tail...)
		if !slices.Equal(run, wantRun) {
			t.Errorf("%s: run env = %q, want %q", c.name, run, wantRun)
		}
	}
	// A relative CLAUDE_CONFIG_DIR is refused for a login (it would resolve in the empty call folder).
	_, _, err := Judgement{CLI: "/c", Dir: "/d", Model: "m", SystemPrompt: "s", Schema: "{}", Home: "/home/u", SignIn: SignInLogin}.Command([]string{"CLAUDE_CONFIG_DIR=rel"})
	if err == nil || err.Error() != `CLAUDE_CONFIG_DIR "rel" is not absolute` {
		t.Errorf("relative config folder: %v", err)
	}
}
