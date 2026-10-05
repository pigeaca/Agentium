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
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/pigeaca/agentium/internal/home"
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

// Stop is why Agentium stopped a run before its agent ended it (Result.Stop).
type Stop string

// Stop reasons.
const (
	StopNone    Stop = ""        // the agent ended by itself (or Agentium was cancelled: Run's error says so)
	StopTimeout Stop = "timeout" // the run's timeout (Invocation.Timeout)
	StopCap     Stop = "cap"     // Agentium's cost cap, from the adapter's watcher (Command.Watch)
	// StopBlind: the watcher could not see what the run spends (Codex: no session rollout where it should be), so it
	// stopped the run rather than let it spend past its cap unseen. Not the agent's doing: infrastructure.
	StopBlind Stop = "blind"
)

// Command is how one run of an agent starts (Adapter.Command).
type Command struct {
	Args []string // after the executable (Invocation.CLI)
	Env  []string // the agent's whole environment
	// Stdin is the agent's standard input (Codex takes its prompt there); empty: none.
	Stdin string
	// Dirs are folders of the run's own the agent needs before it starts (Codex: its run-local home and state), made
	// owner-only by Run.
	Dirs []string
	// Exclusive, when set, is a lock file Run holds while the agent runs: runs that name the same file run one at a time
	// (Codex's ChatGPT login, whose token refreshes would race). Run waits for it until its context ends.
	Exclusive string
	// Watch, when set, watches the running agent: Run calls it once the agent has started, and stops the agent (gently,
	// as at a timeout) when it returns a reason (Codex: StopCap, Agentium's cost cap). It must return StopNone soon after
	// ctx ends, which Run does when the agent ends.
	Watch func(ctx context.Context) Stop
}

// Result is how an agent's run ended: its process's result, and why Agentium stopped it, if it did.
type Result struct {
	runner.Result
	Stop Stop
}

// Adapter is one coding agent: how to start it headless and isolated, and how to read what it reported. Its methods
// are pure apart from Version, Gather and Parse (and the file system reads DeniedPaths makes): Run starts the process.
type Adapter interface {
	// Name is the agent's name in records (ClaudeCode).
	Name() string
	// Command is how the run starts: its arguments (without the executable, inv.CLI), its whole environment, its
	// standard input and the folders it needs. environ is the parent's environment (os.Environ()), which the adapter
	// filters through the shared allowlist (sandbox.EnvironFor); the sign-in's secret is the only credential the agent
	// may receive. It refuses an invocation it cannot run safely (a relative path, a refused local binding, a temp root
	// too long).
	Command(inv Invocation, environ []string) (Command, error)
	// DeniedPaths is every path the run's agent may not read, as its sandbox will list them: the shared deny list
	// (sandbox.AgentDenied) plus the agent's own data.
	DeniedPaths(inv Invocation, environ []string) []string
	// Gather brings what the agent left outside the run's records folder into it, once the agent has ended (Codex: its
	// session rollouts, from its home, configDir: Invocation.ConfigDir). Recovery calls it too, for a run whose
	// Agentium process died, so it must be safe to repeat.
	Gather(configDir, records string) error
	// Parse reads what the run reported from its records folder (the agent's standard output is its stream.jsonl).
	// Partial metrics come back with an error.
	Parse(records string) (Metrics, error)
	// Classify decides a run's outcome from its metrics, why Agentium stopped it (if it did), and its environment's
	// drift.
	Classify(m Metrics, stop Stop, drift []string) string
	// Check lists how a run's environment drifted from what was expected. Any drift makes a run unfair.
	Check(m Metrics, expect Expect) []string
	// Version runs the CLI at cli, outside any repository, and returns its version.
	Version(ctx context.Context, cli string) (string, error)
}

// Transcript is the file in a run's records folder that holds the agent's standard output.
const Transcript = "stream.jsonl"

// Run starts inv through a and waits: the agent's standard output (its transcript) goes to transcript, its standard
// error to errOut. Both are files, not pipes, so a background process that keeps one open cannot hold the run past
// the agent's end. On inv.Timeout, cancellation or the watcher's stop (Command.Watch), the run's process group is
// interrupted first, so the agent can finish its turn and report a result, then killed after inv.Grace.
func Run(ctx context.Context, a Adapter, inv Invocation, environ []string, transcript, errOut *os.File) (Result, error) {
	cmd, err := a.Command(inv, environ)
	if err != nil {
		return Result{}, err
	}
	for _, dir := range cmd.Dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Result{}, fmt.Errorf("the agent's folder: %w", err)
		}
	}
	if cmd.Exclusive != "" {
		unlock, err := home.LockFile(ctx, cmd.Exclusive, nil)
		if err != nil {
			return Result{}, fmt.Errorf("wait for the agent's lock: %w", err)
		}
		defer unlock()
	}
	spec := runner.Spec{Dir: inv.Dir, Args: append([]string{inv.CLI}, cmd.Args...), Environ: cmd.Env,
		Timeout: inv.Timeout, Grace: inv.Grace, Output: transcript, Stderr: errOut, Started: inv.Started}
	if cmd.Stdin != "" {
		spec.Stdin = strings.NewReader(cmd.Stdin)
	}
	var stop Stop
	var watching sync.WaitGroup
	watchCtx, endWatch := context.WithCancel(ctx)
	spec.BeforeStop = inv.BeforeStop
	if inv.Observe != nil {
		started := spec.Started
		spec.Started = func(pid int) {
			if started != nil {
				started(pid)
			}
			poll := inv.Observe(pid) // here, not in the goroutine: the runner waits for the agent only after Started
			if poll == nil {
				return
			}
			watching.Add(1)
			go func() {
				defer watching.Done()
				poll(watchCtx)
			}()
		}
	}
	if cmd.Watch != nil {
		stopNow := make(chan struct{})
		spec.Stop = stopNow
		started := spec.Started
		spec.Started = func(pid int) {
			if started != nil {
				started(pid)
			}
			watching.Add(1)
			go func() {
				defer watching.Done()
				if reason := cmd.Watch(watchCtx); reason != StopNone && watchCtx.Err() == nil {
					stop = reason
					close(stopNow)
				}
			}()
		}
	}
	result, err := runner.Run(ctx, spec)
	endWatch()
	watching.Wait() // stop is written only by the watcher, which has returned
	switch {
	case result.TimedOut:
		return Result{Result: result, Stop: StopTimeout}, err
	case result.Stopped:
		return Result{Result: result, Stop: stop}, err
	}
	return Result{Result: result}, err
}
