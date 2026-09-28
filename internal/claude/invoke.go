// Package claude runs Claude Code headless for Agentium and reads what it reports. Each run is isolated the way the
// Phase 0 spike found necessary (docs/research/2026-09-27-phase0-spike-results.md): project settings only, no account
// connectors, a fixed permission mode, a sandbox without network that cannot read hidden paths or credentials, and an
// environment built from an allowlist. Checked against Claude Code 2.1.281.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// Sign-in modes (the same names as project discovery reports).
const (
	SignInAPIKey    = "api-key"    // ANTHROPIC_API_KEY, passed to the run only, with a fresh config folder
	SignInTokenFile = "token-file" // a `claude setup-token` token, passed to the run only, with a fresh config folder
	SignInLogin     = "login"      // the user's own login and config folder, restricted to project settings
)

// PermissionMode is the mode every run uses: edits are accepted, and nothing prompts.
const PermissionMode = "acceptEdits"

// Invocation is one headless Claude Code run.
type Invocation struct {
	CLI       string // path to the claude executable
	Dir       string // the run's checkout, where Claude Code starts
	Prompt    string
	Model     string
	Effort    string  // empty: the CLI's default
	BudgetUSD float64 // --max-budget-usd; 0: none
	SignIn    string  // SignInAPIKey, SignInTokenFile or SignInLogin
	Secret    string  // the API key or token for SignInAPIKey and SignInTokenFile; never logged or stored
	ConfigDir string  // a fresh, empty CLAUDE_CONFIG_DIR for SignInAPIKey and SignInTokenFile
	TokenFile string  // for SignInTokenFile: the token's file, whose folder the agent may not read
	Home      string  // the user's home folder
	// Deny lists absolute paths the agent must not read, through the sandboxed shell or the Read tool: Agentium's data
	// (other runs, hidden tests, the database), the user's repository, and verification copies.
	Deny []string
}

// UserConfigDir is the user's own Claude Code folder: $CLAUDE_CONFIG_DIR when set in environ, otherwise ~/.claude.
func UserConfigDir(environ []string, home string) string {
	for _, kv := range environ {
		if value, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok && value != "" {
			return value
		}
	}
	return filepath.Join(home, ".claude")
}

// DisallowedTools are outward-facing, scheduling and worktree tools, plus tools that appear only with some account
// setups, so every sign-in mode offers the agent the same tool set.
func DisallowedTools() []string {
	return []string{"WebSearch", "WebFetch", "Artifact", "DesignSync", "CronCreate", "CronDelete", "ScheduleWakeup", "Workflow",
		"SendMessage", "EnterWorktree", "ExitWorktree", "ArtifactComments", "ArtifactData", "Monitor", "PushNotification", "RemoteTrigger"}
}

// credentialFiles are the user's credential stores the sandbox denies (relative to the home folder).
func credentialFiles() []string {
	return []string{".ssh", ".codex", ".config/gh", ".config/agentium", ".netrc", ".git-credentials", ".aws", ".docker",
		".npmrc", ".pypirc", ".kube", ".gnupg"}
}

