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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
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
	// Started, when set, is called with the agent's process ID, which is also its process group, once it runs.
	Started func(pid int)
	// BuildCache, when set, is a folder of the run's own for build caches: the build tools' agent caches point there
	// (buildtool.AgentCacheEnv: Go's GOCACHE), and the sandbox lets the agent write it. The user's own caches are denied
	// (buildtool.UserCaches): they hold what earlier builds compiled, the hidden tests of validations and gradings included.
	BuildCache string
	// TempRoot, when set, is the run's own Claude Code temp root (CLAUDE_CODE_TMPDIR), an existing owner-only folder:
	// Claude Code keeps its temp files and sockets in <TempRoot>/claude-<uid>, and points the agent's shells' TMPDIR
	// there. Then the shared per-user folder every other Claude Code session of the user uses (SharedTempDirs) is
	// denied to the agent, for reading and writing: it would be a channel between runs, and a view of other sessions'
	// temp files. It must be short (TempRootFits): a socket path too long makes Claude Code fall back to the shared
	// folder. The sandbox lets the agent write it without an allowWrite entry (verified in a probe session).
	TempRoot string
	// UID is the user's id (os.Getuid()), which names Claude Code's temp folders (claude-<uid>); read only with TempRoot.
	UID int
}

// socketBudget is what Claude Code adds to its temp root for a socket, beyond "/claude-<uid>": "/cc-socks/" and a name
// of up to a 7-digit process id, "-", 8 hex characters and ".sock" (Claude Code 2.1.285). maxSocketPath is the
// longest socket path it accepts there (Unix sockets allow 104 bytes on macOS, with the final NUL); a longer one
// makes it fall back to the shared /tmp folder, which runs are denied.
const (
	socketBudget  = len("/cc-socks/") + len("1234567-0123abcd.sock")
	maxSocketPath = 103
)

// TempRootFits reports whether Claude Code can keep its sockets under root, as given and with symlinks resolved
// (/tmp is /private/tmp on macOS), for the user uid.
func TempRootFits(root string, uid int) error {
	for _, form := range forms(root) {
		if n := len(form) + len("/claude-"+strconv.Itoa(uid)) + socketBudget; n > maxSocketPath {
			return fmt.Errorf("the run's temp root %s is too long for Claude Code's sockets (%d bytes of %d): it would fall back to the shared temp folder", form, n, maxSocketPath)
		}
	}
	return nil
}

