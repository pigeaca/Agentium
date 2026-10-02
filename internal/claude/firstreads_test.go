package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The isolated-run cost reprices the cache reads of the main session's first request and of each subagent type's first
// launch. testdata/first-reads.jsonl is a warm run: its main session read another run's 16,754-token prefix, one
// subagent type ran twice (the second launch read the prefix its first launch wrote, and is not listed), and another
// type's stream reported no time-to-live split.
func TestParseFirstReadsGolden(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "first-reads.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []FirstRead{
		{Main: true, Model: "claude-sonnet-5", CacheRead: 16754, WriteTTL: TTL1h},
		{Model: "claude-haiku-4-5-20251001", CacheRead: 0, WriteTTL: TTL5m}, // the first launch of the reviewer type
		{Model: "claude-sonnet-5-5", CacheRead: 1200},                       // no split reported: priced as one hour
	}
	if !reflect.DeepEqual(m.FirstReads, want) || m.UnmatchedLaunches != 0 {
		t.Errorf("first reads = %+v (unmatched %d), want %+v", m.FirstReads, m.UnmatchedLaunches, want)
	}
	// Stored: counts, models and times to live, never the subagent types (names can be personal).
	stored, err := json.Marshal(m.FirstReads)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "reviewer") || strings.Contains(string(stored), "explorer") {
		t.Errorf("first reads name a subagent type: %s", stored)
	}
}

func TestParseFirstReads(t *testing.T) {
	main := func(id, usage string) string {
		return `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"` + id + `","model":"claude-sonnet-5","usage":` + usage + `,"content":[]}}`
	}
	sub := func(parent, id, usage string) string {
		return `{"type":"assistant","parent_tool_use_id":"` + parent + `","message":{"id":"` + id + `","model":"claude-sonnet-5","usage":` + usage + `,"content":[]}}`
	}
	agent := func(id, kind string) string {
		return `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"call-` + id + `","model":"claude-sonnet-5","usage":{"input_tokens":1,"cache_read_input_tokens":99},` +
			`"content":[{"type":"tool_use","id":"` + id + `","name":"Task","input":{"subagent_type":"` + kind + `"}}]}}`
	}
	const (
		cold     = `{"input_tokens":6,"cache_creation_input_tokens":30000,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":30000}}`
		warm     = `{"input_tokens":6,"cache_creation_input_tokens":13000,"cache_read_input_tokens":16754,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":13000}}`
		allRead  = `{"input_tokens":6,"cache_creation_input_tokens":0,"cache_read_input_tokens":30000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}`
		write5m  = `{"input_tokens":2,"cache_creation_input_tokens":700,"cache_read_input_tokens":30000,"cache_creation":{"ephemeral_5m_input_tokens":700,"ephemeral_1h_input_tokens":0}}`
		subFirst = `{"input_tokens":3,"cache_creation_input_tokens":6082,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":6082,"ephemeral_1h_input_tokens":0}}`
		subWarm  = `{"input_tokens":3,"cache_creation_input_tokens":100,"cache_read_input_tokens":6082,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0}}`
	)
	parse := func(lines ...string) Metrics {
		t.Helper()
		m, err := Parse(strings.NewReader(strings.Join(lines, "\n")))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, c := range []struct {
		name      string
		lines     []string
		want      []FirstRead
		unmatched int
	}{
		{"a cold start reads nothing", []string{main("m1", cold), main("m2", allRead)},
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", CacheRead: 0, WriteTTL: TTL1h}}, 0},
		{"a warm start reads another run's prefix", []string{main("m1", warm), main("m1", warm)},
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", CacheRead: 16754, WriteTTL: TTL1h}}, 0},
		// The first request wrote nothing: the launch's time to live is that of its first request that wrote.
		{"a five-minute time to live from a later write", []string{main("m1", allRead), main("m2", write5m)},
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", CacheRead: 30000, WriteTTL: TTL5m}}, 0},
		{"the second launch of a type reads the run's own prefix", []string{main("m1", cold), agent("x1", "kind"), sub("x1", "s1", subFirst),
			agent("x2", "kind"), sub("x2", "s2", subWarm), agent("y1", "other"), sub("y1", "s3", subWarm)},
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", WriteTTL: TTL1h}, {Model: "claude-sonnet-5", WriteTTL: TTL5m},
				{Model: "claude-sonnet-5", CacheRead: 6082, WriteTTL: TTL5m}}, 0},
		// A launch whose Agent call is not in the transcript: its type, and so whether it is a first launch, is unknown.
		{"a launch of unknown type", []string{main("m1", cold), sub("lost", "s1", subWarm)},
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", WriteTTL: TTL1h}}, 1},
		// Subagents alone, or a stream without usage: there is no main-session first request to start from.
		{"no main request", []string{sub("x1", "s1", subFirst)}, nil, 0},
		{"no usage", []string{`{"type":"result","subtype":"success","total_cost_usd":0.1}`}, nil, 0},
	} {
		m := parse(c.lines...)
		if !reflect.DeepEqual(m.FirstReads, c.want) || m.UnmatchedLaunches != c.unmatched {
			t.Errorf("%s: first reads = %+v (unmatched %d), want %+v (%d)", c.name, m.FirstReads, m.UnmatchedLaunches, c.want, c.unmatched)
		}
	}
}
