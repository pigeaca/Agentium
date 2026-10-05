package pricing

import (
	"math"
	"testing"
)

// The usage and cost of a real calibration run (Claude Code 2.1.281, claude-sonnet-5, 2026-09-28): Claude Code wrote
// its cache for an hour, and its own cost figure is these tokens at the table's prices.
func TestCostMatchesClaudeCodeOnARealRun(t *testing.T) {
	rates, ok := Lookup("claude-sonnet-5")
	if !ok {
		t.Fatal("claude-sonnet-5 is not priced")
	}
	got := rates.Cost(Usage{Input: 6, CacheWrite1h: 13302, CacheRead: 74369, Output: 654})
	if math.Abs(got-0.0746338) > 1e-9 {
		t.Fatalf("cost = %.7f, Claude Code reported 0.0746338", got)
	}
}

// The study's token profile (§5.6) priced with 5-minute cache writes, as the study did.
func TestCostReproducesTheStudysPerRunPrices(t *testing.T) {
	profile := Usage{CacheWrite5m: 200_000, CacheRead: 2_160_000, Input: 40_000, Output: 30_000}
	for model, want := range map[string]float64{"claude-sonnet-5": 1.312, "claude-opus-5-5": 2.192} {
		rates, _ := Lookup(model)
		if got := rates.Cost(profile); math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: cost = %.4f, want %.3f", model, got, want)
		}
	}
}

func TestLookup(t *testing.T) {
	if r, ok := Lookup("claude-haiku-4-5-20251001"); !ok || r.Input != 1 {
		t.Errorf("a dated model ID is priced as its model: %v %v", r, ok)
	}
	// Opus 5.5 and Fable 5.1 read their cache at 0.05× and 0.025× the input price, not the usual 0.1×.
	if r, _ := Lookup("claude-opus-5-5"); r.CacheRead != 0.05*r.Input {
		t.Errorf("opus 5.5 cache reads = %v", r.CacheRead)
	}
	if r, _ := Lookup("claude-fable-5-1"); r.CacheRead != 0.025*r.Input {
		t.Errorf("fable 5.1 cache reads = %v", r.CacheRead)
	}
	for _, model := range []string{"sonnet", "opus", "", "claude-sonnet-5-extra"} {
		if _, ok := Lookup(model); ok {
			t.Errorf("%q is priced; aliases and unknown IDs must not be", model)
		}
	}
	for model, r := range table() {
		if r.CacheWrite5m != 1.25*r.Input || r.CacheWrite1h != 2*r.Input {
			t.Errorf("%s: cache writes %v/%v do not follow the 1.25× and 2× multipliers", model, r.CacheWrite5m, r.CacheWrite1h)
		}
	}
}

// The Codex spike's spend (23 requests: 261,103 input tokens, 195,200 cached, 3,917 output) at the table's rates is
// the $0.21 notional the spike reported; reasoning is part of output, and a request above the long-context limit is
// not priced.
func TestOpenAICostReproducesTheSpike(t *testing.T) {
	rates, ok := OpenAILookup("gpt-6.1-sol")
	if !ok {
		t.Fatal("gpt-6.1-sol is not priced")
	}
	got, ok := rates.Cost(OpenAIUsage{Input: 261_103, Cached: 195_200, Output: 3_917, Reasoning: 81})
	if !ok || math.Abs(got-0.210016) > 1e-6 {
		t.Errorf("cost = %.6f, %v; want 0.210016", got, ok)
	}
	if _, ok := rates.Cost(OpenAIUsage{Input: OpenAILongContext + 1}); ok {
		t.Error("a long-context request was priced")
	}
	if got, _ := rates.Cost(OpenAIUsage{Input: 100, Cached: 80, CacheWrite: 40}); math.Abs(got-(80*0.2+40*2)/1e6) > 1e-15 {
		t.Errorf("inconsistent counts: uncached input must not go below zero: %.9f", got)
	}
	if _, ok := OpenAILookup("claude-sonnet-5"); ok {
		t.Error("a Claude model has an OpenAI price")
	}
}
