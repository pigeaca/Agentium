package experiment

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"

	"github.com/pigeaca/agentium/internal/stats"
)

// RunData is what the analysis reads from one stored run of an experiment. Runs come in the order they started: the
// bootstrap's draws depend on the order, so the same runs always give the same intervals.
type RunData struct {
	Slot          int
	Task          string
	Arm           string
	Outcome       string
	Passed        *bool
	ConfigChanged []string
	CostUSD       float64
	DurationS     float64 // the agent's run
	OutputTokens  float64
}

// Metrics compared between the arms.
const (
	MetricSuccess = "success"
	MetricCost    = "cost"
	MetricTime    = "time"
	MetricOutput  = "output_tokens"
)

// Roles: the goal's primary metric and the success guard get verdicts; the rest are exploratory.
const (
	RolePrimary   = "primary"
	RoleGuard     = "guard"
	RoleSecondary = "secondary"
)

// BootstrapDraws is the number of bootstrap draws per metric.
const BootstrapDraws = 10000

// MetricResult compares arm B with arm A on one metric: a difference (B − A) for success, a ratio of geometric means (B / A)
// for the others. Intervals are in those units.
type MetricResult struct {
	Metric  string `json:"metric"`
	Role    string `json:"role"`
	Ratio   bool   `json:"ratio"`
	Tasks   int    `json:"tasks"`   // tasks with counted runs in both arms
	Repeats int    `json:"repeats"` // the median, over those tasks, of the fewer counted runs of the two arms
	// FullTasks have at least FloorRepeats counted runs in both arms: the floor counts them, and asks for FloorTasks.
	FullTasks    int `json:"full_tasks"`
	FloorTasks   int `json:"floor_tasks"`
	FloorRepeats int `json:"floor_repeats"`
	// A and B are each arm's level: the success rate, or the geometric mean; nil when the arm has no value.
	A       *float64       `json:"a"`
	B       *float64       `json:"b"`
	Boot95  stats.Interval `json:"bootstrap_95"`
	T95     stats.Interval `json:"t_95"`
	Boot90  stats.Interval `json:"bootstrap_90"`
	T90     stats.Interval `json:"t_90"`
	Verdict string         `json:"verdict"`
	Warning string         `json:"warning,omitempty"` // a regression shown below the floors
	Note    string         `json:"note,omitempty"`    // why there is no result
	// TasksToResolve estimates, from the observed spread, how many tasks would resolve an inconclusive verdict.
	TasksToResolve int `json:"tasks_to_resolve,omitempty"`
}

// NoiseLevel is the confidence level of the noise components' ranges.
const NoiseLevel = 0.95

