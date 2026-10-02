package run

import (
	"cmp"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/pricing"
)

// isolatedCost is the run's isolated-run cost (the wave-3 statistics note, §1): what it would have cost had no other
// run warmed the prompt cache. It starts from the run's cost (Spend().AgentUSD, Claude Code's or the transcript's
// estimate) and reprices, at the cache-write rate of the time to live the launch wrote with (one hour unless the stream
// reports five minutes), the cache reads of the main session's first request and of each subagent type's first launch
// (Metrics.FirstReads). A later launch of a type reads the prefix this run wrote itself and is not repriced. A run that
// started cold reads nothing on those requests, so its isolated-run cost is its cost.
//
// The value is absent (nil), never zero, when it cannot be computed: no first request in the transcript (and records
// made before FirstReads existed), a subagent launch whose type is unknown (it may have been a first launch), or a
// model without a list price in pricing's dated table.
func isolatedCost(rec Record) *float64 {
	m := rec.Metrics
	if len(m.FirstReads) == 0 || m.UnmatchedLaunches > 0 {
		return nil
	}
	cost := rec.Spend().AgentUSD
	for _, read := range m.FirstReads {
		rates, ok := pricing.Lookup(cmp.Or(read.Model, m.Model, rec.Model)) // the request's, the session's, the one asked for
		if !ok {
			return nil
		}
		write := rates.CacheWrite1h
		if read.WriteTTL == claude.TTL5m {
			write = rates.CacheWrite5m
		}
		cost += float64(read.CacheRead) * (write - rates.CacheRead) / 1e6
	}
	return &cost
}
