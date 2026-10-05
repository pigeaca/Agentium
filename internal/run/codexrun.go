package run

import (
	"cmp"
	"context"
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
	"sync"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/codex"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/pricing"
)

// codexSweep is what tells the processes a Codex run may have left (codexReports): its workspace and temp root, its marker
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

// leftProcess is a process a Codex run's commands may have left that Agentium did not see descend from its agent
// (codexReports): reported, never stopped. Kept in the run's records (LeftoverProcesses), which `agentium clean` lists
// (it never signals a process: the user stops one that is theirs to stop).
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

// AgentProcesses, in a run's records, lists every process Agentium saw descend from the run's agent (descendants), so
// that recovery stops them too after a crash.
const AgentProcesses = "agent-processes.json"

// identity tells a process from any later one that takes its ID: its ID and start time (seconds and microseconds).
type identity struct {
	PID     int    `json:"pid"`
	Sec     uint64 `json:"start_sec"`
	Usec    uint64 `json:"start_usec"`
	Command string `json:"command,omitempty"`
}

func (i identity) key() [3]uint64     { return [3]uint64{uint64(i.PID), i.Sec, i.Usec} }
func (i identity) started() time.Time { return time.Unix(int64(i.Sec), int64(i.Usec)*1000) }

// descendantsPoll is how often Agentium looks at the agent's descendants while it runs; descendantsSave is how often,
// at most, what it saw is saved meanwhile (a save is forced on the agent's start, before a stop and at the sweep).
const (
	descendantsPoll = 100 * time.Millisecond
	descendantsSave = time.Second
)

// tableEntry is one process of the user's in a read of the process table: its identity, its parent's ID, and whether
// it has exited, not yet reaped (a zombie: it cannot be stopped, and its children have been reparented).
type tableEntry struct {
	id     identity
	ppid   int
	zombie bool
}

// descendants is what Agentium saw descend from a run's agent: the agent process itself and every process whose parent
// was one seen before, in one of the snapshots taken while it ran (snapshot: a generation each), by full identity.
// Only these are ever stopped. Persisted in the run's records (AgentProcesses), so that a recovery stops them too.
//
// Residuals: a descendant whose parent ends (it detaches) before a snapshot links it, including one whose parent ends
// during the snapshot's read, is not tracked, only reported; and the window between reading a process's identity and
// signalling it is the same as for any kill by ID (macOS has no process handle that pins an ID).
type descendants struct {
	file    string
	mu      sync.Mutex
	tracked map[[3]uint64]identity
	// dirty: tracked changed since the last save that succeeded. tried is the last save's time (the rate limit);
	// failures and lastErr are the saves that failed (descendants.note).
	dirty    bool
	tried    time.Time
	failures int
	lastErr  error
	noRoot   bool // the agent's own process could not be read: nothing descends from it here
	// read, identify and now are the process table, one process's identity as it is now (ok false: gone, or another
	// user's) and the clock. nil: the system's (processTable, identityNow, time.Now); tests replace them.
	read     func() (map[int]tableEntry, error)
	identify func(pid int) (identity, bool)
	now      func() time.Time
}

// loadDescendants is what a run's records say was seen (none when the file is missing or cannot be read).
func loadDescendants(file string) *descendants {
	d := &descendants{file: file, tracked: map[[3]uint64]identity{}}
	if data, err := os.ReadFile(file); err == nil {
		var list []identity
		if json.Unmarshal(data, &list) == nil {
			for _, id := range list {
				d.tracked[id.key()] = id
			}
		}
	}
	return d
}

func (d *descendants) table() (map[int]tableEntry, error) {
	if d.read != nil {
		return d.read()
	}
	return processTable()
}

func (d *descendants) identityOf(pid int) (identity, bool) {
	if d.identify != nil {
		return d.identify(pid)
	}
	return identityNow(pid)
}

func (d *descendants) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// list is what d tracks, in a fixed order.
func (d *descendants) list() []identity {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]identity, 0, len(d.tracked))
	for _, id := range d.tracked {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b identity) int { return cmp.Compare(a.PID, b.PID) })
	return out
}

