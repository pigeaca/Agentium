package stats

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
)

// Group-sequential designs (the wave-3 statistics note, docs/research/2026-10-02-wave3-statistics-note.md, §3): an
// experiment is analysed at looks after growing numbers of tasks, and stops at the first look whose intervals give a
// verdict. Each look's intervals are wider than a fixed design's, by O'Brien–Fleming-type Lan–DeMets spending, so the
// chance of a false difference over all looks stays at the design's alpha.

// SpendingOBF names Lan and DeMets' O'Brien–Fleming-type spending function (OBFSpending), as locks record it.
const SpendingOBF = "lan-demets-obrien-fleming"

// The parameters of method seq-v1 (the note's §3; the user's decisions of 2026-10-02). TestGroupSequentialFalseVerdicts
// is the trust guard's evidence for exactly these values.
const (
	// SeqAlpha is the two-sided error spent on efficacy (improved, regressed). At 4.5% arm-specific noise shapes gave
	// 5.66% false differences; at 3.5% every simulated null group keeps its Wilson upper bound at or under 5%.
	SeqAlpha = 0.035
	// SeqEquivalenceAlpha is the one-sided error each of equivalence's two tests spends.
	SeqEquivalenceAlpha = 0.05
	// SeqFutility is the conditional power below which an interim look without a verdict stops (non-binding).
	SeqFutility = 0.10
	// SeqMaxTasks is the most tasks a seq-v1 design takes, one run per arm each.
	SeqMaxTasks = 16
	// SeqFirstLook is the first look's task count, the cost floor: no look comes before it.
	SeqFirstLook = 8
)

// SeqLooks is where a seq-v1 design of tasks tasks looks: after 8, 12 and 16 tasks; with 12 to 15 tasks after 8 and
// all; with fewer, once after all (a fixed design at the sequential level, below the floor under 8 tasks).
func SeqLooks(tasks int) []int {
	switch {
	case tasks >= SeqMaxTasks:
		return []int{SeqFirstLook, 12, SeqMaxTasks}
	case tasks >= 12:
		return []int{SeqFirstLook, tasks}
	}
	return []int{tasks}
}

func normalCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }
func normalPDF(x float64) float64 { return math.Exp(-x*x/2) / math.Sqrt(2*math.Pi) }

// OBFSpending is Lan and DeMets' O'Brien–Fleming-type spending function: the one-sided error, of alpha in all, spent
// by information fraction t: 2 − 2Φ(z(1 − alpha/2)/√t). It spends nothing at t ≤ 0 and alpha at t ≥ 1.
func OBFSpending(alpha, t float64) float64 {
	switch {
	case t <= 0:
		return 0
	case t >= 1:
		return alpha
	}
	return 2 - 2*normalCDF(NormalQuantile(1-alpha/2)/math.Sqrt(t))
}

// errLooks is SequentialBounds' refusal of its input.
var errLooks = errors.New("sequential bounds: information times must increase from above 0, and the error spent must increase within [0, 1)")

