package experiment

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// ResetMargin is how long --wait waits past the five-hour window's reset, so the first reading comes from the new
// window.
const ResetMargin = time.Minute

// DefaultUsageLimit is experiment run's --usage-limit: the share of the five-hour window (percent) past which no pair
// starts, leaving room for the user's own sessions.
const DefaultUsageLimit = 85.0

// recordUsage is what a stored run's record says about the subscription's usage and its subagents' models.
type recordUsage struct {
	Model   string `json:"model"`
	Metrics struct {
		UsageFirst     *agent.UsageReading `json:"usage_first"`
		UsageLast      *agent.UsageReading `json:"usage_last"`
		SubagentModels map[string][]string `json:"subagent_models"`
	} `json:"metrics"`
}

func decodeUsage(r store.Run) (recordUsage, bool) {
	var u recordUsage
	return u, json.Unmarshal(r.Record, &u) == nil
}

// UsageSamples are the runs' first and last usage readings, for those that read any, with calibration runs marked and
// each run's model and times.
func UsageSamples(runs []store.Run) []UsageSample {
	var samples []UsageSample
	for _, r := range runs {
		if u, ok := decodeUsage(r); ok && u.Metrics.UsageFirst != nil && u.Metrics.UsageLast != nil {
			samples = append(samples, UsageSample{First: *u.Metrics.UsageFirst, Last: *u.Metrics.UsageLast,
				Calibration: r.Kind == "calibration", Model: u.Model, Started: r.Started, Finished: r.Finished})
		}
	}
	return samples
}

// SubagentModels merges, per arm, the models each subagent type ran on across runs, the earliest run first: what later
// runs of that arm must match.
func SubagentModels(runs []store.Run) map[string]map[string][]string {
	seen := map[string]map[string][]string{}
	for _, r := range runs {
		if u, ok := decodeUsage(r); ok && len(u.Metrics.SubagentModels) > 0 {
			if seen[r.Arm] == nil {
				seen[r.Arm] = map[string][]string{}
			}
			MergeSubagentModels(seen[r.Arm], u.Metrics.SubagentModels)
		}
	}
	return seen
}

// MergeSubagentModels adds the types seen has not recorded yet.
func MergeSubagentModels(seen, now map[string][]string) {
	for kind, models := range now {
		if _, ok := seen[kind]; !ok {
			seen[kind] = slices.Clone(models)
		}
	}
}