// observe makes the agent started with process ID pid the root of what d tracks, and saves it, before it returns: it
// runs from agent.Run's Started, before the runner can reap the agent, so even an agent that has already exited is
// read (a zombie keeps its identity). The function it returns looks every poll until its context ends
// (agent.Invocation.Observe).
func (d *descendants) observe(pid int) func(context.Context) {
	if d.identify == nil && !tracksDescendants {
		return func(context.Context) {}
	}
	id, ok := d.identityOf(pid)
	d.mu.Lock()
	if ok {
		d.tracked[id.key()] = id
		d.dirty = true
		d.save(true)
	} else {
		d.noRoot = true
	}
	d.mu.Unlock()
	return d.poll
}

// poll snapshots every descendantsPoll until ctx ends.
func (d *descendants) poll(ctx context.Context) {
	ticker := time.NewTicker(descendantsPoll)
	defer ticker.Stop()
	for {
		_ = d.snapshot(false)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// snapshot reads the process table once and adds to d every process whose parent is one d already tracked before
// this read began and still holds, after it, exactly the identity tracked; tracked processes no longer alive (gone,
// or zombies) are dropped (they can neither be stopped nor be anyone's parent: a process's children are reparented
// when it exits). What changed is saved at most every descendantsSave, or now when force is set; a failed save is
// retried at the next snapshot (descendants.note).
//
// Why that proves descent without trusting the clock or the read's timing: the kernel lists the IDs at once but reads
// each process after, so a parent ID read during the read could name a process that took it meanwhile. A parent alive
// before the read began (tracked then) and still alive after it (the same identity: an ID and a start time no later
// process can have) held its ID throughout, so any parent ID read meanwhile names it. A parent that ended during the
// read, or cannot be read, proves nothing: its children are not linked (they are reported if they were the run's). So
// each snapshot adds one generation: a grandchild is linked by the next one, a poll later.
//
// The whole snapshot (read, link, drop, save) holds d.mu, so two (the poll and BeforeStop) never interleave: a
// stale read cannot drop what a newer one added.
func (d *descendants) snapshot(force bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	before := make(map[int]identity, len(d.tracked)) // by ID: what was tracked before this read began
	for _, id := range d.tracked {
		before[id.PID] = id
	}
	table, err := d.table()
	if err != nil {
		d.save(force)
		return err
	}
	for k, id := range d.tracked {
		if e, ok := table[id.PID]; !ok || e.id.key() != k || e.zombie {
			delete(d.tracked, k)
			d.dirty = true
		}
	}
	verdict := map[int]bool{} // a parent's ID: whether it still holds exactly the identity tracked
	for _, e := range table {
		parent, ok := before[e.ppid]
		if _, tracked := d.tracked[e.id.key()]; tracked || e.zombie || !ok || parent.started().After(e.id.started()) {
			continue
		}
		holds, checked := verdict[parent.PID]
		if !checked {
			now, alive := d.identityOf(parent.PID)
			holds = alive && now.key() == parent.key()
			verdict[parent.PID] = holds
		}
		if holds {
			d.tracked[e.id.key()] = e.id
			d.dirty = true
		}
	}
	d.save(force)
	return nil
}

// save writes what d tracks to its file when it changed, at most every descendantsSave unless force is set. The file is
// replaced by a rename, without waiting for the disk (an fsync takes milliseconds on macOS, and a run's agent shares the
// machine): a crash of Agentium leaves the old file or the new one, a crash of the machine ends the processes anyway.
// Called with d.mu held.
func (d *descendants) save(force bool) {
	if !d.dirty || d.file == "" {
		return
	}
	now := d.clock()
	if !force && now.Sub(d.tried) < descendantsSave {
		return
	}
	d.tried = now
	list := make([]identity, 0, len(d.tracked))
	for _, id := range d.tracked {
		list = append(list, id)
	}
	slices.SortFunc(list, func(a, b identity) int { return cmp.Compare(a.PID, b.PID) })
	data, err := json.Marshal(list)
	if err == nil {
		err = replaceFile(d.file, data, 0o600, false)
	}
	if err != nil {
		d.failures++
		d.lastErr = err
		return
	}
	d.dirty = false
}

// note is what the run's notes say about d's own trouble: the agent's process could not be read, or saves failed ("":
// none).
func (d *descendants) note() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.noRoot:
		return "the agent's own process could not be read when it started: the processes its commands left were not tracked, only looked for"
	case d.failures == 0:
		return ""
	case d.dirty:
		return fmt.Sprintf("the processes seen descending from the agent could not be saved in its records (%d failure(s), the last: %v): a recovery after a crash would not stop the latest of them", d.failures, d.lastErr)
	}
	return fmt.Sprintf("the processes seen descending from the agent could not be saved in its records %d time(s) (the last: %v); a later save succeeded", d.failures, d.lastErr)
}

