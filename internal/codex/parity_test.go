package codex_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/codex"
)

// The deny list holds every path a Claude Code run denies, whichever its sign-in (each in every form), so that a Codex
// run is never denied less than a Claude Code run of the same task: the plan's acceptance 3.
func TestDeniedPathsCoverClaudeCodes(t *testing.T) {
	environ := []string{"PATH=/usr/bin:/bin", "HOME=/hm/u", "CLAUDE_CONFIG_DIR=/hm/u/.claude-work", "XDG_RUNTIME_DIR=/run/u",
		"CLAUDE_CODE_TMPDIR=/hm/u/cc-tmp", "GOCACHE=/hm/u/gocache", "DOCKER_CONFIG=/hm/u/docker"}
	inv := agent.Invocation{CLI: "/bin/codex", Dir: "/data/workspaces/r1/repo", Prompt: "p", Model: "gpt-6.1-sol", SignIn: codex.SignInLogin,
		Home: "/hm/u", AccountHome: "/Users/u", ConfigDir: "/data/codex", State: "/data/workspaces/r1/agent", Records: "/data/records/r1",
		TempRoot: "/tmp/ag-0123456789", UID: 501, Deps: "/data/deps/1", Deny: []string{"/data/projects", "/data/records", "/users/repo"}}
	denied := codex.Adapter{}.DeniedPaths(inv, environ)
	covered := func(p string) bool {
		return slices.ContainsFunc(denied, func(d string) bool {
			rel, err := filepath.Rel(d, p)
			return err == nil && (rel == "." || filepath.IsLocal(rel))
		})
	}
	for _, signIn := range []string{claude.SignInLogin, claude.SignInAPIKey, claude.SignInTokenFile} {
		c := claude.Invocation(inv)
		c.SignIn, c.ConfigDir, c.Secret, c.TokenFile = signIn, "", "", ""
		if signIn != claude.SignInLogin {
			c.ConfigDir, c.Secret = "/data/workspaces/r1/config", "x"
		}
		if signIn == claude.SignInTokenFile {
			c.TokenFile = "/hm/u/.config/agentium/claude-oauth-token"
		}
		for _, p := range c.DeniedPaths(environ) {
			if strings.HasPrefix(p, "/data/workspaces/r1/config/") { // the history in Claude Code's own fresh config folder
				continue
			}
			if !covered(p) {
				t.Errorf("%s (a Claude Code run's, sign-in %s) is not denied to Codex", p, signIn)
			}
		}
	}
}
