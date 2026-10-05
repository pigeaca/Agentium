package pricing

import "strings"

// OpenAI's list prices, dated, for Codex runs: Codex reports tokens but no cost, so Agentium prices each request
// itself ("priced by Agentium"), and its cost cap rests on these rates. A lock records the date, as for Anthropic's.
//
// The rates are the ones the Codex plan cites (.agents/plans/2026-10-04-codex.md, decision 5: gpt-6.1-sol at $2 input
// and $10 output per million tokens, as Sonnet 5.5). Two of them are not pinned to a published figure:
//   - CachedInput, $0.20: assumed (a tenth of the input rate, as for OpenAI's earlier models); the Codex spike could not
//     pin it (docs/research/2026-10-04-codex-spike.md, Gaps).
//   - CacheWrite: assumed to be the input rate; every request the spike saw wrote no cache (cache_write_input_tokens 0).
//
// Only the standard tier is priced: the fast tier (service tier "priority") stays off in Agentium's runs. Above
// OpenAILongContext input tokens a request is billed at a long-context rate, which no source here gives: such a request
// is unpriced (OpenAIRates.Cost), so the cost cap stops the run rather than guess.
const (
	OpenAIDate   = "2026-10-04"
	OpenAISource = "https://openai.com/api/pricing/"
	// OpenAILongContext is the most input tokens a request may have and be priced at these rates.
	OpenAILongContext = 272_000
)

// OpenAIRates are USD per million tokens. Reasoning tokens are output tokens, at the output rate.
type OpenAIRates struct {
	Input       float64 `json:"input"`        // uncached input
	CachedInput float64 `json:"cached_input"` // assumed (see above)
	CacheWrite  float64 `json:"cache_write"`  // assumed (see above)
	Output      float64 `json:"output"`       // reasoning included
}

// openAITable is OpenAI's standard-tier prices for the models Agentium prices; a model missing here is unpriced: its
// runs show tokens only, and a run with a cost cap does not start.
func openAITable() map[string]OpenAIRates {
	return map[string]OpenAIRates{
		"gpt-6.1-sol": {Input: 2, CachedInput: 0.20, CacheWrite: 2, Output: 10},
	}
}

// OpenAILookup returns an OpenAI model's rates.
func OpenAILookup(model string) (OpenAIRates, bool) {
	r, ok := openAITable()[strings.TrimSpace(model)]
	return r, ok
}

// OpenAIUsage is one request's tokens as Codex counts them (its token_usage_record): Input includes Cached and
// CacheWrite, and Output includes Reasoning.
type OpenAIUsage struct {
	Input      int64 `json:"input"`
	Cached     int64 `json:"cached"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
}

// Uncached is the input that is neither read from nor written to the cache (never below zero).
func (u OpenAIUsage) Uncached() int64 { return max(0, u.Input-u.Cached-u.CacheWrite) }

// Cost prices one request at these rates; ok is false for a request above OpenAILongContext input tokens, whose rate
// is not in the table.
func (r OpenAIRates) Cost(u OpenAIUsage) (usd float64, ok bool) {
	if u.Input > OpenAILongContext {
		return 0, false
	}
	return (float64(u.Uncached())*r.Input + float64(u.Cached)*r.CachedInput + float64(u.CacheWrite)*r.CacheWrite +
		float64(u.Output)*r.Output) / 1e6, true
}