// sweepCodex ends what a Codex run's commands left, once the agent has ended (or a dead Agentium's run is recovered):
//   - stopped: every process Agentium saw descend from the agent (d), still alive with exactly the identity recorded
//     and started after it (stopDescendants). Codex's unified exec starts each command in a session of its own, outside
//     the process group the runner kills; such a session is still the agent's child, and the snapshots see it.
//   - reported, never stopped: processes that may be the run's but were not seen descending from it (codexReports: in
//     its sandbox, or using its folders), in the run's notes and its records (LeftoverProcesses).
//
// guard, when set (tests), sees every round's kill targets first and may refuse them: a test then never stops a
// process it did not start.
func sweepCodex(s codexSweep, d *descendants, guard func(pids []int) bool, records string) []string {
	var notes []string
	if d != nil {
		killed, err := stopDescendants(d, s.since, guard)
		if len(killed) > 0 {
			notes = append(notes, fmt.Sprintf("stopped %d process(es) Codex's commands left running: %s", len(killed), strings.Join(killed, ", ")))
		}
		if err != nil {
			notes = append(notes, "Codex's leftover processes could not all be stopped: "+err.Error())
		}
		if note := d.note(); note != "" {
			notes = append(notes, note)
		}
	}
	reported, err := reportCodex(s)
	if err != nil {
		notes = append(notes, "Codex's leftover processes could not all be looked for: "+err.Error())
	}
	if len(reported) > 0 {
		var shown []string
		for _, p := range reported {
			shown = append(shown, fmt.Sprintf("%d %q started %s (%s)", p.PID, p.Command, p.Started().UTC().Format(time.RFC3339), p.Why))
		}
		notes = append(notes, fmt.Sprintf("%d process(es) that may be what Codex's commands left are still running, not stopped (Agentium did not see them descend from the agent): %s; stop any that is yours to stop yourself (`agentium clean` lists them)",
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
		since: rec.Started}, loadDescendants(filepath.Join(records, AgentProcesses)), nil, records)
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
// A run whose stream shows no session (no thread.started) never reached the API and spent nothing, unless its spend is
// marked unverified (RolloutsIncomplete: a recovered run whose records could not be read, its stream missing among
// them, counts the bound too); rollouts read whole, even without requests, are the spend: nothing changes. An estimate
// only ever raises the cost.
func codexSpendFallback(rec *Record) {
	m := &rec.Metrics
	if agent.Name(rec.Agent) != codex.Name || !m.SawInit && !m.RolloutsIncomplete || m.Rollouts > 0 && !m.RolloutsIncomplete {
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
	switch {
	case m.RolloutsIncomplete && m.Rollouts > 0:
		what = fmt.Sprintf("Codex's session rollouts were collected or read only in part ($%.3f read)", m.CostUSD)
	case m.RolloutsIncomplete && !m.SawInit:
		what = "the run's records could not be read (its stream shows no session), and its spend was never settled"
	case m.RolloutsIncomplete:
		what = "Codex's session rollout is missing, and what the run spent could not be verified"
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

// AccountingPending, in a Codex run's records, says that its spend is not settled yet: written before Codex starts,
// removed once the run's final record is saved. A recovery that finds it accepts the spend the rollouts show only when
// it is verified (codex Parse: the stream's usage matched, or the turn closed, and no lost accounting); otherwise the
// run counts its bound.
const AccountingPending = "accounting-pending"

// markAccountingLost makes sure the records say the run's accounting was lost (codex.AccountingLost): the watcher
// writes it the moment it stops a run as blind; this writes it again if that failed.
func markAccountingLost(records string) error {
	path := filepath.Join(records, codex.AccountingLost)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return nil
	}
	return writeFileAtomic(path, []byte("Agentium lost track of what the run spent\n"), 0o600)
}
