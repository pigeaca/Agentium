package run

import (
	"cmp"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/pricing"
)

// isolatedCost is the run's isolated-run cost (the wave-3 statistics note, §1): what it would have cost had no other
// run warmed the prompt cache. It starts from the run's cost (Spend().AgentUSD, Claude Code's or the transcript's
// estimate) and reprices as cache writes, at the rate of the time to live each launch wrote with, the cache reads of
// Metrics.FirstReads: the main session's first request, and the first request of each subagent launch that could not
// read its type's prefix from this run (claude's launchLog.firstReads). A run that started cold reads nothing on those
// requests, so its isolated-run cost is its cost.
//
// The value is absent (nil), never zero, when it cannot be computed: no cost, no first request in the transcript (and
// records made before FirstReads existed), a subagent launch whose type is unknown, a subagent request without a
// model (it may run on another model than the session), or a model without a list price in pricing's dated table.
func isolatedCost(rec Record) *float64 {
	m := rec.Metrics
	cost := rec.Spend().AgentUSD
	if cost <= 0 || len(m.FirstReads) == 0 || m.UnmatchedLaunches > 0 {
		return nil
	}
	for _, read := range m.FirstReads {
		model := read.Model
		if read.Main {
			model = cmp.Or(model, m.Model, rec.Model) // the request's, the session's, the one asked for
		}
		rates, ok := pricing.Lookup(model)
		if !ok {
			return nil
		}
		// Records made before the time-to-live fallback leave it empty when the launch reported no write: the
		// fallback's defaults apply.
		write := rates.CacheWrite5m
		if read.WriteTTL == claude.TTL1h || (read.WriteTTL == "" && read.Main) {
			write = rates.CacheWrite1h
		}
		cost += float64(read.CacheRead) * (write - rates.CacheRead) / 1e6
	}
	return &cost
}
