package sandbox

import (
	"path/filepath"
	"slices"
	"strconv"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/runner"
)

// AgentDenied is the deny list every agent run shares (internal/agent): what an agent's sandbox may not let it read,
// and what it may not write, whichever agent runs. Each adapter fills in its own data (AgentData) and the folders its
// agent's sessions share (Shared); the rest is Agentium's policy for every agent:
//   - Deny: Agentium's data (other runs, hidden tests, the database), the user's repository, and other runs' folders;
//   - credential stores (CredentialPaths: the login and System keychains among them; MovedCredentials) and the sign-in
//     token file's folder;
//   - the build tools' caches of the user (buildtool.UserCaches: Go's build caches), which hold hidden tests compiled
//     before Agentium kept its own, and the deps folder's parts the agent must not read (buildtool.DepsDenied).
//
// The order of Reads and Writes is part of what the agent is given (its settings list them as they come), so it is
// fixed: changing it changes every run's settings.
type AgentDenied struct {
	Deny []string // absolute paths: Agentium's data, the user's repository, other runs
	// AgentData is the agent's own data the run may not read (Claude Code's history, past sessions and other config
	// folders): denied for reading, after Deny.
	AgentData []string
	// SecretFile is the sign-in token's file, when the secret came from one: its folder is denied, as a credential.
	SecretFile string
	// Home and AccountHome are the user's home folder and the account's own (CredentialPaths).
	Home, AccountHome string
	// Environ is the user's environment, which can move credential stores and caches (MovedCredentials, UserCaches).
	Environ []string
	// Deps is the run's deps folder: read-only to the agent, and some of it not readable (buildtool.DepsDenied).
	Deps string
	// Shared are the folders every session of the user shares that the agent's sandbox would let it write (shared temp
	// folders, log folders): denied for reading and writing, last, as a channel between runs and a view of other
	// sessions' files.
	Shared []string
}

// Reads is every path the agent may not read, each in every form the sandbox matches (WithForms): Deny, AgentData,
// the token file's folder, the credential stores, the user's build caches, the deps folder's denied parts, then
// Shared.
func (d AgentDenied) Reads() []string {
	paths := append([]string{}, d.Deny...)
	paths = append(paths, d.AgentData...)
	if d.SecretFile != "" {
		paths = append(paths, filepath.Dir(d.SecretFile))
	}
	paths = append(paths, CredentialPaths(d.Home, d.AccountHome)...)
	paths = append(paths, MovedCredentials(d.Environ, d.Home)...)
	paths = append(paths, buildtool.UserCaches(d.Environ, d.Home)...)
	if d.Deps != "" {
		paths = append(paths, buildtool.DepsDenied(d.Deps)...)
	}
	paths = append(paths, d.Shared...)
	return WithForms(paths)
}

// Writes is every path the agent's sandbox must stop it writing, beyond its default (only its working folders): Deny,
// what belongs to Agentium, the user and other runs (their temp roots among them), which is never the agent's to write
// either; the deps folder, already read-only by default but stated, so no writable folder above it can open it; and
// Shared (denying them for reading alone does not stop writes). Each in every form (WithForms).
func (d AgentDenied) Writes() []string {
	paths := append([]string{}, d.Deny...)
	if d.Deps != "" {
		paths = append(paths, d.Deps)
	}
	paths = append(paths, d.Shared...)
	return WithForms(paths)
}

// Credentials are the credential stores the agent's own credential settings deny, as written: CredentialPaths, then
// the token file's folder.
func (d AgentDenied) Credentials() []string {
	paths := CredentialPaths(d.Home, d.AccountHome)
	if d.SecretFile != "" {
		paths = append(paths, filepath.Dir(d.SecretFile))
	}
	return paths
}

// SharedTempDirs are the folders Claude Code shares between all the user's sessions (2.1.285), in /tmp in both forms
// (the sandbox matches the resolved /private/tmp on macOS, Claude Code's Read tool the path as written). They hold
// other sessions' temp files, so a run with a temp root of its own denies them, whichever agent it runs:
//   - claude-<uid>, the default temp folder, and claude, which Claude Code's sandbox lets every shell write;
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
	// is added after it, see Forms) or not.
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

// SharedLogDirs are the folders outside the temp folders that Claude Code's sandbox (2.1.285) lets every shell write,
// which all the user's sessions share: npm's logs and Claude Code's debug folder, ~/.claude/debug, and, when
// loginConfig is set and is not ~/.claude, its debug folder too (the user's own config folder, which a run signed in
// with the user's login uses; a run with its own config folder writes its own).
func SharedLogDirs(home, loginConfig string) []string {
	dirs := []string{filepath.Join(home, ".npm", "_logs"), filepath.Join(home, ".claude", "debug")}
	if loginConfig != "" && filepath.Clean(loginConfig) != filepath.Join(home, ".claude") {
		dirs = append(dirs, filepath.Join(loginConfig, "debug"))
	}
	return dirs
}

// Environ keeps what a coding agent's tools need from environ (system settings, proxies and certificates, toolchain
// variables, the build tools' among them: buildtool.EnvAllowlist) and nothing else: no credentials, no GIT_*, no
// AGENTIUM_*, no CLAUDE_* (in particular not CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, which silently forces Claude Code's
// default permission mode). Values are passed as given: a proxy URL with a password in it would pass too. It is the
// environment allowlist every agent shares (internal/agent), and the grader's base (buildtool.GraderEnv).
//
// TMPDIR is kept as the user's own (macOS: /var/folders/.../T). Claude Code points its shells' TMPDIR at the run's
// temp root, but the user's folder itself stays readable to the agent: a follow-up (the run temp isolation plan).
//
// SHELL is kept on purpose: runs should behave like the user's own sessions, so a user's zsh stays zsh (an unquoted
// glob such as --include=*.go then fails with "no matches found" there, as it would for them), and both arms get the
// same shell.
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
