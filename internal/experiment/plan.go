package experiment

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/stats"
)

// Planning defaults (the study's §5.6 and the Phase 0 results), until an A/A calibration measures a repository's own.
const (
	SigmaLogCost = 0.19 // per-run spread of log cost, measured in Phase 0
	WSuccess     = 0.20 // per-run variance of success, p(1−p) for tasks of mixed difficulty
	// TauLow and TauHigh bound the spread of the true effect across tasks, which Phase 0 left unresolved.
	TauLow  = 0.10
	TauHigh = 0.25
)

// Floors: below them a metric is exploratory and gets no verdict (the study's §5.6). MinRepeats is the success floor's
// runs per task and arm under every method, and the cost floor's under phase1-v1.
const (
	MinRepeats      = 3
	MinTasksCost    = 8
	MinTasksSuccess = 20
	// MinRepeatsCost is phase1-v2's cost floor: a task counts with one run in each arm. The seeded simulation in
	// internal/stats (TestOneRunCostVerdictsSimulation) gates it: 8–12 tasks × 1 run at σ = 0.19 and τ = 0.10–0.25 give
	// false differences at or below 6% and 95% intervals that cover the true effect at least 93% of the time.
	MinRepeatsCost = 1
)

// Floors are a method's floors: a metric gets a verdict only with at least Tasks tasks that each have at least Repeats
// counted runs in both arms.
type Floors struct {
	CostTasks, CostRepeats       int
	SuccessTasks, SuccessRepeats int
}

// FloorsFor returns the floors of a method. An experiment is analysed by the method it was locked under, so a
// phase1-v1 report keeps its three-run cost floor; an unknown method gets the strictest floors.
func FloorsFor(method string) Floors {
	f := Floors{CostTasks: MinTasksCost, CostRepeats: MinRepeats, SuccessTasks: MinTasksSuccess, SuccessRepeats: MinRepeats}
	if method == MethodV2 {
		f.CostRepeats = MinRepeatsCost
	}
	return f
}

// Metric returns a metric's floor: success has its own, and cost, time and output tokens share cost's.
func (f Floors) Metric(metric string) (tasks, repeats int) {
	if metric == MetricSuccess {
		return f.SuccessTasks, f.SuccessRepeats
	}
	return f.CostTasks, f.CostRepeats
}

// Tier is a preset experiment size.
type Tier struct {
	Name    string
	Tasks   int
	Repeats int
}

// Tiers are the preview's presets: Quick measures cost changes of roughly 15–20%; Confident also certifies that
// success does not drop by more than about 15–20 pp.
func Tiers() []Tier { return []Tier{{"Quick", 12, 3}, {"Confident", 23, 5}} }

// TierByName finds a tier, ignoring case.
func TierByName(name string) (Tier, bool) {
	tiers := Tiers()
	i := slices.IndexFunc(tiers, func(t Tier) bool { return strings.EqualFold(t.Name, name) })
	if i < 0 {
		return Tier{}, false
	}
	return tiers[i], true
}

// Detectable is what a paired design can detect with 80% power, at τ = TauLow and TauHigh. The variance of a task's
// difference between arm means is τ² + 2w/R (Miller's paired, clustered analysis), and n tasks divide it.
type Detectable struct {
	Cost    [2]float64 // the smallest cost reduction detectable (two-sided 5%), as a fraction: 0.14 is 14%
	Success [2]float64 // the smallest success difference detectable (two-sided 5%), as a fraction: 0.30 is 30 pp
	Guard   [2]float64 // the smallest success margin a no-loss guard can certify (one-sided 5%)
}

// Detect computes what tasks × repeats runs per arm can detect.
func Detect(tasks, repeats int) Detectable {
	var d Detectable
	if tasks < 1 || repeats < 1 {
		return d
	}
	for i, tau := range []float64{TauLow, TauHigh} {
		// Cost is compared as a ratio: the log difference, shown as the reduction it amounts to.
		d.Cost[i] = 1 - math.Exp(-stats.MDE(tau*tau, SigmaLogCost*SigmaLogCost, repeats, tasks))
		d.Success[i] = stats.MDE(tau*tau, WSuccess, repeats, tasks)
		d.Guard[i] = stats.GuardMargin(tau*tau, WSuccess, repeats, tasks)
	}
	return d
}