// SequentialBounds returns the z-boundaries of looks at information times (strictly increasing, above 0) such that a
// standard Brownian motion, observed at those times as Z_k = B(t_k)/√t_k, first crosses by look k with probability
// spent[k] (cumulative): |Z| ≥ c when twoSided (spent is then the two-sided total), else Z ≥ c. Each look's boundary
// depends only on the looks up to it, so the boundaries of looks already used stay fixed when later ones change. It
// integrates the continuation region's sub-density numerically (Armitage, McPherson and Rowe's recursion, Simpson's
// rule on the B scale).
//
// Times are information over the planned maximum, so they may end below 1 when tasks are lost: the last look can
// still spend the remainder (spent = alpha), and the recursion uses the information actually observed.
func SequentialBounds(times, spent []float64, twoSided bool) ([]float64, error) {
	if len(times) == 0 || len(times) != len(spent) {
		return nil, errLooks
	}
	for k := range times {
		if times[k] <= 0 || spent[k] < 0 || spent[k] >= 1 || k > 0 && (times[k] <= times[k-1] || spent[k] < spent[k-1]) {
			return nil, errLooks
		}
	}
	const m = 1201 // grid points, odd for Simpson's rule
	bounds := make([]float64, len(times))
	var xs, ws []float64 // the continuation region's grid and its sub-density times the quadrature weights
	prevT, prevSpent := 0.0, 0.0
	for k, t := range times {
		target := spent[k] - prevSpent
		sd := math.Sqrt(t - prevT)
		cross := func(c float64) float64 {
			top := c * math.Sqrt(t)
			if k == 0 {
				p := 1 - normalCDF(c)
				if twoSided {
					p *= 2
				}
				return p
			}
			sum := 0.0
			for j, u := range xs {
				p := 1 - normalCDF((top-u)/sd)
				if twoSided {
					p += normalCDF((-top - u) / sd)
				}
				sum += ws[j] * p
			}
			return sum
		}
		lo, hi := 0.0, 15.0
		for range 100 {
			if mid := (lo + hi) / 2; cross(mid) > target {
				lo = mid
			} else {
				hi = mid
			}
		}
		c := (lo + hi) / 2
		bounds[k] = c
		prevSpent = spent[k]
		top, bottom := c*math.Sqrt(t), -c*math.Sqrt(t)
		if !twoSided {
			bottom = -10 * math.Sqrt(t)
		}
		h := (top - bottom) / float64(m-1)
		nx, nw := make([]float64, m), make([]float64, m)
		for i := range nx {
			b := bottom + float64(i)*h
			dens := 0.0
			if k == 0 {
				dens = normalPDF(b/sd) / sd
			} else {
				for j, u := range xs {
					dens += ws[j] * normalPDF((b-u)/sd) / sd
				}
			}
			weight := 2.0
			switch {
			case i == 0 || i == m-1:
				weight = 1
			case i%2 == 1:
				weight = 4
			}
			nx[i], nw[i] = b, dens*weight*h/3
		}
		xs, ws = nx, nw
		prevT = t
	}
	return bounds, nil
}

// SeqLook is one analysed look of a group-sequential design: its counted tasks, its information fraction (counted tasks
// over the planned maximum), and the z-boundaries and two-sided nominal levels of its efficacy interval (improved,
// regressed) and of its equivalence interval (each side a one-sided test).
type SeqLook struct {
	Tasks    int     `json:"tasks"`
	Fraction float64 `json:"fraction"`
	EffBound float64 `json:"efficacy_z"`
	EqBound  float64 `json:"equivalence_z"`
	EffLevel float64 `json:"efficacy_level"`    // 1 − 2(1 − Φ(EffBound))
	EqLevel  float64 `json:"equivalence_level"` // 1 − 2(1 − Φ(EqBound))
}

// SequentialLooks computes the analysed looks of a design planned for maximum tasks, with O'Brien–Fleming-type
// spending of a two-sided alpha for efficacy (alpha/2 a side) and of a one-sided eqAlpha per side for equivalence.
// counts are the analysed looks' counted tasks, strictly increasing; an interim look's fraction is its count over
// maximum. final marks the last count as the final look, which spends the remainder whatever its count. Looks that were
// not analysed (too few tasks, say) are left out of counts: spending then resumes at the next analysed look.
func SequentialLooks(counts []int, maximum int, final bool, alpha, eqAlpha float64) ([]SeqLook, error) {
	if maximum < 1 || len(counts) == 0 || counts[len(counts)-1] > maximum {
		return nil, fmt.Errorf("sequential looks: counts %v of a maximum of %d", counts, maximum)
	}
	times := make([]float64, len(counts))
	eff, eq := make([]float64, len(counts)), make([]float64, len(counts))
	for k, n := range counts {
		times[k] = float64(n) / float64(maximum)
		eff[k], eq[k] = 2*OBFSpending(alpha/2, times[k]), OBFSpending(eqAlpha, times[k])
	}
	if final {
		eff[len(eff)-1], eq[len(eq)-1] = alpha, eqAlpha
	}
	effBounds, err := SequentialBounds(times, eff, true)
	if err != nil {
		return nil, err
	}
	eqBounds, err := SequentialBounds(times, eq, false)
	if err != nil {
		return nil, err
	}
	looks := make([]SeqLook, len(counts))
	for k, n := range counts {
		looks[k] = SeqLook{Tasks: n, Fraction: times[k], EffBound: effBounds[k], EqBound: eqBounds[k],
			EffLevel: 1 - 2*(1-normalCDF(effBounds[k])), EqLevel: 1 - 2*(1-normalCDF(eqBounds[k]))}
	}
	return looks, nil
}

