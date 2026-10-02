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

// MinPastRuns is how many earlier task runs on a model make their median the estimate for a task without runs of its
// own, instead of the default profile.
const MinPastRuns = 3

// PastRun is an earlier fair task run on a model that reported its cost: what estimates learn from.
type PastRun struct {
	// Task is the task the run ran, or empty when the run no longer belongs to one (its task was removed, or changed
	// after an experiment locked it): such a run counts only toward the project's median.
	Task    string
	CostUSD float64
}

// TaskCost is a task's own estimate: the median cost of its Runs earlier runs on the model.
type TaskCost struct {
	PerRunUSD float64
	Runs      int
}

// Estimate is the expected cost of runs on a model. Tasks differ in cost far more than one task's runs do (a run's log
// cost spreads by about σ = 0.19, while the 16-run A/B's tasks ranged from about $0.4 to $2 a run), so a task's own
// earlier runs predict it best, even one of them. A task without any falls back to PerRunUSD.
type Estimate struct {
	PerRunUSD float64             // a run of a task without runs of its own: the project's median, or the default profile
	Known     bool                // PerRunUSD is known
	Basis     string              // how PerRunUSD was estimated, in words
	Tasks     map[string]TaskCost // the tasks with earlier runs of their own on the model
}

// EstimateRun estimates runs on model from the project's earlier fair task runs on it. A task with runs of its own is
// estimated by their median. Any other task gets the median of all of them when there are at least MinPastRuns, else
// the default profile at list price, else no estimate.
func EstimateRun(model string, past []PastRun) Estimate { return EstimateRunAt(model, "", past, 0) }

// EstimateRunAt is EstimateRun for runs at an effort level, when the earlier runs include some of unknown effort (made
// before runs recorded theirs): unrecorded of past are such, and the basis says so. An empty effort is not named.
func EstimateRunAt(model, effort string, past []PastRun, unrecorded int) Estimate {
	label := model
	if effort != "" {
		label += " at effort " + effort
	}
	est := Estimate{Tasks: map[string]TaskCost{}}
	byTask := map[string][]float64{}
	var all []float64
	for _, r := range past {
		if r.Task != "" {
			byTask[r.Task] = append(byTask[r.Task], r.CostUSD)
		}
		all = append(all, r.CostUSD)
	}
	for task, costs := range byTask {
		est.Tasks[task] = TaskCost{PerRunUSD: median(costs), Runs: len(costs)}
	}
	switch rates, priced := pricing.Lookup(model); {
	case len(all) >= MinPastRuns:
		est.PerRunUSD, est.Known = median(all), true
		est.Basis = fmt.Sprintf("the median of this project's %d earlier task runs on %s", len(all), label)
		if unrecorded > 0 {
			est.Basis += fmt.Sprintf(" (%d of them from before runs recorded their effort, so at an unknown one)", unrecorded)
		}
	case priced:
		est.PerRunUSD, est.Known = rates.Cost(DefaultProfile()), true
		est.Basis = fmt.Sprintf("a default task run's tokens at %s's list prices of %s (fewer than %d earlier task runs on %s)", model, pricing.Date, MinPastRuns, label)
	default:
		est.Basis = fmt.Sprintf("%s has no list price in Agentium's table and fewer than %d earlier task runs", label, MinPastRuns)
	}
	return est
}

func median(values []float64) float64 {
	sorted := slices.Sorted(slices.Values(values))
	m := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		m = (sorted[len(sorted)/2-1] + m) / 2
	}
	return m
}

// TaskUSD is one run of task: its own estimate when it has earlier runs, else PerRunUSD; false when neither is known.
func (e Estimate) TaskUSD(task string) (float64, bool) {
	if own, ok := e.Tasks[task]; ok {
		return own.PerRunUSD, true
	}
	return e.PerRunUSD, e.Known
}

// DesignUSD is the expected cost of every run of d: each task's runs in every arm at its own estimate. False when a
// task has no estimate.
func (e Estimate) DesignUSD(d Design) (float64, bool) { return ArmEstimates{e, e}.DesignUSD(d) }

// ArmEstimates are the two arms' estimates, each on its own model: the same value twice for a context experiment.
type ArmEstimates [2]Estimate

// Same returns the estimates of one profile in both arms.
func Same(e Estimate) ArmEstimates { return ArmEstimates{e, e} }

// DesignUSD is the expected cost of every run of d: each task's runs in each arm at that arm's estimate. False when a
// task has no estimate in an arm.
func (e ArmEstimates) DesignUSD(d Design) (float64, bool) {
	total := 0.0
	for _, t := range d.Tasks {
		pair := 0.0 // a repeat's two runs: a shared estimate gives 2p, so the sum is p × 2R as it always was
		for _, arm := range e {
			perRun, ok := arm.TaskUSD(t)
			if !ok {
				return 0, false
			}
			pair += perRun
		}
		total += pair * float64(d.Repeats)
	}
	return total, true
}

