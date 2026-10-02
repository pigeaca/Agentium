package run

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
)

// The golden stream of internal/claude, through the parser and the repricing: the main session's warm first request at
// the one-hour write rate, the reviewer type's first launch (cold, five minutes) and the explorer's (no split: one hour);
// the reviewer's second launch is not repriced. Rates of 2026-09-29: claude-sonnet-5 and -5-5 write $4 an hour and read
// $0.20; claude-haiku-4-5 writes $1.25 for five minutes and reads $0.10.
func TestIsolatedCostOfTheGoldenStream(t *testing.T) {
	m, err := parseFile(filepath.Join("..", "claude", "testdata", "first-reads.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{Model: "claude-sonnet-5", Metrics: m}
	got := isolatedCost(rec)
	want := 0.25 + (16754*(4-0.2)+0*(1.25-0.1)+1200*(4-0.2))/1e6
	if got == nil || math.Abs(*got-want) > 1e-12 {
		t.Errorf("isolated-run cost = %v, want %.7f", got, want)
	}
}

func TestIsolatedCost(t *testing.T) {
	record := func(cost float64, reads ...claude.FirstRead) Record {
		rec := Record{Model: "claude-sonnet-5"}
		rec.Metrics.Model, rec.Metrics.CostUSD, rec.Metrics.FirstReads = "claude-sonnet-5", cost, reads
		return rec
	}
	main := func(read int64, ttl string) claude.FirstRead {
		return claude.FirstRead{Main: true, Model: "claude-sonnet-5", CacheRead: read, WriteTTL: ttl}
	}
	cases := []struct {
		name string
		rec  func() Record
		want *float64 // nil: absent
	}{
		{"a run that started cold is unchanged", func() Record {
			return record(0.85, main(0, claude.TTL1h), claude.FirstRead{Model: "claude-haiku-4-5", WriteTTL: claude.TTL5m})
		}, ptr(0.85)},
		{"a warm first request is repriced at the one-hour write rate", func() Record { return record(0.85, main(16754, claude.TTL1h)) },
			ptr(0.85 + 16754*(4-0.2)/1e6)},
		{"a five-minute time to live is repriced at its own rate", func() Record { return record(0.85, main(16754, claude.TTL5m)) },
			ptr(0.85 + 16754*(2.5-0.2)/1e6)},
		{"no time to live in the stream: one hour", func() Record { return record(0.85, main(16754, "")) },
			ptr(0.85 + 16754*(4-0.2)/1e6)},
		{"subagents are priced at their own model's rates", func() Record {
			return record(0.85, main(0, claude.TTL1h), claude.FirstRead{Model: "claude-opus-5-5", CacheRead: 6082, WriteTTL: claude.TTL5m})
		}, ptr(0.85 + 6082*(5-0.2)/1e6)},
		{"an estimated cost is the starting point", func() Record {
			rec := record(0, main(1000, claude.TTL1h))
			rec.Metrics.EstimatedCostUSD, rec.Metrics.CostUSD, rec.CostEstimated = 0.3, 0.3, true
			return rec
		}, ptr(0.3 + 1000*(4-0.2)/1e6)},
		{"the judge's spend is not the agent's", func() Record {
			rec := record(0.85, main(0, claude.TTL1h))
			rec.Judge = &judge.Verdict{CostUSD: 0.1}
			return rec
		}, ptr(0.85)},
		{"a request without a model takes the session's", func() Record {
			return record(0.85, claude.FirstRead{Main: true, CacheRead: 1000})
		}, ptr(0.85 + 1000*(4-0.2)/1e6)},
		{"then the model asked for", func() Record {
			rec := record(0.85, claude.FirstRead{Main: true, CacheRead: 1000})
			rec.Metrics.Model, rec.Model = "", "claude-opus-5-5"
			return rec
		}, ptr(0.85 + 1000*(8-0.2)/1e6)},
		{"an unknown model is absent, not zero", func() Record {
			return record(0.85, main(0, claude.TTL1h), claude.FirstRead{Model: "somebody-elses-model", CacheRead: 6082})
		}, nil},
		{"an alias is not a price", func() Record {
			rec := record(0.85, claude.FirstRead{Main: true, CacheRead: 1000})
			rec.Metrics.Model, rec.Model = "", "sonnet"
			return rec
		}, nil},
		{"no first request (an old record) is absent", func() Record { return record(0.85) }, nil},
		{"a launch of unknown type is absent", func() Record {
			rec := record(0.85, main(0, claude.TTL1h))
			rec.Metrics.UnmatchedLaunches = 1
			return rec
		}, nil},
	}
	for _, c := range cases {
		got := isolatedCost(c.rec())
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: %v, want absent", c.name, *got)
		case c.want != nil && (got == nil || math.Abs(*got-*c.want) > 1e-12):
			t.Errorf("%s: %v, want %.7f", c.name, got, *c.want)
		}
	}
}

// Records stored before the isolated-run cost existed load without it, and a computed value, zero included, survives
// storage: absent and zero stay apart.
func TestIsolatedCostStorage(t *testing.T) {
	old := `{"id":"r1","task":"t","arm":"A","model":"claude-sonnet-5","sign_in":"api-key","outcome":"ok","metrics":{"cost_usd":0.5,"first_request_tokens":30000},` +
		`"behavior":{"files_changed":1},"exit_code":0,"started":"2026-09-29T07:15:07Z","finished":"2026-09-29T07:20:00Z","records":"/x"}`
	var rec Record
	if err := json.Unmarshal([]byte(old), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.IsolatedCostUSD != nil || rec.Metrics.FirstReads != nil || rec.Spend().AgentUSD != 0.5 || isolatedCost(rec) != nil {
		t.Errorf("an old record: isolated %v, first reads %v, spend %+v", rec.IsolatedCostUSD, rec.Metrics.FirstReads, rec.Spend())
	}
	for _, value := range []*float64{nil, ptr(0), ptr(0.91)} {
		encoded, err := json.Marshal(Record{IsolatedCostUSD: value})
		if err != nil {
			t.Fatal(err)
		}
		var back Record
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatal(err)
		}
		if (value == nil) != (back.IsolatedCostUSD == nil) || (value != nil && *value != *back.IsolatedCostUSD) {
			t.Errorf("stored %v, loaded %v", value, back.IsolatedCostUSD)
		}
		if stored := StoredSpend(0, encoded); stored != (Spend{}) {
			t.Errorf("the isolated-run cost is not spend: %+v", stored)
		}
	}
}

func ptr(v float64) *float64 { return &v }
