// Package agent is the seam between Agentium and the coding agents it runs: an agent-neutral run (Invocation), what a
// run reports (Metrics) and how it ended (the Outcome* values), what a fair run's environment looks like (Expect), and
// the Adapter each agent implements (internal/claude for Claude Code). The sandbox policy every agent shares, the deny
// list and the environment allowlist, is internal/sandbox's (AgentDenied, EnvironFor), so each agent's run is denied
// the same paths and gets the same environment.
//
// Records name their agent (Name): one without a name is Claude Code's, the only agent before the seam.
package agent

import (
	"context"
	"io"
	"os"

	"github.com/pigeaca/agentium/internal/runner"
)

// ClaudeCode is Claude Code's name in records.
const ClaudeCode = "claude-code"

// Name is the agent a record names (recorded), or Claude Code when it names none: records made before the seam.
func Name(recorded string) string {
	if recorded == "" {
		return ClaudeCode
	}
	return recorded
}

// Outcomes. ok, capped and timeout are the agent's; infra and unfair runs never had a fair attempt and are not counted.
const (
	OutcomeOK      = "ok"
	OutcomeCapped  = "capped"  // hit the turn or budget cap
	OutcomeTimeout = "timeout" // Agentium stopped it
	OutcomeInfra   = "infra"   // no result, a crash, sign-in, limits, overload or transport
	OutcomeUnfair  = "unfair"  // the environment drifted (see Adapter.Check)
	// OutcomeCancelled is Agentium's, not the agent's: the run was interrupted (Ctrl-C, or its Agentium process died
	// and a later one recovered it). Never counted, and not an infrastructure failure either.
	OutcomeCancelled = "cancelled"
)

// Adapter is one coding agent: how to start it headless and isolated, and how to read what it reported. Its methods
// are pure apart from Version (and the file system reads DeniedPaths makes): Run starts the process.
type Adapter interface {
	// Name is the agent's name in records (ClaudeCode).
	Name() string
	// Command is the run's arguments (without the executable, inv.CLI) and its whole environment. environ is the
	// parent's environment (os.Environ()), which the adapter filters through the shared allowlist
	// (sandbox.EnvironFor); the sign-in's secret is the only credential the agent may receive. It refuses an
	// invocation it cannot run safely (a relative path, a refused local binding, a temp root too long).
	Command(inv Invocation, environ []string) (args, env []string, err error)
	// DeniedPaths is every path the run's agent may not read, as its sandbox will list them: the shared deny list
	// (sandbox.AgentDenied) plus the agent's own data.
	DeniedPaths(inv Invocation, environ []string) []string
	// Parse reads the run's transcript, the agent's standard output. Partial metrics come back with an error.
	Parse(r io.Reader) (Metrics, error)
	// Classify decides a run's outcome from its metrics, whether Agentium stopped it, and its environment's drift.
	Classify(m Metrics, timedOut bool, drift []string) string
	// Check lists how a run's environment drifted from what was expected. Any drift makes a run unfair.
	Check(m Metrics, expect Expect) []string
	// Version runs the CLI at cli, outside any repository, and returns its version.
	Version(ctx context.Context, cli string) (string, error)
}

// Run starts inv through a and waits: the agent's standard output (its transcript) goes to transcript, its standard
// error to errOut. Both are files, not pipes, so a background process that keeps one open cannot hold the run past
// the agent's end. On inv.Timeout or cancellation the run's process group is interrupted first, so the agent can
// finish its turn and report a result, then killed after inv.Grace.
func Run(ctx context.Context, a Adapter, inv Invocation, environ []string, transcript, errOut *os.File) (runner.Result, error) {
	args, env, err := a.Command(inv, environ)
	if err != nil {
		return runner.Result{}, err
	}
	return runner.Run(ctx, runner.Spec{Dir: inv.Dir, Args: append([]string{inv.CLI}, args...), Environ: env,
		Timeout: inv.Timeout, Grace: inv.Grace, Output: transcript, Stderr: errOut, Started: inv.Started})
}
