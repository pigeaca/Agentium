package agent

import "time"

// Invocation is one headless agent run, in terms every agent shares: where it starts and what it may write, what it
// may not read, the build tools that choose its environment allowlist and its network needs, its cap, timeout, model,
// effort and prompt, and its sign-in. An adapter turns it into its agent's command (Adapter.Command); fields an agent
// does not use stay empty.
type Invocation struct {
	CLI       string // path to the agent's executable
	Dir       string // where the agent starts: the run's checkout, or a monorepo module's folder inside it (Repo)
	Prompt    string
	Model     string
	Effort    string  // empty: the CLI's default
	BudgetUSD float64 // the run's cost cap; 0: none
	// Timeout bounds the run (0: none beyond the context), and Grace is the wait between the gentle stop (SIGINT) and
	// SIGKILL once it ends (Run).
	Timeout, Grace time.Duration
	SignIn         string // the sign-in mode, in the agent's own terms (Claude Code: claude.SignInAPIKey, ...)
	Secret         string // the API key or token, for the modes that take one; never logged or stored
	ConfigDir      string // a fresh, empty folder of the run's own for the agent's configuration, for the modes with a secret
	TokenFile      string // the sign-in token's file, when it came from one: its folder the agent may not read
	Home           string // the user's home folder
	// Repo, when set, is the run's whole checkout and Dir a folder inside it (a monorepo module's, where the agent starts
	// as a developer would, so the agent loads the root's and the module's instructions). The whole checkout stays the
	// agent's, as at the root: it is a working folder the sandbox lets it write, and the build tools' environment names
	// it (Python's import root is relative to it). Empty: Dir is the checkout, and the command is exactly what it was
	// before modules.
	Repo string
	// AccountHome is the account's home folder in the user database (user.Current), when known. HOME (Home) can point
	// elsewhere, but the account's login keychain stays in the real home folder, where an explicit path opens it, so
	// that folder is denied too (sandbox.CredentialPaths). Empty: only Home's.
	AccountHome string
	// Deny lists absolute paths the agent must not read, through its sandboxed shell or its file tools: Agentium's data
	// (other runs, hidden tests, the database), the user's repository, and verification copies.
	Deny []string
	// Started, when set, is called with the agent's process ID, which is also its process group, once it runs.
	Started func(pid int)
	// BuildCache, when set, is a folder of the run's own for build caches: the build tools' agent caches point there
	// (buildtool.AgentCacheEnv: Go's GOCACHE), and the sandbox lets the agent write it. The user's own caches are denied
	// (buildtool.UserCaches): they hold what earlier builds compiled, the hidden tests of validations and gradings included.
	BuildCache string
	// TempRoot, when set, is the run's own temp root, an existing owner-only folder: the agent keeps its temp files
	// there, so the folders every other session of the user shares are denied to it (sandbox.SharedTempDirs).
	TempRoot string
	// UID is the user's id (os.Getuid()), which names the shared temp folders; read only with TempRoot.
	UID int
	// Tools names the build-tool profiles the run's repository has (buildtool.DetectedNames); the always-on ones (Go's)
	// apply besides. They choose the environment allowlist, the agent's environment and caches, and the sandbox's
	// local-binding setting.
	Tools []string
	// AgentTools names the always-on profiles whose agent side stays on though Tools lacks them (buildtool.AgentKept of
	// the base commit: Go, with a go.mod, go.work or .go file anywhere). See buildtool.SelectRun.
	AgentTools []string
	// Deps is the folder of warmed dependencies (home.Layout.Deps for the project): the agent's offline builds read it,
	// and the sandbox keeps it read-only. It lies outside every denied folder. Empty: none.
	Deps string
	// AllowLocalBinding is the user's opt-in (`agentium init --allow-local-binding`) for the sandbox's local binding,
	// the one network need a run may have (Gradle's file-lock service). Without it such a run does not start.
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
