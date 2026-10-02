// Package project inspects a repository for `agentium init`: its git root and commit, the Claude Code CLI, the sign-in
// mode, candidate test commands and the instruction files present. It only reads: nothing is written to the repository.
package project

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/gitx"
)

// MinClaudeVersion is the oldest Claude Code with the flags isolated runs need (`--permission-prompts none`).
const MinClaudeVersion = "2.1.259"

// Info is what `agentium init` found. It never holds credential values.
type Info struct {
	Root         string     `json:"root"`
	Head         string     `json:"head"`
	Claude       ClaudeInfo `json:"claude"`
	TestCommands []string   `json:"test_commands"`
	Instructions []File     `json:"instructions"`
	Skills       int        `json:"skills"`
	Rules        int        `json:"rules"`
	Warnings     []string   `json:"warnings"`
}

// ClaudeInfo describes the Claude Code CLI Agentium would run.
type ClaudeInfo struct {
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	SignIn  string `json:"sign_in"` // api-key | token-file | login
}

// File is an instruction file at the repository root.
type File struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// Env is the process environment discovery reads; tests supply their own.
type Env struct {
	Getenv   func(string) string
	LookPath func(string) (string, error)
}

// Sign-in modes, in the order Agentium prefers them.
const (
	SignInAPIKey    = "api-key"    // ANTHROPIC_API_KEY is set: runs get it, and a fresh config folder
	SignInTokenFile = "token-file" // a `claude setup-token` token file: runs get it, and a fresh config folder
	SignInLogin     = "login"      // the user's own Claude login, restricted to project settings; checked on the first run
)

// TokenFile is where a `claude setup-token` token is looked for (override: AGENTIUM_CLAUDE_TOKEN_FILE).
func TokenFile(env Env) string {
	if file := env.Getenv("AGENTIUM_CLAUDE_TOKEN_FILE"); file != "" {
		return file
	}
	if home := env.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "agentium", "claude-oauth-token")
	}
	return "" // never a path relative to the working directory
}

// Discover inspects the repository containing dir.
func Discover(ctx context.Context, dir string, env Env) (Info, error) {
	root, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if ctx.Err() != nil {
		return Info{}, fmt.Errorf("discover %s: %w", dir, ctx.Err())
	}
	if err != nil {
		return Info{}, fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return Info{}, fmt.Errorf("resolve repository root: %w", err)
	}
	head, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return Info{}, fmt.Errorf("repository %s has no commits yet: %w", root, err)
	}
	info := Info{Root: root, Head: head}
	info.Claude, info.Warnings = detectClaude(ctx, env)
	info.TestCommands = testCommands(root)
	if len(info.TestCommands) == 0 {
		info.TestCommands = []string{} // stored as [], not null
		info.Warnings = append(info.Warnings, "No test command detected: tasks will need explicit verification commands.")
	}
	info.Instructions, info.Skills, info.Rules = instructionFiles(root)
	if len(info.Instructions) == 0 {
		info.Warnings = append(info.Warnings, "No CLAUDE.md or AGENTS.md: there is no project context to compare yet.")
	}
	return info, nil
}

// JSON is the stored form of Info.
func (i Info) JSON() ([]byte, error) {
	return json.Marshal(i)
}

func detectClaude(ctx context.Context, env Env) (ClaudeInfo, []string) {
	var warnings []string
	info := ClaudeInfo{SignIn: SignInLogin}
	switch {
	case env.Getenv("ANTHROPIC_API_KEY") != "": // presence only; the value is never read
		info.SignIn = SignInAPIKey
	case fileExists(TokenFile(env)):
		info.SignIn = SignInTokenFile
	}
	path := env.Getenv("AGENTIUM_CLAUDE")
	if path == "" {
		found, err := env.LookPath("claude")
		if err != nil {
			return info, append(warnings, "Claude Code not found on PATH: install it, or set AGENTIUM_CLAUDE to its path.")
		}
		path = found
	}
	info.Path = path
	version, err := ClaudeVersion(ctx, path)
	if err != nil {
		return info, append(warnings, fmt.Sprintf("Could not read the Claude Code version from %s: %v", path, err))
	}
	info.Version = version
	if olderThan(version, MinClaudeVersion) {
		warnings = append(warnings, fmt.Sprintf("Claude Code %s is older than %s; experiments need %s or later.", version, MinClaudeVersion, MinClaudeVersion))
	}
	return info, warnings
}

