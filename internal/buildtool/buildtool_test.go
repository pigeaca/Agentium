package buildtool

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/runner"
)

// Every profile keeps the table's isolation contracts: caches stay inside the folder they are given, the allowlist
// passes no credentials, the user's caches are absolute, and patterns compile.
func TestProfilesKeepTheirContracts(t *testing.T) {
	names := map[string]bool{}
	for _, p := range Profiles() {
		if p.Name == "" || names[p.Name] {
			t.Errorf("profile name %q is empty or repeated", p.Name)
		}
		names[p.Name] = true
		if len(p.Detect) > 0 && p.TestCommand == nil {
			t.Errorf("%s: detected but proposes no test command", p.Name)
		}
		for _, c := range append(slices.Clone(p.CommandCaches), p.AgentCaches...) {
			if c.Name == "" || (c.Dir != "" && !filepath.IsLocal(c.Dir)) || (c.Clear && c.Dir != "") {
				t.Errorf("%s: cache variable %+v must name a folder inside the cache root, or clear the variable", p.Name, c)
			}
		}
		for _, name := range p.EnvNames {
			if name == "" || runner.IsCredential(name) || reserved(name, false) {
				t.Errorf("%s: allowlists %q, which is empty, a credential or reserved", p.Name, name)
			}
		}
		for _, prefix := range p.EnvPrefixes {
			if prefix == "" || reserved(prefix, true) {
				t.Errorf("%s: allowlists the prefix %q, which is empty or overlaps a reserved one", p.Name, prefix)
			}
		}
		if p.AgentEnv != nil {
			for _, kv := range p.AgentEnv(agentFixture, agentFixture, "/nonexistent-home") {
				name, _, _ := strings.Cut(kv, "=")
				if name == "" || runner.IsCredential(name) || reserved(name, false) {
					t.Errorf("%s: AgentEnv sets %q, which is empty, a credential or reserved", p.Name, name)
				}
			}
		}
		for _, f := range append(slices.Clone(p.Configs), p.Detect...) {
			if !filepath.IsLocal(f) {
				t.Errorf("%s: %q is not a path inside the repository", p.Name, f)
			}
		}
		for _, pattern := range p.TestPatterns {
			if _, err := regexp.Compile(`\b(` + pattern + `|other)\b`); err != nil || strings.ContainsAny(pattern, "^$") {
				t.Errorf("%s: test pattern %q: %v", p.Name, pattern, err)
			}
		}
		if p.UserCaches != nil {
			for _, path := range p.UserCaches([]string{"GOCACHE=relative", "XDG_CACHE_HOME=x"}, "/home/u") {
				if !filepath.IsAbs(path) {
					t.Errorf("%s: user cache %q is not absolute", p.Name, path)
				}
			}
		}
	}
	// Profiles returns a copy: a caller cannot change the table.
	first := Profiles()
	first[0].EnvNames[0] = "CHANGED"
	if Profiles()[0].EnvNames[0] == "CHANGED" {
		t.Error("Profiles shares its table with callers")
	}
}

// reservedPrefixes are kept out of the agent's environment only because no allowlist entry matches them: git's
// redirections, Claude Code's own settings (CLAUDE_CODE_SUBPROCESS_ENV_SCRUB forces the default permission mode),
// Agentium's and Anthropic's credentials. The run sets the ones it needs itself.
var reservedPrefixes = []string{"GIT_", "CLAUDE", "AGENTIUM_", "ANTHROPIC_"}

// reserved reports whether a name (or, with prefix, every name a prefix allows) could be a reserved one.
func reserved(s string, prefix bool) bool {
	for _, r := range reservedPrefixes {
		if strings.HasPrefix(s, r) || (prefix && strings.HasPrefix(r, s)) {
			return true
		}
	}
	return false
}

// agentFixture is a user's environment with the build tools' variables, credentials and reserved names.
var agentFixture = []string{"PATH=/usr/bin", "HOME=/home/u", "GOFLAGS=-mod=mod", "GOENV=off", "GOCACHE=/home/u/gocache",
	"GITHUB_TOKEN=x", "ANTHROPIC_API_KEY=x", "CLAUDE_CODE_OAUTH_TOKEN=x", "CLAUDE_CONFIG_DIR=/home/u/.c", "GIT_DIR=/r/.git",
	"AGENTIUM_HOME=/data", "AWS_SECRET_ACCESS_KEY=x"} // secret-scan: allow (fake values)

