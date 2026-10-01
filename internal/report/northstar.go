package report

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
)

// NorthStar is the product's north-star measure for one project: the time and the dollars from its registration
// (`agentium init`) to its first decisive verdict. It is computed from stored data (the project's registration, its
// experiments and runs), so it needs no migration and is the same whenever it is read.
//
// Only experiments that ran to their end (status done) count: one that stopped for its budget or the usage window may
// still change its verdict when it resumes.
//
// A verdict is decisive when it is improved, regressed or no loss beyond the margin (also "improved, but small" and
// "equivalent", which are as firm); inconclusive and exploratory verdicts do not count. Only the primary metric and the
// guard have verdicts that count (the others are exploratory by role). An A/A calibration never counts: it measures
// noise and answers nothing about a change.
type NorthStar struct {
	Decisive bool `json:"decisive"`
	// Experiment, Metric and Verdict name the first decisive verdict, the one whose experiment's last run finished first.
	Experiment string `json:"experiment,omitempty"`
	Metric     string `json:"metric,omitempty"`
	Verdict    string `json:"verdict,omitempty"`
	// Seconds is the time from the project's registration to the end of that experiment's last run.
	Seconds float64 `json:"seconds,omitempty"`
	// SpentUSD is what every run of the project started up to that experiment's last run spent, the agent's and the
	// judge's (run.Spend). Without a decisive verdict, it is what all the project's runs spent.
	SpentUSD float64 `json:"spent_usd"`
}

// Decisive reports whether verdict is one that ends the search for a first answer.
func Decisive(verdict string) bool {
	return slices.Contains([]string{stats.Improved, stats.ImprovedSmall, stats.Regressed, stats.NoLoss, stats.Equivalent}, verdict)
}

// LoadNorthStar computes the project's north star from what is stored.
func LoadNorthStar(ctx context.Context, p experiment.Project) (NorthStar, error) {
	projects, err := p.DB.Projects(ctx)
	if err != nil {
		return NorthStar{}, err
	}
	i := slices.IndexFunc(projects, func(x store.Project) bool { return x.ID == p.ID })
	if i < 0 {
		return NorthStar{}, fmt.Errorf("north star: project %d: %w", p.ID, store.ErrNotFound)
	}
	registered := projects[i].CreatedAt
	all, err := p.DB.Runs(ctx, p.ID)
	if err != nil {
		return NorthStar{}, err
	}
	experiments, err := p.DB.Experiments(ctx, p.ID)
	if err != nil {
		return NorthStar{}, err
	}
	var first *firstVerdict
	for _, e := range experiments {
		if e.Lock == nil || e.Status != store.StatusDone { // only an experiment that ran to its end gives a verdict to count
			continue
		}
		found, err := decisiveOf(ctx, p, e)
		if err != nil {
			return NorthStar{}, err
		}
		if found != nil && (first == nil || found.finished.Before(first.finished)) {
			first = found
		}
	}
	var out NorthStar
	for _, r := range all {
		if first == nil || !r.Started.After(first.started) {
			out.SpentUSD += run.StoredSpend(r.CostUSD, r.Record).TotalUSD()
		}
	}
	if first != nil {
		out.Decisive, out.Experiment, out.Metric, out.Verdict = true, first.experiment, first.metric, first.verdict
		out.Seconds = max(first.finished.Sub(registered).Seconds(), 0)
	}
	return out, nil
}

// firstVerdict is an experiment's decisive verdict and when its last run started and ended.
type firstVerdict struct {
	experiment, metric, verdict string
	started, finished           time.Time
}

// decisiveOf analyzes a locked experiment as its report would and returns its decisive verdict, preferring the primary
// metric's, or nil when it has none (or no runs).
func decisiveOf(ctx context.Context, p experiment.Project, e store.Experiment) (*firstVerdict, error) {
	var lock experiment.Lock
	if err := json.Unmarshal(e.Lock, &lock); err != nil {
		return nil, fmt.Errorf("experiment %s: its lock cannot be read: %w", e.Name, err)
	}
	runs, err := p.DB.ExperimentRuns(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	if lock.Design.Template == experiment.TemplateAA {
		return nil, nil // a calibration measures noise: it answers nothing about a change
	}
	var data []experiment.RunData
	out := &firstVerdict{experiment: e.Name}
	for _, r := range runs {
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return nil, fmt.Errorf("run %s: %w", r.ID, err)
		}
		data = append(data, runData(r.Slot, rec))
		out.started = latest(out.started, r.Started)
		out.finished = latest(out.finished, r.Finished)
	}
	analysis, err := experiment.Analyze(lock, data)
	if err != nil {
		return nil, fmt.Errorf("experiment %s: %w", e.Name, err)
	}
	for _, role := range []string{experiment.RolePrimary, experiment.RoleGuard} {
		for _, res := range analysis.Results {
			if res.Role == role && Decisive(res.Verdict) {
				out.metric, out.verdict = res.Metric, res.Verdict
				return out, nil
			}
		}
	}
	return nil, nil
}

// runData is what the analysis reads of a run: the one mapping Build and the north star share, so the verdict counted
// is the one the experiment's report shows.
func runData(slot int, rec run.Record) experiment.RunData {
	return experiment.RunData{Slot: slot, Task: rec.Task, Arm: rec.Arm, Outcome: rec.Outcome, Passed: rec.Passed,
		ConfigChanged: rec.Behavior.ConfigChanged, CostUSD: rec.Spend().AgentUSD, DurationS: float64(rec.Metrics.DurationMS) / 1000,
		OutputTokens: float64(rec.Metrics.OutputTokens)}
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Line is the one-line summary `start` and the report show.
func (n NorthStar) Line() string {
	if !n.Decisive {
		return fmt.Sprintf("First decisive verdict: none yet ($%.2f spent since init)", n.SpentUSD)
	}
	return fmt.Sprintf("First decisive verdict: %s on %s in %s, %s after init, $%.2f spent up to it",
		n.Verdict, n.Metric, n.Experiment, formatSpan(time.Duration(n.Seconds*float64(time.Second))), n.SpentUSD)
}

// formatSpan writes d in its two largest units: "2d 3h", "3h 12m", "7m", "40s".
func formatSpan(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
