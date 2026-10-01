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
			if runner.IsCredential(name) {
				t.Errorf("%s: allowlists %s, a credential", p.Name, name)
			}
		}
		for _, f := range append(slices.Clone(p.Configs), p.Detect...) {
			if !filepath.IsLocal(f) {
				t.Errorf("%s: %q is not a path inside the repository", p.Name, f)
			}
		}
		for _, pattern := range p.TestPatterns {
			if _, err := regexp.Compile(pattern); err != nil {
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
