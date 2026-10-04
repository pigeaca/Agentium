package run

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/codex"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/pricing"
)

// sweepCodex stops the processes a Codex run's commands left that still use its workspace or temp root, and says what
// it stopped, or could not. Codex's unified exec starts each command in a session of its own, outside the process
// group the runner kills, so the sweep a grade's leftovers get (stopProcessesUnder) runs after the agent too. None was
// left in the spike, even after SIGINT; a setsid child was not tried.
func sweepCodex(workspace, tempRoot string) []string {
	var notes []string
	killed, err := stopProcessesUnder([]string{workspace, tempRoot})
	if len(killed) > 0 {
		notes = append(notes, fmt.Sprintf("stopped %d process(es) Codex's commands left running: %s", len(killed), strings.Join(killed, ", ")))
	}
	if err != nil {
		notes = append(notes, "Codex's leftover processes could not all be stopped: "+err.Error())
	}
	return notes
}

// codexHomeOf is the Codex home of a run recorded with the sign-in mode, whose workspace was workspace (Env.codexHome).
func codexHomeOf(layout home.Layout, signIn, workspace string) string {
	return Env{Layout: layout, SignIn: signIn}.codexHome(workspace)
}

// gatherOrphan is what a run a dead Agentium left needs before its workspace goes: for Codex, what its commands left
// running is stopped, and its sessions' rollouts move into its records (an API key run's Codex home is in the
// workspace). Claude Code's runs need neither (its Gather does nothing).
func gatherOrphan(layout home.Layout, rec Record, workspace, records string) []string {
	if agent.Name(rec.Agent) != codex.Name {
		return nil
	}
	notes := sweepCodex(workspace, layout.RunTemp(filepath.Base(workspace)))
	if err := (codex.Adapter{}).Gather(codexHomeOf(layout, rec.SignIn, workspace), records); err != nil {
		notes = append(notes, "the agent's session could not be moved into the run's records: "+err.Error())
	}
	return notes
}

// sniffAdapter is the adapter of the agent whose transcript this is, when nothing else names it (a start file too
// damaged to read): Codex's when the first event the transcript holds is Codex's thread.started, else Claude Code's,
// the agent of every record made before Codex.
func sniffAdapter(transcript string) agent.Adapter {
	if codex.ThreadID(transcript) != "" {
		return codex.Adapter{}
	}
	return claude.Adapter{}
}

// codexSpendFallback gives a Codex run whose spend could not be read from its rollouts (Gather failed, or Agentium died
// before it ran and the rollout is gone) an estimate that never undercounts, marked as one (CostEstimated, a note):
//   - with the stream's whole-turn totals (turn.completed), those totals at the requested model's list prices; per
//     request sizes are unknown, so the long-context limit cannot be checked;
//   - with no usage at all (an interrupted run: Codex prints usage only when its turn completes), or no list price,
//     the run's cap (CapUSD), which Agentium's watcher kept its spend under.
//
// A run whose stream shows no session (no thread.started) never reached the API and spent nothing. A rollout that was
// read, even without requests, is the spend: nothing changes.
func codexSpendFallback(rec *Record) {
	m := &rec.Metrics
	if agent.Name(rec.Agent) != codex.Name || m.Rollouts > 0 || !m.SawInit {
		return
	}
	if rates, ok := pricing.OpenAILookup(rec.Model); ok && m.InputTokens+m.CacheReadTokens+m.CacheWriteTokens+m.OutputTokens > 0 {
		usd := (float64(m.InputTokens)*rates.Input + float64(m.CacheReadTokens)*rates.CachedInput + float64(m.CacheWriteTokens)*rates.CacheWrite +
			float64(m.OutputTokens)*rates.Output) / 1e6
		m.CostUSD, m.EstimatedCostUSD, m.UnpricedRequests, rec.CostEstimated = usd, usd, 0, true
		rec.Notes = append(rec.Notes, fmt.Sprintf("Codex's session rollout is missing: the cost is estimated from the stream's whole-turn tokens at %s's list prices of %s (per-request sizes are unknown, so the long-context limit cannot be checked)",
			rec.Model, pricing.OpenAIDate))
		return
	}
	if rec.CapUSD <= 0 {
		rec.Notes = append(rec.Notes, "Codex's session rollout is missing and the run had no cap: what it spent is unknown")
		return
	}
	m.CostUSD, m.EstimatedCostUSD, m.UnpricedRequests, rec.CostEstimated = rec.CapUSD, rec.CapUSD, 0, true
	rec.Notes = append(rec.Notes, fmt.Sprintf("Codex's session rollout is missing and its stream holds no usage: the run's $%.2f cap is counted as its spend", rec.CapUSD))
}
