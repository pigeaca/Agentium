// Package claude runs Claude Code headless for Agentium and reads what it reports. Each run is isolated the way the
// Phase 0 spike found necessary (docs/research/2026-09-27-phase0-spike-results.md): project settings only, no account
// connectors, a fixed permission mode, a sandbox without network that cannot read hidden paths or credentials, and an
// environment built from an allowlist. Checked against Claude Code 2.1.285. It is Claude Code's adapter behind the
// agent seam (Adapter, internal/agent); the deny list and the environment allowlist it applies are the ones every agent
// shares (internal/sandbox: AgentDenied, EnvironFor).
package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/sandbox"
)

// Sign-in modes (the same names as project discovery reports).
const (
	SignInAPIKey    = "api-key"    // ANTHROPIC_API_KEY, passed to the run only, with a fresh config folder
	SignInTokenFile = "token-file" // a `claude setup-token` token, passed to the run only, with a fresh config folder
	SignInLogin     = "login"      // the user's own login and config folder, restricted to project settings
)

// PermissionMode is the mode every run uses: edits are accepted, and nothing prompts.
const PermissionMode = "acceptEdits"

// Invocation is one headless Claude Code run: the agent-neutral run (agent.Invocation), with Claude Code's methods
// (Command, DeniedPaths). Adapter takes an agent.Invocation and converts it. Claude Code reads its fields so:
//   - BudgetUSD is --max-budget-usd; SignIn is SignInAPIKey, SignInTokenFile or SignInLogin; ConfigDir is a fresh, empty
//     CLAUDE_CONFIG_DIR for SignInAPIKey and SignInTokenFile; TokenFile is the SignInTokenFile token's file.
//   - Repo: the checkout is a working folder (--add-dir) the sandbox lets the agent write (allowWrite), so Claude Code
//     loads the root's and the module's instructions and accepts edits anywhere in it.
//   - Deny: denied through the sandboxed shell and the Read tool.
//   - TempRoot is the run's own Claude Code temp root (CLAUDE_CODE_TMPDIR), an existing owner-only folder: Claude Code
//     keeps its temp files in <TempRoot>/claude-<uid>, points the agent's shells' TMPDIR there, and keeps its sockets in
//     <TempRoot>/cc-socks (unless XDG_RUNTIME_DIR is set). Then the folders every other Claude Code session of the user
//     shares (sandbox.SharedTempDirs) are denied to the agent, for reading and writing: they would be a channel between
//     runs, and a view of other sessions' temp files. It must be short (TempRootFits): a longer one makes Claude Code
//     fall back to the shared folders. The sandbox lets the agent write it without an allowWrite entry (verified in a
//     probe session). UID names Claude Code's temp folders (claude-<uid>).
//   - AllowLocalBinding sets the sandbox's allowLocalBinding (see LocalBindingRefusal).
type Invocation agent.Invocation

// LocalBindingRefusal is why a run may not start: its tools (Gradle) need the sandbox's allowLocalBinding and the user
// has not allowed it. The setting is more than its name says (Claude Code 2.1.285 writes allow rules for network-bind
// on any local port, network-inbound on any local port, and network-outbound to localhost on any port): the agent
// could bind a port and connect to any service listening on this machine, a database or a dev server. Outbound
// network to other hosts stays blocked. Hence an opt-in, per project.
func LocalBindingRefusal(tools []string, allowed bool) error {
	if allowed || !buildtool.LocalBinding(buildtool.Select(tools)) {
		return nil
	}
	return errors.New("this project builds with Gradle, whose file-lock service needs the sandbox to let the agent bind local ports and connect to localhost. " +
		"That also lets the agent reach any service listening on this machine (a database, a dev server, and the other agents' runs, which can reach each other when they run side by side); outbound network to other hosts stays blocked. " +
		"Agent runs on this project do not start until you allow it: agentium init --allow-local-binding")
}

