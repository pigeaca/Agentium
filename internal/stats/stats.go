// Package stats is the paired analysis of experiments (the study's §5.6), ported from the Phase 0 spike's statistics:
// the task is the unit, each task contributes the difference between its arm means, and intervals come from a
// two-stage cluster bootstrap (tasks, then runs within each task and arm) with a t-interval on the per-task differences
// as a cross-check.
package stats

import (
	"errors"
	"math"
	"slices"
)

// Table holds one metric's observations per task and arm. Tasks keep the order they were first added in, and each
// cell its observations' order: the bootstrap's draws depend on both.
type Table struct {
	tasks []string
	cells map[string]map[string][]float64
}

// NewTable returns an empty table.
func NewTable() *Table { return &Table{cells: map[string]map[string][]float64{}} }

// Add records an observation.
func (t *Table) Add(task, arm string, v float64) {
	if t.cells[task] == nil {
		t.cells[task] = map[string][]float64{}
		t.tasks = append(t.tasks, task)
	}
	t.cells[task][arm] = append(t.cells[task][arm], v)
}

// Tasks lists the tasks in the order they were first added.
func (t *Table) Tasks() []string { return slices.Clone(t.tasks) }

// Cell returns a task's observations in an arm.
func (t *Table) Cell(task, arm string) []float64 { return slices.Clone(t.cells[task][arm]) }

// Transform maps an observation before averaging: identity for success, log for cost and time.
type Transform func(float64) float64

// Identity is the identity transform.
func Identity(x float64) float64 { return x }

// paired lists the tasks with observations in both arms.
func (t *Table) paired(a, b string) []string {
	var out []string
	for _, task := range t.tasks {
		if len(t.cells[task][a]) > 0 && len(t.cells[task][b]) > 0 {
			out = append(out, task)
		}
	}
	return out
}

// Paired returns each task's difference of arm means, b − a, after transform, for tasks with both arms.
func (t *Table) Paired(a, b string, transform Transform) []float64 {
	var diffs []float64
	for _, task := range t.paired(a, b) {
		diffs = append(diffs, meanOf(t.cells[task][b], transform)-meanOf(t.cells[task][a], transform))
	}
	return diffs
}

func meanOf(values []float64, transform Transform) float64 {
	sum := 0.0
	for _, v := range values {
		sum += transform(v)
	}
	return sum / float64(len(values))
}

// Mean is the arithmetic mean.
func Mean(values []float64) float64 { return meanOf(values, Identity) }

// Variance is the sample variance (n − 1); NaN below two values.
func Variance(values []float64) float64 {
	if len(values) < 2 {
		return math.NaN()
	}
	m, sum := Mean(values), 0.0
	for _, v := range values {
		sum += (v - m) * (v - m)
	}
	return sum / float64(len(values)-1)
}

// Source picks uniformly in [0, n). *math/rand/v2.Rand is one.
type Source interface{ IntN(n int) int }

// ErrNoPairs is returned when no task has observations in both arms.
var ErrNoPairs = errors.New("no task has runs in both arms")

// Bootstrap is a two-stage cluster bootstrap of the mean paired difference: the estimate and the sorted statistics of
// its draws.
type Bootstrap struct {
	Estimate float64
	Draws    []float64 // sorted
}

// NewBootstrap draws the bootstrap: each draw resamples the tasks with replacement, then, within each sampled task,
// each arm's observations with replacement (b's first, then a's), and takes the mean of the tasks' differences. The
// order of choices matches the spike's, so a Python-compatible source reproduces its numbers.
func NewBootstrap(t *Table, a, b string, transform Transform, draws int, src Source) (Bootstrap, error) {
	tasks := t.paired(a, b)
	if len(tasks) == 0 {
		return Bootstrap{}, ErrNoPairs
	}
	boot := Bootstrap{Estimate: Mean(t.Paired(a, b, transform)), Draws: make([]float64, 0, draws)}
	sample := make([]string, len(tasks))
	diffs := make([]float64, len(tasks))
	resampled := func(values []float64) float64 {
		sum := 0.0
		for range values {
			sum += transform(values[src.IntN(len(values))])
		}
		return sum / float64(len(values))
	}
	for range draws {
		for i := range sample {
			sample[i] = tasks[src.IntN(len(tasks))]
		}
		for i, task := range sample {
			diffs[i] = resampled(t.cells[task][b])
			diffs[i] -= resampled(t.cells[task][a])
		}
		boot.Draws = append(boot.Draws, Mean(diffs))
	}
	slices.Sort(boot.Draws)
	return boot, nil
}