// MeanUSD is the pair's average cost over tasks: the arms' means added (a task's run in each arm), which is twice the
// mean when the arms share an estimate. False when either arm has a task without an estimate.
func (e ArmEstimates) MeanUSD(tasks []string) (float64, bool) {
	total := 0.0
	for _, arm := range e {
		mean, ok := arm.MeanUSD(tasks)
		if !ok {
			return 0, false
		}
		total += mean
	}
	return total, true
}

// MeanUSD is the average run over tasks, which is what a run of a task drawn from them is expected to cost; PerRunUSD
// without tasks. False when a task has no estimate.
func (e Estimate) MeanUSD(tasks []string) (float64, bool) {
	if len(tasks) == 0 {
		return e.PerRunUSD, e.Known
	}
	total := 0.0
	for _, t := range tasks {
		perRun, ok := e.TaskUSD(t)
		if !ok {
			return 0, false
		}
		total += perRun
	}
	return total / float64(len(tasks)), true
}

// Reserve is what the budget must hold back for runs that may be in flight: a run (or a pair's two runs) starts only
// when the spend so far, the caps of the runs in flight and its own caps fit the budget, so spending never passes it.
// With concurrency c, at most c−1 runs are in flight when a pair's first run starts, so c+1 caps are reserved. A run's
// cap includes its judgement's (Design.RunCapUSD).
func Reserve(d Design) float64 { return float64(d.Concurrency+1) * d.RunCapUSD() }

// DefaultBudget is a quarter above the estimate (the judge's included) plus the reserve, in whole dollars; zero when
// the estimate is unknown.
func DefaultBudget(d Design, est Estimate) float64 { return DefaultBudgetFor(d, Same(est)) }

// DefaultBudgetFor is DefaultBudget with each arm's own estimate.
func DefaultBudgetFor(d Design, est ArmEstimates) float64 {
	expected, ok := est.DesignUSD(d)
	if !ok {
		return 0
	}
	return math.Ceil(1.25*(expected+d.JudgeEstimateUSD()) + Reserve(d))
}

// Row is one line of a preview.
type Row struct {
	Name        string
	Tasks       int
	Repeats     int
	Runs        int
	Short       bool    // fewer tasks are eligible than the tier asks for
	CostUSD     float64 // the agent's expected cost; zero when the estimate is unknown
	CostKnown   bool    // every task the row may hold has an estimate
	JudgeUSD    float64 // the judge's expected cost at the pilot's figure (Design.JudgeEstimateUSD); zero without it
	WorstUSD    float64 // every run, and its judgement, at its cap
	Detect      Detectable
	Exploratory []string
}

// Preview sizes each tier, limited to the eligible tasks, and the design itself. The design's cost is its own tasks'
// estimates; a tier, which would draw its tasks from the eligible ones, costs their average run. The judge's estimate
// is kept apart from the agent's (Row.JudgeUSD).
func Preview(d Design, eligible []string, est Estimate) []Row {
	return PreviewFor(d, eligible, Same(est))
}

// PreviewFor is Preview with each arm's own estimate and cap: a pair costs the arms' estimates added, and its worst
// case is the arms' caps added.
func PreviewFor(d Design, eligible []string, est ArmEstimates) []Row {
	row := func(name string, tasks, repeats int, pair float64, known bool) Row {
		runs := tasks * repeats * len(d.Arms)
		sized := d
		sized.Tasks, sized.Repeats = make([]string, tasks), repeats
		r := Row{Name: name, Tasks: tasks, Repeats: repeats, Runs: runs, WorstUSD: float64(tasks*repeats) * d.PairCapUSD(),
			JudgeUSD: sized.JudgeEstimateUSD(), Detect: Detect(tasks, repeats), Exploratory: Exploratory(tasks, repeats), CostKnown: known}
		if known {
			r.CostUSD = float64(tasks*repeats) * pair
		}
		return r
	}
	mean, known := est.MeanUSD(eligible)
	var rows []Row
	for _, t := range Tiers() {
		r := row(t.Name, min(t.Tasks, len(eligible)), t.Repeats, mean, known)
		r.Short = len(eligible) < t.Tasks
		rows = append(rows, r)
	}
	own := row("This experiment", len(d.Tasks), d.Repeats, 0, false)
	own.CostUSD, own.CostKnown = est.DesignUSD(d)
	return append(rows, own)
}
