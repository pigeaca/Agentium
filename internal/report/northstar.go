package report

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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
// noise and answers nothing about a change. Nor does an experiment with judge-graded tasks, until the paid real check
// validates the judge (.agents/plans/2026-10-01-ticket-tasks.md, step 4): its cost verdict has no correctness guard from
// tests over those tasks. LeftOut names such experiments, so the line says why they are not counted.
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
	// LeftOut lists the finished experiments the north star leaves out for a reason it states (judge-graded tasks), in
	// stored order; absent when none.
	LeftOut []LeftOut `json:"left_out,omitempty"`
}

// LeftOut is a finished experiment the north star does not count, and why, in words.
type LeftOut struct {
	Experiment string `json:"experiment"`
	Why        string `json:"why"`
}

// whyJudgeGraded is why the north star leaves out an experiment with judge-graded tasks.
const whyJudgeGraded = "it has judge-graded tasks, whose grades are unvalidated until the judge's paid real check"

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
	var leftOut []LeftOut
	for _, e := range experiments {
		if e.Lock == nil || e.Status != store.StatusDone { // only an experiment that ran to its end gives a verdict to count
			continue
		}
		found, why, err := decisiveOf(ctx, p, e)
		if err != nil {
			return NorthStar{}, err
		}
		if why != "" {
			leftOut = append(leftOut, LeftOut{Experiment: e.Name, Why: why})
			continue
		}
		if found != nil && (first == nil || found.finished.Before(first.finished)) {
			first = found
		}
	}
	out := NorthStar{LeftOut: leftOut}
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
// metric's, or nil when it has none (or no runs). An experiment the north star leaves out whatever its verdict (one
// with judge-graded tasks) gives no verdict and why, in words.
func decisiveOf(ctx context.Context, p experiment.Project, e store.Experiment) (*firstVerdict, string, error) {
	var lock experiment.Lock
	if err := json.Unmarshal(e.Lock, &lock); err != nil {
		return nil, "", fmt.Errorf("experiment %s: its lock cannot be read: %w", e.Name, err)
	}
	if lock.Design.Template == experiment.TemplateAA {
		return nil, "", nil // a calibration measures noise: it answers nothing about a change
	}
	if lock.JudgeGraded() {
		return nil, whyJudgeGraded, nil
	}
	runs, err := p.DB.ExperimentRuns(ctx, e.ID)
	if err != nil {
		return nil, "", err
	}
	if len(runs) == 0 {
		return nil, "", nil
	}
	var data []experiment.RunData
	out := &firstVerdict{experiment: e.Name}
	for _, r := range runs {
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return nil, "", fmt.Errorf("run %s: %w", r.ID, err)
		}
		data = append(data, experiment.RunDataOf(r.Slot, rec))
		out.started = latest(out.started, r.Started)
		out.finished = latest(out.finished, r.Finished)
	}
	analysis, err := experiment.Analyze(lock, data)
	if err != nil {
		return nil, "", fmt.Errorf("experiment %s: %w", e.Name, err)
	}
	for _, role := range []string{experiment.RolePrimary, experiment.RoleGuard} {
		for _, res := range analysis.Results {
			if res.Role == role && Decisive(res.Verdict) {
				out.metric, out.verdict = res.Metric, res.Verdict
				return out, "", nil
			}
		}
	}
	return nil, "", nil
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Line is the one-line summary `start` and the report show, with what it left out and why: "First decisive verdict:
// none yet ($4.20 spent since init); left out: tickets (it has judge-graded tasks, …)".
func (n NorthStar) Line() string {
	line := fmt.Sprintf("First decisive verdict: none yet ($%.2f spent since init)", n.SpentUSD)
	if n.Decisive {
		line = fmt.Sprintf("First decisive verdict: %s on %s in %s, %s after init, $%.2f spent up to it",
			n.Verdict, n.Metric, n.Experiment, formatSpan(time.Duration(n.Seconds*float64(time.Second))), n.SpentUSD)
	}
	if len(n.LeftOut) > 0 {
		var parts []string
		for _, l := range n.LeftOut {
			parts = append(parts, fmt.Sprintf("%s (%s)", l.Experiment, l.Why))
		}
		line += "; left out: " + strings.Join(parts, ", ")
	}
	return line
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
