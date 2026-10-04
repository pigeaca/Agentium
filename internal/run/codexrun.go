package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/codex"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/pricing"
)

// codexSweep is what tells a Codex run's leftover processes (codexLeftovers): its workspace and temp root, its marker
// (a folder of a random name in its workspace that only its sandbox profile lets a process write; "": none known) and
// when its agent started (no process older than that is its).
type codexSweep struct {
	workspace, tempRoot, marker string
	since                       time.Time
}

// markerPrefix starts the name of a Codex run's marker folder in its workspace (newMarker).
const markerPrefix = "own-"

// newMarker makes a Codex run's marker folder: a random, unguessable name in its workspace, which the run's profile
// lists as writable (agent.Invocation.Marker) and no other profile can (a grant for workspaces/*/repo does not cover
// it). The agent can write in it but not rename or remove it: the workspace folder is not writable to it.
func newMarker(workspace string) (string, error) {
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("the run's marker: %w", err)
	}
	marker := filepath.Join(workspace, markerPrefix+hex.EncodeToString(suffix))
	if err := os.Mkdir(marker, 0o700); err != nil {
		return "", fmt.Errorf("the run's marker: %w", err)
	}
	return marker, nil
}

// markerIn finds a Codex run's marker folder in its workspace (recovery: the start file does not name it), or "" when
// there is not exactly one.
func markerIn(workspace string) string {
	found, _ := filepath.Glob(filepath.Join(workspace, markerPrefix+"*"))
	var dirs []string
	for _, f := range found {
		if info, err := os.Lstat(f); err == nil && info.IsDir() {
			dirs = append(dirs, f)
		}
	}
	if len(dirs) != 1 {
		return ""
	}
	return dirs[0]
}

// leftProcess is a process a Codex run's commands may have left, which the sweep only reported (codexLeftovers): kept
// in the run's records (LeftoverProcesses) so that `agentium clean` can list it and, with --yes, stop it.
type leftProcess struct {
	PID       int    `json:"pid"`
	Command   string `json:"command"`
	StartSec  uint64 `json:"start_sec"`
	StartUsec uint64 `json:"start_usec"`
	Why       string `json:"why"`
}

// Started is when the process started.
func (p leftProcess) Started() time.Time {
	return time.Unix(int64(p.StartSec), int64(p.StartUsec)*1000)
}

// LeftoverProcesses, in a run's records, lists the processes its sweep reported but did not stop (leftProcess).
const LeftoverProcesses = "leftover-processes.json"

// sweepCodex stops the processes a Codex run's commands left that are shown to be its own on every count, and reports
// the ones shown on one count only (codexLeftovers): in the run's notes, with their IDs, names and start times, and in
// its records (LeftoverProcesses), which `agentium clean` reads. Codex's unified exec starts each command in a session
// of its own, outside the process group the runner kills. guard, when set (tests), sees every round's targets before
// any is stopped, and may refuse them: a test then never stops a process it did not start.
//
// In the spike none was left, even after SIGINT.
func sweepCodex(s codexSweep, guard func(pids []int) bool, records string) []string {
	var notes []string
	killed, reported, err := stopCodexLeftovers(s, guard)
	if len(killed) > 0 {
		notes = append(notes, fmt.Sprintf("stopped %d process(es) Codex's commands left running: %s", len(killed), strings.Join(killed, ", ")))
	}
	if err != nil {
		notes = append(notes, "Codex's leftover processes could not all be stopped: "+err.Error())
	}
	if len(reported) > 0 {
		var shown []string
		for _, p := range reported {
			shown = append(shown, fmt.Sprintf("%d %q started %s (%s)", p.PID, p.Command, p.Started().UTC().Format(time.RFC3339), p.Why))
		}
		notes = append(notes, fmt.Sprintf("%d process(es) that may be what Codex's commands left are still running, not stopped (not shown to be the run's on every count): %s; `agentium clean` lists them, and stops them with --yes",
			len(reported), strings.Join(shown, ", ")))
		if err := recordLeftovers(records, reported); err != nil {
			notes = append(notes, "the processes left could not be recorded for `agentium clean`: "+err.Error())
		}
	}
	return notes
}

// recordLeftovers adds processes to the records' list of what the run left (LeftoverProcesses), each once.
func recordLeftovers(records string, add []leftProcess) error {
	if records == "" {
		return nil
	}
	path := filepath.Join(records, LeftoverProcesses)
	var all []leftProcess
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &all) // a list that cannot be read is replaced
	}
	for _, p := range add {
		if !slices.ContainsFunc(all, func(q leftProcess) bool {
			return q.PID == p.PID && q.StartSec == p.StartSec && q.StartUsec == p.StartUsec
		}) {
			all = append(all, p)
		}
	}
	data, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
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
	notes := sweepCodex(codexSweep{workspace: workspace, tempRoot: layout.RunTemp(filepath.Base(workspace)), marker: markerIn(workspace),
		since: rec.Started}, nil, records)
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
//     of what was read and the most a run with its cap (CapUSD) can spend: the cap and one more request (codex.Bound).
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
	usd := max(m.CostUSD, codex.Bound(rec.Model, rec.CapUSD))
	m.CostUSD, m.EstimatedCostUSD, m.UnpricedRequests, rec.CostEstimated = usd, usd, 0, true
	rec.Notes = append(rec.Notes, fmt.Sprintf("%s: $%.2f, the larger of that and the most a run with its $%.2f cap can spend (the cap and one more request), is counted as its spend",
		what, usd, rec.CapUSD))
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
		// Keys are redacted too (a tool or subagent name comes from the transcript). Two keys that redact to one are
		// merged, in the keys' sorted order so the result does not depend on the map's: counts add up, lists join,
		// otherwise the first stays.
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface()) })
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for _, key := range keys {
			k, value := redactValue(key, secrets), redactValue(v.MapIndex(key), secrets)
			if prev := out.MapIndex(k); prev.IsValid() {
				value = mergeValues(prev, value)
			}
			out.SetMapIndex(k, value)
		}
		return out
	}
	return v
}

// mergeValues is two map values whose keys redact to one: numbers summed, slices joined, else the first.
func mergeValues(a, b reflect.Value) reflect.Value {
	out := reflect.New(a.Type()).Elem()
	switch a.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		out.SetInt(a.Int() + b.Int())
	case reflect.Float32, reflect.Float64:
		out.SetFloat(a.Float() + b.Float())
	case reflect.Slice:
		return reflect.AppendSlice(reflect.AppendSlice(reflect.MakeSlice(a.Type(), 0, a.Len()+b.Len()), a), b)
	default:
		return a
	}
	return out
}
