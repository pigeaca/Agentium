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
	t.Parallel()
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
	expect(t, f.run(ctx, "experiment", "new", "limits", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--concurrency", "1"), ExitOK)

	// No reading yet: the first pair starts; its runs report 76% and 82%, and the next pair (6% a run) would pass 85%.
	first := f.run(ctx, "experiment", "run", "limits")
	expect(t, first, ExitOK, "2/6] value", "Paused before the usage limit; the window resets at "+experiment.Clock(resets, time.Now()), // "Thu 00:06" after 23:00
		"paused at the usage limit: the five-hour usage window is at 82%, and the next pair (about 6% a run) would pass the 85% limit")
	if strings.Contains(first.stdout, ": cancelled,") {
		t.Errorf("a run was cancelled:\n%s", first.stdout)
	}
	if runs := experimentRuns(t, f, "limits"); len(runs) != 2 {
		t.Fatalf("%d runs stored, want the first pair's 2", len(runs))
	}
	expect(t, f.run(ctx, "experiment", "plan", "limits"), ExitOK,
		"Usage: about 6% of the five-hour window per run on claude-sonnet-5-5 (a default until 3 task runs on claude-sonnet-5-5 in one window measure it); 6 runs need about 0.4 window(s) at the 85% limit.",
		"The window was 82% used at the last reading, ", "about 0 more run(s) fit before the limit", "add --wait")
	expect(t, f.run(ctx, "experiment", "show", "limits"), ExitOK, "paused at the usage limit: the five-hour usage window is at 82%")

	// --wait: the wait lasts until the reset (plus a margin), then the new window takes the other two pairs.
	var waited time.Duration
	*f.sleep = func(ctx context.Context, d time.Duration) error {
		waited = d
		writeUsage(0, resets.Add(5*time.Hour))
		return nil
	}
	second := f.run(ctx, "experiment", "run", "limits", "--wait")
	expect(t, second, ExitOK, "Usage: the five-hour window is at 82%; waiting for it to reset at "+experiment.Clock(resets, time.Now()), "Every run is done")
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
	if plan := jsonRun(t, f, ExitOK, "experiment", "plan", "limits"); plan.get("usage") != nil {
		t.Errorf("with an API key, usage is not null: %s", plan.stdout)
	}
}

// A subagent type whose model changes from earlier runs (a role's alias moving to a newer model) stops the experiment:
// later runs would not compare. run show lists each run's subagents.
func TestExperimentStopsWhenASubagentChangesModel(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(ctrl, "subagent"), []byte("s0-t1 claude-sonnet-5\ns1-t1 claude-sonnet-5\ns2-t1 claude-sonnet-5-5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "new", "roles", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--concurrency", "1"), ExitOK)
	out := f.run(ctx, "experiment", "run", "roles")
	expect(t, out, ExitError, "subagent investigator ran on claude-sonnet-5-5; earlier runs used claude-sonnet-5: later runs would not compare")
	runs := experimentRuns(t, f, "roles")
	if len(runs) != 3 {
		t.Fatalf("%d runs, want 3: the third run reports the change and stops the experiment", len(runs))
	}
	expect(t, f.run(ctx, "run", "show", runs[0].ID), ExitOK, "subagents    investigator on claude-sonnet-5")
}

// Calibration runs are a few short turns (about 1% of the window against a task run's 6–10% in the 16-run A/B): the
// usage per run comes from task runs only, from an older window when the latest holds only calibrations, or else the
// default. The latest reading still comes from every run.
func TestUsagePreviewLeavesOutCalibrationRuns(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "ab", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2"), ExitOK)
	now := time.Now()
	older, latest := now.Add(-2*time.Hour).Truncate(time.Second), now.Add(2*time.Hour).Truncate(time.Second)
	read := func(kind string, first, last float64, resets time.Time) store.Run {
		var rec struct {
			Model   string         `json:"model"`
			Metrics claude.Metrics `json:"metrics"`
		}
		rec.Model = "claude-sonnet-5-5"
		rec.Metrics.UsageFirst = &claude.UsageReading{FiveHour: first, FiveHourResets: resets}
		rec.Metrics.UsageLast = &claude.UsageReading{FiveHour: last, FiveHourResets: resets}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		return store.Run{TaskName: "value", Kind: kind, Outcome: "ok", Record: data}
	}
	saveRuns(t, f, read("calibration", 0.40, 0.41, latest), read("calibration", 0.41, 0.42, latest),
		read("calibration", 0.42, 0.43, latest), read("calibration", 0.43, 0.44, latest))
	expect(t, f.run(ctx, "experiment", "plan", "ab"), ExitOK,
		"Usage: about 6% of the five-hour window per run on claude-sonnet-5-5 (a default until 3 task runs on claude-sonnet-5-5 in one window measure it); 4 runs need about 0.3 window(s)",
		"The window was 44% used at the last reading, less than a minute ago", "about 6 more run(s) fit before the limit")

	saveRuns(t, f, read("task", 0.50, 0.60, older), read("task", 0.60, 0.70, older), read("task", 0.70, 0.80, older))
	expect(t, f.run(ctx, "experiment", "plan", "ab"), ExitOK,
		"Usage: about 10% of the five-hour window per run on claude-sonnet-5-5 (measured over 3 task run(s) on claude-sonnet-5-5 in one window); 4 runs need about 0.5 window(s)",
		"The window was 44% used at the last reading", "about 4 more run(s) fit before the limit")
}

