package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The isolated-run cost reprices the cache reads of the main session's first request and of each subagent launch that
// could not read its type's prefix from the run. testdata/first-reads.jsonl is a warm run: its main session read
// another run's 16,754-token prefix, one subagent type ran twice (the second launch, 12 seconds after the first one's
// last request, read the prefix the first wrote, and is not listed), and another type's stream reported no
// time-to-live split.
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
		{Model: "claude-haiku-4-5-20251001", CacheRead: 0, WriteTTL: TTL5m},              // the first launch of the reviewer type
		{Model: "claude-sonnet-5-5", CacheRead: 1200, WriteTTL: TTL5m, TTLAssumed: true}, // no split reported: a subagent's five minutes
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

// timed builds transcripts whose assistant events carry timestamps, seconds after 10:00 (a negative time: none).
type timed struct{ lines []string }

func (s *timed) event(parent, id, model, usage string, sec int, content string) {
	p := "null"
	if parent != "-" {
		p = `"` + parent + `"`
	}
	stamp := ""
	if sec >= 0 {
		stamp = fmt.Sprintf(`"timestamp":"2026-10-02T10:%02d:%02d.000Z",`, sec/60, sec%60)
	}
	s.lines = append(s.lines, `{"type":"assistant","parent_tool_use_id":`+p+`,`+stamp+`"message":{"id":"`+id+`","model":"`+model+`","usage":`+usage+`,"content":[`+content+`]}}`)
}

// main is a main-session request; calls are the Agent calls it makes, as id:type.
func (s *timed) main(id, usage string, sec int, calls ...string) {
	var blocks []string
	for _, c := range calls {
		call, kind, _ := strings.Cut(c, ":")
		blocks = append(blocks, `{"type":"tool_use","id":"`+call+`","name":"Agent","input":{"subagent_type":"`+kind+`"}}`)
	}
	s.event("-", id, "claude-sonnet-5", usage, sec, strings.Join(blocks, ","))
}

func (s *timed) sub(parent, id, usage string, sec int) {
	s.event(parent, id, "claude-sonnet-5", usage, sec, "")
}

func usage(input, read, write5m, write1h int) string {
	return fmt.Sprintf(`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}`,
		input, read, write5m+write1h, write5m, write1h)
}

