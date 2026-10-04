package experiment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/store"
)

// The stored runs' readings and subagent models are read back for the gate and the check.
func TestUsageFromStoredRecords(t *testing.T) {
	t.Parallel()
	resets := time.Date(2026, 9, 30, 1, 20, 0, 0, time.UTC)
	rec := func(first, last float64, models map[string][]string) []byte {
		var m struct {
			Metrics agent.Metrics `json:"metrics"`
		}
		m.Metrics.UsageFirst = &agent.UsageReading{FiveHour: first, FiveHourResets: resets}
		m.Metrics.UsageLast = &agent.UsageReading{FiveHour: last, FiveHourResets: resets}
		m.Metrics.SubagentModels = models
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var runs []store.Run
	for _, r := range [][]byte{rec(0.10, 0.16, map[string][]string{"investigator": {"claude-sonnet-5"}}), rec(0.16, 0.22, nil),
		rec(0.22, 0.28, map[string][]string{"investigator": {"claude-sonnet-5-5"}, "Explore": {"claude-haiku-4-5"}}), []byte("not json")} {
		runs = append(runs, store.Run{Record: r})
	}
	samples := UsageSamples(runs)
	if r := UsagePerRun(samples, ""); r.Runs != 3 || r.PerRun < 0.0599 || r.PerRun > 0.0601 {
		t.Errorf("per run %+v", r)
	}
	seen := SubagentModels(runs)[""] // per arm; the earliest run wins: later ones must match it
	if got := seen["investigator"]; len(got) != 1 || got[0] != "claude-sonnet-5" || len(seen["Explore"]) != 1 {
		t.Errorf("seen = %v", seen)
	}
	now := time.Date(resets.Year(), resets.Month(), resets.Day(), 0, 0, 0, 0, resets.Location()) // the same day, at any hour
	if got := Clock(resets, now); got != resets.In(now.Location()).Format("15:04") {
		t.Errorf("clock today = %q", got)
	}
	if got := Clock(resets.Add(24*time.Hour), now); !strings.Contains(got, " ") {
		t.Errorf("clock on another day = %q, want the weekday too", got)
	}
}