// Arms may give a role different models on purpose: that is a context difference, not a change mid-experiment.
func TestExperimentAllowsArmsWithDifferentSubagentModels(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(ctrl, "subagent-by-arm"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "new", "arms", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--concurrency", "1"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "arms"), ExitOK, "Every run is done")
}

// An API key uses no subscription: its experiment never pauses, whatever the subscription's window reads.
func TestExperimentWithAnAPIKeyNeverPauses(t *testing.T) {
	t.Parallel()
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
	expect(t, f.run(ctx, "experiment", "new", "keyed", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--concurrency", "1"), ExitOK)
	expect(t, f.run(ctx, "experiment", "run", "keyed"), ExitOK, "sign-in api-key", "Every run is done")
}

// The seq-v1 smoke rerun's preview, fixed: claude-sonnet-5 runs from two hours ago neither set claude-sonnet-5-5's rate
// (the default stands in, and says so) nor pass as the window's current state (the reading's age is shown, with the
// use it cannot see); with no runs on the model, the estimate says it assumes each run reaches its cap.
func TestUsagePreviewPerModelAndReadingAge(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "smoke", "--run-budget", "0.3", "--budget", "5", "--seed", "7"), ExitOK)
	now := time.Now()
	resets := now.Add(3 * time.Hour).Truncate(time.Second)
	var runs []store.Run
	for i := range 6 { // 3% → 13% over six runs, about two hours ago
		var rec struct {
			Model   string         `json:"model"`
			Metrics claude.Metrics `json:"metrics"`
		}
		rec.Model = "claude-sonnet-5"
		rec.Metrics.UsageFirst = &claude.UsageReading{FiveHour: 0.03 + float64(i)*0.10/6, FiveHourResets: resets}
		rec.Metrics.UsageLast = &claude.UsageReading{FiveHour: 0.03 + float64(i+1)*0.10/6, FiveHourResets: resets}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		start := now.Add(-2*time.Hour + time.Duration(i)*100*time.Second)
		runs = append(runs, store.Run{TaskName: "value", Kind: "task", Outcome: "ok", CostUSD: 0.4, Record: data, Started: start, Finished: start.Add(100 * time.Second)})
	}
	saveRuns(t, f, runs...)
	plan := f.run(ctx, "experiment", "plan", "smoke")
	expect(t, plan, ExitOK,
		"value, without runs of their own: $0.30, the run cap: no runs on this model yet: the estimate assumes each run reaches its cap",
		"No runs on claude-sonnet-5-5 yet: the estimated spend above assumes each run reaches its cap",
		"Usage: about 6% of the five-hour window per run on claude-sonnet-5-5 (a default until 3 task runs on claude-sonnet-5-5 in one window measure it); 2 runs need about 0.1 window(s) at the 85% limit.",
		"The window was 13% used at the last reading, 1 h 50 min ago, and resets at "+experiment.Clock(resets, now)+": about 12 more run(s) fit before the limit, fewer if\nanything has used the window since (the reading does not see it).")
	t.Log("experiment plan, the smoke rerun's case:\n" + plan.stdout)
	doc := jsonRun(t, f, ExitOK, "experiment", "plan", "smoke")
	basis := doc.get("spend", "estimate_basis").([]any)[0].(map[string]any)
	model := doc.get("usage", "models").([]any)[0].(map[string]any)
	if basis["basis"] != "cap" || basis["history_runs"] != float64(0) || basis["model"] != "claude-sonnet-5-5" || basis["per_run_usd"] != 0.3 ||
		model["measured_runs"] != float64(0) || model["per_run"] != experiment.DefaultUsagePerRun || doc.get("usage", "latest", "current") != true {
		t.Errorf("experiment plan --json: %s", doc.stdout)
	}
}