var versionPattern = regexp.MustCompile(`\b(\d+\.\d+\.\d+)\b`)

// ClaudeVersion runs the CLI at path with --version, outside any repository, and returns its dotted version.
func ClaudeVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Dir = os.TempDir()          // not the user's repository: the CLI must not see or write it
	cmd.WaitDelay = 2 * time.Second // a child that keeps stdout open cannot hold Output past the timeout
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	match := versionPattern.FindStringSubmatch(string(out))
	if match == nil {
		return "", fmt.Errorf("unrecognized output %q", strings.TrimSpace(string(out)))
	}
	return match[1], nil
}

// olderThan compares dotted numeric versions.
func olderThan(version, minimum string) bool {
	a, b := strings.Split(version, "."), strings.Split(minimum, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		x, _ := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if x != y {
			return x < y
		}
	}
	return len(a) < len(b)
}

// testCommands proposes verification commands from the files at the repository root: the build tools' first, from
// their profiles, then other runners'.
func testCommands(root string) []string {
	commands := buildtool.TestCommands(func(name string) bool { return fileExists(filepath.Join(root, name)) })
	if script := npmTestScript(filepath.Join(root, "package.json")); script {
		switch {
		case fileExists(filepath.Join(root, "pnpm-lock.yaml")):
			commands = append(commands, "pnpm test")
		case fileExists(filepath.Join(root, "yarn.lock")):
			commands = append(commands, "yarn test")
		default:
			commands = append(commands, "npm test")
		}
	}
	if usesPytest(root) {
		commands = append(commands, pytestCommand(root))
	}
	if makefileHasTarget(filepath.Join(root, "Makefile"), "test") {
		commands = append(commands, "make test")
	}
	if fileExists(filepath.Join(root, "scripts", "harness.py")) {
		commands = append(commands, "python3 scripts/harness.py check ci")
	}
	return commands
}

// usesPytest reports whether the repository's files name pytest: its configuration, or a locked dependency on it.
func usesPytest(root string) bool {
	for _, name := range []string{"pyproject.toml", "pytest.ini", "setup.cfg", "tox.ini"} {
		if fileContains(filepath.Join(root, name), "pytest") || (name == "pytest.ini" && fileExists(filepath.Join(root, name))) {
			return true
		}
	}
	return fileContains(filepath.Join(root, "uv.lock"), `name = "pytest"`)
}

// pytestCommand runs pytest in the project's venv, which a run's warm-up builds (internal/buildtool's Python profile):
// with uv.lock through uv (`uv run` then uses that venv and syncs nothing), otherwise with python3, which the venv puts
// first on PATH (and which is what a host without the venv has).
func pytestCommand(root string) string {
	if fileExists(filepath.Join(root, "uv.lock")) {
		return "uv run pytest"
	}
	return "python3 -m pytest"
}

func npmTestScript(file string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return false
	}
	script, ok := manifest.Scripts["test"]
	return ok && !strings.Contains(script, "no test specified") // npm init's placeholder always fails
}

func makefileHasTarget(file, target string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), target+":") {
			return true
		}
	}
	return false
}

// instructionFiles lists root-level instruction files and counts project skills and rules. Personal files
// (CLAUDE.local.md) are deliberately left out: experiments never use them.
func instructionFiles(root string) (files []File, skills, rules int) {
	for _, name := range []string{"CLAUDE.md", ".claude/CLAUDE.md", "AGENTS.md"} {
		if info, err := os.Stat(filepath.Join(root, name)); err == nil && info.Mode().IsRegular() {
			files = append(files, File{Path: name, Bytes: info.Size()})
		}
	}
	skillFiles, _ := filepath.Glob(filepath.Join(root, ".claude", "skills", "*", "SKILL.md"))
	filepath.WalkDir(filepath.Join(root, ".claude", "rules"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") { // Claude Code loads nested rule folders too
			rules++
		}
		return nil
	})
	return files, len(skillFiles), rules
}

// gitOutput runs git in dir through gitx: no hooks, no prompts, no inherited GIT_DIR.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return gitx.Run(ctx, append([]string{"-C", dir}, args...)...)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), needle)
}
