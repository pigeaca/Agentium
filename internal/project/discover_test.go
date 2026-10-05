package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// repo creates a git repository with the given files committed.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fakeClaude writes an executable that prints the given version output.
func fakeClaude(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+output+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEnv(values map[string]string, claude string) Env {
	return Env{
		Getenv: func(key string) string { return values[key] },
		LookPath: func(name string) (string, error) {
			if claude == "" || name != "claude" {
				return "", errors.New("not found")
			}
			return claude, nil
		},
	}
}

func TestDiscoverFindsCommandsInstructionsAndClaude(t *testing.T) {
	dir := repo(t, map[string]string{
		"go.mod":                          "module x\n",
		"package.json":                    `{"scripts":{"test":"vitest run"}}`,
		"pnpm-lock.yaml":                  "lockfileVersion: 9\n",
		"pyproject.toml":                  "[tool.pytest.ini_options]\n",
		"Makefile":                        "build:\n\techo\ntest:\n\tgo test ./...\n",
		"scripts/harness.py":              "",
		"CLAUDE.md":                       "@AGENTS.md\n",
		"AGENTS.md":                       "# Rules\n",
		"CLAUDE.local.md":                 "personal\n",
		".claude/skills/review/SKILL.md":  "---\nname: review\n---\n",
		".claude/rules/go.md":             "rule\n",
		".claude/skills/notaskill/README": "",
	})
	claude := fakeClaude(t, "2.1.281 (Claude Code)")
	info, err := Discover(context.Background(), filepath.Join(dir, "scripts"), testEnv(map[string]string{"HOME": t.TempDir()}, claude))
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := filepath.EvalSymlinks(dir)
	if info.Root != wantRoot || len(info.Head) != 40 {
		t.Errorf("root/head = %q/%q", info.Root, info.Head)
	}
	wantCommands := []string{"go test ./...", "pnpm test", "python3 -m pytest", "make test", "python3 scripts/harness.py check ci"}
	if !reflect.DeepEqual(info.TestCommands, wantCommands) {
		t.Errorf("test commands = %q, want %q", info.TestCommands, wantCommands)
	}
	wantFiles := []File{{Path: "CLAUDE.md", Bytes: 11}, {Path: "AGENTS.md", Bytes: 8}}
	if !reflect.DeepEqual(info.Instructions, wantFiles) || info.Skills != 1 || info.Rules != 1 {
		t.Errorf("instructions = %+v, skills %d, rules %d", info.Instructions, info.Skills, info.Rules)
	}
	if info.Claude != (ClaudeInfo{Path: claude, Version: "2.1.281", SignIn: SignInLogin}) {
		t.Errorf("claude = %+v", info.Claude)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("unexpected warnings %q", info.Warnings)
	}
}

func TestSignInModesUsePresenceOnly(t *testing.T) {
	dir := repo(t, map[string]string{"CLAUDE.md": "x\n"})
	claude := fakeClaude(t, "2.1.281 (Claude Code)")
	home := t.TempDir()
	info, err := Discover(context.Background(), dir, testEnv(map[string]string{"HOME": home, "ANTHROPIC_API_KEY": "sk-test-value"}, claude))
	if err != nil {
		t.Fatal(err)
	}
	if info.Claude.SignIn != SignInAPIKey {
		t.Errorf("sign-in = %q, want api-key", info.Claude.SignIn)
	}
	stored, _ := info.JSON()
	if strings.Contains(string(stored), "sk-test-value") {
		t.Error("the API key value leaked into the stored discovery")
	}
	tokenFile := filepath.Join(home, ".config", "agentium", "claude-oauth-token")
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("secret-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ = Discover(context.Background(), dir, testEnv(map[string]string{"HOME": home}, claude))
	if info.Claude.SignIn != SignInTokenFile {
		t.Errorf("sign-in = %q, want token-file", info.Claude.SignIn)
	}
}

func TestWarnings(t *testing.T) {
	dir := repo(t, nil)
	info, err := Discover(context.Background(), dir, testEnv(map[string]string{"HOME": t.TempDir()}, ""))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(info.Warnings, "\n")
	for _, want := range []string{"Claude Code not found", "No test command", "No CLAUDE.md or AGENTS.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q lack %q", info.Warnings, want)
		}
	}
	old := fakeClaude(t, "2.1.132 (Claude Code)")
	info, _ = Discover(context.Background(), dir, testEnv(map[string]string{"HOME": t.TempDir(), "AGENTIUM_CLAUDE": old}, ""))
	if info.Claude.Path != old || !strings.Contains(strings.Join(info.Warnings, "\n"), "older than "+MinClaudeVersion) {
		t.Errorf("old version not flagged: %+v %q", info.Claude, info.Warnings)
	}
}