// Clock shows a time in the user's zone: the hour, and the day when it is not today.
func Clock(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

// UsageModel is one model's part of an experiment's runs and what a run of it is expected to use.
type UsageModel struct {
	UsageRate
	PlannedRuns int // the experiment's runs on the model, both arms
}

// UsageLatest is the newest usage reading and how far it can be trusted now.
type UsageLatest struct {
	Reading agent.UsageReading
	// ReadAt is when the reading was taken, at the latest (its run's end); zero when unknown.
	ReadAt time.Time
	// Current is false when the window has reset since the reading, or the reading is older than a window: what the
	// window holds now is then unknown, and Used and Fits mean nothing.
	Current bool
	Used    float64 // the share used at the reading: at least that much is used now, as only a reset lowers it
	Fits    int     // runs at the experiment's rate that fit between Used and the limit, at most
}

// Age is how old the reading is at now; false when unknown.
func (l UsageLatest) Age(now time.Time) (time.Duration, bool) {
	if l.ReadAt.IsZero() || l.ReadAt.After(now) {
		return 0, false
	}
	return now.Sub(l.ReadAt), true
}

// UsagePreview is how much of the subscription's five-hour window an experiment needs, per model, and what the newest
// reading says of the current window. An API key reports no usage.
type UsagePreview struct {
	APIKey  bool
	Limit   float64 // share of the window past which no pair starts
	Runs    int     // the experiment's runs, both arms
	Models  []UsageModel
	Windows float64      // windows the runs need, filling each to Limit
	Latest  *UsageLatest // nil when no run has read the window
}

// FiveHourWindow is the subscription's usage window: a reading older than this is from an earlier window, whatever its
// reset time says.
const FiveHourWindow = 5 * time.Hour

// PlanUsage previews the window's use by an experiment of arms (each arm's model and runs) from the project's stored
// runs: each model at its own measured rate (UsagePerRun), or the default when none of its runs measured one.
func PlanUsage(runs []store.Run, arms []UsageModel, signIn string, limit float64, now time.Time) UsagePreview {
	return planUsage(UsageSamples(runs), arms, signIn, limit, now)
}

func planUsage(samples []UsageSample, arms []UsageModel, signIn string, limit float64, now time.Time) UsagePreview {
	p := UsagePreview{APIKey: signIn == claude.SignInAPIKey, Limit: limit}
	if p.APIKey {
		return p
	}
	need, mean := 0.0, 0.0
	for _, a := range arms {
		p.Runs += a.PlannedRuns
		i := slices.IndexFunc(p.Models, func(m UsageModel) bool { return m.Model == a.Model })
		if i >= 0 {
			p.Models[i].PlannedRuns += a.PlannedRuns
		} else {
			p.Models = append(p.Models, UsageModel{UsageRate: UsagePerRun(samples, a.Model), PlannedRuns: a.PlannedRuns})
			i = len(p.Models) - 1
		}
		need += float64(a.PlannedRuns) * p.Models[i].PerRun
	}
	if p.Runs > 0 {
		mean = need / float64(p.Runs)
	}
	p.Windows = UsageWindows(p.Runs, mean, limit)
	latest, ok := LatestUsage(samples)
	if !ok {
		return p
	}
	l := UsageLatest{Reading: latest.Last, ReadAt: latest.Finished}
	age, known := l.Age(now)
	// A reading is this window's while its window has not reset and it is younger than a window; 0% is a reading too.
	l.Current = now.Before(latest.Last.FiveHourResets) && (!known || age < FiveHourWindow)
	if l.Current {
		l.Used = latest.Last.FiveHour
		perRun := 0.0 // the gate holds every model to the largest rate
		for _, m := range p.Models {
			perRun = max(perRun, m.PerRun)
		}
		if perRun > 0 {
			l.Fits = max(int((limit-l.Used)/perRun), 0)
		}
	}
	p.Latest = &l
	return p
}

// describeAge is an age in words: minutes under an hour, else hours and minutes.
func describeAge(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h %02d min", int(d.Hours()), int(d.Minutes())%60)
}

// Write says how much of the window the experiment needs, on what each model's figure rests, and what the newest
// reading says of the current window: its age, and that use since then is not seen; a reading from before a reset, or
// older than a window, is no reading of the current window.
func (p UsagePreview) Write(out io.Writer, st term.Style, now time.Time) {
	if p.APIKey {
		fmt.Fprintln(out, "Usage: runs with an API key report no subscription usage, so they never pause for it.")
		return
	}
	var parts []string
	for _, m := range p.Models {
		basis := fmt.Sprintf("measured over %d task run(s) on %s in one window", m.Runs, m.Model)
		if m.Runs == 0 {
			basis = fmt.Sprintf("a default until %d task runs on %s in one window measure it", MinUsageRuns, m.Model)
		}
		parts = append(parts, fmt.Sprintf("about %s of the five-hour window per run on %s (%s)", sharePercent(m.PerRun), m.Model, basis))
	}
	fmt.Fprintf(out, "Usage: %s; %d runs need about %.1f window(s) at the %.0f%% limit.\n", strings.Join(parts, "; "), p.Runs, p.Windows, 100*p.Limit)
	l := p.Latest
	if l == nil {
		return
	}
	age, known := l.Age(now)
	when := "at an unknown time"
	if known {
		when = describeAge(age) + " ago"
	}
	if !l.Current {
		why := "its window has reset since"
		if now.Before(l.Reading.FiveHourResets) {
			why = "it is older than a five-hour window"
		}
		fmt.Fprintf(out, "The last reading (%s) is not this window's: %s, so this window's use is unknown until a run reports it.\n", when, why)
		return
	}
	fmt.Fprintf(out, "The window was %.0f%% used at the last reading, %s, and resets at %s: about %d more run(s) fit before the limit, fewer if\n"+
		"anything has used the window since (the reading does not see it).\n", 100*l.Used, when, Clock(l.Reading.FiveHourResets, now), l.Fits)
	if l.Fits < p.Runs {
		fmt.Fprintln(out, st.Warn("experiment run pauses between pairs at the limit (--usage-limit); run it again later, or add --wait to wait for the reset."))
	}
}

// sharePercent is a share of the window as a percentage, with a decimal below 1% (a small model's 0.2% is not 0%).
func sharePercent(v float64) string {
	if 100*v < 0.95 {
		return fmt.Sprintf("%.1f%%", 100*v)
	}
	return fmt.Sprintf("%.0f%%", 100*v)
}