// Component is one noise component: its estimate, a range at NoiseLevel, and how both were estimated, in words.
type Component struct {
	Estimate float64 `json:"estimate"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
	Basis    string  `json:"basis"`
	// Bootstrap marks a range from a bootstrap over tasks, which runs narrow with few tasks: indicative, not a bound.
	Bootstrap bool `json:"bootstrap,omitempty"`
}

// normalOnly ends the basis of every chi-square range.
const normalOnly = "; it assumes normal noise, and heavier tails make it too narrow"

// Noise is the noise the runs show, for planning later experiments (the planner's defaults are SigmaLogCost, WSuccess
// and TauLow–TauHigh). A component the design cannot estimate is nil.
type Noise struct {
	Tasks   int     `json:"tasks"`   // tasks with cost in both arms
	Repeats float64 `json:"repeats"` // counted runs with a cost per task and arm, on average
	// Sigma is the per-run spread of log cost. An A/B with one run per arm cannot separate it from Tau: the paired
	// differences' variance is 2σ² + τ² there.
	Sigma *Component `json:"sigma_log_cost,omitempty"`
	// Tau is the spread of the true cost effect across tasks (log scale); an A/B's only, since an A/A has none.
	Tau *Component `json:"tau_log_cost,omitempty"`
	// W is the per-run variance of success, and TauSuccess its effect's spread across tasks (an A/B's); both only when
	// success varies.
	W             *Component `json:"w_success,omitempty"`
	TauSuccess    *Component `json:"tau_success,omitempty"`
	SuccessVaries bool       `json:"success_varies"` // the counted runs hold both successes and failures
}

// Analysis is an experiment's results.
type Analysis struct {
	Results           []MetricResult     `json:"results"`
	Counted           map[string]int     `json:"counted"`  // fair runs per arm
	Excluded          map[string]int     `json:"excluded"` // other runs per outcome
	PassAt1           map[string]float64 `json:"pass_at_1"`
	PassAll           map[string]float64 `json:"pass_all"` // pass^k: tasks whose every counted run succeeded
	NotDiscriminating []string           `json:"not_discriminating,omitempty"`
	Noise             *Noise             `json:"noise,omitempty"`
}

type metric struct {
	name  string
	ratio bool
	value func(RunData) (float64, bool)
}

// Analyze compares the experiment's arms (B against A) on its fair runs, as the lock's design declares: the goal's
// primary metric and the success guard get verdicts (stats.Decide) with the design's margins and the floors of the
// lock's method; the others are exploratory. A pass graded with changed runner configuration counts as a failure.
func Analyze(l Lock, runs []RunData) (Analysis, error) {
	if len(l.Design.Arms) != 2 {
		return Analysis{}, errors.New("analyze: the lock has no two arms")
	}
	a, b := l.Design.Arms[0].Name, l.Design.Arms[1].Name
	metrics := []metric{
		{MetricSuccess, false, func(r RunData) (float64, bool) {
			if Success(r.Outcome, r.Passed, r.ConfigChanged) {
				return 1, true
			}
			return 0, true
		}},
		{MetricCost, true, func(r RunData) (float64, bool) { return r.CostUSD, r.CostUSD > 0 }},
		{MetricTime, true, func(r RunData) (float64, bool) { return r.DurationS, r.DurationS > 0 }},
		{MetricOutput, true, func(r RunData) (float64, bool) { return r.OutputTokens, r.OutputTokens > 0 }},
	}
	floors := FloorsFor(l.Method)
	out := Analysis{Counted: map[string]int{}, Excluded: map[string]int{}, PassAt1: map[string]float64{}, PassAll: map[string]float64{}}
	tables := map[string]*stats.Table{}
	for _, m := range metrics {
		tables[m.name] = stats.NewTable()
	}
	for _, r := range runs {
		if !Fair(r.Outcome) {
			out.Excluded[r.Outcome]++
			continue
		}
		out.Counted[r.Arm]++
		for _, m := range metrics {
			if v, ok := m.value(r); ok {
				tables[m.name].Add(r.Task, r.Arm, v)
			}
		}
	}
	primary := MetricCost
	if l.Design.Goal == GoalBetter {
		primary = MetricSuccess
	}
	for i, m := range metrics {
		res := MetricResult{Metric: m.name, Role: RoleSecondary, Ratio: m.ratio}
		switch {
		case m.name == primary:
			res.Role = RolePrimary
		case m.name == MetricSuccess && l.Design.Goal == GoalCheaper:
			res.Role = RoleGuard
		}
		dir := stats.LowerIsBetter
		if m.name == MetricSuccess {
			dir = stats.HigherIsBetter
		}
		transform, back, margin := stats.Transform(stats.Identity), stats.Identity, stats.Symmetric(l.Design.SuccessMargin)
		if m.ratio {
			transform, back, margin = math.Log, math.Exp, stats.RatioMargin(l.Design.CostMargin, dir)
		}
		table := tables[m.name]
		res.A, res.B = level(table, a, transform, back), level(table, b, transform, back)
		diffs := table.Paired(a, b, transform)
		res.Tasks = len(diffs)
		res.FloorTasks, res.FloorRepeats = floors.Metric(m.name)
		res.Repeats, res.FullTasks = pairedRuns(table, a, b, res.FloorRepeats)
		if len(diffs) < 2 {
			res.Verdict, res.Note = stats.Exploratory, "fewer than two tasks have counted runs in both arms"
			out.Results = append(out.Results, res)
			continue
		}
		boot, err := stats.NewBootstrap(table, a, b, transform, BootstrapDraws, rand.New(rand.NewPCG(l.Design.Seed, uint64(3+i))))
		if err != nil {
			return Analysis{}, err
		}
		t95, err := stats.TInterval(diffs, 0.95)
		if err != nil {
			return Analysis{}, err
		}
		t90, _ := stats.TInterval(diffs, 0.90)
		evidence := stats.Evidence{Boot95: boot.Percentile(0.95), T95: t95, Boot90: boot.Percentile(0.90), T90: t90}
		res.Boot95, res.T95, res.Boot90, res.T90 = evidence.Boot95.Map(back), t95.Map(back), evidence.Boot90.Map(back), t90.Map(back)
		belowFloor := res.FullTasks < res.FloorTasks // enough tasks with the floor's runs per arm
		if res.Role == RoleSecondary {
			res.Verdict = stats.Exploratory // no verdict: one primary metric, and the guard
			out.Results = append(out.Results, res)
			continue
		}
		res.Verdict, res.Warning = stats.Decide(evidence, dir, margin, res.Role == RoleGuard, belowFloor)
		if res.Verdict == stats.Inconclusive {
			wide := math.Max(halfWidth(evidence.Boot95), halfWidth(evidence.T95))
			res.TasksToResolve, _ = stats.TasksToResolve(res.Tasks, wide, math.Min(margin.Better, margin.Worse))
		}
		out.Results = append(out.Results, res)
	}
	success := tables[MetricSuccess]
	for _, arm := range []string{a, b} {
		if rate := level(success, arm, stats.Identity, stats.Identity); rate != nil {
			out.PassAt1[arm] = *rate
			out.PassAll[arm], _ = stats.Consistency(success, arm)
		}
	}
	out.NotDiscriminating = stats.NotDiscriminating(success, a, b)
	out.Noise = noise(tables[MetricCost], success, a, b, l.Design)
	return out, nil
}

func halfWidth(i stats.Interval) float64 { return (i.High - i.Low) / 2 }

// level is an arm's level over all its counted runs: the mean after transform, mapped back (a geometric mean for
// log); nil without values (JSON has no NaN).
func level(t *stats.Table, arm string, transform, back stats.Transform) *float64 {
	sum, n := 0.0, 0
	for _, task := range t.Tasks() {
		for _, v := range t.Cell(task, arm) {
			sum += transform(v)
			n++
		}
	}
	if n == 0 {
		return nil
	}
	v := back(sum / float64(n))
	return &v
}

// pairedRuns is the median, over tasks with runs in both arms, of the fewer counted runs of the two arms, and how many
// such tasks have at least floor in both: a slot lost to failures lowers one task, not the whole metric.
func pairedRuns(t *stats.Table, a, b string, floor int) (median, full int) {
	var fewer []int
	for _, task := range t.Tasks() {
		if n := min(len(t.Cell(task, a)), len(t.Cell(task, b))); n > 0 {
			fewer = append(fewer, n)
			if n >= floor {
				full++
			}
		}
	}
	if len(fewer) == 0 {
		return 0, 0
	}
	slices.Sort(fewer)
	return fewer[len(fewer)/2], full
}

// noiseDraws is the number of task-level bootstrap draws for the success components' ranges.
const noiseDraws = 2000

// noise estimates the noise components with their ranges; nil without two tasks with cost in both arms.
//   - σ (log cost): pooled within task and arm when runs repeat, with a chi-square range. With one run per arm, an A/A
//     takes it from the paired differences (var(d) = 2σ², as τ = 0), and an A/B cannot separate it from τ.
//   - τ (log cost, an A/B's): var(d) − 2σ²/R, floored at zero, bounded over both variances' ranges; with one run per
//     arm, σ is taken as the planner's SigmaLogCost.
//   - w and τ for success, when success varies: the same estimates, with ranges from a bootstrap over tasks (success is
//     not normal), which run narrow with few tasks.
func noise(cost, success *stats.Table, a, b string, d Design) *Noise {
	diffs := cost.Paired(a, b, math.Log)
	if len(diffs) < 2 {
		return nil
	}
	aa := d.Template == TemplateAA
	n := &Noise{Tasks: len(diffs), Repeats: meanRuns(cost, a, b)}
	varD, dfD := stats.Variance(diffs), len(diffs)-1
	s2, dfW := stats.PooledWithin(cost, math.Log)
	sqrt := func(low, high float64) (float64, float64) { return math.Sqrt(low), math.Sqrt(high) }
	switch {
	case dfW > 0:
		low, high := sqrt(stats.VarianceInterval(s2, dfW, NoiseLevel))
		n.Sigma = &Component{Estimate: math.Sqrt(s2), Low: low, High: high,
			Basis: fmt.Sprintf("the pooled spread of runs within each task and arm, on %d degrees of freedom; chi-square range"+normalOnly, dfW)}
	case aa:
		low, high := stats.VarianceInterval(varD, dfD, NoiseLevel)
		n.Sigma = &Component{Estimate: math.Sqrt(varD / 2), Low: math.Sqrt(low / 2), High: math.Sqrt(high / 2),
			Basis: fmt.Sprintf("the paired differences' spread over √2: with one run per arm var(d) = 2σ² + τ², and τ = 0 in an A/A; chi-square range on %d degrees of freedom"+normalOnly, dfD)}
	}
	if !aa {
		if dfW > 0 {
			low, high := sqrt(stats.HeterogeneityRange(varD, dfD, s2, dfW, n.Repeats, NoiseLevel))
			n.Tau = &Component{Estimate: math.Sqrt(stats.Heterogeneity(diffs, s2, n.Repeats)), Low: low, High: high,
				Basis: "var(d) − 2σ²/R, floored at zero; the range spans both variances' chi-square ranges at 97.5%, so it holds with at least 95%" + normalOnly}
		} else {
			assumed := SigmaLogCost * SigmaLogCost
			low, high := sqrt(stats.HeterogeneityRange(varD, dfD, assumed, 0, 1, NoiseLevel))
			n.Tau = &Component{Estimate: math.Sqrt(stats.Heterogeneity(diffs, assumed, 1)), Low: low, High: high,
				Basis: fmt.Sprintf("var(d) − 2σ², floored at zero, taking σ = %.2f (the planner's default): one run per arm cannot separate σ from τ; chi-square range of var(d) on %d degrees of freedom"+normalOnly, SigmaLogCost, dfD)}
		}
	}
	if n.SuccessVaries = varies(success, a, b); !n.SuccessVaries {
		return n
	}
	src := rand.New(rand.NewPCG(d.Seed, 20)) // streams 3–6 are the metrics' bootstraps
	within := func(t *stats.Table) float64 { v, _ := stats.PooledWithin(t, stats.Identity); return v }
	sw, dfSW := stats.PooledWithin(success, stats.Identity)
	boot := fmt.Sprintf("range from a bootstrap over tasks (%d draws), which runs narrow with few tasks", noiseDraws)
	switch {
	case dfSW > 0:
		low, high := stats.BootstrapRange(success, within, noiseDraws, NoiseLevel, src)
		n.W = &Component{Estimate: sw, Low: low, High: high, Basis: "the pooled variance of runs within each task and arm; " + boot, Bootstrap: true}
		if !aa {
			repeats := meanRuns(success, a, b)
			tau := func(t *stats.Table) float64 {
				return math.Sqrt(stats.Heterogeneity(t.Paired(a, b, stats.Identity), within(t), repeats))
			}
			low, high := stats.BootstrapRange(success, tau, noiseDraws, NoiseLevel, src)
			n.TauSuccess = &Component{Estimate: tau(success), Low: low, High: high, Basis: "var(d) − 2w/R, floored at zero; " + boot, Bootstrap: true}
		}
	case aa && len(success.Paired(a, b, stats.Identity)) >= 2:
		half := func(t *stats.Table) float64 { return stats.Variance(t.Paired(a, b, stats.Identity)) / 2 }
		low, high := stats.BootstrapRange(success, half, noiseDraws, NoiseLevel, src)
		n.W = &Component{Estimate: half(success), Low: low, High: high,
			Basis: "half the paired differences' variance, as τ = 0 in an A/A; " + boot, Bootstrap: true}
	}
	return n
}

// meanRuns is the counted runs per task and arm, on average over the cells with any.
func meanRuns(t *stats.Table, a, b string) float64 {
	cells, runs := 0, 0
	for _, task := range t.Tasks() {
		for _, arm := range []string{a, b} {
			if n := len(t.Cell(task, arm)); n > 0 {
				cells++
				runs += n
			}
		}
	}
	return float64(runs) / float64(cells)
}

// varies reports whether the arms' counted runs hold both successes and failures.
func varies(t *stats.Table, a, b string) bool {
	seen := map[float64]bool{}
	for _, task := range t.Tasks() {
		for _, v := range append(t.Cell(task, a), t.Cell(task, b)...) {
			seen[v] = true
		}
	}
	return len(seen) > 1
}
