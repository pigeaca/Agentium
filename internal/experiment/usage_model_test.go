package experiment

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/term"
)

const (
	sonnet5  = "claude-sonnet-5"
	sonnet55 = "claude-sonnet-5-5"
)

// usageRun is a task run on model from start to start+length, reading first and last in the window resetting at resets.
func usageRun(model string, start time.Time, length time.Duration, first, last float64, resets time.Time) UsageSample {
	return UsageSample{Model: model, Started: start, Finished: start.Add(length),
		First: claude.UsageReading{FiveHour: first, FiveHourResets: resets}, Last: claude.UsageReading{FiveHour: last, FiveHourResets: resets}}
}

// The seq-v1 smoke rerun's case: another model's runs measured 2% a run in a window other work shared, which said
// nothing about Sonnet 5.5's 0.2%. A model's rate comes from its own runs only, and the default stands in until it has
// some; use between stretches of runs (hours of other work) is not counted, overlapping runs count their rise once.
func TestUsagePerRunIsPerModel(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 15, 30, 0, 0, time.UTC)
	resets := now.Add(2 * time.Hour)
	var samples []UsageSample
	// Six claude-sonnet-5 runs, 3% → 13%, two hours ago.
	for i := range 6 {
		start := now.Add(-2*time.Hour + time.Duration(i)*2*time.Minute)
		samples = append(samples, usageRun(sonnet5, start, 2*time.Minute, 0.03+float64(i)*0.0167, 0.03+float64(i+1)*0.0167, resets))
	}
	if r := UsagePerRun(samples, sonnet55); r.Runs != 0 || r.PerRun != DefaultUsagePerRun || r.Model != sonnet55 {
		t.Errorf("no runs on the model: %+v, want the default: another model's runs measure nothing for it", r)
	}
	if r := UsagePerRun(samples, sonnet5); r.Runs != 6 || abs(r.PerRun-0.0167) > 1e-3 {
		t.Errorf("claude-sonnet-5's own runs: %+v", r)
	}

	// Sonnet 5.5, two at a time: stage 1's four runs take the window 59% → 60%, an hour of other work takes it to 63%,
	// then stage 2's four runs take it to 64%. Overlapping runs share their rise; the gap's 3 points are not theirs.
	stage := func(at time.Time, from float64) []UsageSample {
		return []UsageSample{
			usageRun(sonnet55, at, 30*time.Second, from, from+0.003, resets),
			usageRun(sonnet55, at.Add(5*time.Second), 30*time.Second, from+0.001, from+0.005, resets),
			usageRun(sonnet55, at.Add(31*time.Second), 30*time.Second, from+0.005, from+0.008, resets),
			usageRun(sonnet55, at.Add(36*time.Second), 30*time.Second, from+0.006, from+0.010, resets),
		}
	}
	samples = append(samples, stage(now.Add(-70*time.Minute), 0.59)...)
	samples = append(samples, stage(now.Add(-5*time.Minute), 0.63)...)
	r := UsagePerRun(samples, sonnet55)
	if r.Runs != 8 || abs(r.PerRun-0.0025) > 1e-9 { // (0.010 + 0.010) / 8: not (0.64 − 0.59) / 8
		t.Errorf("claude-sonnet-5-5: %+v, want 0.25%% a run over 8 runs", r)
	}
	if r := UsagePerRun(samples, sonnet5); r.Runs != 6 {
		t.Errorf("another model's runs changed claude-sonnet-5's rate: %+v", r)
	}
	// A gate over both models counts on the larger rate.
	if got := UsageRateFor(samples, sonnet55, sonnet5); got.Model != sonnet5 {
		t.Errorf("the gate's rate = %+v, want claude-sonnet-5's", got)
	}
	// Runs back to back (within UsageJoinGap) are one stretch: the rise between one run's last reading and the next's
	// first counts, so a first request read after it was sent is not lost.
	chained := []UsageSample{usageRun(sonnet55, now, time.Minute, 0.10, 0.11, resets), usageRun(sonnet55, now.Add(70*time.Second), time.Minute, 0.12, 0.13, resets),
		usageRun(sonnet55, now.Add(140*time.Second), time.Minute, 0.14, 0.15, resets)}
	if r := UsagePerRun(chained, sonnet55); r.Runs != 3 || abs(r.PerRun-0.05/3) > 1e-9 {
		t.Errorf("runs back to back: %+v, want (0.15 − 0.10) / 3", r)
	}
}

func writeUsage(p UsagePreview, now time.Time) string {
	var out bytes.Buffer
	p.Write(&out, term.Style{}, now)
	return out.String()
}

