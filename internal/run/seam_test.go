package run

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
)

// recordingAgent is Claude Code under another name, noting each call the run makes through the seam.
type recordingAgent struct {
	claude.Adapter
	calls *[]string
}

func (r recordingAgent) Name() string { return "recording-agent" }

func (r recordingAgent) Command(inv agent.Invocation, environ []string) ([]string, []string, error) {
	*r.calls = append(*r.calls, "Command")
	return r.Adapter.Command(inv, environ)
}

func (r recordingAgent) DeniedPaths(inv agent.Invocation, environ []string) []string {
	*r.calls = append(*r.calls, "DeniedPaths")
	return r.Adapter.DeniedPaths(inv, environ)
}

func (r recordingAgent) Parse(rd io.Reader) (agent.Metrics, error) {
	*r.calls = append(*r.calls, "Parse")
	return r.Adapter.Parse(rd)
}

func (r recordingAgent) Classify(m agent.Metrics, timedOut bool, drift []string) string {
	*r.calls = append(*r.calls, "Classify")
	return r.Adapter.Classify(m, timedOut, drift)
}

func (r recordingAgent) Check(m agent.Metrics, expect agent.Expect) []string {
	*r.calls = append(*r.calls, "Check")
	return r.Adapter.Check(m, expect)
}

// A run starts its agent, and reads what it reported, through the seam (Env.Agent), and its record names the agent.
func TestOnceRunsTheAgentThroughTheSeam(t *testing.T) {
	f := newModuleOnce(t, "", "decoy", "printf 'new\\n' > value.txt")
	var calls []string
	f.env.Agent = recordingAgent{calls: &calls}
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != agent.OutcomeOK || rec.Passed == nil || !*rec.Passed {
		t.Fatalf("outcome %s, passed %v, notes %v", rec.Outcome, rec.Passed, rec.Notes)
	}
	if want := []string{"DeniedPaths", "Command", "Parse", "Check", "Classify"}; !slices.Equal(calls, want) {
		t.Errorf("calls through the seam %q, want %q", calls, want)
	}
	if rec.Agent != "recording-agent" || rec.AgentName() != "recording-agent" {
		t.Errorf("the record names %q (%q)", rec.Agent, rec.AgentName())
	}
}

// A Claude Code run's record says so ("agent": "claude-code"); a record made before agents were named has no "agent"
// key, reads as Claude Code's, and is written back without one: old records stay as they were.
func TestRecordsNameTheirAgent(t *testing.T) {
	f := newModuleOnce(t, "", "decoy", "")
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Agent != agent.ClaudeCode || !strings.Contains(string(encoded), `"agent":"claude-code"`) {
		t.Errorf("a Claude Code record: agent %q in %s", rec.Agent, encoded)
	}

	old := `{"id":"r1","task":"t","arm":"base","model":"claude-sonnet-5","sign_in":"login","outcome":"ok"}`
	var read Record
	if err := json.Unmarshal([]byte(old), &read); err != nil {
		t.Fatal(err)
	}
	if read.AgentName() != agent.ClaudeCode {
		t.Errorf("an old record's agent: %q", read.AgentName())
	}
	again, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(again), `"agent"`) {
		t.Errorf("an old record gained an agent key: %s", again)
	}
}

// Recovery and context use read a stored transcript with its record's agent: Claude Code's for a record that names
// none; one this Agentium does not know is not read as Claude Code's.
func TestTranscriptsAreReadWithTheirAgent(t *testing.T) {
	fixture := filepath.Join("..", "claude", "testdata", "ok.jsonl")
	for _, name := range []string{"", agent.ClaudeCode} {
		if m, err := parseFile(adapterFor(name), fixture); err != nil || !m.SawResult {
			t.Errorf("agent %q: %+v, %v", name, m, err)
		}
	}
	if a := adapterFor("some-later-agent"); a != nil {
		t.Errorf("an unknown agent has an adapter: %T", a)
	}
	if _, err := parseFile(adapterFor("some-later-agent"), fixture); err == nil {
		t.Error("a transcript of an unknown agent was read")
	}
}

// The run's timeout reaches the agent through the seam's invocation (agent.Invocation.Timeout): an agent that runs past
// it is stopped, and the run is the agent's timeout.
func TestOnceStopsTheAgentAtItsTimeout(t *testing.T) {
	f := newModuleOnce(t, "", "decoy", "sleep 60")
	f.spec.Timeout = time.Second
	start := time.Now()
	rec, err := Once(context.Background(), f.env, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != agent.OutcomeTimeout || time.Since(start) > 30*time.Second {
		t.Errorf("outcome %s after %v, notes %v: want the agent stopped at its timeout", rec.Outcome, time.Since(start), rec.Notes)
	}
}