// Claude Code 2.1.285's limits on its temp root, which TempRootFits checks:
//   - maxTempDir: the shells' TMPDIR, <root>/claude-<uid> as written, is used only up to 44 bytes, and the root itself
//     too (longer, Claude Code falls back to /tmp/claude-<uid>, and to /tmp);
//   - maxSocketPath: a socket, <root>/cc-socks/<pid>.sock, up to 103 bytes (a Unix socket path holds 104 with its final
//     NUL on macOS); longer, it falls back to /tmp/cc-socks-<uid>. socketName is the longest name: a 7-digit process id.
const (
	maxTempDir    = 44
	maxSocketPath = 103
	socketName    = "/cc-socks/4194304.sock"
)

// TempRootFits reports whether Claude Code keeps its temp files and sockets under root for the user uid, rather than
// falling back to the shared folders. Both root as written and its symlink-resolved form (/tmp is /private/tmp on
// macOS) must fit: Claude Code checks the path as given, and a resolved form that fits is the safe side.
func TempRootFits(root string, uid int) error {
	measured := []string{filepath.Clean(root)} // measured, not denied: any link is followed (sandbox.Forms would not, in /tmp)
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != measured[0] {
		measured = append(measured, resolved)
	}
	for _, form := range measured {
		if n := len(form + "/claude-" + strconv.Itoa(uid)); n > maxTempDir {
			return fmt.Errorf("the run's temp root %s is too long for Claude Code (%s/claude-%d is %d bytes of %d): it would fall back to the shared temp folder", form, form, uid, n, maxTempDir)
		}
		if n := len(form + socketName); n > maxSocketPath {
			return fmt.Errorf("the run's temp root %s is too long for Claude Code's sockets (%d bytes of %d): it would fall back to the shared socket folder", form, n, maxSocketPath)
		}
	}
	return nil
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

// Command returns the arguments and environment for the run. environ is the parent's environment (os.Environ()),
// filtered through an allowlist; the sign-in secret is the only credential the child receives.
func (inv Invocation) Command(environ []string) (args, env []string, err error) {
	if err := checkSignIn(inv.SignIn, inv.Secret, inv.ConfigDir); err != nil {
		return nil, nil, err
	}
	if inv.CLI == "" || inv.Dir == "" || inv.Prompt == "" || inv.Model == "" || inv.Home == "" {
		return nil, nil, errors.New("a run needs the CLI, a folder, a prompt, a model and the home folder")
	}
	if inv.Repo != "" {
		if rel, err := filepath.Rel(inv.Repo, inv.Dir); !filepath.IsAbs(inv.Repo) || err != nil || !filepath.IsLocal(rel) && rel != "." {
			return nil, nil, fmt.Errorf("the run's folder %q is not inside its checkout %q", inv.Dir, inv.Repo)
		}
	}
	userConfig := UserConfigDir(environ, inv.Home)
	for _, p := range append([]string{userConfig, inv.Home}, inv.Deny...) {
		if !filepath.IsAbs(p) {
			return nil, nil, fmt.Errorf("path %q (a denied path, the home folder or CLAUDE_CONFIG_DIR) is not absolute", p)
		}
	}
	for _, p := range []string{inv.ConfigDir, inv.TokenFile, inv.BuildCache, inv.TempRoot, inv.Deps, inv.JavaHome, inv.Venv, inv.ProjectMetadata, inv.AccountHome} {
		if p != "" && !filepath.IsAbs(p) {
			return nil, nil, fmt.Errorf("path %q is not absolute", p)
		}
	}
	// The metadata folder is on the agent's PYTHONPATH: only inside the deps folder, which the sandbox keeps read-only,
	// can the agent not plant code there.
	if inv.ProjectMetadata != "" {
		if rel, err := filepath.Rel(inv.Deps, inv.ProjectMetadata); inv.Deps == "" || err != nil || !filepath.IsLocal(rel) || rel == "." {
			return nil, nil, fmt.Errorf("the project's metadata %q is not inside the deps folder", inv.ProjectMetadata)
		}
	}
	if inv.TempRoot != "" {
		if err := TempRootFits(inv.TempRoot, inv.UID); err != nil {
			return nil, nil, err
		}
	}
	if err := LocalBindingRefusal(inv.Tools, inv.AllowLocalBinding); err != nil {
		return nil, nil, err
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
	if inv.inModule() {
		args = append(args, "--add-dir", inv.Repo) // edits anywhere in the checkout are accepted, as at its root
	}
	// The build tools' own variables (Go's GOFLAGS) replace any of the same name the allowlist kept, and come right
	// after it; the run's build cache variables (Go's GOCACHE) replace the user's and come after Claude Code's own.
	profiles := buildtool.SelectRun(inv.Tools, inv.AgentTools)
	allowed := sandbox.EnvironFor(environ, profiles)
	toolEnv := buildtool.AgentEnv(profiles, buildtool.AgentContext{Allowed: allowed, Environ: environ, Home: inv.Home, Repo: inv.checkout(),
		BuildCache: inv.BuildCache, Deps: inv.Deps, JavaHome: inv.JavaHome, Venv: inv.Venv, Metadata: inv.ProjectMetadata, ImportRoot: inv.ImportRoot})
	replaced := map[string]bool{}
	for _, kv := range toolEnv {
		name, _, _ := strings.Cut(kv, "=")
		replaced[name] = true
	}
	if inv.BuildCache != "" {
		for _, name := range buildtool.AgentCacheNames(profiles) {
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
		env = append(env, buildtool.AgentCacheEnv(profiles, inv.BuildCache)...)
	}
	if inv.TempRoot != "" { // the parent's own CLAUDE_CODE_TMPDIR was dropped with every CLAUDE_* (sandbox.Environ)
		env = append(env, "CLAUDE_CODE_TMPDIR="+inv.TempRoot)
	}
	env = append(env, signInEnv(inv.SignIn, inv.Secret, inv.ConfigDir, inv.Home, userConfig)...) // requirement 4
	return args, env, nil
}

// checkout is the run's whole checkout: Repo, else Dir.
func (inv Invocation) checkout() string {
	if inv.Repo != "" {
		return inv.Repo
	}
	return inv.Dir
}

// inModule reports whether the agent starts in a folder inside its checkout (Repo) rather than at its root.
func (inv Invocation) inModule() bool {
	return inv.Repo != "" && filepath.Clean(inv.Repo) != filepath.Clean(inv.Dir)
}

// checkSignIn refuses a sign-in the mode cannot use: a login takes no secret, and the other modes need a secret and a
// fresh config folder. Runs (Invocation) and judge calls (Judgement) share it.
func checkSignIn(mode, secret, configDir string) error {
	switch mode {
	case SignInAPIKey, SignInTokenFile:
		if secret == "" || configDir == "" {
			return fmt.Errorf("sign-in %s needs a secret and a fresh config folder", mode)
		}
	case SignInLogin:
		if secret != "" {
			return errors.New("sign-in login takes no secret")
		}
	default:
		return fmt.Errorf("unknown sign-in mode %q", mode)
	}
	return nil
}

// signInEnv is the sign-in's variables, after checkSignIn: a fresh config folder cannot use a subscription login. A
// login keeps the user's own config folder (userConfig, UserConfigDir) when it is not ~/.claude; the other modes get
// the fresh folder and the one credential the child receives.
func signInEnv(mode, secret, configDir, home, userConfig string) []string {
	switch mode {
	case SignInLogin:
		if userConfig != filepath.Join(home, ".claude") { // the user's login lives in their own config folder
			return []string{"CLAUDE_CONFIG_DIR=" + userConfig}
		}
	case SignInAPIKey:
		return []string{"CLAUDE_CONFIG_DIR=" + configDir, "ANTHROPIC_API_KEY=" + secret}
	case SignInTokenFile:
		return []string{"CLAUDE_CONFIG_DIR=" + configDir, "CLAUDE_CODE_OAUTH_TOKEN=" + secret}
	}
	return nil
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

// denied is the run's deny list (sandbox.AgentDenied), in every sign-in mode:
//   - inv.Deny;
//   - Claude Code's data. The run's active config folder (the user's in login mode, the fresh one otherwise) loses only
//     its history paths: Claude Code keeps working files there that its Bash tool reads, such as the shell snapshot.
//     Every other Claude folder (~/.claude, the user's CLAUDE_CONFIG_DIR, when not active) is denied whole, and so is
//     ~/.claude.json. The Claude Code process itself is not sandboxed, so none of this affects sign-in;
//   - the shared policy: credential stores and the token file's folder, the user's build caches, the deps folder's
//     denied parts;
//   - with a temp root of the run's own, the user's shared Claude Code temp folders (sandbox.SharedTempDirs);
//   - the log folders the sandbox lets every shell write (sandbox.SharedLogDirs).
func (inv Invocation) denied(userConfig string, environ []string) sandbox.AgentDenied {
	active := userConfig
	if inv.SignIn != SignInLogin {
		active = inv.ConfigDir
	}
	var own []string
	for _, name := range historyPaths() {
		own = append(own, filepath.Join(active, name))
	}
	own = append(own, SessionFolders(active)...)
	for _, dir := range []string{filepath.Join(inv.Home, ".claude"), userConfig} {
		if filepath.Clean(dir) != filepath.Clean(active) {
			own = append(own, dir)
		}
	}
	own = append(own, filepath.Join(inv.Home, ".claude.json"))
	var shared []string
	if inv.TempRoot != "" {
		shared = append(shared, sandbox.SharedTempDirs(environ, inv.UID)...)
	}
	loginConfig := ""
	if inv.SignIn == SignInLogin {
		loginConfig = userConfig
	}
	shared = append(shared, sandbox.SharedLogDirs(inv.Home, loginConfig)...)
	return sandbox.AgentDenied{Deny: inv.Deny, AgentData: own, SecretFile: inv.TokenFile, Home: inv.Home, AccountHome: inv.AccountHome,
		Environ: environ, Deps: inv.Deps, Shared: shared}
}

// DeniedPaths is every path the run's agent may not read, as its settings will list them (see denied): each path is
// cleaned, and its symlink-resolved form (/var and /private/var on macOS) is denied too.
func (inv Invocation) DeniedPaths(environ []string) []string {
	return inv.denied(UserConfigDir(environ, inv.Home), environ).Reads()
}

// settings are the per-run Claude Code settings (--settings).
func (inv Invocation) settings(userConfig string, environ []string) map[string]any {
	deny := inv.denied(userConfig, environ)
	denied := deny.Reads()
	readRules := make([]string, len(denied))
	for i, p := range denied {
		readRules[i] = "Read(/" + p + "/**)" // an absolute path in a permission rule starts with //
	}
	var files []map[string]string
	for _, p := range deny.Credentials() {
		files = append(files, map[string]string{"path": p, "mode": "deny"})
	}
	filesystem := map[string]any{"denyRead": denied} // requirement 5
	// Beyond the sandbox's default (only the checkout and the allowWrite folders): the shared temp and log folders the
	// sandbox lets every shell write, inv.Deny and the deps folder (sandbox.AgentDenied.Writes).
	if writes := deny.Writes(); len(writes) > 0 {
		filesystem["denyWrite"] = writes
	}
	var writable []string
	if inv.BuildCache != "" {
		writable = sandbox.Forms(inv.BuildCache) // it exists by now, so a symlinked data folder resolves
	}
	if inv.inModule() {
		// The sandbox lets the agent write only where it starts (the module) by default: the rest of its checkout too.
		writable = append(writable, sandbox.Forms(inv.Repo)...)
	}
	if len(writable) > 0 {
		filesystem["allowWrite"] = writable
	}
	network := map[string]any{"strictAllowlist": true, "allowedDomains": []string{}}
	if inv.AllowLocalBinding && buildtool.LocalBinding(buildtool.Select(inv.Tools)) {
		// Gradle's file-lock service binds a local UDP socket. This also allows binding any local port and connecting
		// to localhost (see LocalBindingRefusal), so it is set only for Gradle projects whose user opted in.
		network["allowLocalBinding"] = true
	}
	return map[string]any{
		"sandbox": map[string]any{
			"enabled": true, "failIfUnavailable": true, "allowUnsandboxedCommands": false, "autoAllowBashIfSandboxed": true,
			"network":    network,
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
