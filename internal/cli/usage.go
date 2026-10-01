package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// resetMargin is how long --wait waits past the five-hour window's reset, so the first reading comes from the new
// window.
const resetMargin = time.Minute

// defaultUsageLimit is experiment run's --usage-limit: the share of the five-hour window (percent) past which no pair
// starts, leaving room for the user's own sessions.
const defaultUsageLimit = 85.0

// recordUsage is what a stored run's record says about the subscription's usage and its subagents' models.
type recordUsage struct {
	Metrics struct {
		UsageFirst     *claude.UsageReading `json:"usage_first"`
		UsageLast      *claude.UsageReading `json:"usage_last"`
		SubagentModels map[string][]string  `json:"subagent_models"`
	} `json:"metrics"`
}

func decodeUsage(r store.Run) (recordUsage, bool) {
	var u recordUsage
	return u, json.Unmarshal(r.Record, &u) == nil
}

// usageSamples are the runs' first and last usage readings, for those that read any, with calibration runs marked.
func usageSamples(runs []store.Run) []experiment.UsageSample {
	var samples []experiment.UsageSample
	for _, r := range runs {
		if u, ok := decodeUsage(r); ok && u.Metrics.UsageFirst != nil && u.Metrics.UsageLast != nil {
			samples = append(samples, experiment.UsageSample{First: *u.Metrics.UsageFirst, Last: *u.Metrics.UsageLast,
				Calibration: r.Kind == "calibration"})
		}
	}
	return samples
}

// subagentModels merges, per arm, the models each subagent type ran on across runs, the earliest run first: what later
// runs of that arm must match.
func subagentModels(runs []store.Run) map[string]map[string][]string {
	seen := map[string]map[string][]string{}
	for _, r := range runs {
		if u, ok := decodeUsage(r); ok && len(u.Metrics.SubagentModels) > 0 {
			if seen[r.Arm] == nil {
				seen[r.Arm] = map[string][]string{}
			}
			mergeSubagentModels(seen[r.Arm], u.Metrics.SubagentModels)
		}
	}
	return seen
}

// mergeSubagentModels adds the types seen has not recorded yet.
func mergeSubagentModels(seen, now map[string][]string) {
	for kind, models := range now {
		if _, ok := seen[kind]; !ok {
			seen[kind] = slices.Clone(models)
		}
	}
}

// waitUntil sleeps until a little past until (resetMargin), or until ctx is cancelled.
func waitUntil(ctx context.Context, env Env, until time.Time) error {
	d := until.Sub(env.Now()) + resetMargin
	if env.Sleep != nil {
		return env.Sleep(ctx, d)
	}
	timer := time.NewTimer(max(d, 0))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clock shows a time in the user's zone: the hour, and the day when it is not today.
func clock(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

// printUsagePreview says how much of the subscription's five-hour window an experiment of runs needs, and whether the
// current window fits it. An API key reports no usage: its runs never pause.
func printUsagePreview(out io.Writer, st term.Style, runs []store.Run, experimentRuns int, signIn string, limit float64, now time.Time) {
	if signIn == claude.SignInAPIKey {
		fmt.Fprintln(out, "Usage: runs with an API key report no subscription usage, so they never pause for it.")
		return
	}
	samples := usageSamples(runs)
	perRun, measured := experiment.UsagePerRun(samples)
	basis := fmt.Sprintf("measured over %d task runs in one window", measured)
	if measured == 0 {
		basis = fmt.Sprintf("a default until %d task runs in one window measure it", experiment.MinUsageRuns)
	}
	fmt.Fprintf(out, "Usage: about %.0f%% of the five-hour window per run (%s), so %d runs need about %.1f window(s) at the %.0f%% limit.\n",
		100*perRun, basis, experimentRuns, experiment.UsageWindows(experimentRuns, perRun, limit), 100*limit)
	latest, ok := experiment.LatestUsage(samples)
	if !ok {
		return
	}
	used := latest.FiveHourAt(now)
	if used == 0 {
		fmt.Fprintln(out, "The last reading's window has reset: this window's use is unknown until a run reports it.")
		return
	}
	fits := 0
	if perRun > 0 {
		fits = max(int((limit-used)/perRun), 0)
	}
	fmt.Fprintf(out, "The window was %.0f%% used at the last reading and resets at %s: about %d more run(s) fit before the limit.\n",
		100*used, clock(latest.FiveHourResets, now), fits)
	if fits < experimentRuns {
		fmt.Fprintln(out, st.Warn("experiment run pauses between pairs at the limit (--usage-limit); run it again later, or add --wait to wait for the reset."))
	}
}