func TestDiscoverRejectsNonRepositoriesAndEmptyRepos(t *testing.T) {
	if _, err := Discover(context.Background(), t.TempDir(), testEnv(nil, "")); err == nil || !strings.Contains(err.Error(), "not inside a git repository") {
		t.Errorf("err = %v", err)
	}
	empty := t.TempDir()
	git(t, empty, "init", "-q")
	if _, err := Discover(context.Background(), empty, testEnv(nil, "")); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Errorf("err = %v", err)
	}
}

func TestOlderThan(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{{"2.1.258", "2.1.259", true}, {"2.1.259", "2.1.259", false}, {"2.2.0", "2.1.259", false}, {"2.1", "2.1.0", true}} {
		if got := olderThan(tt.a, tt.b); got != tt.want {
			t.Errorf("olderThan(%s, %s) = %v", tt.a, tt.b, got)
		}
	}
}

func TestNpmPlaceholderIsNotATestCommand(t *testing.T) {
	dir := repo(t, map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`})
	if commands := testCommands(dir); len(commands) != 0 {
		t.Errorf("commands = %q", commands)
	}
}

// init proposes each build tool's test command from fixture repositories, the repository's wrapper first.
func TestJVMAndRustTestCommands(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  []string
	}{
		"maven wrapper":   {map[string]string{"pom.xml": "<project/>", "mvnw": "#!/bin/sh\n"}, []string{"./mvnw -q test"}},
		"maven":           {map[string]string{"pom.xml": "<project/>"}, []string{"mvn -q test"}},
		"gradle wrapper":  {map[string]string{"build.gradle.kts": "", "settings.gradle.kts": "", "gradlew": "#!/bin/sh\n"}, []string{"./gradlew test"}},
		"gradle":          {map[string]string{"build.gradle": ""}, []string{"gradle test"}},
		"cargo":           {map[string]string{"Cargo.toml": "[package]\n"}, []string{"cargo test"}},
		"wrapper only":    {map[string]string{"mvnw": "#!/bin/sh\n", "gradlew": "#!/bin/sh\n"}, nil},
		"nested pom only": {map[string]string{"service/pom.xml": "<project/>"}, nil},
		"maven and cargo": {map[string]string{"pom.xml": "<project/>", "Cargo.toml": "[package]\n", "Makefile": "test:\n\ttrue\n"}, []string{"mvn -q test", "cargo test", "make test"}},
	} {
		if got := testCommands(repo(t, c.files)); !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("%s: commands %q, want %q", name, got, c.want)
		}
	}
}

func TestClaudeRunsOutsideTheRepositoryAndEdgeCases(t *testing.T) {
	dir := repo(t, map[string]string{".claude/rules/go.md": "a\n", ".claude/rules/web/react.md": "b\n"})
	// A CLI that drops a file wherever it runs: it must not run in the repository.
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\ntouch agentium-probe-$$\necho '2.1.281 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // it ran in the temporary folder; tidy up after it
		probes, _ := filepath.Glob(filepath.Join(os.TempDir(), "agentium-probe-*"))
		for _, p := range probes {
			os.Remove(p)
		}
	})
	t.Chdir(dir)                                                           // as when a user runs agentium inside their repository
	info, err := Discover(context.Background(), dir, testEnv(nil, claude)) // HOME unset
	if err != nil {
		t.Fatal(err)
	}
	if status := git(t, dir, "status", "--porcelain", "--ignored"); status != "" {
		t.Errorf("claude --version ran in the repository: %q", status)
	}
	if info.Claude.Version != "2.1.281" || info.Claude.SignIn != SignInLogin || info.Rules != 2 {
		t.Errorf("info = %+v (want version, login sign-in, 2 rules including the nested one)", info)
	}
	if TokenFile(testEnv(nil, "")) != "" {
		t.Error("without HOME the token file must not be a relative path")
	}
	stored, _ := info.JSON()
	if !strings.Contains(string(stored), `"test_commands":[]`) {
		t.Errorf("no test commands must be stored as []: %s", stored)
	}
}

// Python projects: pytest runs in the venv a run's warm-up builds, through uv when the project locks with uv; a project
// whose files never name pytest (unittest only) gets no proposal; the Python profile adds no command of its own.
func TestDiscoverProposesPytestPerPackageManager(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"uv with pytest in a dependency group (click)", map[string]string{"pyproject.toml": "[dependency-groups]\ntests = [\"pytest\"]\n",
			"uv.lock": "[[package]]\nname = \"pytest\"\n"}, []string{"uv run pytest"}},
		{"uv with pytest only in the lock", map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n", "uv.lock": "[[package]]\nname = \"pytest\"\n"},
			[]string{"uv run pytest"}},
		{"pip, pytest configured in setup.cfg", map[string]string{"setup.cfg": "[tool:pytest]\n", "requirements.txt": ""}, []string{"python3 -m pytest"}},
		{"pip, pytest.ini alone", map[string]string{"pytest.ini": "", "requirements.txt": ""}, []string{"python3 -m pytest"}},
		{"unittest only (more-itertools)", map[string]string{"pyproject.toml": "[project]\nname = \"more-itertools\"\n",
			"requirements/testing.txt": "coverage\n"}, nil},
		{"uv without pytest", map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n", "uv.lock": "[[package]]\nname = \"ruff\"\n"}, nil},
	} {
		info, err := Discover(context.Background(), repo(t, c.files), testEnv(map[string]string{"HOME": t.TempDir()}, ""))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(info.TestCommands, c.want) {
			t.Errorf("%s: test commands %q, want %q", c.name, info.TestCommands, c.want)
		}
	}
}

// Codex is optional: its absence is no warning; found (AGENTIUM_CODEX, else PATH), it is reported with its version, and
// a version Codex runs refuse is a warning. Its sign-in is told by presence only: an API key in the environment, else
// Agentium's own ChatGPT login (checked when a run starts, never here).
func TestDiscoverReportsCodex(t *testing.T) {
	dir := repo(t, map[string]string{"AGENTS.md": "x\n", "go.mod": "module x\n"})
	claude := fakeClaude(t, "2.1.281 (Claude Code)")
	home := t.TempDir()
	info, err := Discover(context.Background(), dir, testEnv(map[string]string{"HOME": home}, claude))
	if err != nil {
		t.Fatal(err)
	}
	if info.Codex != (CodexInfo{SignIn: "login"}) || len(info.Warnings) != 0 {
		t.Errorf("without Codex: %+v, warnings %q", info.Codex, info.Warnings)
	}
	supported := fakeClaude(t, "codex-cli 0.160.0")
	info, err = Discover(context.Background(), dir, testEnv(map[string]string{"HOME": home, "AGENTIUM_CODEX": supported, "CODEX_API_KEY": "sk-not-real"}, claude))
	if err != nil {
		t.Fatal(err)
	}
	if info.Codex != (CodexInfo{Path: supported, Version: "0.160.0", SignIn: "api-key"}) || len(info.Warnings) != 0 {
		t.Errorf("Codex 0.160.0: %+v, warnings %q", info.Codex, info.Warnings)
	}
	newer := fakeClaude(t, "codex-cli 0.161.2")
	info, err = Discover(context.Background(), dir, testEnv(map[string]string{"HOME": home, "AGENTIUM_CODEX": newer}, claude))
	if err != nil {
		t.Fatal(err)
	}
	if info.Codex.Version != "0.161.2" || len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], "runs Codex 0.160 only") {
		t.Errorf("Codex 0.161.2: %+v, warnings %q", info.Codex, info.Warnings)
	}
}
