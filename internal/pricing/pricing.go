// Package pricing holds Anthropic's list prices, dated, for estimating what runs will cost. Claude Code reports each
// run's cost itself; this table is for plans made before any run, and for transcripts that end without a result.
package pricing

import (
	"regexp"
	"strings"
)

// The table's date and source. A lock records the date, so a later price change shows in an experiment's provenance.
const (
	Date   = "2026-09-29"
	Source = "https://platform.claude.com/docs/en/about-claude/pricing"
)

// Rates are USD per million tokens.
type Rates struct {
	Input        float64 `json:"input"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	CacheRead    float64 `json:"cache_read"` // cache hits and refreshes
	Output       float64 `json:"output"`
}

// table holds the Claude API's standard (global) prices, without batch, fast-mode or data-residency pricing, which
// Claude Code's runs here do not use.
var table = map[string]Rates{
	"claude-fable-5-1":  {Input: 10, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 0.25, Output: 50},
	"claude-fable-5":    {Input: 10, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 1, Output: 50},
	"claude-opus-5-5":   {Input: 4, CacheWrite5m: 5, CacheWrite1h: 8, CacheRead: 0.20, Output: 20},
	"claude-opus-5":     {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50, Output: 25},
	"claude-opus-4-8":   {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50, Output: 25},
	"claude-opus-4-7":   {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50, Output: 25},
	"claude-opus-4-6":   {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50, Output: 25},
	"claude-opus-4-5":   {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50, Output: 25},
	"claude-sonnet-5-5": {Input: 2, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.20, Output: 10},
	"claude-sonnet-5":   {Input: 2, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.20, Output: 10},
	"claude-sonnet-4-6": {Input: 3, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.30, Output: 15},
	"claude-sonnet-4-5": {Input: 3, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.30, Output: 15},
	"claude-haiku-4-5":  {Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.10, Output: 5},
}

var dated = regexp.MustCompile(`-\d{8}$`)

// Lookup returns a model's rates. Dated model IDs (claude-haiku-4-5-20251001) are priced as their model; aliases such
// as "sonnet", which Claude Code resolves itself, are not known.
func Lookup(model string) (Rates, bool) {
	r, ok := table[dated.ReplaceAllString(strings.TrimSpace(model), "")]
	return r, ok
}

// Usage is a run's or a request's token counts.
type Usage struct {
	Input        int64 `json:"input"` // uncached input
	CacheWrite5m int64 `json:"cache_write_5m"`
	CacheWrite1h int64 `json:"cache_write_1h"`
	CacheRead    int64 `json:"cache_read"`
	Output       int64 `json:"output"` // thinking included
}

// Cost prices u at these rates.
func (r Rates) Cost(u Usage) float64 {
	return (float64(u.Input)*r.Input + float64(u.CacheWrite5m)*r.CacheWrite5m + float64(u.CacheWrite1h)*r.CacheWrite1h +
		float64(u.CacheRead)*r.CacheRead + float64(u.Output)*r.Output) / 1e6
}
