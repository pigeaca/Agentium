package run

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/codex"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/pricing"
)

// sweepCodex stops the processes a Codex run's commands left, and says what it stopped, or could not. Codex's unified
// exec starts each command in a session of its own, outside the process group the runner kills, so two sweeps follow
// the agent, as for a grade's leftovers:
//   - by sandbox (stopSandboxed): every process in the run's own Seatbelt sandbox, the one profile that lets a process
//     write the run's checkout but not the workspace folder that holds it (another run's agent, a grade, an unsandboxed
//     process and the system's own sandboxed agents each fail one of the two: theirs allow both or neither). It finds a
//     detached child however little it holds (setsid, cd /, every descriptor closed, a system binary exec'd), as long
//     as the checkout is the folder the profile names: the agent can write in it, but not rename or replace it (the
//     workspace folder is not writable to it). Not the temp root: in /tmp, which the system's sandboxed agents may
//     write too (cfprefsd, sharingd: found by a first version of this sweep);
//   - by path (stopProcessesUnder): whatever still uses the workspace or the temp root, sandboxed or not.
//
// In the spike none was left, even after SIGINT.
func sweepCodex(workspace, tempRoot string) []string {
	var notes []string
	killed, err := stopSandboxed(filepath.Join(workspace, "repo"), workspace)
	more, err2 := stopProcessesUnder([]string{workspace, tempRoot})
	killed = append(killed, more...)
	if len(killed) > 0 {
		notes = append(notes, fmt.Sprintf("stopped %d process(es) Codex's commands left running: %s", len(killed), strings.Join(killed, ", ")))
	}
	if err := errors.Join(err, err2); err != nil {
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

// codexSpendFallback gives a Codex run whose spend could not be read whole from its rollouts an estimate that never
// undercounts, marked as one (CostEstimated, a note):
//   - no rollout at all (Gather failed, or Agentium died and it is gone), with the stream's whole-turn totals
//     (turn.completed): those totals at the requested model's list prices; per-request sizes are unknown, so the
//     long-context limit cannot be checked;
//   - a collection or a read left incomplete (Metrics.RolloutsIncomplete: a rollout left behind, one cut short), or no
//     usage at all (an interrupted run: Codex prints usage only when its turn completes), or no list price: the larger
//     of what was read and the run's cap (CapUSD), which Agentium's watcher kept its spend under.
//
// A run whose stream shows no session (no thread.started) never reached the API and spent nothing; rollouts read whole,
// even without requests, are the spend: nothing changes. An estimate only ever raises the cost.
func codexSpendFallback(rec *Record) {
	m := &rec.Metrics
	if agent.Name(rec.Agent) != codex.Name || !m.SawInit || m.Rollouts > 0 && !m.RolloutsIncomplete {
		return
	}
	if rates, ok := pricing.OpenAILookup(rec.Model); ok && !m.RolloutsIncomplete && m.InputTokens+m.CacheReadTokens+m.CacheWriteTokens+m.OutputTokens > 0 {
		usd := (float64(m.InputTokens)*rates.Input + float64(m.CacheReadTokens)*rates.CachedInput + float64(m.CacheWriteTokens)*rates.CacheWrite +
			float64(m.OutputTokens)*rates.Output) / 1e6
		m.CostUSD, m.EstimatedCostUSD, m.UnpricedRequests, rec.CostEstimated = max(usd, m.CostUSD), max(usd, m.CostUSD), 0, true
		rec.Notes = append(rec.Notes, fmt.Sprintf("Codex's session rollout is missing: the cost is estimated from the stream's whole-turn tokens at %s's list prices of %s (per-request sizes are unknown, so the long-context limit cannot be checked)",
			rec.Model, pricing.OpenAIDate))
		return
	}
	what := "Codex's session rollout is missing and its stream holds no usage"
	if m.RolloutsIncomplete {
		what = fmt.Sprintf("Codex's session rollouts were collected or read only in part ($%.3f read)", m.CostUSD)
	}
	if rec.CapUSD <= 0 {
		rec.Notes = append(rec.Notes, what+", and the run had no cap: what it spent is unknown beyond what was read")
		return
	}
	usd := max(m.CostUSD, rec.CapUSD)
	m.CostUSD, m.EstimatedCostUSD, m.UnpricedRequests, rec.CostEstimated = usd, usd, 0, true
	rec.Notes = append(rec.Notes, fmt.Sprintf("%s: $%.2f, the larger of that and the run's cap, is counted as its spend", what, usd))
}

// redactRecord is rec with each secret, and every credential-shaped string (Redact), removed from all its text: the
// record is what is stored (the database, the start file), and much of it is the agent's output (its final message,
// notes, drift, the judge's reasons). Redacting the files on disk is not enough. Pointers, slices and maps are copied,
// never changed in place, so a caller's record stays as it was.
func redactRecord(rec Record, secrets ...string) Record {
	return redactValue(reflect.ValueOf(rec), secrets).Interface().(Record)
}

func redactValue(v reflect.Value, secrets []string) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		return reflect.ValueOf(string(Redact([]byte(v.String()), secrets...))).Convert(v.Type())
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if out.Field(i).CanSet() {
				out.Field(i).Set(redactValue(v.Field(i), secrets))
			}
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(redactValue(v.Elem(), secrets))
		return out
	case reflect.Slice:
		if v.IsNil() || v.Type().Elem().Kind() == reflect.Uint8 {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(redactValue(v.Index(i), secrets))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for _, key := range v.MapKeys() {
			out.SetMapIndex(key, redactValue(v.MapIndex(key), secrets))
		}
		return out
	}
	return v
}
