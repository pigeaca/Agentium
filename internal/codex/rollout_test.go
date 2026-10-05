package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/pricing"
)

const (
	mainThread  = "00000000-0000-7000-8000-000000000001" // the fixtures' thread
	subThread   = "00000000-0000-7000-8000-0000000000aa"
	otherThread = "00000000-0000-7000-8000-0000000000bb"
)

// withAccount is a fixture rollout as Codex writes it, with the account fields the fixtures had dropped put back, and
// session_meta's id, folder and time set.
func withAccount(t *testing.T, fixture, id, cwd, timestamp string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "rollout-"+fixture+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var v map[string]any
		if err := json.Unmarshal(line, &v); err != nil {
			t.Fatal(err)
		}
		payload, _ := v["payload"].(map[string]any)
		switch {
		case v["type"] == "session_meta":
			payload["creator_user_id"], payload["creator_account_id"] = "user-private", "account-private"
			payload["id"], payload["cwd"], payload["timestamp"] = id, cwd, timestamp
		case payload["type"] == "token_count":
			if limits, ok := payload["rate_limits"].(map[string]any); ok {
				limits["plan_type"], limits["credits"] = "plan-private", map[string]any{"balance": "credits-private"}
			}
		}
		encoded, _ := json.Marshal(v)
		out.Write(append(encoded, '\n'))
	}
	return out.Bytes()
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Gather moves the run's rollouts out of the Codex home into its records without the account's fields: the main
// thread's and its subagents' (same folder, no earlier), not another run's or an earlier session's; it deletes those
// threads' shell snapshots; it never touches the sign-in. Again, it finds nothing more to do.
func TestGather(t *testing.T) {
	home, dir := t.TempDir(), records(t, "ok", "")
	day := filepath.Join(home, "sessions", "2026", "10", "04")
	mainFile := filepath.Join(day, "rollout-2026-10-04T14-58-55-"+mainThread+".jsonl")
	subFile := filepath.Join(day, "rollout-2026-10-04T14-59-00-"+subThread+".jsonl")
	otherFile := filepath.Join(day, "rollout-2026-10-04T14-59-01-"+otherThread+".jsonl")
	earlierFile := filepath.Join(home, "sessions", "2026", "10", "03", "rollout-2026-10-03T10-00-00-00000000-0000-7000-8000-0000000000cc.jsonl")
	writeFile(t, mainFile, withAccount(t, "ok", mainThread, "<run>/checkout", "2026-10-04T14:58:55.792Z"))
	writeFile(t, subFile, withAccount(t, "interrupted", subThread, "<run>/checkout", "2026-10-04T14:59:00.000Z"))
	writeFile(t, otherFile, withAccount(t, "interrupted", otherThread, "<other-run>/checkout", "2026-10-04T14:59:01.000Z"))
	writeFile(t, earlierFile, withAccount(t, "interrupted", "00000000-0000-7000-8000-0000000000cc", "<run>/checkout", "2026-10-03T10:00:00.000Z"))
	auth := filepath.Join(home, "auth.json")
	writeFile(t, auth, []byte(`{"sentinel":"never opened"}`))
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(auth, past, past); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{mainThread, subThread, otherThread} {
		writeFile(t, filepath.Join(home, "shell_snapshots", id+".1791125779.sh"), []byte("export SECRET_LOOKING=1\n"))
	}

	for range 2 {
		if err := (Adapter{}).Gather(home, dir); err != nil {
			t.Fatal(err)
		}
	}
	for _, gone := range []string{mainFile, subFile, filepath.Join(home, "shell_snapshots", mainThread+".1791125779.sh"),
		filepath.Join(home, "shell_snapshots", subThread+".1791125779.sh")} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s is still in the Codex home", gone)
		}
	}
	for _, kept := range []string{otherFile, earlierFile, filepath.Join(home, "shell_snapshots", otherThread+".1791125779.sh")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s (not this run's) was touched: %v", kept, err)
		}
	}
	if data, err := os.ReadFile(auth); err != nil || string(data) != `{"sentinel":"never opened"}` {
		t.Errorf("the sign-in changed: %q, %v", data, err)
	}
	if info, err := os.Stat(auth); err != nil || !info.ModTime().Equal(past) {
		t.Errorf("the sign-in was written: %v", err)
	}
	for _, f := range []string{filepath.Join(dir, Rollout), filepath.Join(dir, Subagents, filepath.Base(subFile))} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"private", "creator_", "plan_type", "credits"} {
			if strings.Contains(string(data), private) {
				t.Errorf("%s keeps %q", filepath.Base(f), private)
			}
		}
		if info, _ := os.Stat(f); info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v", f, info.Mode().Perm())
		}
	}
	m := parse(t, dir)
	if want := cost(44416, 21248, 316) + cost(54875, 43008, 396); m.Rollouts != 2 || fmt.Sprintf("%.9f", m.CostUSD) != fmt.Sprintf("%.9f", want) ||
		m.UsageLast == nil || m.UsageLast.FiveHourResets.Unix() != 1791143360 {
		t.Errorf("after Gather: rollouts %d, cost $%.7f (want $%.7f), usage %+v", m.Rollouts, m.CostUSD, want, m.UsageLast)
	}
	// A stream without a thread gathers nothing.
	empty := t.TempDir()
	writeFile(t, filepath.Join(empty, agent.Transcript), []byte(`{"type":"turn.started"}`+"\n"))
	if err := (Adapter{}).Gather(home, empty); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(empty, Rollout)); err == nil {
		t.Error("a rollout was gathered without a thread")
	}
}