// ConditionalPower is the chance of crossing the final efficacy boundary finalBound from an interim look at
// information fraction f with statistic z (the per-task differences' t-statistic, taken as normal), under the trend
// observed so far: 1 − Φ((finalBound − |z|/√f)/√(1 − f)). At f ≥ 1 there is nothing left to observe: 1 when |z| is at
// or past the boundary, else 0.
func ConditionalPower(z, f, finalBound float64) float64 {
	if f >= 1 {
		if math.Abs(z) >= finalBound {
			return 1
		}
		return 0
	}
	return 1 - normalCDF((finalBound-math.Abs(z)/math.Sqrt(f))/math.Sqrt(1-f))
}

// LookEvidence is the evidence of a look: the bootstrap (draws from src) and the t-interval of the per-task
// differences of arm b against arm a after transform, at the look's efficacy level in the "95%" slots and its
// equivalence level in the "90%" slots, which is how Decide takes a look's levels. z is the differences' t-statistic
// (0 when they do not vary), for the futility rule.
func LookEvidence(t *Table, a, b string, transform Transform, draws int, src Source, effLevel, eqLevel float64) (e Evidence, z float64, err error) {
	boot, err := NewBootstrap(t, a, b, transform, draws, src)
	if err != nil {
		return Evidence{}, 0, err
	}
	diffs := t.Paired(a, b, transform)
	tEff, err := TInterval(diffs, effLevel)
	if err != nil {
		return Evidence{}, 0, err
	}
	tEq, err := TInterval(diffs, eqLevel)
	if err != nil {
		return Evidence{}, 0, err
	}
	if se := math.Sqrt(Variance(diffs) / float64(len(diffs))); se > 0 {
		z = Mean(diffs) / se
	}
	return Evidence{Boot95: boot.Percentile(effLevel), T95: tEff, Boot90: boot.Percentile(eqLevel), T90: tEq}, z, nil
}

// SequentialExpectedTasks estimates the tasks a design uses on average, from paths simulated experiments (seeded, so
// the same inputs give the same figure): normal per-task differences with a true mean of drift standard deviations,
// analysed at each of looks (planned, the last the final) as the analysis does on the t-interval: a verdict when the
// look's efficacy interval excludes no difference, and at an interim look a futility stop when the conditional power
// is below futility (0: none). Equivalence stops and the bootstrap are left out, so the estimate leans high, the
// cautious side for spend.
func SequentialExpectedTasks(looks []SeqLook, futility, drift float64, paths int, seed uint64) float64 {
	if len(looks) == 0 || paths < 1 {
		return 0
	}
	r := rand.New(rand.NewPCG(seed, 7))
	final := looks[len(looks)-1]
	crit := make([]float64, len(looks))
	for k, l := range looks {
		crit[k] = math.Inf(1)
		if l.Tasks >= 2 {
			crit[k] = TQuantile((1+l.EffLevel)/2, l.Tasks-1)
		}
	}
	total := 0
	for range paths {
		sum, sq, n := 0.0, 0.0, 0
		for k, l := range looks {
			for ; n < l.Tasks; n++ {
				d := drift + r.NormFloat64()
				sum += d
				sq += d * d
			}
			z := 0.0
			if v := (sq - sum*sum/float64(n)) / float64(n-1); n >= 2 && v > 0 {
				z = sum / float64(n) / math.Sqrt(v/float64(n))
			}
			last := k == len(looks)-1
			if last || math.Abs(z) >= crit[k] || futility > 0 && ConditionalPower(z, l.Fraction, final.EffBound) < futility {
				total += n
				break
			}
		}
	}
	return float64(total) / float64(paths)
}
