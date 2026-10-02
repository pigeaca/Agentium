// Package claude runs Claude Code headless for Agentium and reads what it reports. Each run is isolated the way the
// Phase 0 spike found necessary (docs/research/2026-09-27-phase0-spike-results.md): project settings only, no account
// connectors, a fixed permission mode, a sandbox without network that cannot read hidden paths or credentials, and an
// environment built from an allowlist. Checked against Claude Code 2.1.285.
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
	"syscall"
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
	// AccountHome is the account's home folder in the user database (user.Current), when known. HOME (Home) can point
	// elsewhere, but the account's login keychain stays in the real home folder, where an explicit path opens it, so
	// that folder is denied too (credentialPaths). Empty: only Home's.
	AccountHome string
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
	// Claude Code keeps its temp files in <TempRoot>/claude-<uid>, points the agent's shells' TMPDIR there, and keeps
	// its sockets in <TempRoot>/cc-socks (unless XDG_RUNTIME_DIR is set). Then the folders every other Claude Code
	// session of the user shares (SharedTempDirs) are denied to the agent, for reading and writing: they would be a
	// channel between runs, and a view of other sessions' temp files. It must be short (TempRootFits): a longer one
	// makes Claude Code fall back to the shared folders. The sandbox lets the agent write it without an allowWrite
	// entry (verified in a probe session).
	TempRoot string
	// UID is the user's id (os.Getuid()), which names Claude Code's temp folders (claude-<uid>); read only with TempRoot.
	UID int
	// Tools names the build-tool profiles the run's repository has (buildtool.DetectedNames); the always-on ones (Go's)
	// apply besides. They choose the environment allowlist, the agent's environment and caches, and the sandbox's
	// local-binding setting.
	Tools []string
	// Deps is the folder of warmed dependencies (home.Layout.Deps for the project): the agent's offline builds read it,
	// and the sandbox keeps it read-only. It lies outside every denied folder. Empty: none.
	Deps string
	// AllowLocalBinding is the user's opt-in (`agentium init --allow-local-binding`) for the sandbox's local binding,
	// which a Gradle project needs (see LocalBindingRefusal). Without it such a run does not start.
	AllowLocalBinding bool
	// JavaHome is a JDK resolved on the host (buildtool.ResolveJavaHome), which the JVM tools' environments name.
	JavaHome string
	// Venv is the Python venv the run's warm-up chose in Deps (buildtool.Warmed), which the Python profile's
	// environment activates. Empty: none.
	Venv string
	// ProjectMetadata is the base's Python metadata folder (buildtool.Warmed.Metadata), a read-only folder in Deps holding
	// only the project's .dist-info, which the Python profile puts on PYTHONPATH after the checkout. Empty: none.
	ProjectMetadata string
	// ImportRoot is where the project's Python code imports from, relative to Dir (buildtool.ImportRoot of the base
	// commit): "src", or "" for Dir itself.
	ImportRoot string
}

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
	measured := []string{filepath.Clean(root)} // measured, not denied: any link is followed (forms would not, in /tmp)
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