// Command returns the arguments and environment for the run. environ is the parent's environment (os.Environ()),
// filtered through an allowlist; the sign-in secret is the only credential the child receives.
func (inv Invocation) Command(environ []string) (args, env []string, err error) {
	switch inv.SignIn {
	case SignInAPIKey, SignInTokenFile:
		if inv.Secret == "" || inv.ConfigDir == "" {
			return nil, nil, fmt.Errorf("sign-in %s needs a secret and a fresh config folder", inv.SignIn)
		}
	case SignInLogin:
		if inv.Secret != "" {
			return nil, nil, errors.New("sign-in login takes no secret")
		}
	default:
		return nil, nil, fmt.Errorf("unknown sign-in mode %q", inv.SignIn)
	}
	if inv.CLI == "" || inv.Dir == "" || inv.Prompt == "" || inv.Model == "" || inv.Home == "" {
		return nil, nil, errors.New("a run needs the CLI, a folder, a prompt, a model and the home folder")
	}
	userConfig := UserConfigDir(environ, inv.Home)
	for _, p := range append([]string{userConfig, inv.Home}, inv.Deny...) {
		if !filepath.IsAbs(p) {
			return nil, nil, fmt.Errorf("path %q (a denied path, the home folder or CLAUDE_CONFIG_DIR) is not absolute", p)
		}
	}
	for _, p := range []string{inv.ConfigDir, inv.TokenFile} {
		if p != "" && !filepath.IsAbs(p) {
			return nil, nil, fmt.Errorf("path %q is not absolute", p)
		}
	}
	settings, err := json.Marshal(inv.settings(userConfig))
	if err != nil {
		return nil, nil, fmt.Errorf("encode settings: %w", err)
	}
	args = []string{"-p", inv.Prompt, "--model", inv.Model, "--output-format", "stream-json", "--verbose",
		"--permission-mode", PermissionMode, "--permission-prompts", "none", "--no-session-persistence",
		"--setting-sources", "project", // requirement 1: no user-level skills, settings or memory
		"--strict-mcp-config", // requirement 2: only MCP servers given here (none)
		"--disallowedTools", strings.Join(DisallowedTools(), ","), "--settings", string(settings)}
	if inv.Effort != "" {
		args = append(args, "--effort", inv.Effort)
	}
	if inv.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(inv.BudgetUSD, 'f', -1, 64))
	}
	env = append(Environ(environ),
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTOUPDATER=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false") // requirement 2: no claude.ai connectors
	switch inv.SignIn { // requirement 4: a fresh config folder cannot use a subscription login
	case SignInLogin:
		if userConfig != filepath.Join(inv.Home, ".claude") { // the user's login lives in their own config folder
			env = append(env, "CLAUDE_CONFIG_DIR="+userConfig)
		}
	case SignInAPIKey:
		env = append(env, "CLAUDE_CONFIG_DIR="+inv.ConfigDir, "ANTHROPIC_API_KEY="+inv.Secret)
	case SignInTokenFile:
		env = append(env, "CLAUDE_CONFIG_DIR="+inv.ConfigDir, "CLAUDE_CODE_OAUTH_TOKEN="+inv.Secret)
	}
	return args, env, nil
}

// historyPaths are the parts of a Claude Code config folder that record past work: session transcripts, file history,
// prompt history, todos, plans and the state file. Since tasks come from the user's own history, they can hold the
// task's solution.
func historyPaths() []string {
	return []string{"projects", "file-history", "history.jsonl", "todos", "sessions", "plans", ".claude.json"}
}

// deniedPaths are the paths the agent may not read, in every sign-in mode:
//   - inv.Deny;
//   - Claude Code's data. The run's active config folder (the user's in login mode, the fresh one otherwise) loses only
//     its history paths: Claude Code keeps working files there that its Bash tool reads, such as the shell snapshot.
//     Every other Claude folder (~/.claude, the user's CLAUDE_CONFIG_DIR, when not active) is denied whole, and so is
//     ~/.claude.json. The Claude Code process itself is not sandboxed, so none of this affects sign-in;
//   - credential stores and the token file's folder.
//
// Each path is cleaned, and its symlink-resolved form (/var and /private/var on macOS) is denied too.
func (inv Invocation) deniedPaths(userConfig string) []string {
	active := userConfig
	if inv.SignIn != SignInLogin {
		active = inv.ConfigDir
	}
	paths := append([]string{}, inv.Deny...)
	for _, name := range historyPaths() {
		paths = append(paths, filepath.Join(active, name))
	}
	for _, dir := range []string{filepath.Join(inv.Home, ".claude"), userConfig} {
		if filepath.Clean(dir) != filepath.Clean(active) {
			paths = append(paths, dir)
		}
	}
	paths = append(paths, filepath.Join(inv.Home, ".claude.json"))
	if inv.TokenFile != "" {
		paths = append(paths, filepath.Dir(inv.TokenFile))
	}
	for _, name := range credentialFiles() {
		paths = append(paths, filepath.Join(inv.Home, name))
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		forms := []string{filepath.Clean(p)}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			forms = append(forms, resolved)
		}
		for _, form := range forms {
			if !seen[form] {
				seen[form] = true
				out = append(out, form)
			}
		}
	}
	return out
}