// Exploratory lists the metrics a design is too small to give verdicts on, by the floors of the method new experiments
// are locked under.
func Exploratory(tasks, repeats int) []string {
	var out []string
	for _, m := range []string{MetricCost, MetricSuccess} {
		if minTasks, minRepeats := FloorsFor(MethodVersion).Metric(m); tasks < minTasks || repeats < minRepeats {
			out = append(out, m)
		}
	}
	return out
}

// DefaultProfile is one task run's tokens as the study assumed them (§5.6): 0.2M cache writes, 2.16M cache reads,
// 0.04M uncached input and 30k output, with the cache written for an hour as Claude Code does.
func DefaultProfile() pricing.Usage {
	return pricing.Usage{CacheWrite1h: 200_000, CacheRead: 2_160_000, Input: 40_000, Output: 30_000}
}

// MinPastRuns is how many earlier runs make their median the estimate instead of the default profile.
const MinPastRuns = 3

// Estimate is the expected cost of one run.
type Estimate struct {
	PerRunUSD float64
	Known     bool
	Basis     string
}

// EstimateRun estimates a run on model: the median cost of the project's earlier fair task runs on that model when
// there are enough, else the default profile at list price, else unknown.
func EstimateRun(model string, past []float64) Estimate {
	if len(past) >= MinPastRuns {
		costs := slices.Sorted(slices.Values(past))
		median := costs[len(costs)/2]
		if len(costs)%2 == 0 {
			median = (costs[len(costs)/2-1] + median) / 2
		}
		return Estimate{PerRunUSD: median, Known: true, Basis: fmt.Sprintf("the median of this project's %d earlier task runs on %s", len(past), model)}
	}
	if rates, ok := pricing.Lookup(model); ok {
		return Estimate{PerRunUSD: rates.Cost(DefaultProfile()), Known: true,
			Basis: fmt.Sprintf("a default task run's tokens at %s's list prices of %s (fewer than %d earlier task runs on it)", model, pricing.Date, MinPastRuns)}
	}
	return Estimate{Basis: fmt.Sprintf("%s has no list price in Agentium's table and fewer than %d earlier task runs", model, MinPastRuns)}
}

// Reserve is what the budget must hold back for runs that may be in flight: a run (or a pair's two runs) starts only
// when the spend so far, the caps of the runs in flight and its own caps fit the budget, so spending never passes it.
// With concurrency c, at most c−1 runs are in flight when a pair's first run starts, so c+1 caps are reserved.
func Reserve(d Design) float64 { return float64(d.Concurrency+1) * d.RunBudgetUSD }

// DefaultBudget is a quarter above the estimate plus the reserve, in whole dollars; zero when the estimate is unknown.
func DefaultBudget(d Design, est Estimate) float64 {
	if !est.Known {
		return 0
	}
	return math.Ceil(1.25*float64(d.Runs())*est.PerRunUSD + Reserve(d))
}

// Row is one line of a preview.
type Row struct {
	Name        string
	Tasks       int
	Repeats     int
	Runs        int
	Short       bool    // fewer tasks are eligible than the tier asks for
	CostUSD     float64 // expected; zero when the estimate is unknown
	WorstUSD    float64 // every run at its cap
	Detect      Detectable
	Exploratory []string
}

// Preview sizes each tier, limited to the eligible tasks, and the design itself.
func Preview(d Design, eligible int, est Estimate) []Row {
	row := func(name string, tasks, repeats int) Row {
		runs := tasks * repeats * len(d.Arms)
		r := Row{Name: name, Tasks: tasks, Repeats: repeats, Runs: runs, WorstUSD: float64(runs) * d.RunBudgetUSD,
			Detect: Detect(tasks, repeats), Exploratory: Exploratory(tasks, repeats)}
		if est.Known {
			r.CostUSD = float64(runs) * est.PerRunUSD
		}
		return r
	}
	var rows []Row
	for _, t := range Tiers() {
		r := row(t.Name, min(t.Tasks, eligible), t.Repeats)
		r.Short = eligible < t.Tasks
		rows = append(rows, r)
	}
	return append(rows, row("This experiment", len(d.Tasks), d.Repeats))
}