// The preview names each model's figure and what it rests on, and a reading's age; a reading from before a reset, or
// older than a five-hour window, is not quoted as the current window's.
func TestUsagePreviewSaysWhatItRestsOn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 15, 30, 0, 0, time.Local)
	resets := now.Add(3 * time.Hour)
	runs := func(samples ...UsageSample) []UsageSample { return samples }
	arms := []UsageModel{{UsageRate: UsageRate{Model: sonnet55}, PlannedRuns: 16}, {UsageRate: UsageRate{Model: sonnet55}, PlannedRuns: 16}}

	// The smoke rerun's preview: only claude-sonnet-5 runs, two hours old, at 13%.
	old := runs(usageRun(sonnet5, now.Add(-125*time.Minute), 5*time.Minute, 0.03, 0.07, resets), usageRun(sonnet5, now.Add(-120*time.Minute), 5*time.Minute, 0.07, 0.10, resets),
		usageRun(sonnet5, now.Add(-115*time.Minute), 5*time.Minute, 0.10, 0.13, resets))
	p := planUsage(old, arms, "login", 0.85, now)
	if len(p.Models) != 1 || p.Models[0].Runs != 0 || p.Models[0].PlannedRuns != 32 || p.Latest == nil || !p.Latest.Current {
		t.Fatalf("preview = %+v", p)
	}
	got := writeUsage(p, now)
	for _, want := range []string{
		"Usage: about 6% of the five-hour window per run on claude-sonnet-5-5 (a default until 3 task runs on claude-sonnet-5-5 in one window measure it); 32 runs need about 2.3 window(s) at the 85% limit.",
		"The window was 13% used at the last reading, 1 h 50 min ago, and resets at " + Clock(resets, now) + ": about 12 more run(s) fit before the limit, fewer if\nanything has used the window since (the reading does not see it).",
		"experiment run pauses between pairs at the limit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preview lacks %q:\n%s", want, got)
		}
	}

	// Measured on the model itself: a share under 1% keeps its decimal.
	measured := runs(usageRun(sonnet55, now.Add(-3*time.Minute), time.Minute, 0.59, 0.592, resets), usageRun(sonnet55, now.Add(-2*time.Minute), time.Minute, 0.592, 0.594, resets),
		usageRun(sonnet55, now.Add(-time.Minute), 30*time.Second, 0.594, 0.596, resets))
	got = writeUsage(planUsage(measured, arms, "login", 0.85, now), now)
	if want := "Usage: about 0.2% of the five-hour window per run on claude-sonnet-5-5 (measured over 3 task run(s) on claude-sonnet-5-5 in one window); 32 runs need about 0.1 window(s)"; !strings.Contains(got, want) {
		t.Errorf("preview lacks %q:\n%s", want, got)
	}
	if want := "The window was 60% used at the last reading, less than a minute ago"; !strings.Contains(got, want) {
		t.Errorf("preview lacks %q:\n%s", want, got)
	}

	// A reading from before a reset is no reading of this window.
	reset := runs(usageRun(sonnet55, now.Add(-4*time.Hour), time.Minute, 0.40, 0.41, now.Add(-time.Hour)))
	p = planUsage(reset, arms, "login", 0.85, now)
	if p.Latest == nil || p.Latest.Current || p.Latest.Used != 0 || p.Latest.Fits != 0 {
		t.Errorf("a reading before a reset: %+v", p.Latest)
	}
	got = writeUsage(p, now)
	if want := "The last reading (3 h 59 min ago) is not this window's: its window has reset since, so this window's use is unknown until a run reports it."; !strings.Contains(got, want) {
		t.Errorf("preview lacks %q:\n%s", want, got)
	}
	if strings.Contains(got, "used at the last reading") || strings.Contains(got, "pauses between pairs") {
		t.Errorf("a reading before a reset is quoted as current:\n%s", got)
	}
	// One older than a window is not current either, whatever its reset time says.
	stale := runs(usageRun(sonnet55, now.Add(-6*time.Hour), time.Minute, 0.40, 0.41, now.Add(time.Hour)))
	if got := writeUsage(planUsage(stale, arms, "login", 0.85, now), now); !strings.Contains(got, "(5 h 59 min ago) is not this window's: it is older than a five-hour window") {
		t.Errorf("a reading older than a window:\n%s", got)
	}
	// A model-ab experiment: each model at its own rate, and the windows from both.
	both := []UsageModel{{UsageRate: UsageRate{Model: sonnet55}, PlannedRuns: 10}, {UsageRate: UsageRate{Model: sonnet5}, PlannedRuns: 10}}
	p = planUsage(append(measured, old...), both, "login", 0.85, now)
	if len(p.Models) != 2 || p.Models[0].Runs != 3 || p.Models[1].Runs != 3 || abs(p.Windows-(10*0.002+10*p.Models[1].PerRun)/0.85) > 1e-9 {
		t.Errorf("model-ab preview: %+v", p)
	}
	if p := planUsage(measured, arms, claude.SignInAPIKey, 0.85, now); !p.APIKey || p.Latest != nil || len(p.Models) != 0 {
		t.Errorf("an API key: %+v", p)
	}
}

// A 0% reading in a live window is current: the window has not reset, nothing of it is used yet.
func TestUsagePreviewZeroReadingIsCurrent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 15, 30, 0, 0, time.Local)
	resets := now.Add(4 * time.Hour)
	arms := []UsageModel{{UsageRate: UsageRate{Model: sonnet55}, PlannedRuns: 2}}
	p := planUsage([]UsageSample{usageRun(sonnet55, now.Add(-10*time.Minute), time.Minute, 0, 0, resets)}, arms, "login", 0.85, now)
	if p.Latest == nil || !p.Latest.Current || p.Latest.Used != 0 || p.Latest.Fits != 14 { // 0.85 / 0.06
		t.Fatalf("a 0%% reading: %+v", p.Latest)
	}
	got := writeUsage(p, now)
	if want := "The window was 0% used at the last reading, 9 min ago"; !strings.Contains(got, want) || strings.Contains(got, "has reset since") {
		t.Errorf("preview lacks %q, or says the window reset:\n%s", want, got)
	}
}