// The reserved check itself: names and prefixes that would let a reserved variable through are caught.
func TestReservedNames(t *testing.T) {
	for _, c := range []struct {
		s      string
		prefix bool
		want   bool
	}{{"GIT_DIR", false, true}, {"CLAUDE_CODE_X", false, true}, {"GOFLAGS", false, false}, {"G", true, true},
		{"CLA", true, true}, {"AGENTIUM_X", true, true}, {"CGO_", true, false}, {"GITHUB", false, false}} {
		if got := reserved(c.s, c.prefix); got != c.want {
			t.Errorf("reserved(%q, %v) = %v", c.s, c.prefix, got)
		}
	}
}

func TestCacheVarValue(t *testing.T) {
	if got := (CacheVar{Name: "GOCACHE", Dir: "go-build"}).Value("/data/cache"); got != "GOCACHE=/data/cache/go-build" {
		t.Errorf("a folder: %s", got)
	}
	if got := (CacheVar{Name: "GOCACHE"}).Value("/run/go-build"); got != "GOCACHE=/run/go-build" {
		t.Errorf("the root itself: %s", got)
	}
	if got := (CacheVar{Name: "GOCACHEPROG", Clear: true}).Value("/data/cache"); got != "GOCACHEPROG=" {
		t.Errorf("a cleared variable: %s", got)
	}
}

func TestTestCommandsNeedAMarker(t *testing.T) {
	has := func(files ...string) func(string) bool {
		return func(name string) bool { return slices.Contains(files, name) }
	}
	if got := TestCommands(has("go.mod", "README.md")); !slices.Equal(got, []string{"go test ./..."}) {
		t.Errorf("a Go module: %q", got)
	}
	if got := TestCommands(has("README.md", "tools/go.mod")); len(got) != 0 {
		t.Errorf("no marker at the root: %q", got)
	}
}

// The agent's GOFLAGS: ours after the user's from the environment, or else after those saved with `go env -w`; GOENV=off
// and a missing file give ours alone.
func TestGoAgentEnv(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("GOFLAGS=-tags=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		allowed, environ []string
		want             string
	}{
		{nil, nil, "GOFLAGS=-buildvcs=false"},
		{[]string{"GOFLAGS=-mod=mod"}, []string{"GOFLAGS=-mod=mod", "GOENV=" + envFile}, "GOFLAGS=-mod=mod -buildvcs=false"},
		{nil, []string{"GOENV=" + envFile}, "GOFLAGS=-tags=x -buildvcs=false"},
		{nil, []string{"GOENV=off"}, "GOFLAGS=-buildvcs=false"},
		{nil, []string{"GOENV=" + envFile + ".missing"}, "GOFLAGS=-buildvcs=false"},
	} {
		if got := AgentEnv(c.allowed, c.environ, "/nonexistent-home"); !slices.Equal(got, []string{c.want}) {
			t.Errorf("AgentEnv(%q, %q) = %q, want %s", c.allowed, c.environ, got, c.want)
		}
	}
}

func TestAllowlistAndRunners(t *testing.T) {
	names, prefixes := EnvAllowlist()
	if !slices.Contains(names, "GOFLAGS") || !slices.Contains(prefixes, "CGO_") || slices.Contains(names, "GOCACHEPROG") {
		t.Errorf("Go's allowlist: %q, %q", names, prefixes)
	}
	if got := RunnerConfigs()["go"]; !slices.Equal(got, []string{"go.mod"}) {
		t.Errorf("go's configuration: %q", got)
	}
	if got := strings.Join(TestPatterns(), "|"); got != "go test" {
		t.Errorf("test patterns: %s", got)
	}
}

// Detected follows the Detect files, and a detected profile names the languages task mine keeps.
func TestDetected(t *testing.T) {
	if got := Detected(func(string) bool { return false }); len(got) != 0 {
		t.Fatalf("nothing at the root detects %d profile(s)", len(got))
	}
	got := Detected(func(name string) bool { return name == "go.mod" })
	if len(got) != 1 || got[0].Name != "go" || !slices.Equal(got[0].Languages, []string{"go"}) {
		t.Fatalf("go.mod detects %+v", got)
	}
	for _, p := range Profiles() {
		if len(p.Detect) > 0 && len(p.Languages) == 0 {
			t.Errorf("%s: detected but names no test language", p.Name)
		}
	}
}