// settings are the per-run Claude Code settings (--settings).
func (inv Invocation) settings(userConfig string) map[string]any {
	denied := inv.deniedPaths(userConfig)
	readRules := make([]string, len(denied))
	for i, p := range denied {
		readRules[i] = "Read(/" + p + "/**)" // an absolute path in a permission rule starts with //
	}
	var files []map[string]string
	for _, name := range credentialFiles() {
		files = append(files, map[string]string{"path": filepath.Join(inv.Home, name), "mode": "deny"})
	}
	if inv.TokenFile != "" {
		files = append(files, map[string]string{"path": filepath.Dir(inv.TokenFile), "mode": "deny"})
	}
	return map[string]any{
		"sandbox": map[string]any{
			"enabled": true, "failIfUnavailable": true, "allowUnsandboxedCommands": false, "autoAllowBashIfSandboxed": true,
			"network":    map[string]any{"strictAllowlist": true, "allowedDomains": []string{}},
			"filesystem": map[string]any{"denyRead": denied}, // requirement 5
			"credentials": map[string]any{
				"envVars": []map[string]string{{"name": "CLAUDE_CODE_OAUTH_TOKEN", "mode": "deny"}, {"name": "ANTHROPIC_API_KEY", "mode": "deny"}},
				"files":   files,
			},
		},
		"permissions":               map[string]any{"deny": readRules}, // the Read tool, which the sandbox does not cover
		"autoMemoryEnabled":         false,
		"disableClaudeAiConnectors": true, // requirement 2
	}
}

// Environ keeps what a coding agent's tools need from environ (system settings, proxies and certificates, toolchain
// variables) and nothing else: no credentials, no GIT_*, no AGENTIUM_*, no CLAUDE_* (in particular not
// CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, which silently forces the default permission mode: requirement 3). Values are
// passed as given: a proxy URL with a password in it would pass too.
func Environ(environ []string) []string {
	exact := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true,
		"LANG": true, "TERM": true, "TZ": true, "VIRTUAL_ENV": true, "JAVA_HOME": true, "CARGO_HOME": true,
		"RUSTUP_HOME": true, "PNPM_HOME": true, "BUN_INSTALL": true, "DENO_DIR": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "no_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
		"GOPATH": true, "GOROOT": true, "GOBIN": true, "GOCACHE": true, "GOMODCACHE": true, "GOENV": true, "GOFLAGS": true,
		"GOTOOLCHAIN": true, "GOPROXY": true, "GOPRIVATE": true, "GONOPROXY": true, "GONOSUMDB": true, "GOSUMDB": true,
		"GOINSECURE": true, "GOWORK": true, "GO111MODULE": true, "GOTMPDIR": true, "GOEXPERIMENT": true, "GODEBUG": true,
		"GOMAXPROCS": true, "GOGC": true, "GOMEMLIMIT": true, "GOOS": true, "GOARCH": true, "GOAMD64": true, "GOARM64": true}
	prefixes := []string{"LC_", "CGO_", "PYTHON", "NODE_", "NVM_", "CONDA_", "PIP_", "UV_", "RUSTC", "XDG_", "HOMEBREW_"}
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		keep := exact[name]
		for _, prefix := range prefixes {
			keep = keep || strings.HasPrefix(name, prefix)
		}
		if keep && !runner.IsCredential(name) {
			out = append(out, kv)
		}
	}
	return out
}

// ReadToken reads a `claude setup-token` token file. The file must be readable by its owner only and hold one token.
func ReadToken(file string) (string, error) {
	info, err := os.Stat(file)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file %s is readable by other users: run chmod 600 %s", file, file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(strings.Fields(token)) != 1 {
		return "", fmt.Errorf("token file %s must hold only the token", file)
	}
	return token, nil
}

// Run starts the invocation and waits for it: stdout (the stream-json transcript) goes to transcript, stderr to
// errOut. On timeout or cancel the run's process group is interrupted first, so Claude Code can finish its turn and
// report a result, then killed after grace.
func Run(ctx context.Context, inv Invocation, environ []string, transcript, errOut *os.File, timeout, grace time.Duration) (runner.Result, error) {
	args, env, err := inv.Command(environ)
	if err != nil {
		return runner.Result{}, err
	}
	return runner.Run(ctx, runner.Spec{Dir: inv.Dir, Args: append([]string{inv.CLI}, args...), Environ: env,
		Timeout: timeout, Grace: grace, Output: transcript, Stderr: errOut})
}
