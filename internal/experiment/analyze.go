package experiment

import (
	"errors"
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
	// FullTasks have at least MinRepeats counted runs in both arms: the floors count them.
	FullTasks int `json:"full_tasks"`
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

// Variance holds the components an A/A calibration measures for later plans.
type Variance struct {
	SigmaLogCost float64 `json:"sigma_log_cost"`
	TauLogCost   float64 `json:"tau_log_cost"`
	WSuccess     float64 `json:"w_success"`
	TauSuccess   float64 `json:"tau_success"`
	Repeats      float64 `json:"repeats"` // counted runs per task and arm, on average
}

// Analysis is an experiment's results.
type Analysis struct {
	Results           []MetricResult     `json:"results"`
	Counted           map[string]int     `json:"counted"`  // fair runs per arm
	Excluded          map[string]int     `json:"excluded"` // other runs per outcome
	PassAt1           map[string]float64 `json:"pass_at_1"`
	PassAll           map[string]float64 `json:"pass_all"` // pass^k: tasks whose every counted run succeeded
	NotDiscriminating []string           `json:"not_discriminating,omitempty"`
	Variance          *Variance          `json:"variance,omitempty"`
}

type metric struct {
	name    string
	ratio   bool
	value   func(RunData) (float64, bool)
	minTask int
}

// Analyze compares the experiment's arms (B against A) on its fair runs, as the lock's design declares: the goal's
// primary metric and the success guard get verdicts (stats.Decide) with the design's margins and the floors; the
// others are exploratory. A pass graded with changed runner configuration counts as a failure.
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
		}, MinTasksSuccess},
		{MetricCost, true, func(r RunData) (float64, bool) { return r.CostUSD, r.CostUSD > 0 }, MinTasksCost},
		{MetricTime, true, func(r RunData) (float64, bool) { return r.DurationS, r.DurationS > 0 }, MinTasksCost},
		{MetricOutput, true, func(r RunData) (float64, bool) { return r.OutputTokens, r.OutputTokens > 0 }, MinTasksCost},
	}
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
		res.Repeats, res.FullTasks = pairedRuns(table, a, b)
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
		belowFloor := res.FullTasks < m.minTask // "at least 3 runs per task per arm", on enough tasks
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
	if v, ok := variance(tables[MetricCost], success, a, b); ok {
		out.Variance = &v
	}
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
// such tasks have at least MinRepeats in both: a slot lost to failures lowers one task, not the whole metric.
func pairedRuns(t *stats.Table, a, b string) (median, full int) {
	var fewer []int
	for _, task := range t.Tasks() {
		if n := min(len(t.Cell(task, a)), len(t.Cell(task, b))); n > 0 {
			fewer = append(fewer, n)
			if n >= MinRepeats {
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

// variance estimates σ (log cost), w (success) and both τ from the runs, as an A/A calibration reports them.
func variance(cost, success *stats.Table, a, b string) (Variance, bool) {
	sigma2, okCost := stats.WithinVariance(cost, math.Log)
	w, okSuccess := stats.WithinVariance(success, stats.Identity)
	if !okCost || !okSuccess {
		return Variance{}, false
	}
	cells, runs := 0, 0
	for _, task := range cost.Tasks() {
		for _, arm := range []string{a, b} {
			if n := len(cost.Cell(task, arm)); n > 0 {
				cells++
				runs += n
			}
		}
	}
	repeats := float64(runs) / float64(cells)
	return Variance{SigmaLogCost: math.Sqrt(sigma2), WSuccess: w, Repeats: repeats,
		TauLogCost: math.Sqrt(stats.Heterogeneity(cost.Paired(a, b, math.Log), sigma2, repeats)),
		TauSuccess: math.Sqrt(stats.Heterogeneity(success.Paired(a, b, stats.Identity), w, repeats))}, true
}
