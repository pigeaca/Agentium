package sandbox

import (
	"slices"
	"strings"
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
	// A Cargo project without go.mod: no GO* or CGO_* names (Go's agent side is off where only another profile is detected).
	var cargo []string
	for _, kv := range base {
		if !strings.HasPrefix(kv, "GO") {
			cargo = append(cargo, kv)
		}
	}
	cargo = append(cargo, "RUST_BACKTRACE=1", "RUSTUP_TOOLCHAIN=stable") // in the order of environ
	goCargo := append(slices.Clone(base), "RUST_BACKTRACE=1", "RUSTUP_TOOLCHAIN=stable", "CGO_ENABLED=0")
	for name, c := range map[string]struct {
		got, want []string
	}{
		"Environ":     {Environ(pinnedEnviron()), withGo},
		"no profiles": {EnvironFor(pinnedEnviron(), buildtool.Select(nil)), withGo},
		"cargo":       {EnvironFor(pinnedEnviron(), buildtool.Select([]string{"cargo"})), cargo},
		"go, cargo":   {EnvironFor(pinnedEnviron(), buildtool.Select([]string{"go", "cargo"})), goCargo},
		"empty":       {Environ(nil), nil},
	} {
		if !slices.Equal(c.got, c.want) || (c.want == nil) != (c.got == nil) {
			t.Errorf("%s = %#v\nwant %#v", name, c.got, c.want)
		}
	}
}
