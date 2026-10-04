// Package codex runs OpenAI's Codex CLI headless for Agentium and reads what it reports: Codex's adapter behind the
// agent seam (Adapter, internal/agent). Each run follows the recipe the Codex spike verified with Codex CLI 0.160.0
// (docs/research/2026-10-04-codex-spike.md; the Codex plan, .agents/plans/2026-10-04-codex.md):
//
//   - `codex exec --json` with the prompt on stdin, `--ignore-user-config`, and every setting as a `-c` override: no
//     configuration file of the user's or the project's is read, and the checkout stays untrusted;
//   - Codex's own Seatbelt sandbox, with an Agentium permission profile: the disk readable, the checkout, the run's build
//     cache and temp root writable, the shared deny list (internal/sandbox) plus Codex's and Claude Code's own data
//     denied, and no network;
//   - an environment built from the shared allowlist, with a run-local HOME, TMPDIR and TMPPREFIX; the agent's shells get
//     the user's HOME back, and never Codex's variables or a key (shell_environment_policy);
//   - sign-in: the ChatGPT login in Agentium's own Codex home (<data>/codex, CODEX_HOME), shared by every run and run
//     one at a time, or an API key (CODEX_API_KEY) given to Codex alone with a fresh CODEX_HOME per run;
//   - Agentium's cost cap, since Codex has none: a watcher prices each request from the session's rollout as it is
//     written, and stops the run before one more full-context request could pass the cap;
//   - after the run, the session's rollouts move from the Codex home into the run's records (Gather), without the
//     account's IDs, and the shell snapshot Codex may leave behind is deleted.
//
// Agentium never reads, copies or writes Codex's credentials (auth.json), never uses the user's own ~/.codex, and never
// runs `codex login` or `logout`: the user signs in.
package codex

import (
	"context"

	"github.com/pigeaca/agentium/internal/agent"
)

// Name is Codex's name in records.
const Name = "codex"

// SupportedVersion is the Codex CLI the recipe was verified with. Runs refuse another minor version (CheckVersion):
// a default that changes between versions (0.160.0 reads shell_environment_policy.ignore_default_excludes as true
// from TOML) can quietly undo the isolation.
const SupportedVersion = "0.160.0"

// Sign-in modes (the names Claude Code's use).
const (
	SignInLogin  = "login"   // the ChatGPT login in Agentium's own Codex home, shared by every run, one run at a time
	SignInAPIKey = "api-key" // CODEX_API_KEY (or OPENAI_API_KEY) from Agentium's environment, to Codex alone
)

// DefaultModel is the model a Codex run uses when none is given (the plan's decision 6).
const DefaultModel = "gpt-6.1-sol"

// Model is what Agentium knows of a Codex model beyond its price (internal/pricing): its default effort, which a run
// always passes (Codex's rollout records the effort only when it is passed), and the size of its largest request,
// which the cost cap holds back as an allowance.
type Model struct {
	// DefaultEffort is the catalog's (`codex debug models`, 2026-10-04).
	DefaultEffort string
	// ContextWindow is the most input tokens one request can carry: the effective window Codex reports
	// (model_context_window in the rollout's task_started), 258,400 for gpt-6.1-sol, though the catalog says 272,000.
	ContextWindow int64
	// MaxOutput is the most output tokens one request can return. Assumed, not pinned: OpenAI's limit for its GPT-5
	// models (128,000); the spike could not read one for gpt-6.1-sol. It errs high, as an allowance should.
	MaxOutput int64
}

// models is the catalog Agentium knows. A model missing here runs only with an explicit effort and without a cost
// cap's allowance: a run with a cap refuses it.
func models() map[string]Model {
	return map[string]Model{
		"gpt-6.1-sol": {DefaultEffort: "low", ContextWindow: 258_400, MaxOutput: 128_000},
	}
}

// LookupModel returns what Agentium knows of a Codex model.
func LookupModel(model string) (Model, bool) {
	m, ok := models()[model]
	return m, ok
}

// Adapter is Codex behind the agent seam (agent.Adapter).
type Adapter struct{}

var _ agent.Adapter = Adapter{}

// Name is Codex's name in records.
func (Adapter) Name() string { return Name }

// Version is the Codex CLI's dotted version (Version).
func (Adapter) Version(ctx context.Context, cli string) (string, error) { return Version(ctx, cli) }
