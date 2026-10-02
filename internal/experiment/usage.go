package experiment

import (
	"context"
	"slices"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
)

// StatusUsage ends an execution that paused before the subscription's five-hour usage limit. Like a budget stop, it
// resumes with a later `experiment run`.
const StatusUsage = "usage"

// DefaultUsagePerRun is the share of a five-hour window one run is assumed to use until task runs on its model measure
// it: step 7's runs on claude-sonnet-5 used about 5–6.7% each, the coordinating session's own use included. Smaller
// models use far less (the seq-v1 smoke rerun's claude-sonnet-5-5 runs about 0.2%), so the default errs high.
const DefaultUsagePerRun = 0.06

// MinUsageRuns is how many task runs on a model in one window UsagePerRun needs before it trusts their rise over the
// default.
const MinUsageRuns = 3

// UsageJoinGap is how close runs must follow one another to measure the window's rise across them: a run starting more
// than this after every earlier run of the window has ended begins a new stretch, and whatever used the window in the
// gap (the user's own sessions, another experiment) is not counted. Within it, the rise between one run's last reading
// and the next run's first (its first request, read after it was sent) still counts.
const UsageJoinGap = 2 * time.Minute

// UsageSample is one run's first and last usage readings.
type UsageSample struct {
	First, Last claude.UsageReading
	// Calibration marks a calibration run (agentium run calibrate): a few short turns, which use far less of the window
	// than a task run (1% against 6–10% in the 16-run A/B), so UsagePerRun leaves it out. Its readings still count as
	// the latest.
	Calibration bool
	// Model is the model the run asked for (its record's), which UsagePerRun measures apart from every other.
	Model string
	// Started and Finished bound the run: Last was read before Finished, so Finished is the latest the reading can be
	// from. Zero when unknown.
	Started, Finished time.Time
}

// UsageRate is the share of a five-hour window one task run on Model is expected to use, and what it rests on.
type UsageRate struct {
	Model  string
	PerRun float64
	Runs   int // the task runs on Model it was measured over; 0 when it is DefaultUsagePerRun
}

// UsagePerRun estimates the share of a five-hour window one task run on model uses, from that model's task runs only:
// another model's runs use the window at their own rate. It takes the latest window in which at least MinUsageRuns of
// them read a rise, and divides that rise by those runs. The rise is counted only while the model's runs were going:
// runs are joined into stretches (each starting within UsageJoinGap of the stretch's end so far), and each stretch adds
// the rise from its lowest first reading to its highest last reading, so overlapping runs count their shared rise once
// and use between stretches (hours of other work, as in the seq-v1 smoke check) is left out. Calibration runs, runs
// with no readings and runs that crossed a reset are left out; anything else using the subscription during the
// stretches (the user's own sessions) is included, which errs high. Without such a window it returns
// DefaultUsagePerRun and 0 runs.
func UsagePerRun(samples []UsageSample, model string) UsageRate {
	windows := map[time.Time][]UsageSample{}
	for _, s := range samples {
		resets := s.Last.FiveHourResets
		if s.Calibration || s.Model != model || resets.IsZero() || !s.First.FiveHourResets.Equal(resets) {
			continue // a calibration run, another model, no readings, or the run crossed a reset
		}
		windows[resets] = append(windows[resets], s)
	}
	var latest time.Time
	var rise float64
	var runs int
	for resets, in := range windows {
		if len(in) < MinUsageRuns || !resets.After(latest) {
			continue
		}
		if r := stretchRise(in); r > 0 {
			latest, rise, runs = resets, r, len(in)
		}
	}
	if latest.IsZero() {
		return UsageRate{Model: model, PerRun: DefaultUsagePerRun}
	}
	return UsageRate{Model: model, PerRun: rise / float64(runs), Runs: runs}
}

// stretchRise is the window's rise over the stretches of runs (UsagePerRun): runs in order of start, each joining the
// stretch so far when it starts within UsageJoinGap of the stretch's latest end. Runs without times join any stretch.
func stretchRise(runs []UsageSample) float64 {
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(a, b UsageSample) int { return a.Started.Compare(b.Started) })
	total := 0.0
	var low, high float64
	var end time.Time
	for i, s := range runs {
		if i > 0 && !s.Started.IsZero() && !end.IsZero() && s.Started.Sub(end) > UsageJoinGap {
			total += max(high-low, 0)
			low, high, end = s.First.FiveHour, s.Last.FiveHour, time.Time{}
		}
		if i == 0 {
			low, high = s.First.FiveHour, s.Last.FiveHour
		}
		low, high = min(low, s.First.FiveHour), max(high, s.Last.FiveHour)
		if s.Finished.After(end) {
			end = s.Finished
		}
	}
	return total + max(high-low, 0)
}

// UsageRateFor is the larger of the models' rates: what a gate over runs of any of them can count on per run.
func UsageRateFor(samples []UsageSample, models ...string) UsageRate {
	var out UsageRate
	for i, m := range models {
		if r := UsagePerRun(samples, m); i == 0 || r.PerRun > out.PerRun {
			out = r
		}
	}
	return out
}

// LatestUsage is the newest of the samples' last readings, with the sample it came from (its Finished is when it was
// read, at the latest); false when none has a reading.
func LatestUsage(samples []UsageSample) (UsageSample, bool) {
	var latest UsageSample
	found := false
	for _, s := range samples {
		if !s.Last.FiveHourResets.IsZero() && (!found || s.Last.Newer(latest.Last)) {
			latest, found = s, true
		}
	}
	return latest, found
}

// UsageGate keeps an execution under a share of the subscription's five-hour window. A new pair starts only when the
// latest reading, plus the expected use of the runs in flight and of the pair, stays within Limit. A pair already
// started always gets its second run, so pairs stay whole. Runs that report no readings (an API key) never pause.
type UsageGate struct {
	Limit  float64             // share of the window, such as 0.85
	PerRun float64             // the expected share per run (UsagePerRun; the larger of the arms' models)
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
