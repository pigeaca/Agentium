package claude

import (
	"slices"
	"testing"

	"github.com/pigeaca/agentium/internal/buildtool"
)

// pinnedEnviron: credentials (also on allowlisted names), GIT_*, AGENTIUM_*, CLAUDE_*, system and toolchain settings, and
// variables the allowlist does not know. RUST_BACKTRACE, RUSTUP_TOOLCHAIN and CGO_ENABLED belong to build-tool profiles.
func pinnedEnviron() []string {
	return []string{"PATH=/bin", "HOME=/h", "GIT_DIR=/x", "GIT_AUTHOR_NAME=a", "AGENTIUM_HOME=/a", "AGENTIUM_X=1", "ANTHROPIC_API_KEY=k",
		"GITHUB_TOKEN=t", "SSH_AUTH_SOCK=/s", "MY_PASSWORD=p", "CLAUDE_CODE_TMPDIR=/t", "LC_ALL=C", "GOFLAGS=-mod=mod", "JAVA_HOME=/j",
		"RUSTC_WRAPPER=w", "HTTPS_PROXY=http://p", "XDG_RUNTIME_DIR=/r", "FOO=bar", "NODE_OPTIONS=x", "TERM=xterm", "EMPTY=", "GOPATH=/g",
		"MAVEN_OPTS=-X", "GRADLE_USER_HOME=/gu", "CARGO_HOME=/c", "SHELL=/bin/zsh", "TMPDIR=/tmp", "AWS_PROFILE=p", "NETRC=/n", "GIT=ok", "GITHUB=ok",
		"RUST_BACKTRACE=1", "RUSTUP_TOOLCHAIN=stable", "CGO_ENABLED=0",
		// Credentials on names the allowlist (or a profile) would otherwise keep: the credential check must win.
		"NODE_AUTH_TOKEN=x", "HOMEBREW_GITHUB_API_TOKEN=x", "PIP_PASSWORD=x", "CGO_SECRET=x", "my_token=x", "DOCKER_AUTH_CONFIG=x"}
}

// TestEnvironPinned pins the agent's allowlist byte for byte: the base list, then what the selected profiles add.
func TestEnvironPinned(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "GOFLAGS=-mod=mod", "JAVA_HOME=/j", "RUSTC_WRAPPER=w", "HTTPS_PROXY=http://p",
		"XDG_RUNTIME_DIR=/r", "NODE_OPTIONS=x", "TERM=xterm", "GOPATH=/g", "CARGO_HOME=/c", "SHELL=/bin/zsh", "TMPDIR=/tmp"}
	withGo := append(slices.Clone(base), "CGO_ENABLED=0")
	cargo := append(slices.Clone(base), "RUST_BACKTRACE=1", "RUSTUP_TOOLCHAIN=stable", "CGO_ENABLED=0") // in the order of environ
	for name, c := range map[string]struct {
		got, want []string
	}{
		"Environ":     {Environ(pinnedEnviron()), withGo},
		"no profiles": {EnvironFor(pinnedEnviron(), buildtool.Select(nil)), withGo},
		"cargo":       {EnvironFor(pinnedEnviron(), buildtool.Select([]string{"cargo"})), cargo},
		"empty":       {Environ(nil), nil},
	} {
		if !slices.Equal(c.got, c.want) || (c.want == nil) != (c.got == nil) {
			t.Errorf("%s = %#v\nwant %#v", name, c.got, c.want)
		}
	}
}

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
