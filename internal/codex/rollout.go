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
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/pricing"
)

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
	files := rolloutFiles(filepath.Join(configDir, "sessions"))
	mainFile := filepath.Join(records, Rollout)
	for _, f := range files {
		if isThreads(f, thread) {
			if err := moveScrubbed(f, mainFile); err != nil {
				return err
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
			if err := os.MkdirAll(filepath.Join(records, Subagents), 0o700); err != nil {
				return fmt.Errorf("Codex rollouts: %w", err)
			}
			if err := moveScrubbed(f, filepath.Join(records, Subagents, filepath.Base(f))); err != nil {
				return err
			}
			if threadPattern.MatchString(meta.ID) {
				threads = append(threads, meta.ID)
			}
		}
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
// What it cannot promise: the next request can start before the watcher reads the last one (it looks every poll);
// subagents can each have a request in flight; and a request too large to price stops the run as capped. A thread
// whose rollout it cannot find within blindAfter stops the run as agent.StopBlind: its spend would be unseen.
type watcher struct {
	transcript, codexHome string
	rates                 pricing.OpenAIRates
	capUSD, allowanceUSD  float64
	poll, blindAfter      time.Duration // zero: 50ms and a minute
}

// tail is one rollout the watcher reads: how far, and whether it is the run's.
type tail struct {
	offset int64
	run    bool // the main thread's, or a subagent's of the run
	known  bool // decided whether it is the run's (its first line was whole)
}

func (w watcher) watch(ctx context.Context) agent.Stop {
	poll, blindAfter := w.poll, w.blindAfter
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	if blindAfter <= 0 {
		blindAfter = time.Minute
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
			usd, priced := t.read(f, w.rates)
			spent += usd
			if !priced {
				return agent.StopCap // a request Agentium cannot price: the cap cannot be kept
			}
		}
		if spent+w.allowanceUSD > w.capUSD {
			return agent.StopCap
		}
		if !mainFound && time.Since(threadSeen) > blindAfter {
			return agent.StopBlind
		}
	}
}

// read prices the requests written to the rollout since the last read (whole lines only); priced is false when one
// could not be priced.
func (t *tail) read(file string, rates pricing.OpenAIRates) (usd float64, priced bool) {
	f, err := os.Open(file)
	if err != nil {
		return 0, true // moved or gone: what it held was counted
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return 0, true
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLine))
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, true
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return 0, true
	}
	priced = true
	for _, line := range bytes.Split(data[:end], []byte("\n")) {
		var l rolloutLine
		if json.Unmarshal(line, &l) != nil || l.Type != "token_usage_record" {
			continue
		}
		var r tokenUsageRecord
		if json.Unmarshal(l.Payload, &r) != nil {
			continue
		}
		cost, ok := rates.Cost(r.usage())
		usd += cost
		priced = priced && ok
	}
	t.offset += int64(end) + 1
	return usd, priced
}
