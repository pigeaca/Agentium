package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/pricing"
)

// Incomplete, in a run's records, lists the rollouts of the run that could not be moved there (Gather), one
// records-relative path a line: what the records hold of its spend is a part, and the run's cost is estimated so that
// it never undercounts (run's codexSpendFallback). Only a gather that finds each listed rollout in the records removes
// it: a source that is gone (an API key run's workspace removed by recovery) is not a collection.
const Incomplete = "rollouts-incomplete"

// unreadableList stands, in the list of missing rollouts, for an earlier list that was empty or cut short: it is never
// collected, so the list stays.
const unreadableList = "(an earlier list that could not be read)"

// markIncomplete updates the records' list of missing rollouts (Incomplete): what this gather could not move, plus
// what an earlier one listed that is still not in the records (a line it cannot read as such stays, as missing). The
// list goes only once it is empty; its error says what is still missing.
func markIncomplete(records string, missing map[string]error) error {
	marker := filepath.Join(records, Incomplete)
	if data, err := os.ReadFile(marker); err == nil {
		// A list with nothing on it was cut short (a crash while an older Agentium wrote it in place): what it held is
		// unknown, so it stays, and the run counts its bound.
		if listed := strings.TrimSpace(string(data)); listed == "" || !strings.Contains(listed, "\n") && strings.HasPrefix(listed, "#") {
			missing[unreadableList] = errors.New("an earlier list of missing rollouts was empty or cut short")
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if _, already := missing[line]; already {
				continue
			}
			collected := false
			if line == Rollout || filepath.Dir(line) == Subagents && filepath.IsLocal(line) {
				info, err := os.Lstat(filepath.Join(records, line))
				collected = err == nil && info.Mode().IsRegular()
			}
			if !collected {
				missing[line] = errors.New("listed as missing by an earlier gather, and still not in the records")
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Codex rollouts: %w", err)
	}
	if len(missing) == 0 {
		if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("Codex rollouts: %w", err)
		}
		return nil
	}
	lines := []string{"# Codex rollouts of this run that are not in these records"}
	var errs []error
	for dest, err := range missing {
		lines = append(lines, dest)
		errs = append(errs, fmt.Errorf("%s: %w", dest, err))
	}
	sort.Strings(lines[1:])
	if err := writeAtomic(marker, []byte(strings.Join(lines, "\n")+"\n")); err != nil { // never empty, even after a crash
		errs = append(errs, fmt.Errorf("list the missing Codex rollouts: %w", err))
	}
	return fmt.Errorf("Codex rollouts not collected: %w", errors.Join(errs...))
}

// Codex keeps each session's rollout at CODEX_HOME/sessions/YYYY/MM/DD/rollout-<time>-<thread>.jsonl, always: no
// setting moves it (the spike, [source]). In login mode CODEX_HOME is the shared login home, which Agentium's runs
// take one at a time; with an API key it is the run's own. Either way, a run's sessions are its main thread's rollout
// and the rollouts of subagents it started in the same folder (session_meta.cwd), after it.

// threadPattern is what a thread ID may look like before it names a file: a UUID's characters only.
var threadPattern = regexp.MustCompile(`^[0-9A-Za-z-]{8,64}$`)

// ThreadID is the run's main thread, from its exec stream's thread.started event; empty when the stream has none (or
// names something that is not an ID).
func ThreadID(transcript string) string {
	f, err := os.Open(transcript)
	if err != nil {
		return ""
	}
	defer f.Close()
	thread := ""
	_ = eachLine(io.LimitReader(f, maxLine), func(line []byte) {
		var e streamEvent
		if thread == "" && json.Unmarshal(line, &e) == nil && e.Type == "thread.started" && threadPattern.MatchString(e.ThreadID) {
			thread = e.ThreadID
		}
	})
	return thread
}

// rolloutFiles lists the regular files named rollout-*.jsonl under sessions, up to four folders deep; links are not
// followed. A missing folder lists nothing.
func rolloutFiles(sessions string) []string {
	var files []string
	_ = filepath.WalkDir(sessions, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if rel, _ := filepath.Rel(sessions, p); rel != "." && strings.Count(rel, string(filepath.Separator)) >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// isThreads reports whether a rollout's file name ends in thread's ID.
func isThreads(file, thread string) bool {
	return strings.HasSuffix(filepath.Base(file), "-"+thread+".jsonl")
}

// readMeta reads a rollout's first line, its session_meta; ok is false until that line is whole.
func readMeta(file string) (sessionMeta, bool) {
	f, err := os.Open(file)
	if err != nil {
		return sessionMeta{}, false
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(io.LimitReader(f, maxLine), 64<<10).ReadBytes('\n')
	if err != nil {
		return sessionMeta{}, false
	}
	var l rolloutLine
	var meta sessionMeta
	if json.Unmarshal(line, &l) != nil || l.Type != "session_meta" || json.Unmarshal(l.Payload, &meta) != nil {
		return sessionMeta{}, false
	}
	return meta, true
}

// belongs reports whether a rollout other than the main one is a subagent's of the same run: started in the main
// session's folder, no earlier than it (timestamps are RFC 3339 in UTC, which sort as text).
func belongs(meta, main sessionMeta) bool {
	return main.CWD != "" && meta.CWD == main.CWD && meta.ID != main.ID && meta.Timestamp >= main.Timestamp
}

// Gather moves the run's rollouts from its Codex home (configDir) into its records: the main thread's to Rollout, its
// subagents' into Subagents, each without the account's fields (scrub); then it deletes the shell snapshots Codex
// leaves for those threads when it is interrupted (they hold the shell's environment). It is safe to repeat: what was
// moved is not found again. A stream without a thread (Codex never started a session) gathers nothing.
func (Adapter) Gather(configDir, records string) error {
	thread := ThreadID(filepath.Join(records, agent.Transcript))
	if thread == "" || configDir == "" {
		return nil
	}
	// Each rollout is moved on its own: one that fails is left where it is (moveScrubbed removes a source only once its
	// copy is written), the others still move, and the records list what is missing (Incomplete), so the run's spend is
	// never read as whole from part of it.
	missing := map[string]error{} // records-relative destinations not collected
	files := rolloutFiles(filepath.Join(configDir, "sessions"))
	mainFile := filepath.Join(records, Rollout)
	for _, f := range files {
		if isThreads(f, thread) {
			if err := moveScrubbed(f, mainFile); err != nil {
				missing[Rollout] = err
			}
		}
	}
	threads := []string{thread}
	if main, ok := readMeta(mainFile); ok {
		for _, f := range files {
			if isThreads(f, thread) {
				continue
			}
			meta, ok := readMeta(f)
			if !ok || !belongs(meta, main) {
				continue
			}
			dest := filepath.Join(Subagents, filepath.Base(f))
			if err := os.MkdirAll(filepath.Join(records, Subagents), 0o700); err != nil {
				missing[dest] = fmt.Errorf("Codex rollouts: %w", err)
				continue
			}
			if err := moveScrubbed(f, filepath.Join(records, dest)); err != nil {
				missing[dest] = err
				continue
			}
			if threadPattern.MatchString(meta.ID) {
				threads = append(threads, meta.ID)
			}
		}
	}
	if err := markIncomplete(records, missing); err != nil {
		return err
	}
	for _, t := range threads {
		snapshots, _ := filepath.Glob(filepath.Join(configDir, "shell_snapshots", t+".*.sh"))
		for _, s := range snapshots {
			if info, err := os.Lstat(s); err == nil && info.Mode().IsRegular() {
				if err := os.Remove(s); err != nil {
					return fmt.Errorf("remove Codex's shell snapshot: %w", err)
				}
			}
		}
	}
	return nil
}

// scrubbed are the keys a rollout may hold that name the account, its user or organization, or the plan: they never
// reach a run's records.
var scrubbed = map[string]bool{"creator_user_id": true, "creator_account_id": true, "account_id": true, "user_id": true, "org_id": true,
	"organization_id": true, "chatgpt_account_id": true, "chatgpt_user_id": true, "email": true, "plan_type": true, "credits": true,
	"individual_limit": true, "spend_control_reached": true}

// moveScrubbed writes src's lines to dst without the scrubbed keys (a line that is not JSON, such as one cut short,
// is dropped), owner-only and atomically, then removes src. An existing dst is replaced.
func moveScrubbed(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("Codex rollout: %w", err)
	}
	var out bytes.Buffer
	err = eachLine(in, func(line []byte) {
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber() // counts and times stay exact
		var v any
		if dec.Decode(&v) != nil {
			return
		}
		clean, err := json.Marshal(scrub(v))
		if err != nil {
			return
		}
		out.Write(clean)
		out.WriteByte('\n')
	})
	in.Close()
	if err != nil {
		return fmt.Errorf("Codex rollout %s: %w", filepath.Base(src), err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
		return fmt.Errorf("Codex rollout: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("Codex rollout: %w", err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("Codex rollout: %w", err)
	}
	return nil
}

// scrub drops the scrubbed keys from v, at any depth.
func scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if scrubbed[k] {
				delete(x, k)
				continue
			}
			x[k] = scrub(e)
		}
	case []any:
		for i, e := range x {
			x[i] = scrub(e)
		}
	}
	return v
}

// watcher is Agentium's cost cap for a Codex run (Codex has none): it reads the run's rollouts as Codex writes them,
// prices each request's token_usage_record (written as each model response ends) at the model's list prices, and
// stops the run once what it spent plus one full-context request (the allowance) would pass the cap: a request in
// flight when the run stops is never recorded, so the cap holds while at most one is. Subagents' rollouts count too.
//
// The bound: Codex runs with its subagents off (configOverrides), so one request is in flight at a time, and the spend
// stays under the cap, unless the next request starts before the watcher reads the last one (it looks every poll):
// then at most the cap plus one allowance (Bound), as long as Codex records each request's usage. A request too large
// to price stops the run as capped.
//
// Lost accounting stops the run as agent.StopBlind (and marks its records, AccountingLost, so it counts its bound
// whatever happens next), in deterministic cases only: a thread whose rollout it cannot find within blindAfter; and a
// rollout, once found, that goes, cannot be opened or read, or holds a line too long to ever end. Whether the rollout
// holds every request is checked once Codex has ended, against the stream (Parse): a gap found there protects no more
// spend, so it never stops a run; it makes the run's spend its bound.
type watcher struct {
	transcript, codexHome string
	rates                 pricing.OpenAIRates
	capUSD, allowanceUSD  float64
	// poll is how often it looks (zero: 50 ms); blindAfter how long a started thread may go without a rollout (zero: a
	// minute).
	poll, blindAfter time.Duration
}

// tail is one rollout the watcher reads: how far, and whether it is the run's.
type tail struct {
	offset int64
	run    bool // the main thread's, or a subagent's of the run
	known  bool // decided whether it is the run's (its first line was whole)
}

// readResult is what a rollout's new whole lines held: the requests' price (priced is false when one could not be
// priced) and tokens. lost is true when the rollout cannot be read on: gone, unreadable, or a line too long to ever end.
type readResult struct {
	usd    float64
	priced bool
	tokens pricing.OpenAIUsage
	lost   bool
}

// AccountingLost, in a run's records, says that Agentium lost track of what the run spent while it ran (the watcher's
// StopBlind): written at once, before anything else, so that recovery after a crash still counts the run's bound even
// if the rollouts it finds parse whole.
const AccountingLost = "accounting-lost"

func (w watcher) watch(ctx context.Context) agent.Stop {
	poll, blindAfter := w.poll, w.blindAfter
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	if blindAfter <= 0 {
		blindAfter = time.Minute
	}
	blind := func(why string) agent.Stop {
		_ = writeAtomic(filepath.Join(filepath.Dir(w.transcript), AccountingLost), []byte(why+"\n"))
		return agent.StopBlind
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	thread, threadSeen := "", time.Time{}
	var main sessionMeta
	mainFound := false
	tails := map[string]*tail{}
	spent := 0.0
	for {
		select {
		case <-ctx.Done():
			return agent.StopNone
		case <-ticker.C:
		}
		if thread == "" {
			if thread = ThreadID(w.transcript); thread == "" {
				continue
			}
			threadSeen = time.Now()
		}
		for _, f := range rolloutFiles(filepath.Join(w.codexHome, "sessions")) {
			t := tails[f]
			if t == nil {
				t = &tail{}
				tails[f] = t
			}
			if !t.known {
				meta, ok := readMeta(f)
				switch {
				case !ok:
					continue
				case isThreads(f, thread):
					main, mainFound, t.run, t.known = meta, true, true, true
				case mainFound:
					t.run, t.known = belongs(meta, main), true
				default:
					continue // decided once the main session's folder is known
				}
			}
			if !t.run {
				continue
			}
			r := t.read(f, w.rates)
			if r.lost {
				return blind("the run's rollout " + filepath.Base(f) + " could not be read on")
			}
			spent += r.usd
			if !r.priced {
				return agent.StopCap // a request Agentium cannot price: the cap cannot be kept
			}
		}
		if mainFound && mainRolloutOf(tails, thread) == "" {
			return blind("the run's rollout went away")
		}
		if spent+w.allowanceUSD > w.capUSD {
			return agent.StopCap
		}
		if !mainFound && time.Since(threadSeen) > blindAfter {
			return blind("no rollout of the run's thread appeared")
		}
	}
}

// behind reports whether the rollouts' recorded tokens fall short of the stream's usage by more than 1% (of input, or
// of output): the rollouts missed requests. Equal counts are the spike's finding (3 short sessions, without compaction
// or retries: the plan's step 7 checks a long one); 1% absorbs rounding only, and decides incompleteness, never the
// cost (Parse takes the larger).
func behind(recorded, turns pricing.OpenAIUsage) bool {
	return float64(recorded.Input) < 0.99*float64(turns.Input) || float64(recorded.Output) < 0.99*float64(turns.Output)
}

func addUsage(a, b pricing.OpenAIUsage) pricing.OpenAIUsage {
	return pricing.OpenAIUsage{Input: a.Input + b.Input, Cached: a.Cached + b.Cached, CacheWrite: a.CacheWrite + b.CacheWrite,
		Output: a.Output + b.Output, Reasoning: a.Reasoning + b.Reasoning}
}

// mainRolloutOf is the path of the main thread's rollout among the tails, while it exists ("" otherwise).
func mainRolloutOf(tails map[string]*tail, thread string) string {
	for f := range tails {
		if isThreads(f, thread) {
			if _, err := os.Stat(f); err == nil {
				return f
			}
		}
	}
	return ""
}

// read prices the requests written to the rollout since the last read (whole lines only).
func (t *tail) read(file string, rates pricing.OpenAIRates) readResult {
	f, err := os.Open(file)
	if err != nil {
		return readResult{lost: true}
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return readResult{lost: true}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLine))
	if err != nil && !errors.Is(err, io.EOF) {
		return readResult{lost: true}
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return readResult{priced: true, lost: len(data) >= maxLine}
	}
	r := readResult{priced: true}
	for _, line := range bytes.Split(data[:end], []byte("\n")) {
		var l rolloutLine
		if json.Unmarshal(line, &l) != nil || l.Type != "token_usage_record" {
			continue
		}
		var u tokenUsageRecord
		if json.Unmarshal(l.Payload, &u) != nil {
			continue
		}
		cost, ok := rates.Cost(u.usage())
		r.usd += cost
		r.priced = r.priced && ok
		r.tokens = addUsage(r.tokens, u.usage())
	}
	t.offset += int64(end) + 1
	return r
}

// writeAtomic replaces path with data, owner-only, through a temp file renamed into place: a crash leaves the old
// file or the new one, never a part.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Bound is the most a capped run can spend: its cap, plus one allowance for a request that starts in the watcher's
// poll gap, before it read the request that ended just before (one request at a time: subagents are off).
func Bound(model string, capUSD float64) float64 {
	allowance, _ := Allowance(model)
	return capUSD + allowance
}