func TestParseFirstReads(t *testing.T) {
	var (
		cold    = usage(6, 0, 0, 30000)
		warm    = usage(6, 16754, 0, 13000)
		allRead = usage(6, 30000, 0, 0)
		write5m = usage(2, 30000, 700, 0)
		subCold = usage(3, 0, 6082, 0)
		subWarm = usage(3, 6082, 100, 0)
		subRead = usage(3, 6082, 0, 0)
	)
	mainCold := FirstRead{Main: true, Model: "claude-sonnet-5", WriteTTL: TTL1h}
	repriced := func(read int64, ttl string, assumed bool) FirstRead {
		return FirstRead{Model: "claude-sonnet-5", CacheRead: read, WriteTTL: ttl, TTLAssumed: assumed}
	}
	cases := []struct {
		name      string
		build     func(s *timed)
		want      []FirstRead
		unmatched int
	}{
		{"a cold start reads nothing", func(s *timed) { s.main("m1", cold, 1); s.main("m2", allRead, 9) }, []FirstRead{mainCold}, 0},
		{"a warm start reads another run's prefix", func(s *timed) { s.main("m1", warm, 1); s.main("m1", warm, 2) },
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", CacheRead: 16754, WriteTTL: TTL1h}}, 0},
		// The first request wrote nothing: the launch's time to live is that of its first request that wrote.
		{"a five-minute time to live from a later write", func(s *timed) { s.main("m1", allRead, 1); s.main("m2", write5m, 9) },
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", CacheRead: 30000, WriteTTL: TTL5m}}, 0},
		// The first writing request decides, not any or the last one.
		{"the first write's time to live, five minutes", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind")
			s.sub("x1", "s1", subRead, 3)
			s.sub("x1", "s2", usage(3, 6082, 50, 0), 5)
			s.sub("x1", "s3", usage(3, 6082, 0, 50), 7)
		}, []FirstRead{mainCold, repriced(6082, TTL5m, false)}, 0},
		{"the first write's time to live, one hour", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind")
			s.sub("x1", "s1", usage(3, 0, 0, 6082), 3)
			s.sub("x1", "s2", usage(3, 6082, 50, 0), 5)
		}, []FirstRead{mainCold, repriced(0, TTL1h, false)}, 0},
		// The second launch follows the first one's last request within five minutes: it reads the run's own prefix.
		{"a quick relaunch is not repriced", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind")
			s.sub("x1", "s1", subCold, 3)
			s.sub("x1", "s2", subWarm, 60)
			s.main("m2", allRead, 100, "x2:kind")
			s.sub("x2", "s3", subWarm, 60+4*60)
			s.main("m3", allRead, 400, "y1:other")
			s.sub("y1", "s4", subWarm, 402)
		}, []FirstRead{mainCold, repriced(0, TTL5m, false), repriced(6082, TTL5m, false)}, 0},
		// Two launches of a type from one message run at once: neither can read the other's write, so both are
		// repriced; likewise when the message's calls arrive as separate events.
		{"parallel launches are both repriced", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind", "x2:kind")
			s.sub("x1", "s1", subWarm, 3)
			s.sub("x2", "s2", subWarm, 3)
			s.sub("x1", "s3", subWarm, 9)
			s.sub("x2", "s4", subWarm, 9)
		}, []FirstRead{mainCold, repriced(6082, TTL5m, false), repriced(6082, TTL5m, false)}, 0},
		{"parallel calls in separate events", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind")
			s.main("m1", cold, 1, "x2:kind")
			s.sub("x2", "s2", subWarm, 3)
			s.sub("x1", "s1", subWarm, 4)
		}, []FirstRead{mainCold, repriced(6082, TTL5m, false), repriced(6082, TTL5m, false)}, 0},
		// More than five minutes after the type's last request, its five-minute prefix is gone.
		{"a late relaunch is repriced", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind")
			s.sub("x1", "s1", subCold, 3)
			s.sub("x1", "s2", subWarm, 30)
			s.main("m2", allRead, 300, "x2:kind")
			s.sub("x2", "s3", subWarm, 30+5*60+1)
		}, []FirstRead{mainCold, repriced(0, TTL5m, false), repriced(6082, TTL5m, false)}, 0},
		{"a one-hour prefix outlives five minutes", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind")
			s.sub("x1", "s1", usage(3, 0, 0, 6082), 3)
			s.main("m2", allRead, 400, "x2:kind")
			s.sub("x2", "s2", usage(3, 6082, 0, 100), 401)
		}, []FirstRead{mainCold, repriced(0, TTL1h, false)}, 0},
		// Without a timestamp, whether the prefix was still there is unknown: the launch is repriced.
		{"a relaunch without a timestamp is repriced", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind")
			s.sub("x1", "s1", subCold, 3)
			s.main("m2", allRead, 10, "x2:kind")
			s.sub("x2", "s2", subWarm, -1)
		}, []FirstRead{mainCold, repriced(0, TTL5m, false), repriced(6082, TTL5m, false)}, 0},
		// A launch that wrote nothing takes its type's time to live from another launch, then a subagent's default.
		{"a time to live from the type's other launch", func(s *timed) {
			s.main("m1", cold, 1, "x1:kind", "x2:kind")
			s.sub("x1", "s1", subRead, 3)
			s.sub("x2", "s2", usage(3, 6082, 0, 40), 4)
			s.main("m2", cold, 5, "y1:other")
			s.sub("y1", "s3", subRead, 6)
		}, []FirstRead{mainCold, repriced(6082, TTL1h, true), repriced(6082, TTL1h, false), repriced(6082, TTL5m, true)}, 0},
		{"a main session that wrote nothing: one hour", func(s *timed) { s.main("m1", allRead, 1) },
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", CacheRead: 30000, WriteTTL: TTL1h, TTLAssumed: true}}, 0},
		// Messages that are not model requests are not the first request.
		{"usage null is not a request", func(s *timed) { s.main("m0", "null", 0); s.main("m1", warm, 1) },
			[]FirstRead{{Main: true, Model: "claude-sonnet-5", CacheRead: 16754, WriteTTL: TTL1h}}, 0},
		{"a synthetic message is not a request", func(s *timed) {
			s.event("-", "m0", "<synthetic>", `{"input_tokens":0,"output_tokens":0}`, 0, "")
			s.main("m1", warm, 1)
		}, []FirstRead{{Main: true, Model: "claude-sonnet-5", CacheRead: 16754, WriteTTL: TTL1h}}, 0},
		// A launch whose Agent call is not in the transcript, or an empty parent: its type, and so whether it could
		// read the run's own prefix, is unknown.
		{"a launch of unknown type", func(s *timed) { s.main("m1", cold, 1); s.sub("lost", "s1", subWarm, 3) }, []FirstRead{mainCold}, 1},
		{"an empty parent is not the main session", func(s *timed) { s.main("m1", cold, 1); s.sub("", "s1", subWarm, 3) }, []FirstRead{mainCold}, 1},
		// Subagents alone, or a stream without usage: there is no main-session first request to start from.
		{"no main request", func(s *timed) { s.sub("x1", "s1", subCold, 3) }, nil, 0},
		{"no usage", func(s *timed) {
			s.lines = append(s.lines, `{"type":"result","subtype":"success","total_cost_usd":0.1}`)
		}, nil, 0},
	}
	for _, c := range cases {
		var s timed
		c.build(&s)
		m, err := Parse(strings.NewReader(strings.Join(s.lines, "\n")))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(m.FirstReads, c.want) || m.UnmatchedLaunches != c.unmatched {
			t.Errorf("%s: first reads = %+v (unmatched %d), want %+v (%d)", c.name, m.FirstReads, m.UnmatchedLaunches, c.want, c.unmatched)
		}
	}
}