// A thread ID from the stream is used in file names only when it looks like one.
func TestThreadIDIsChecked(t *testing.T) {
	dir := t.TempDir()
	for stream, want := range map[string]string{
		`{"type":"thread.started","thread_id":"` + mainThread + `"}`: mainThread,
		`{"type":"thread.started","thread_id":"../../auth"}`:         "",
		`{"type":"thread.started","thread_id":"*"}`:                  "",
	} {
		writeFile(t, filepath.Join(dir, agent.Transcript), []byte(stream+"\n"))
		if got := ThreadID(filepath.Join(dir, agent.Transcript)); got != want {
			t.Errorf("%s: %q, want %q", stream, got, want)
		}
	}
}

// rolloutLines is a rollout's session_meta and turn_context, as Codex writes them first.
func rolloutLines(id, cwd string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"cli_version":"0.160.0","timestamp":"2026-10-04T15:00:00.000Z"}}`+"\n"+
		`{"type":"turn_context","payload":{"model":"gpt-6.1-sol","effort":"low","approval_policy":"never"}}`+"\n", id, cwd)
}

// request is one token_usage_record of input uncached tokens and no output.
func request(input int64) string {
	return fmt.Sprintf(`{"type":"token_usage_record","payload":{"usage":{"input_tokens":%d,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0}}}`+"\n", input)
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// watchFixture is a run's stream (thread started) and Codex home; the watcher prices 100,000 input tokens at $0.20,
// with a $1 cap and a $0.50 allowance: it lets two such requests by, and stops at the third.
func watchFixture(t *testing.T) (w watcher, rollout string) {
	t.Helper()
	dir, home := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(dir, agent.Transcript), []byte(`{"type":"thread.started","thread_id":"`+mainThread+`"}`+"\n"))
	rates, _ := pricing.OpenAILookup("gpt-6.1-sol")
	w = watcher{transcript: filepath.Join(dir, agent.Transcript), codexHome: home, rates: rates, capUSD: 1, allowanceUSD: 0.5,
		poll: 5 * time.Millisecond, blindAfter: 10 * time.Second}
	return w, filepath.Join(home, "sessions", "2026", "10", "04", "rollout-2026-10-04T15-00-00-"+mainThread+".jsonl")
}

func startWatch(w watcher) (cancel func(), stopped <-chan agent.Stop) {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan agent.Stop, 1)
	go func() { out <- w.watch(ctx) }()
	return cancel, out
}

// The cap: spend plus the allowance may not pass it. Requests as the rollout records them count, subagents' too
// (same folder), another session's not; a line still being written waits.
func TestWatcherStopsAtTheCap(t *testing.T) {
	w, rollout := watchFixture(t)
	writeFile(t, rollout, []byte(rolloutLines(mainThread, "/run/checkout")+request(100_000)))
	other := filepath.Join(filepath.Dir(rollout), "rollout-2026-10-04T15-00-01-"+otherThread+".jsonl")
	writeFile(t, other, []byte(rolloutLines(otherThread, "/other/checkout")+request(10_000_000)+request(10_000_000)))
	cancel, stopped := startWatch(w)
	defer cancel()
	sub := filepath.Join(filepath.Dir(rollout), "rollout-2026-10-04T15-00-02-"+subThread+".jsonl")
	writeFile(t, sub, []byte(rolloutLines(subThread, "/run/checkout")))
	appendTo(t, sub, strings.TrimSuffix(request(100_000), "\n")) // $0.40 with it, but not whole yet
	select {
	case s := <-stopped:
		t.Fatalf("stopped (%q) at $0.20 + $0.50 of $1, or by another run's requests", s)
	case <-time.After(300 * time.Millisecond):
	}
	appendTo(t, sub, "\n") // $0.40 + $0.50: still under
	select {
	case s := <-stopped:
		t.Fatalf("stopped (%q) at $0.40 + $0.50 of $1", s)
	case <-time.After(300 * time.Millisecond):
	}
	appendTo(t, rollout, request(100_000)) // $0.60 + $0.50 > $1
	select {
	case s := <-stopped:
		if s != agent.StopCap {
			t.Errorf("stopped %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not stop the run past its cap")
	}
}

// A request too large to price (above the long-context limit) stops the run: the cap could not be kept.
func TestWatcherStopsOnAnUnpricedRequest(t *testing.T) {
	w, rollout := watchFixture(t)
	w.capUSD = 100
	writeFile(t, rollout, []byte(rolloutLines(mainThread, "/run/checkout")+request(pricing.OpenAILongContext+1)))
	cancel, stopped := startWatch(w)
	defer cancel()
	select {
	case s := <-stopped:
		if s != agent.StopCap {
			t.Errorf("stopped %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unpriced request did not stop the run")
	}
}

// A thread whose rollout never appears stops the run as blind; a watcher whose run ends returns no reason.
func TestWatcherBlindAndEnd(t *testing.T) {
	w, _ := watchFixture(t)
	w.blindAfter = 50 * time.Millisecond
	cancel, stopped := startWatch(w)
	defer cancel()
	select {
	case s := <-stopped:
		if s != agent.StopBlind {
			t.Errorf("stopped %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a run without its rollout was not stopped")
	}
	w, rollout := watchFixture(t)
	writeFile(t, rollout, []byte(rolloutLines(mainThread, "/run/checkout")))
	cancel, stopped = startWatch(w)
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case s := <-stopped:
		if s != agent.StopNone {
			t.Errorf("an ended run: %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher outlived its run")
	}
}

// Lost accounting stops the run as blind, deterministically, and marks the records at once (AccountingLost): a rollout
// that goes after it was found. Nothing about timing or a turn's end ever stops a run: a message landing in the stream
// a poll after its request's rollout lines and a long silent tool call; a turn whose stream usage the rollouts fall short
// of (checked once Codex has ended, by Parse, where it makes the spend the bound).
func TestWatcherFailsClosedOnLostAccounting(t *testing.T) {
	run := func(w watcher, rollout string, feed func(w watcher, rollout string)) (agent.Stop, bool) {
		t.Helper()
		cancel, out := startWatch(w)
		defer cancel()
		fed := make(chan struct{})
		go func() { defer close(fed); feed(w, rollout) }()
		defer func() { <-fed }()
		select {
		case s := <-out:
			return s, true
		case <-time.After(800 * time.Millisecond):
			return agent.StopNone, false
		}
	}
	appendLine := func(file, line string) { // from a goroutine: no t
		if f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
			f.WriteString(line + "\n")
			f.Close()
		}
	}
	message := `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"working"}}`
	turnEnd := `{"type":"turn.completed","usage":{"input_tokens":50000,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0}}`
	lost := func(w watcher) bool {
		_, err := os.Stat(filepath.Join(filepath.Dir(w.transcript), AccountingLost))
		return err == nil
	}
	setup := func() (watcher, string) {
		w, rollout := watchFixture(t)
		w.capUSD = 100
		writeFile(t, rollout, []byte(rolloutLines(mainThread, "/run/checkout")+request(1000)))
		return w, rollout
	}

	w, rollout := setup()
	if s, ok := run(w, rollout, func(_ watcher, rollout string) { time.Sleep(200 * time.Millisecond); os.Remove(rollout) }); !ok || s != agent.StopBlind || !lost(w) {
		t.Errorf("the rollout gone mid-run: %q (stopped %v, marked %v)", s, ok, lost(w))
	}

	w, rollout = setup()
	if s, ok := run(w, rollout, func(w watcher, rollout string) {
		time.Sleep(100 * time.Millisecond)
		appendLine(rollout, strings.TrimSuffix(request(2000), "\n")) // the next request's usage, this poll
		time.Sleep(60 * time.Millisecond)
		appendLine(w.transcript, message) // its message, the next: then a long silent tool call
	}); ok {
		t.Errorf("a healthy run whose writers interleave was stopped: %q", s)
	}

	w, rollout = setup()
	if s, ok := run(w, rollout, func(w watcher, _ string) {
		time.Sleep(100 * time.Millisecond)
		appendLine(w.transcript, turnEnd) // more than the rollouts recorded: Parse's to judge, after the run
	}); ok || lost(w) {
		t.Errorf("a turn's end stopped the run: %q (marked %v)", s, lost(w))
	}
}
