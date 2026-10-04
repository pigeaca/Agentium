package sandbox

import (
	"path/filepath"
	"strings"
)

// CredentialFiles are the user's credential stores every sandbox denies (relative to the home folder): agent runs
// (internal/claude) and grading profiles. Library/Keychains holds the macOS login keychain: Claude Code's sandbox
// (2.1.285) allows the security server's Mach lookups (com.apple.SecurityServer, com.apple.securityd.xpc), and its
// settings offer no way to deny them, only to allow more (network.allowMachLookup). With the folder readable,
// `security` inside the agent's shell searches the login keychain, where Claude Code and gh keep their tokens, stored
// through /usr/bin/security and so likely readable by it without a prompt. Denied, the keychain file cannot be opened
// even by an explicit path (the security client fails with "Operation not permitted"), and the login keychain leaves
// the shell's search list. Claude Code reads its own login outside the sandbox, which covers only its tools' commands,
// so sign-in is unaffected. (Grading profiles also deny the security server's lookups: Profile.) .config/uv is uv's
// configuration folder (uv.toml can name an index with a token): the folder, not only uv.toml, which the run's
// build-tool lists (buildtool.UserCaches) already deny; the interpreters in ~/.local/share/uv/python stay readable.
func CredentialFiles() []string {
	return []string{".ssh", ".codex", ".config/gh", ".config/agentium", ".netrc", ".git-credentials", ".aws", ".docker",
		".npmrc", ".pypirc", ".kube", ".gnupg", "Library/Keychains", ".config/uv"}
}

// graderCredentialFiles are credential stores grading profiles deny beyond CredentialFiles (relative to the home
// folder): pip's configuration (which can hold an index URL with a token) and uv's credentials; Maven's settings and
// its master password, Gradle's gradle.properties and Cargo's registry tokens. Agent runs deny every one of them too,
// through the build tools' user caches (buildtool.UserCaches: pip's and uv's configs and credentials, poetry's
// auth.toml, keyring's folder, the machine's pip.conf, and PIP_CONFIG_FILE, UV_CONFIG_FILE and the like; ~/.m2,
// gradle.properties and Cargo's credentials folder and files); a test holds the two lists together (the agent's
// denied paths cover this list). The grader's offline recipe reads none of them: Maven runs offline from the deps'
// repository, and Gradle and Cargo use homes of their own. uv's interpreters (~/.local/share/uv/python) stay readable.
func graderCredentialFiles() []string {
	return []string{".config/pip", "Library/Application Support/pip", ".local/share/uv/credentials",
		".m2/settings.xml", ".m2/settings-security.xml", ".gradle/gradle.properties", ".cargo/credentials", ".cargo/credentials.toml"}
}

// MachineCredentials are the machine's credential stores every sandbox denies: the System keychain's folder, which
// holds no user secrets but which no sandboxed process needs (TLS trust is trustd's, outside the sandbox, and the
// network is off). It is listed on every system, so the settings do not depend on the machine; denying a missing path
// is harmless.
func MachineCredentials() []string {
	return []string{"/Library/Keychains"}
}

// CredentialPaths are every credential store a sandbox denies, as absolute paths: the user's (CredentialFiles) under
// home; the account's own login keychain folder when accountHome (its home folder in the user database, user.Current)
// is set and is not home (HOME redirected: the login keychain stays in the account's real home folder, and an
// explicit path opens it); then the machine's.
func CredentialPaths(home, accountHome string) []string {
	var paths []string
	for _, name := range CredentialFiles() {
		paths = append(paths, filepath.Join(home, name))
	}
	if accountHome != "" && filepath.Clean(accountHome) != filepath.Clean(home) {
		paths = append(paths, filepath.Join(accountHome, "Library", "Keychains"))
	}
	return append(paths, MachineCredentials()...)
}

// MovedCredentials are credential stores the user's environment moves out of CredentialFiles' places: gh's config
// (GH_CONFIG_DIR, else $XDG_CONFIG_HOME/gh), which can hold a plain-text token. Only absolute values count, and never
// the home folder or one above it (a misconfigured variable would deny everything).
func MovedCredentials(environ []string, home string) []string {
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

// lookup is the value of name in environ, or "".
func lookup(environ []string, name string) string {
	for _, kv := range environ {
		if value, ok := strings.CutPrefix(kv, name+"="); ok {
			return value
		}
	}
	return ""
}
