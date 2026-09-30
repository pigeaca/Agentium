package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
)

// With a subscription, an experiment pauses between pairs before the usage limit and says when the window resets; the
// preview shows what the runs read; a resume with --wait waits for the reset and finishes. Nothing is cancelled.
func TestExperimentPausesAtTheUsageLimit(t *testing.T) {
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	resets := time.Now().Add(time.Hour).Truncate(time.Second)
	usage := filepath.Join(ctrl, "usage")
	writeUsage := func(used float64, resets time.Time) {
		if err := os.WriteFile(usage, []byte(fmt.Sprintf("%.2f 0.06 %d\n", used, resets.Unix())), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeUsage(0.70, resets)
	expect(t, f.run(ctx, "experiment", "new", "limits", "--b", "lean", "--task", "value", "--repeats", "3", "--concurrency", "1"), ExitOK)

	// No reading yet: the first pair starts; its runs report 76% and 82%, and the next pair (6% a run) would pass 85%.
	first := f.run(ctx, "experiment", "run", "limits")
	expect(t, first, ExitOK, "2/6] value", "Paused before the usage limit; the window resets at "+resets.Local().Format("15:04"),
		"paused at the usage limit: the five-hour usage window is at 82%, and the next pair (about 6% a run) would pass the 85% limit")
	if strings.Contains(first.stdout, ": cancelled,") {
		t.Errorf("a run was cancelled:\n%s", first.stdout)
	}
	if runs := experimentRuns(t, f, "limits"); len(runs) != 2 {
		t.Fatalf("%d runs stored, want the first pair's 2", len(runs))
	}
	expect(t, f.run(ctx, "experiment", "plan", "limits"), ExitOK,
		"Usage: about 6% of the five-hour window per run (a default until runs measure it), so 6 runs need about 0.4 window(s) at the 85% limit.",
		"The window was 82% used at the last reading", "about 0 more run(s) fit before the limit", "add --wait")
	expect(t, f.run(ctx, "experiment", "show", "limits"), ExitOK, "paused at the usage limit: the five-hour usage window is at 82%")

	// --wait: the wait lasts until the reset (plus a margin), then the new window takes the other two pairs.
	var waited time.Duration
	*f.sleep = func(ctx context.Context, d time.Duration) error {
		waited = d
		writeUsage(0, resets.Add(5*time.Hour))
		return nil
	}
	second := f.run(ctx, "experiment", "run", "limits", "--wait")
	expect(t, second, ExitOK, "Usage: the five-hour window is at 82%; waiting for it to reset at "+resets.Local().Format("15:04"), "Every run is done")
	if waited < time.Until(resets) || waited > time.Until(resets)+2*time.Minute {
		t.Errorf("waited %s for a reset %s away", waited, time.Until(resets).Round(time.Second))
	}
	if runs := experimentRuns(t, f, "limits"); len(runs) != 6 {
		t.Errorf("%d runs stored, want 6", len(runs))
	}
	expect(t, f.run(ctx, "run", "show", experimentRuns(t, f, "limits")[5].ID), ExitOK, "usage        five-hour window 18% → 24%, seven-day 20%")

	// A limit is a percentage.
	expect(t, f.run(ctx, "experiment", "run", "limits", "--usage-limit", "0"), ExitUsage)
	// An API key reports no usage, so its runs never pause; the preview says so.
	f.vars["ANTHROPIC_API_KEY"] = "sk-ant-test-usage" // secret-scan: allow
	expect(t, f.run(ctx, "experiment", "plan", "limits"), ExitOK, "Usage: runs with an API key report no subscription usage, so they never pause for it.")
}

// A subagent type whose model changes from earlier runs (a role's alias moving to a newer model) stops the experiment:
// later runs would not compare. run show lists each run's subagents.
func TestExperimentStopsWhenASubagentChangesModel(t *testing.T) {
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(ctrl, "subagent"), []byte("s0-t1 claude-sonnet-5\ns1-t1 claude-sonnet-5\ns2-t1 claude-sonnet-5-5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "new", "roles", "--b", "lean", "--task", "value", "--repeats", "2", "--concurrency", "1"), ExitOK)
	out := f.run(ctx, "experiment", "run", "roles")
	expect(t, out, ExitError, "subagent investigator ran on claude-sonnet-5-5; earlier runs used claude-sonnet-5: later runs would not compare")
	runs := experimentRuns(t, f, "roles")
	if len(runs) != 3 {
		t.Fatalf("%d runs, want 3: the third run reports the change and stops the experiment", len(runs))
	}
	expect(t, f.run(ctx, "run", "show", runs[0].ID), ExitOK, "subagents    investigator on claude-sonnet-5")
}

// The stored runs' readings and subagent models are read back for the gate and the check.
func TestUsageFromStoredRecords(t *testing.T) {
	resets := time.Date(2026, 9, 30, 1, 20, 0, 0, time.UTC)
	rec := func(first, last float64, models map[string][]string) []byte {
		var m struct {
			Metrics claude.Metrics `json:"metrics"`
		}
		m.Metrics.UsageFirst = &claude.UsageReading{FiveHour: first, FiveHourResets: resets}
		m.Metrics.UsageLast = &claude.UsageReading{FiveHour: last, FiveHourResets: resets}
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
	samples := usageSamples(runs)
	if per, n := experiment.UsagePerRun(samples); n != 3 || per < 0.0599 || per > 0.0601 {
		t.Errorf("per run %v over %d runs", per, n)
	}
	seen := subagentModels(runs)[""] // per arm; the earliest run wins: later ones must match it
	if got := seen["investigator"]; len(got) != 1 || got[0] != "claude-sonnet-5" || len(seen["Explore"]) != 1 {
		t.Errorf("seen = %v", seen)
	}
	now := resets.Add(-time.Hour)
	if got := clock(resets, now); got != resets.In(now.Location()).Format("15:04") {
		t.Errorf("clock today = %q", got)
	}
	if got := clock(resets.Add(24*time.Hour), now); !strings.Contains(got, " ") {
		t.Errorf("clock on another day = %q, want the weekday too", got)
	}
}

// Arms may give a role different models on purpose: that is a context difference, not a change mid-experiment.
func TestExperimentAllowsArmsWithDifferentSubagentModels(t *testing.T) {
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(ctrl, "subagent-by-arm"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "new", "arms", "--b", "lean", "--task", "value", "--repeats", "2", "--concurrency", "1"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "arms"), ExitOK, "Every run is done")
}

// An API key uses no subscription: its experiment never pauses, whatever the subscription's window reads.
func TestExperimentWithAnAPIKeyNeverPauses(t *testing.T) {
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	ctx := context.Background()
	f.vars["ANTHROPIC_API_KEY"] = "sk-ant-test-usage" // secret-scan: allow
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nKeep it short.\n")
	expect(t, f.run(ctx, "context", "snapshot", "lean", "--working-tree"), ExitOK)
	gitIn(t, f.repo, "checkout", "--", "CLAUDE.md")
	expect(t, f.run(ctx, "task", "edit", "value", "--reviewed"), ExitOK)
	expect(t, f.run(ctx, "task", "validate", "value", "--snapshot", "lean"), ExitOK)
	f.vars["AGENTIUM_CLAUDE"] = versioned(t, calibratingAgent(t, `"Bash","Edit","Read"`, `"review"`, 25000, ""), "2.1.281")
	expect(t, f.run(ctx, "run", "calibrate", "--snapshot", "lean"), ExitOK)
	ctrl := t.TempDir()
	f.vars["AGENTIUM_CLAUDE"] = experimentAgent(t, ctrl)
	if err := os.WriteFile(filepath.Join(ctrl, "usage"), []byte(fmt.Sprintf("0.84 0.06 %d\n", time.Now().Add(time.Hour).Unix())), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "new", "keyed", "--b", "lean", "--task", "value", "--repeats", "2", "--concurrency", "1"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "keyed"), ExitOK, "sign-in api-key", "Every run is done")
}