// SharedTempDirs are the user's Claude Code temp folders shared by all their sessions: /tmp/claude-<uid> in both its
// forms (the sandbox matches the resolved /private/tmp on macOS, the Read tool the path as written), and the folder
// under the user's own CLAUDE_CODE_TMPDIR, when environ sets one.
func SharedTempDirs(environ []string, uid int) []string {
	name := "claude-" + strconv.Itoa(uid)
	dirs := []string{filepath.Join("/tmp", name), filepath.Join("/private/tmp", name)}
	for _, kv := range environ {
		if value, ok := strings.CutPrefix(kv, "CLAUDE_CODE_TMPDIR="); ok && filepath.IsAbs(value) {
			if dir := filepath.Join(value, name); !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
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
	for _, p := range []string{inv.ConfigDir, inv.TokenFile, inv.BuildCache, inv.TempRoot} {
		if p != "" && !filepath.IsAbs(p) {
			return nil, nil, fmt.Errorf("path %q is not absolute", p)
		}
	}
	if inv.TempRoot != "" {
		if err := TempRootFits(inv.TempRoot, inv.UID); err != nil {
			return nil, nil, err
		}
	}
	settings, err := json.Marshal(inv.settings(userConfig, environ))
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
	// The build tools' own variables (Go's GOFLAGS) replace any of the same name the allowlist kept, and come right
	// after it; the run's build cache variables (Go's GOCACHE) replace the user's and come after Claude Code's own.
	allowed := Environ(environ)
	toolEnv := buildtool.AgentEnv(allowed, environ, inv.Home)
	replaced := map[string]bool{}
	for _, kv := range toolEnv {
		name, _, _ := strings.Cut(kv, "=")
		replaced[name] = true
	}
	if inv.BuildCache != "" {
		for _, name := range buildtool.AgentCacheNames() {
			replaced[name] = true
		}
	}
	for _, kv := range allowed {
		if name, _, _ := strings.Cut(kv, "="); !replaced[name] {
			env = append(env, kv)
		}
	}
	env = append(env, toolEnv...)
	env = append(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTOUPDATER=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false") // requirement 2: no claude.ai connectors
	if inv.BuildCache != "" {
		env = append(env, buildtool.AgentCacheEnv(inv.BuildCache)...)
	}
	if inv.TempRoot != "" { // the parent's own CLAUDE_CODE_TMPDIR was dropped with every CLAUDE_* (Environ)
		env = append(env, "CLAUDE_CODE_TMPDIR="+inv.TempRoot)
	}
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

// historyPaths are the parts of a Claude Code config folder the agent may not read: what records past work (file
// history, prompt history, todos, plans, the state file), since tasks come from the user's own history and can hold the
// task's solution; and the login itself (.credentials.json on Linux; macOS keeps it in the Keychain). Past session
// transcripts in projects/ are denied folder by folder (SessionFolders).
func historyPaths() []string {
	return []string{"file-history", "history.jsonl", "todos", "sessions", "plans", ".claude.json", ".credentials.json"}
}

// SessionFolders lists the session folders already in a config folder's projects/ (past sessions' transcripts). A run's
// own session folder is created after it starts, so it is not among them: Claude Code saves large tool outputs there,
// and the agent must be able to read them back.
func SessionFolders(configDir string) []string {
	entries, err := os.ReadDir(filepath.Join(configDir, "projects"))
	if err != nil {
		return nil
	}
	var folders []string
	for _, e := range entries {
		folders = append(folders, filepath.Join(configDir, "projects", e.Name()))
	}
	return folders
}

// deniedPaths are the paths the agent may not read, in every sign-in mode:
//   - inv.Deny;
//   - Claude Code's data. The run's active config folder (the user's in login mode, the fresh one otherwise) loses only
//     its history paths: Claude Code keeps working files there that its Bash tool reads, such as the shell snapshot.
//     Every other Claude folder (~/.claude, the user's CLAUDE_CONFIG_DIR, when not active) is denied whole, and so is
//     ~/.claude.json. The Claude Code process itself is not sandboxed, so none of this affects sign-in;
//   - credential stores and the token file's folder;
//   - the build tools' caches of the user (buildtool.UserCaches: Go's build caches), which hold hidden tests compiled
//     before Agentium kept its own;
//   - with a temp root of the run's own, the user's shared Claude Code temp folders (SharedTempDirs).
//
// Each path is cleaned, and its symlink-resolved form (/var and /private/var on macOS) is denied too.
func (inv Invocation) deniedPaths(userConfig string, environ []string) []string {
	active := userConfig
	if inv.SignIn != SignInLogin {
		active = inv.ConfigDir
	}
	paths := append([]string{}, inv.Deny...)
	for _, name := range historyPaths() {
		paths = append(paths, filepath.Join(active, name))
	}
	paths = append(paths, SessionFolders(active)...)
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
	paths = append(paths, buildtool.UserCaches(environ, inv.Home)...)
	if inv.TempRoot != "" {
		paths = append(paths, SharedTempDirs(environ, inv.UID)...)
	}
	return withForms(paths)
}

// withForms lists every path's forms (see forms), each once, in order.
func withForms(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		for _, form := range forms(p) {
			if !seen[form] {
				seen[form] = true
				out = append(out, form)
			}
		}
	}
	return out
}

// deniedWrites are the paths the sandbox must stop the agent writing, beyond its default (only the checkout and the
// allowWrite folders): the sandbox lets it write Claude Code's temp folder, so with a temp root of the run's own the
// shared one is denied (SharedTempDirs: denyRead alone does not stop writes); and inv.Deny, what belongs to Agentium,
// the user and other runs (their temp roots under /tmp among them), which is never the agent's to write either.
func (inv Invocation) deniedWrites(environ []string) []string {
	paths := append([]string{}, inv.Deny...)
	if inv.TempRoot != "" {
		paths = append(paths, SharedTempDirs(environ, inv.UID)...)
	}
	return withForms(paths)
}

// forms are p cleaned and, when it exists, its symlink-resolved form: the sandbox matches the real path.
func forms(p string) []string {
	out := []string{filepath.Clean(p)}
	if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != out[0] {
		out = append(out, resolved)
	}
	return out
}

// DeniedPaths is every path the run's agent may not read, as its settings will list them (see deniedPaths).
func (inv Invocation) DeniedPaths(environ []string) []string {
	return inv.deniedPaths(UserConfigDir(environ, inv.Home), environ)
}

// settings are the per-run Claude Code settings (--settings).
func (inv Invocation) settings(userConfig string, environ []string) map[string]any {
	denied := inv.deniedPaths(userConfig, environ)
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
	filesystem := map[string]any{"denyRead": denied} // requirement 5
	if writes := inv.deniedWrites(environ); len(writes) > 0 {
		filesystem["denyWrite"] = writes
	}
	if inv.BuildCache != "" {
		filesystem["allowWrite"] = forms(inv.BuildCache) // it exists by now, so a symlinked data folder resolves
	}
	return map[string]any{
		"sandbox": map[string]any{
			"enabled": true, "failIfUnavailable": true, "allowUnsandboxedCommands": false, "autoAllowBashIfSandboxed": true,
			"network":    map[string]any{"strictAllowlist": true, "allowedDomains": []string{}},
			"filesystem": filesystem,
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
// variables, the build tools' among them: buildtool.EnvAllowlist) and nothing else: no credentials, no GIT_*, no
// AGENTIUM_*, no CLAUDE_* (in particular not CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, which silently forces the default
// permission mode: requirement 3). Values are passed as given: a proxy URL with a password in it would pass too.
//
// SHELL is kept on purpose: runs should behave like the user's own Claude Code sessions, so a user's zsh stays zsh
// (an unquoted glob such as --include=*.go then fails with "no matches found" there, as it would for them), and
// both arms get the same shell.
func Environ(environ []string) []string {
	exact := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true,
		"LANG": true, "TERM": true, "TZ": true, "VIRTUAL_ENV": true, "JAVA_HOME": true, "CARGO_HOME": true,
		"RUSTUP_HOME": true, "PNPM_HOME": true, "BUN_INSTALL": true, "DENO_DIR": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "no_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true}
	prefixes := []string{"LC_", "PYTHON", "NODE_", "NVM_", "CONDA_", "PIP_", "UV_", "RUSTC", "XDG_", "HOMEBREW_"}
	toolNames, toolPrefixes := buildtool.EnvAllowlist() // Go's GO* variables and CGO_
	for _, name := range toolNames {
		exact[name] = true
	}
	prefixes = append(prefixes, toolPrefixes...)
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
		Timeout: timeout, Grace: grace, Output: transcript, Stderr: errOut, Started: inv.Started})
}
