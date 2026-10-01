package run

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
	discover "github.com/pigeaca/agentium/internal/project"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// TestGoProfileGolden pins everything a Go project's runs depend on from the build tool: the agent's arguments and
// environment, its denied paths, the environment of Agentium's own commands, discovery's test commands, the files
// grading treats as checks, and test-runner detection. It was recorded before Go's special cases moved into a build-tool
// profile, so the refactor (and every later profile) must leave a Go project's runs exactly as they were. The
// sandbox's denied paths and the environment allowlist guard against hidden-test leaks and credential exposure: a
// change to this file is a security change, to be checked line by line, never regenerated blindly.
func TestGoProfileGolden(t *testing.T) {
	var out bytes.Buffer
	scratch := t.TempDir()
	replace := map[string]string{} // temporary paths -> stable placeholders

	// The agent's command. Paths are fixed and do not exist, so no symlink resolution adds machine-specific forms;
	// only the GOENV files live in a temporary folder, and they are replaced by placeholders.
	savedFlags := filepath.Join(scratch, "goenv-flags")
	writeFile(t, savedFlags, "GOFLAGS=-tags=integration\nGOCACHE=/golden/saved-cache\nGOPROXY=off\n")
	savedCache := filepath.Join(scratch, "goenv-cache")
	writeFile(t, savedCache, "GOCACHE=/golden/written-cache\n")
	replace[savedFlags], replace[savedCache] = "<GOENV-FLAGS>", "<GOENV-CACHE>"

	user := []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "HOME=/golden/home", "USER=u", "SHELL=/bin/zsh", "TMPDIR=/golden/tmp",
		"LANG=en_US.UTF-8", "LC_ALL=C", "TERM=xterm", "GOPATH=/golden/home/go", "GOROOT=/usr/local/go", "GOMODCACHE=/golden/home/go/pkg/mod",
		"GOPROXY=https://proxy.golang.org", "GOPRIVATE=example.com", "GOTOOLCHAIN=local", "GOTMPDIR=/golden/gotmp", "CGO_ENABLED=1",
		"GOEXPERIMENT=", "GODEBUG=x=1", "GOMAXPROCS=4", "GOCACHEPROG=/golden/bin/cacheprog", "JAVA_HOME=/golden/jdk",
		"CARGO_HOME=/golden/home/.cargo", "RUSTUP_HOME=/golden/home/.rustup", "RUSTC_WRAPPER=sccache", "XDG_CONFIG_HOME=/golden/home/.config",
		"PYTHONPATH=/golden/py", "NODE_OPTIONS=--max-old-space-size=4096", "HOMEBREW_PREFIX=/opt/homebrew", "HTTPS_PROXY=http://proxy:3128",
		"SSL_CERT_FILE=/golden/certs.pem", "GRADLE_USER_HOME=/golden/home/.gradle", "MAVEN_OPTS=-Xmx1g", "EDITOR=vim",
		"ANTHROPIC_API_KEY=parent-key", "CLAUDE_CODE_OAUTH_TOKEN=parent-token", "CLAUDE_CONFIG_DIR=/golden/home/.claude-work",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1", "GITHUB_TOKEN=parent-gh", "GIT_DIR=/golden/repo/.git", "AGENTIUM_HOME=/golden/data",
		"SSH_AUTH_SOCK=/golden/agent.sock", "GOPROXY_TOKEN=parent-proxy"}
	base := claude.Invocation{CLI: "/golden/bin/claude", Dir: "/golden/data/workspaces/r1/repo", Prompt: "Fix the parser.",
		Model: "claude-sonnet-5", Effort: "medium", BudgetUSD: 3, Home: "/golden/home",
		Deny: []string{"/golden/data/projects", "/golden/data/records", "/golden/repo"}}
	cases := []struct {
		name    string
		inv     func(claude.Invocation) claude.Invocation
		environ []string
	}{
		{"login, no build cache, nothing Go-specific set", func(inv claude.Invocation) claude.Invocation {
			inv.SignIn = claude.SignInLogin
			return inv
		}, []string{"PATH=/usr/bin:/bin", "HOME=/golden/home"}},
		{"login, the run's build cache, the user's Go caches and flags in the environment", func(inv claude.Invocation) claude.Invocation {
			inv.SignIn = claude.SignInLogin
			inv.BuildCache = "/golden/data/workspaces/r1/go-build"
			return inv
		}, append(slices.Clone(user), "GOCACHE=/golden/home/gocache", "GOFLAGS=-mod=mod -race", "XDG_CACHE_HOME=/golden/home/xdg", "GOENV="+savedFlags)},
		{"api key, the run's build cache, flags and a cache saved with go env -w", func(inv claude.Invocation) claude.Invocation {
			inv.SignIn, inv.Secret, inv.ConfigDir = claude.SignInAPIKey, "run-key", "/golden/data/workspaces/r1/config"
			inv.BuildCache = "/golden/data/workspaces/r1/go-build"
			return inv
		}, append(slices.Clone(user), "GOENV="+savedFlags)},
		{"token file, values Go ignores (GOCACHE=off, a relative XDG_CACHE_HOME)", func(inv claude.Invocation) claude.Invocation {
			inv.SignIn, inv.Secret, inv.ConfigDir, inv.TokenFile = claude.SignInTokenFile, "run-token", "/golden/data/workspaces/r1/config", "/golden/home/.config/agentium/claude-oauth-token"
			inv.BuildCache = "/golden/data/workspaces/r1/go-build"
			return inv
		}, append(slices.Clone(user), "GOCACHE=off", "XDG_CACHE_HOME=relative", "GOENV="+savedCache)},
		{"login, GOENV=off and no saved flags", func(inv claude.Invocation) claude.Invocation {
			inv.SignIn = claude.SignInLogin
			inv.BuildCache = "/golden/data/workspaces/r1/go-build"
			return inv
		}, append(slices.Clone(user), "GOENV=off", "GOCACHE=/golden/home/gocache")},
	}
	for _, c := range cases {
		inv := c.inv(base)
		args, env, err := inv.Command(c.environ)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		fmt.Fprintf(&out, "== agent: %s\n-- args\n", c.name)
		for _, a := range args {
			fmt.Fprintf(&out, "%s\n", a)
		}
		fmt.Fprintf(&out, "-- env\n")
		for _, kv := range env {
			fmt.Fprintf(&out, "%s\n", kv)
		}
		fmt.Fprintf(&out, "-- denied paths\n")
		for _, p := range inv.DeniedPaths(c.environ) {
			fmt.Fprintf(&out, "%s\n", p)
		}
	}
	fmt.Fprintf(&out, "== Environ (the allowlist alone)\n")
	for _, kv := range claude.Environ(append(slices.Clone(user), "GOCACHE=/golden/home/gocache", "GOFLAGS=-mod=mod", "GOENV=/golden/goenv")) {
		fmt.Fprintf(&out, "%s\n", kv)
	}

	// The environment of Agentium's own commands.
	cache := filepath.Join(scratch, "data", "cache")
	replace[cache] = "<CACHE>"
	buildEnv, err := BuildEnv(home.Layout{Root: filepath.Join(scratch, "data"), Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(cache, "tmp")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("BuildEnv's temporary folder: %v, %v", info, err)
	}
	fmt.Fprintf(&out, "== BuildEnv\n")
	for _, kv := range buildEnv {
		fmt.Fprintf(&out, "%s\n", kv)
	}

	// Discovery's proposed test commands.
	discoveries := []struct {
		name  string
		files map[string]string
	}{
		{"go.mod only", map[string]string{"go.mod": "module example.com/m\n\ngo 1.27\n"}},
		{"go.mod, Makefile with test, harness", map[string]string{"go.mod": "module example.com/m\n", "Makefile": "build:\n\tgo build\ntest:\n\tgo test ./...\n",
			"scripts/harness.py": "print('ok')\n"}},
		{"go.mod in a subfolder only", map[string]string{"tools/go.mod": "module example.com/tools\n", "README.md": "x\n"}},
		{"every tool discovery knows", map[string]string{"go.mod": "module example.com/m\n", "package.json": `{"scripts":{"test":"jest"}}`,
			"yarn.lock": "", "pyproject.toml": "[tool.pytest.ini_options]\n", "Makefile": "test:\n\ttrue\n", "scripts/harness.py": "",
			"Cargo.toml": "[package]\n", "pom.xml": "<project/>\n", "build.gradle": "", "gradlew": ""}},
	}
	noClaude := discover.Env{Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "", errors.New("not found") }}
	for _, d := range discoveries {
		root := goldenRepo(t, d.files)
		info, err := discover.Discover(context.Background(), root, noClaude)
		if err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		fmt.Fprintf(&out, "== discovery: %s\n%q\n", d.name, info.TestCommands)
	}

	// The files grading treats as the verification's scripts and test-runner configuration. Configurations are sorted:
	// their order follows a map today, and only membership matters to grading.
	files := fakeSource{"Cargo.toml", "GNUmakefile", "Makefile", "conftest.py", "go.mod", "go.sum", "go.work", "jest.config.js",
		"package.json", "pytest.ini", "scripts/check.sh", "test.sh", "vitest.config.ts"}
	sort.Strings(files)
	for _, verify := range [][]string{{"go test ./..."}, {"go vet ./... && go test -race -count=1 ./pkg/..."}, {"GOFLAGS=-mod=mod go test ./..."},
		{"gofmt -l ."}, {"make test"}, {"./scripts/check.sh"}, {"sh scripts/check.sh go"}, {"bash test.sh"}, {"cargo test"},
		{"go test ./...", "make lint", "npx jest"}, {"golangci-lint run", "go-junit-report"}, {"python3 -m pytest -q"}, {"vitest run"}} {
		scripts, configs := checkFiles(verify, files)
		sort.Strings(configs)
		fmt.Fprintf(&out, "== checkFiles %q\nscripts %q\nconfigs %q\n", verify, scripts, configs)
	}

	// Test-runner detection in the agent's commands.
	fmt.Fprintf(&out, "== ranTests\n")
	for _, c := range []string{"go test ./...", "cd internal && go test -run TestX ./run", "GOFLAGS=-mod=mod go test ./...", "go vet ./...",
		"go build ./...", "gotestsum ./...", "go tool test2json", "cat go.test", "make test", "make lint", "python3 scripts/harness.py check go",
		"cargo test", "cargo nextest run", "mvn test", "mvn -q test", "./mvnw -q test", "mvn verify", "./gradlew test", "gradle test",
		"npm test", "pnpm run test", "bun test", "python -m pytest", "python3 -m unittest", "jest", "vitest", "rspec", "dotnet test"} {
		fmt.Fprintf(&out, "%v %s\n", ranTests([]string{c}), c)
	}

	// Added after the refactor, at the end so earlier lines stay as recorded: without a build cache of the run's own,
	// the user's GOCACHE reaches the agent as it is (and is still denied to it).
	noCache := base
	noCache.SignIn = claude.SignInLogin
	noCacheEnviron := append(slices.Clone(user), "GOCACHE=/golden/home/gocache")
	_, env, err := noCache.Command(noCacheEnviron)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&out, "== agent: login, no build cache, the user's GOCACHE\n-- env\n")
	for _, kv := range env {
		fmt.Fprintf(&out, "%s\n", kv)
	}
	fmt.Fprintf(&out, "-- denied paths\n")
	for _, p := range noCache.DeniedPaths(noCacheEnviron) {
		fmt.Fprintf(&out, "%s\n", p)
	}

	got := out.String()
	keys := make([]string, 0, len(replace))
	for k := range replace {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) }) // longer paths first
	for _, k := range keys {
		got = strings.ReplaceAll(got, k, replace[k])
	}
	if strings.Contains(got, scratch) {
		t.Fatalf("a temporary path is left in the output:\n%s", got)
	}
	path := filepath.Join("testdata", "go-profile.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, got)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("a Go project's runs changed (compare with %s; run with -update only after checking every line):\n%s", path, lineDiff(string(want), got))
	}
}

// fakeSource is a source.Source of file names only; its paths must be sorted.
type fakeSource []string

func (f fakeSource) Paths() []string                 { return f }
func (f fakeSource) ReadFile(string) ([]byte, error) { return nil, errors.New("not read") }
func (f fakeSource) Executable(string) bool          { return false }
func (f fakeSource) Describe() string                { return "fixture" }

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// goldenRepo is a git repository with files committed.
func goldenRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		writeFile(t, filepath.Join(dir, name), body)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "fixture"}} {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// lineDiff lists the lines that differ, by position: enough to see what moved.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	for i := 0; i < len(w) || i < len(g); i++ {
		var x, y string
		if i < len(w) {
			x = w[i]
		}
		if i < len(g) {
			y = g[i]
		}
		if x != y {
			fmt.Fprintf(&b, "line %d:\n- %s\n+ %s\n", i+1, x, y)
		}
	}
	return b.String()
}