// Interval is an estimate with its interval.
type Interval struct {
	Estimate float64 `json:"estimate"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
}

// Map applies f (exp, for ratios) to every bound.
func (i Interval) Map(f func(float64) float64) Interval {
	return Interval{Estimate: f(i.Estimate), Low: f(i.Low), High: f(i.High)}
}

// Percentile returns the percentile interval at level (0.95), indexed as the spike did (for 10,000 draws at 95%: the
// 251st and the 9,750th). Indices are rounded, not truncated: (1 − 0.9)/2 × 10,000 is 499.99999999999994 in floating
// point.
func (b Bootstrap) Percentile(level float64) Interval {
	n := len(b.Draws)
	low := int(math.Round((1 - level) / 2 * float64(n)))
	high := int(math.Round((1+level)/2*float64(n))) - 1
	return Interval{Estimate: b.Estimate, Low: b.Draws[low], High: b.Draws[high]}
}

// TInterval is the mean of the per-task differences with its two-sided t-interval at level. With few tasks the
// percentile bootstrap runs narrow and the two-stage bootstrap (which counts within-cell noise twice) runs wide, so
// verdicts ask both.
func TInterval(diffs []float64, level float64) (Interval, error) {
	if len(diffs) < 2 {
		return Interval{}, errors.New("a t-interval needs at least two tasks")
	}
	center := Mean(diffs)
	half := TQuantile((1+level)/2, len(diffs)-1) * math.Sqrt(Variance(diffs)/float64(len(diffs)))
	return Interval{Estimate: center, Low: center - half, High: center + half}, nil
}

// WithinVariance is the pooled within-cell variance of one run (cells are task × arm), weighted by degrees of
// freedom; ok is false when no cell has two observations.
func WithinVariance(t *Table, transform Transform) (v float64, ok bool) {
	total, dof := 0.0, 0
	for _, task := range t.tasks {
		for _, values := range t.cells[task] {
			if len(values) > 1 {
				transformed := make([]float64, len(values))
				for i, x := range values {
					transformed[i] = transform(x)
				}
				total += Variance(transformed) * float64(len(values)-1)
				dof += len(values) - 1
			}
		}
	}
	if dof == 0 {
		return 0, false
	}
	return total / float64(dof), true
}

// Heterogeneity is τ², the spread of the true effect across tasks, by the method of moments: the variance of the
// paired differences less its sampling part (2·within/repeats), floored at zero.
func Heterogeneity(diffs []float64, within, repeats float64) float64 {
	if len(diffs) < 2 {
		return 0
	}
	return math.Max(0, Variance(diffs)-2*within/repeats)
}

// Normal quantiles used for planning (TestZConstants checks them against NormalQuantile): 80% power; 5% two-sided, or
// one-sided for a no-loss guard.
const (
	zPower    = 0.8416212335729143 // NormalQuantile(0.80)
	zTwoSided = 1.959963984540054  // NormalQuantile(0.975)
	zOneSided = 1.6448536269514722 // NormalQuantile(0.95)
)

var errNoTasks = errors.New("need at least two tasks and a positive target")

// MDE is the smallest paired difference n tasks × r runs per arm detect with 80% power at two-sided 5%, given τ² and
// the within-task variance of one run.
func MDE(tau2, within float64, repeats, tasks int) float64 {
	return (zTwoSided + zPower) * math.Sqrt((tau2+2*within/float64(repeats))/float64(tasks))
}

// GuardMargin is the smallest margin a no-loss guard certifies with 80% power at one-sided 5%.
func GuardMargin(tau2, within float64, repeats, tasks int) float64 {
	return (zOneSided + zPower) * math.Sqrt((tau2+2*within/float64(repeats))/float64(tasks))
}

// NonInferiorityTasks is how many tasks show "no loss beyond margin" with 80% power when there is no true difference.
func NonInferiorityTasks(tau2, within float64, repeats int, margin float64) int {
	z := zOneSided + zPower
	return int(math.Ceil(z * z * (tau2 + 2*within/float64(repeats)) / (margin * margin)))
}

// TasksToResolve estimates how many tasks would resolve an inconclusive result: an interval's half-width shrinks
// with the square root of the tasks, and detecting an effect of target with 80% power at two-sided 5% needs a 95%
// half-width of target·z(0.975)/(z(0.975) + z(0.8)). halfWidth is the widest observed 95% half-width at tasks. An
// interval already that narrow can still be inconclusive (an estimate between zero and the margin), so the answer is
// always more than tasks.
func TasksToResolve(tasks int, halfWidth, target float64) (int, error) {
	if tasks < 2 || halfWidth <= 0 || target <= 0 {
		return 0, errNoTasks
	}
	needed := target * zTwoSided / (zTwoSided + zPower)
	return max(tasks+1, int(math.Ceil(float64(tasks)*(halfWidth/needed)*(halfWidth/needed)))), nil
}
