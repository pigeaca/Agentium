package experiment

import (
	"context"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
)

// StatusUsage ends an execution that paused before the subscription's five-hour usage limit. Like a budget stop, it
// resumes with a later `experiment run`.
const StatusUsage = "usage"

// DefaultUsagePerRun is the share of a five-hour window one run is assumed to use until a project's task runs measure
// it: step 7's runs on claude-sonnet-5 used about 5–6.7% each, the coordinating session's own use included.
const DefaultUsagePerRun = 0.06

// MinUsageRuns is how many task runs in one window UsagePerRun needs before it trusts their rise over the default.
const MinUsageRuns = 3

// UsageSample is one run's first and last usage readings.
type UsageSample struct {
	First, Last claude.UsageReading
	// Calibration marks a calibration run (agentium run calibrate): a few short turns, which use far less of the window
	// than a task run (1% against 6–10% in the 16-run A/B), so UsagePerRun leaves it out. Its readings still count as
	// the latest.
	Calibration bool
}

// UsagePerRun estimates the share of a five-hour window one task run uses. It takes the latest window that at least
// MinUsageRuns task runs read from start to end, and divides the rise from their lowest first reading to their highest
// last reading by those runs. Runs that overlapped share that rise, so each counts once. Calibration runs are left
// out; anything else using the subscription meanwhile (calibrations between task runs, the user's own sessions) is
// included, which errs high. Without such a window it returns DefaultUsagePerRun and 0 runs.
func UsagePerRun(samples []UsageSample) (perRun float64, runs int) {
	type window struct {
		low, high float64
		runs      int
	}
	windows := map[time.Time]*window{}
	var latest time.Time
	for _, s := range samples {
		resets := s.Last.FiveHourResets
		if s.Calibration || resets.IsZero() || !s.First.FiveHourResets.Equal(resets) {
			continue // a calibration run, no readings, or the run crossed a reset
		}
		w := windows[resets]
		if w == nil {
			w = &window{low: s.First.FiveHour, high: s.Last.FiveHour}
			windows[resets] = w
		}
		w.low, w.high, w.runs = min(w.low, s.First.FiveHour), max(w.high, s.Last.FiveHour), w.runs+1
		if w.runs >= MinUsageRuns && w.high > w.low && resets.After(latest) {
			latest = resets
		}
	}
	if latest.IsZero() {
		return DefaultUsagePerRun, 0
	}
	w := windows[latest]
	return (w.high - w.low) / float64(w.runs), w.runs
}

// LatestUsage is the newest of the samples' last readings; false when none has a reading.
func LatestUsage(samples []UsageSample) (claude.UsageReading, bool) {
	var latest claude.UsageReading
	found := false
	for _, s := range samples {
		if !s.Last.FiveHourResets.IsZero() && (!found || s.Last.Newer(latest)) {
			latest, found = s.Last, true
		}
	}
	return latest, found
}

// UsageGate keeps an execution under a share of the subscription's five-hour window. A new pair starts only when the
// latest reading, plus the expected use of the runs in flight and of the pair, stays within Limit. A pair already
// started always gets its second run, so pairs stay whole. Runs that report no readings (an API key) never pause.
type UsageGate struct {
	Limit  float64             // share of the window, such as 0.85
	PerRun float64             // the expected share per run (UsagePerRun)
	Latest claude.UsageReading // the newest reading so far; finished runs update it
	// Wait, when set, waits until the window resets instead of pausing, and returns ctx's error if it is cancelled.
	Wait func(ctx context.Context, until time.Time) error
}

// projected is the window's share after runs more runs.
func (g *UsageGate) projected(now time.Time, runs int) float64 {
	return g.Latest.FiveHourAt(now) + g.PerRun*float64(runs)
}

// UsageWindows is how many five-hour windows runs need at perRun each, filling each window up to limit.
func UsageWindows(runs int, perRun, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return float64(runs) * perRun / limit
}