// SharedTempDirs are the folders Claude Code shares between all the user's sessions (2.1.285), in /tmp in both forms
// (the sandbox matches the resolved /private/tmp on macOS, the Read tool the path as written):
//   - claude-<uid>, the default temp folder, and claude, which the sandbox lets every shell write;
//   - cc-socks, cc-socks-<uid> and cc-daemon-<uid>, the sockets' and the background daemon's folders (Anthropic's own
//     eval-shell isolation denies writes to exactly these);
//   - under the user's own CLAUDE_CODE_TMPDIR, when environ sets one: claude-<uid> and cc-socks;
//   - $XDG_RUNTIME_DIR/cc-socks, when environ sets XDG_RUNTIME_DIR (it passes the allowlist, and Claude Code then keeps
//     every session's sockets there, the run's own included: its Claude Code process is not sandboxed, its agent is).
func SharedTempDirs(environ []string, uid int) []string {
	id := strconv.Itoa(uid)
	var dirs []string
	add := func(dir string) {
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	// Each name in both forms, side by side: the order stays the same whether /tmp/<name> exists (and its resolved form
	// is added after it, see forms) or not.
	for _, name := range []string{"claude-" + id, "claude", "cc-socks", "cc-socks-" + id, "cc-daemon-" + id} {
		add(filepath.Join("/tmp", name))
		add(filepath.Join("/private/tmp", name))
	}
	if root := lookup(environ, "CLAUDE_CODE_TMPDIR"); filepath.IsAbs(root) {
		add(filepath.Join(root, "claude-"+id))
		add(filepath.Join(root, "cc-socks"))
	}
	if runtime := lookup(environ, "XDG_RUNTIME_DIR"); filepath.IsAbs(runtime) {
		add(filepath.Join(runtime, "cc-socks"))
	}
	return dirs
}

// sharedLogDirs are the folders outside the temp folders that the sandbox lets every shell write (Claude Code
// 2.1.285): npm's logs and Claude Code's debug folder, ~/.claude/debug, and in login mode the user's config folder's
// (a run with its own config folder writes its own). All the user's sessions share them.
func (inv Invocation) sharedLogDirs(userConfig string) []string {
	dirs := []string{filepath.Join(inv.Home, ".npm", "_logs"), filepath.Join(inv.Home, ".claude", "debug")}
	if inv.SignIn == SignInLogin && filepath.Clean(userConfig) != filepath.Join(inv.Home, ".claude") {
		dirs = append(dirs, filepath.Join(userConfig, "debug"))
	}
	return dirs
}

// lookup is the value of name in environ, or "".
func lookup(environ []string, name string) string {
	for _, kv := range environ {
		if value, ok := strings.CutPrefix(kv, name+"="); ok {
			return value
		}
	}
	return ""
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

// credentialFiles are the user's credential stores the sandbox denies (relative to the home folder). Library/Keychains
// holds the macOS login keychain: Claude Code's sandbox (2.1.285) allows the security server's Mach lookups
// (com.apple.SecurityServer, com.apple.securityd.xpc), and its settings offer no way to deny them, only to allow more
// (network.allowMachLookup). With the folder readable, `security` inside the agent's shell searches the login keychain,
// where Claude Code and gh keep their tokens, stored through /usr/bin/security and so likely readable by it without a
// prompt. Denied, the keychain file cannot be opened even by an explicit path (the security client fails with
// "Operation not permitted"), and the login keychain leaves the shell's search list. Claude Code reads its own login
// outside the sandbox, which covers only its tools' commands, so sign-in is unaffected.
func credentialFiles() []string {
	return []string{".ssh", ".codex", ".config/gh", ".config/agentium", ".netrc", ".git-credentials", ".aws", ".docker",
		".npmrc", ".pypirc", ".kube", ".gnupg", "Library/Keychains"}
}

// machineCredentials are the machine's credential stores the sandbox denies: the System keychain's folder, which holds
// no user secrets but which no agent needs (TLS trust is trustd's, outside the sandbox, and the network is off). It is
// listed on every system, so the settings do not depend on the machine; denying a missing path is harmless.
func machineCredentials() []string {
	return []string{"/Library/Keychains"}
}

// credentialPaths are every credential store the sandbox denies, as absolute paths: the user's (credentialFiles) under
// home; the account's own login keychain folder when accountHome (Invocation.AccountHome) is set and is not home (HOME
// redirected: the login keychain stays in the account's real home folder, and an explicit path opens it); then the
// machine's.
func credentialPaths(home, accountHome string) []string {
	var paths []string
	for _, name := range credentialFiles() {
		paths = append(paths, filepath.Join(home, name))
	}
	if accountHome != "" && filepath.Clean(accountHome) != filepath.Clean(home) {
		paths = append(paths, filepath.Join(accountHome, "Library", "Keychains"))
	}
	return append(paths, machineCredentials()...)
}

// movedCredentials are credential stores the user's environment moves out of credentialFiles' places: gh's config
// (GH_CONFIG_DIR, else $XDG_CONFIG_HOME/gh), which can hold a plain-text token. Only absolute values count, and never
// the home folder or one above it (a misconfigured variable would deny everything).
func movedCredentials(environ []string, home string) []string {
	var paths []string
	add := func(p string) {
		if !filepath.IsAbs(p) {
			return
		}
		p = filepath.Clean(p)
		if rel, err := filepath.Rel(p, home); err == nil && filepath.IsLocal(rel) {
			return
		}
		paths = append(paths, p)
	}
	add(lookup(environ, "GH_CONFIG_DIR"))
	if x := lookup(environ, "XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		add(filepath.Join(x, "gh"))
	}
	return paths
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
	// The build tools' own variables (Go's GOFLAGS) replace any of the same name the allowlist kept, and come right
	// after it; the run's build cache variables (Go's GOCACHE) replace the user's and come after Claude Code's own.
	profiles := buildtool.Select(inv.Tools)
	allowed := EnvironFor(environ, profiles)
	toolEnv := buildtool.AgentEnv(profiles, buildtool.AgentContext{Allowed: allowed, Environ: environ, Home: inv.Home, Repo: inv.Dir,
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
	if inv.TempRoot != "" { // the parent's own CLAUDE_CODE_TMPDIR was dropped with every CLAUDE_* (Environ)
		env = append(env, "CLAUDE_CODE_TMPDIR="+inv.TempRoot)
	}
	env = append(env, signInEnv(inv.SignIn, inv.Secret, inv.ConfigDir, inv.Home, userConfig)...) // requirement 4
	return args, env, nil
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

// deniedPaths are the paths the agent may not read, in every sign-in mode:
//   - inv.Deny;
//   - Claude Code's data. The run's active config folder (the user's in login mode, the fresh one otherwise) loses only
//     its history paths: Claude Code keeps working files there that its Bash tool reads, such as the shell snapshot.
//     Every other Claude folder (~/.claude, the user's CLAUDE_CONFIG_DIR, when not active) is denied whole, and so is
//     ~/.claude.json. The Claude Code process itself is not sandboxed, so none of this affects sign-in;
//   - credential stores (credentialPaths: the login and System keychains among them) and the token file's folder;
//   - the build tools' caches of the user (buildtool.UserCaches: Go's build caches), which hold hidden tests compiled
//     before Agentium kept its own;
//   - with a temp root of the run's own, the user's shared Claude Code temp folders (SharedTempDirs);
//   - the log folders the sandbox lets every shell write (sharedLogDirs).
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
	paths = append(paths, credentialPaths(inv.Home, inv.AccountHome)...)
	paths = append(paths, movedCredentials(environ, inv.Home)...)
	paths = append(paths, buildtool.UserCaches(environ, inv.Home)...)
	if inv.Deps != "" {
		paths = append(paths, buildtool.DepsDenied(inv.Deps)...)
	}
	if inv.TempRoot != "" {
		paths = append(paths, SharedTempDirs(environ, inv.UID)...)
	}
	paths = append(paths, inv.sharedLogDirs(userConfig)...)
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
// allowWrite folders): the folders the sandbox lets every shell write, shared by all the user's Claude Code sessions,
// so with a temp root of the run's own the shared temp folders (SharedTempDirs: denyRead alone does not stop writes),
// and the log folders (sharedLogDirs); and inv.Deny, what belongs to Agentium, the user and other runs (their temp
// roots under /tmp among them), which is never the agent's to write either.
func (inv Invocation) deniedWrites(userConfig string, environ []string) []string {
	paths := append([]string{}, inv.Deny...)
	if inv.Deps != "" {
		paths = append(paths, inv.Deps) // already read-only by default; stated, so no allowWrite above it can open it
	}
	if inv.TempRoot != "" {
		paths = append(paths, SharedTempDirs(environ, inv.UID)...)
	}
	paths = append(paths, inv.sharedLogDirs(userConfig)...)
	return withForms(paths)
}

// forms are p cleaned and, when different, its real form (realForm): the sandbox matches real paths, the Read tool the
// path as written.
func forms(p string) []string {
	out := []string{filepath.Clean(p)}
	if real := realForm(out[0]); real != out[0] {
		out = append(out, real)
	}
	return out
}

// realForm is p as the macOS sandbox matches it: symbolic links resolved in the longest existing prefix, and the
// missing tail appended as written, so a denied path that a warm-up or another run creates after the agent starts is
// still denied where it will lie (Claude Code itself lists only the unresolved form of a path that does not exist, or
// of one that resolves elsewhere, and the sandbox ignores a deny on a form that is not the real one).
//
// Below /tmp (or /private/tmp) the entry directly in it decides, by its owner (tmpForm). /tmp is sticky: only an
// entry's owner (or root) can remove or replace it, but any local user can create a name not taken yet, as a link too
// (/tmp/claude and /tmp/cc-socks carry no uid). Following another user's link would deny its target, anything they
// chose, which can make a run refuse to start; resolving through another user's folder races them swapping it for
// such a link between two looks. So only the user's own entries and root's are resolved through.
func realForm(p string) string {
	for _, tmp := range []string{"/tmp", "/private/tmp"} {
		rel, err := filepath.Rel(tmp, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		name, rest, _ := strings.Cut(rel, string(filepath.Separator))
		owner, exists := entryOwner(filepath.Join(tmp, name))
		return tmpForm(p, tmp, name, rest, exists, owner, os.Getuid())
	}
	return resolvedPrefix(p)
}

// tmpForm is the real form of p, the entry name in tmp (/tmp or /private/tmp) followed by rest, given who owns the
// entry: the user uid's own entry or root's is resolved through, like any path (its owner alone can replace it); for
// another user's entry, or a missing one, only tmp itself (the system's /tmp → /private/tmp link) is resolved, and the
// rest is kept as written, so neither a link that user made nor one they make after this look is followed.
func tmpForm(p, tmp, name, rest string, exists bool, owner uint32, uid int) string {
	if exists && (owner == 0 || int64(owner) == int64(uid)) {
		return resolvedPrefix(p)
	}
	return filepath.Join(resolvedPrefix(tmp), name, rest)
}

// entryOwner is the uid owning path itself (a link is not followed), and whether it exists. An entry whose owner
// cannot be told is reported missing, so it is not resolved through.
func entryOwner(path string) (uint32, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// resolvedPrefix resolves symbolic links in the longest existing prefix of p and appends the rest as written.
func resolvedPrefix(p string) string {
	var missing []string
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, missing...)...)
		}
		missing = append([]string{filepath.Base(p)}, missing...)
		p = parent
	}
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
	for _, p := range credentialPaths(inv.Home, inv.AccountHome) {
		files = append(files, map[string]string{"path": p, "mode": "deny"})
	}
	if inv.TokenFile != "" {
		files = append(files, map[string]string{"path": filepath.Dir(inv.TokenFile), "mode": "deny"})
	}
	filesystem := map[string]any{"denyRead": denied} // requirement 5
	if writes := inv.deniedWrites(userConfig, environ); len(writes) > 0 {
		filesystem["denyWrite"] = writes
	}
	if inv.BuildCache != "" {
		filesystem["allowWrite"] = forms(inv.BuildCache) // it exists by now, so a symlinked data folder resolves
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

// Environ keeps what a coding agent's tools need from environ (system settings, proxies and certificates, toolchain
// variables, the build tools' among them: buildtool.EnvAllowlist) and nothing else: no credentials, no GIT_*, no
// AGENTIUM_*, no CLAUDE_* (in particular not CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, which silently forces the default
// permission mode: requirement 3). Values are passed as given: a proxy URL with a password in it would pass too.
//
// TMPDIR is kept as the user's own (macOS: /var/folders/.../T). Claude Code points its shells' TMPDIR at the run's
// temp root, but the user's folder itself stays readable to the agent: a follow-up (the run temp isolation plan).
//
// SHELL is kept on purpose: runs should behave like the user's own Claude Code sessions, so a user's zsh stays zsh
// (an unquoted glob such as --include=*.go then fails with "no matches found" there, as it would for them), and
// both arms get the same shell.
func Environ(environ []string) []string {
	return EnvironFor(environ, buildtool.Select(nil))
}

// EnvironFor is Environ for a repository whose build-tool profiles are selected (buildtool.Select): the allowlist
// widens by theirs. The base list keeps JAVA_HOME, CARGO_HOME and the RUSTC* prefix for every project, as before
// profiles: a profile that owns one of them (Maven's and Gradle's JAVA_HOME, Cargo's CARGO_HOME and wrappers) sets or
// clears it in the agent's environment instead.
func EnvironFor(environ []string, selected []buildtool.Profile) []string {
	names := []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR",
		"LANG", "TERM", "TZ", "JAVA_HOME", "CARGO_HOME",
		"RUSTUP_HOME", "PNPM_HOME", "BUN_INSTALL", "DENO_DIR",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}
	// No PYTHON*, PIP_*, UV_* or VIRTUAL_ENV, for any project: a PIP_INDEX_URL may carry a token, and PYTHONPATH,
	// VIRTUAL_ENV or UV_CACHE_DIR would point the agent at other code or at the user's caches. A Python project's
	// profile sets its own (buildtool's pythonEnv).
	prefixes := []string{"LC_", "NODE_", "NVM_", "CONDA_", "RUSTC", "XDG_", "HOMEBREW_"}
	toolNames, toolPrefixes := buildtool.EnvAllowlist(selected)
	// Credentials are dropped by the shared policy; GIT_*, AGENTIUM_* and CLAUDE_* are simply not on the list.
	return runner.EnvPolicy{Allowlist: true, Names: append(names, toolNames...), Prefixes: append(prefixes, toolPrefixes...)}.Filter(environ)
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
