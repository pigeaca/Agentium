package stats

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

// Noise components come with ranges. The ranges for variances assume normal noise (log cost is close to it); they are
// wide with few tasks, which is the honest answer, not a defect.

// PooledWithin is the pooled within-cell variance of one run (cells are task × arm), weighted by degrees of freedom,
// and those degrees of freedom: zero when no cell has two observations.
func PooledWithin(t *Table, transform Transform) (v float64, dof int) {
	total := 0.0
	for _, task := range t.tasks {
		for _, arm := range slices.Sorted(maps.Keys(t.cells[task])) { // a fixed order: the same sum every time
			if values := t.cells[task][arm]; len(values) > 1 {
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
		return 0, 0
	}
	return total / float64(dof), dof
}

// ChiSquareQuantile is the chi-square distribution's quantile function with df degrees of freedom, found by bisection
// on the distribution function.
func ChiSquareQuantile(p float64, df int) float64 {
	if df < 1 || math.IsNaN(p) || p <= 0 || p >= 1 {
		return math.NaN()
	}
	k := float64(df)
	lo, hi := 0.0, k+1
	for lowerGamma(k/2, hi/2) < p {
		hi *= 2
	}
	for range 200 {
		mid := (lo + hi) / 2
		if lowerGamma(k/2, mid/2) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// lowerGamma is the regularized lower incomplete gamma function P(a, x): its series below a + 1, else one minus the
// continued fraction of the upper function (Lentz's method), where each converges fast.
func lowerGamma(a, x float64) float64 {
	if x <= 0 {
		return 0
	}
	lga, _ := math.Lgamma(a)
	front := math.Exp(-x + a*math.Log(x) - lga)
	if x < a+1 {
		sum, term := 1/a, 1/a
		for n := 1; n < 1000; n++ {
			term *= x / (a + float64(n))
			sum += term
			if math.Abs(term) < math.Abs(sum)*1e-16 {
				break
			}
		}
		return front * sum
	}
	const tiny = 1e-300
	b := x + 1 - a
	c, d := 1/tiny, 1/b
	h := d
	for n := 1; n < 1000; n++ {
		an := -float64(n) * (float64(n) - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = b + an/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		if math.Abs(d*c-1) < 1e-16 {
			break
		}
	}
	return 1 - front*h
}

// VarianceInterval is the two-sided chi-square interval at level of a normal variance estimated as s2 on df degrees of
// freedom: [df·s2/χ²((1+level)/2), df·s2/χ²((1−level)/2)].
func VarianceInterval(s2 float64, df int, level float64) (low, high float64) {
	k := float64(df)
	return k * s2 / ChiSquareQuantile((1+level)/2, df), k * s2 / ChiSquareQuantile((1-level)/2, df)
}

// HeterogeneityRange bounds τ² = var(d) − 2·within/repeats (floored at zero), where var(d) is the paired differences'
// variance on dfD degrees of freedom and within the per-run variance on dfW. Each variance gets its chi-square interval
// at 1 − (1 − level)/2, so both hold together with probability at least level (Bonferroni), and τ² is bounded over
// that rectangle. dfW = 0 takes within as known (assumed, not measured): then var(d)'s interval is at level.
func HeterogeneityRange(varD float64, dfD int, within float64, dfW int, repeats, level float64) (low, high float64) {
	each := 1 - (1-level)/2
	wLow, wHigh := within, within
	if dfW > 0 {
		wLow, wHigh = VarianceInterval(within, dfW, each)
	} else {
		each = level
	}
	dLow, dHigh := VarianceInterval(varD, dfD, each)
	return math.Max(0, dLow-2*wHigh/repeats), math.Max(0, dHigh-2*wLow/repeats)
}

// Resample returns a table of the tasks drawn with replacement (each draw a task of its own, so a task drawn twice
// counts twice): one draw of a task-level bootstrap of any statistic of the table.
func Resample(t *Table, src Source) *Table {
	out := NewTable()
	for i := range t.tasks {
		task := t.tasks[src.IntN(len(t.tasks))]
		name := fmt.Sprintf("%d\x00%s", i, task)
		out.tasks = append(out.tasks, name)
		out.cells[name] = t.cells[task] // shared, read-only
	}
	return out
}

// BootstrapRange is the percentile range at level of stat over draws task-level resamples of t.
func BootstrapRange(t *Table, stat func(*Table) float64, draws int, level float64, src Source) (low, high float64) {
	values := make([]float64, 0, draws)
	for range draws {
		values = append(values, stat(Resample(t, src)))
	}
	slices.Sort(values)
	b := Bootstrap{Draws: values}
	i := b.Percentile(level)
	return i.Low, i.High
}
