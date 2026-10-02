package stats

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
)

// Research helpers and evidence for the wave-3 statistics note (docs/research/2026-10-02-wave3-statistics-note.md):
// a group-sequential cost design (looks after 8, 12 and 16 tasks, O'Brien–Fleming-type Lan–DeMets spending,
// non-binding futility), run reuse with a bias allowance, the reused-against-fresh A/A, and the drift chart. Test-only:
// no production code uses any of it yet, and production verdicts (Decide) are called exactly as they are.
//
// The default run is sized for CI under the race detector. AGENTIUM_LONG_SIM=<factor> multiplies every replicate count
// (the note's figures come from AGENTIUM_LONG_SIM=20); run it with -v to read the figures.

// longFactor is the replicate multiplier from AGENTIUM_LONG_SIM (1 when unset or invalid).
func longFactor() int {
	if f, err := strconv.Atoi(os.Getenv("AGENTIUM_LONG_SIM")); err == nil && f > 1 {
		return f
	}
	return 1
}

func normalCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }
func normalPDF(x float64) float64 { return math.Exp(-x*x/2) / math.Sqrt(2*math.Pi) }

// obfSpending is Lan and DeMets' O'Brien–Fleming-type spending function: the one-sided error, of alpha in all, spent by
// information fraction t: 2 − 2Φ(z(1 − alpha/2)/√t).
func obfSpending(alpha, t float64) float64 {
	switch {
	case t <= 0:
		return 0
	case t >= 1:
		return alpha
	}
	return 2 - 2*normalCDF(NormalQuantile(1-alpha/2)/math.Sqrt(t))
}

// groupSequentialBounds returns the z-boundaries at information fractions (increasing, the last 1) such that a
// standard Brownian motion, observed at those fractions as Z_k = B(t_k)/√t_k, first crosses by look k with probability
// spend(t_k): |Z| ≥ c when twoSided (spend is then the two-sided total), else Z ≥ c. It integrates the continuation
// region's sub-density numerically (Armitage, McPherson and Rowe's recursion, Simpson's rule on the B scale).
func groupSequentialBounds(fractions []float64, spend func(float64) float64, twoSided bool) []float64 {
	const m = 1201 // grid points, odd for Simpson's rule
	bounds := make([]float64, len(fractions))
	var xs, ws []float64 // the continuation region's grid and its sub-density times the quadrature weights
	prevT, prevSpent := 0.0, 0.0
	for k, t := range fractions {
		target := spend(t) - prevSpent
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
		prevSpent = spend(t)
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
	return bounds
}

// seqAlpha is the note's two-sided error for efficacy (improved, regressed). A first long run at 5% gave 5.12%
// [4.99, 5.24] false differences under normal noise (the t-interval at nominal levels runs slightly liberal with 7–15
// degrees of freedom), so the design spends 4.5%, and the gate is checked on that.
const seqAlpha = 0.045

// seqDesign is a group-sequential cost design: looks after Looks[k] tasks (the last is the maximum), the nominal
// two-sided level of each look's efficacy interval (improved, regressed) and of its equivalence interval (each side a
// one-sided test), from O'Brien–Fleming-type spending of a two-sided Alpha (Alpha/2 a side) and of a one-sided EqAlpha.
type seqDesign struct {
	Looks     []int
	Fractions []float64
	Alpha     float64
	EqAlpha   float64
	EffBounds []float64 // z-boundaries, two-sided
	EqBounds  []float64 // z-boundaries, one-sided
	EffLevel  []float64 // 1 − 2(1 − Φ(EffBounds[k])): the efficacy interval's two-sided level
	EqLevel   []float64 // 1 − 2(1 − Φ(EqBounds[k])): the equivalence interval's two-sided level
	// Futility: an interim look with no verdict stops when the conditional power to cross the final efficacy boundary,
	// under the trend observed so far, is below Futility (0: never). Non-binding: the efficacy boundaries ignore it.
	Futility float64
}

func newSeqDesign(looks []int, alpha, eqAlpha, futility float64) seqDesign {
	d := seqDesign{Looks: looks, Alpha: alpha, EqAlpha: eqAlpha, Futility: futility}
	for _, n := range looks {
		d.Fractions = append(d.Fractions, float64(n)/float64(looks[len(looks)-1]))
	}
	d.EffBounds = groupSequentialBounds(d.Fractions, func(t float64) float64 { return 2 * obfSpending(alpha/2, t) }, true)
	d.EqBounds = groupSequentialBounds(d.Fractions, func(t float64) float64 { return obfSpending(eqAlpha, t) }, false)
	for k := range looks {
		d.EffLevel = append(d.EffLevel, 1-2*(1-normalCDF(d.EffBounds[k])))
		d.EqLevel = append(d.EqLevel, 1-2*(1-normalCDF(d.EqBounds[k])))
	}
	return d
}

// conditionalPower is the chance of crossing the final efficacy boundary from look k, under the trend so far, with z
// the look's statistic (the t-statistic of the per-task differences, taken as normal).
func (d seqDesign) conditionalPower(k int, z float64) float64 {
	t := d.Fractions[k]
	final := d.EffBounds[len(d.EffBounds)-1]
	return 1 - normalCDF((final-math.Abs(z)/math.Sqrt(t))/math.Sqrt(1-t))
}

func TestGroupSequentialBounds(t *testing.T) {
	// Published values (gsDesign, three equally spaced looks, one-sided 0.025, O'Brien–Fleming-type Lan–DeMets
	// spending): 3.7103, 2.5114, 1.9930.
	got := groupSequentialBounds([]float64{1.0 / 3, 2.0 / 3, 1}, func(t float64) float64 { return obfSpending(0.025, t) }, false)
	for i, want := range []float64{3.7103, 2.5114, 1.9930} {
		if math.Abs(got[i]-want) > 0.002 {
			t.Errorf("look %d: boundary %.4f, want %.4f", i+1, got[i], want)
		}
	}
	// One look spends everything: the fixed test's 1.96.
	if one := groupSequentialBounds([]float64{1}, func(t float64) float64 { return 2 * obfSpending(0.025, t) }, true); math.Abs(one[0]-1.95996) > 1e-3 {
		t.Errorf("one look: %.4f, want 1.96", one[0])
	}
	// The note's design, checked by a Brownian simulation: the crossing probabilities are the spending function's.
	d := newSeqDesign([]int{8, 12, 16}, seqAlpha, 0.05, 0)
	t.Logf("looks %v, fractions %.3f: efficacy z %.4f (two-sided nominal levels %.5f); equivalence z %.4f (interval levels %.4f)",
		d.Looks, d.Fractions, d.EffBounds, d.EffLevel, d.EqBounds, d.EqLevel)
	r := rand.New(rand.NewPCG(11, 12))
	const paths = 400000
	crossed := make([]int, len(d.Looks))
	for range paths {
		b, prev := 0.0, 0.0
		for k, f := range d.Fractions {
			b += math.Sqrt(f-prev) * r.NormFloat64()
			prev = f
			if math.Abs(b/math.Sqrt(f)) >= d.EffBounds[k] {
				crossed[k]++
				break
			}
		}
	}
	cumulative := 0
	for k, f := range d.Fractions {
		cumulative += crossed[k]
		got, want := float64(cumulative)/paths, 2*obfSpending(seqAlpha/2, f)
		if se := math.Sqrt(want * (1 - want) / paths); math.Abs(got-want) > 4*se {
			t.Errorf("look %d: crossed by then in %.4f%% of paths, want %.4f%%", k+1, 100*got, 100*want)
		}
	}
}

// noiseShape is how simulated noise and task effects are distributed, each standardized to mean 0 and variance 1.
type noiseShape int

const (
	shapeNormal noiseShape = iota
	shapeSkewed            // the standardized lognormal of TestOneRunCostVerdictsSimulation (skewness about 0.9)
	shapeHeavy             // Student's t with 5 degrees of freedom, scaled to variance 1 (kurtosis 9)
)

func (s noiseShape) String() string { return [...]string{"normal", "skewed", "heavy-tailed"}[s] }

func (s noiseShape) draw(r *rand.Rand) float64 {
	switch s {
	case shapeSkewed:
		return standardSkewed(r)
	case shapeHeavy:
		chi := 0.0
		for range 5 {
			z := r.NormFloat64()
			chi += z * z
		}
		return r.NormFloat64() / math.Sqrt(chi/5) / math.Sqrt(5.0/3)
	}
	return r.NormFloat64()
}

// seqCell is one simulated scenario: per-run log-cost spread Sigma, a true effect of mean Effect (log ratio B/A) and
// spread Tau across tasks, one fresh run per task and arm, unless Stored is set: then arm A is Stored stored runs per
// task whose log cost sits Bias above a fresh run's, and verdicts use the reuse allowance Allowance.
type seqCell struct {
	Sigma, Tau, Effect float64
	Shape              noiseShape
	Stored             int
	Bias, Allowance    float64
}

// seqOutcome counts what reps simulated experiments concluded: with futility stops obeyed, and with them ignored
// (the non-binding worst case for false verdicts), plus the t-interval alone ignoring futility (an upper bound on
// the false verdicts of the widest-interval rule, since that interval contains the t-interval).
type seqOutcome struct {
	reps                                        int
	differences, improved, equivalent           int // futility obeyed
	tasks                                       int // tasks used, summed (futility obeyed)
	differencesNoFutility, equivalentNoFutility int
	tasksNoFutility                             int
	differencesTOnly                            int
	stopsAt                                     []int // the look each experiment stopped at, futility obeyed
}

func isDifference(v string) bool { return v == Improved || v == ImprovedSmall || v == Regressed }

// verdictAt analyses the first n tasks of tb at look k: Decide with the look's nominal levels (the efficacy interval
// in the 95% slots, the equivalence interval in the 90% ones), or, with a reuse allowance, a difference only when the
// widest efficacy interval clears ±allowance (and no equivalence). It also returns the t-only verdict and the t-statistic.
func verdictAt(d seqDesign, k int, tb *Table, draws int, r *rand.Rand, allowance float64) (verdict, tOnly string, z float64) {
	boot, _ := NewBootstrap(tb, "A", "B", math.Log, draws, r)
	diffs := tb.Paired("A", "B", math.Log)
	tEff, _ := TInterval(diffs, d.EffLevel[k])
	tEq, _ := TInterval(diffs, d.EqLevel[k])
	z = Mean(diffs) / math.Sqrt(Variance(diffs)/float64(len(diffs)))
	margin := RatioMargin(0.10, LowerIsBetter)
	if allowance > 0 {
		clear := func(i Interval) string {
			switch {
			case i.High < -allowance:
				return Improved
			case i.Low > allowance:
				return Regressed
			}
			return Inconclusive
		}
		return clear(widest(boot.Percentile(d.EffLevel[k]), tEff)), clear(tEff), z
	}
	e := Evidence{Boot95: boot.Percentile(d.EffLevel[k]), T95: tEff, Boot90: boot.Percentile(d.EqLevel[k]), T90: tEq}
	verdict, _ = Decide(e, LowerIsBetter, margin, false, false)
	tOnly, _ = Decide(Evidence{Boot95: tEff, T95: tEff, Boot90: tEq, T90: tEq}, LowerIsBetter, margin, false, false)
	return verdict, tOnly, z
}

func simulateSequential(d seqDesign, c seqCell, reps, draws int, seed uint64) seqOutcome {
	r := rand.New(rand.NewPCG(seed, 9))
	maxTasks := d.Looks[len(d.Looks)-1]
	out := seqOutcome{reps: reps, stopsAt: make([]int, len(d.Looks))}
	names := make([]string, maxTasks)
	for i := range names {
		names[i] = fmt.Sprintf("t%02d", i)
	}
	for range reps {
		tb := NewTable()
		added := 0
		stopped, futile := false, false // futility obeyed: stopped by a verdict or a futility stop
		tDone := false
		for k, n := range d.Looks {
			for ; added < n; added++ {
				level := r.NormFloat64()
				effect := c.Effect + c.Tau*c.Shape.draw(r)
				if c.Stored > 0 {
					for range c.Stored {
						tb.Add(names[added], "A", math.Exp(level+c.Bias+c.Sigma*c.Shape.draw(r)))
					}
				} else {
					tb.Add(names[added], "A", math.Exp(level+c.Sigma*c.Shape.draw(r)))
				}
				tb.Add(names[added], "B", math.Exp(level+effect+c.Sigma*c.Shape.draw(r)))
			}
			verdict, tOnly, z := verdictAt(d, k, tb, draws, r, c.Allowance)
			if !tDone && isDifference(tOnly) {
				out.differencesTOnly++
				tDone = true
			}
			if tOnly == Equivalent {
				tDone = true
			}
			decided := isDifference(verdict) || verdict == Equivalent
			if !stopped && !futile {
				switch {
				case decided:
					stopped = true
					out.tasks += n
					out.stopsAt[k]++
					if isDifference(verdict) {
						out.differences++
						if verdict == Improved || verdict == ImprovedSmall {
							out.improved++
						}
					} else {
						out.equivalent++
					}
				case k < len(d.Looks)-1 && d.Futility > 0 && d.conditionalPower(k, z) < d.Futility:
					futile = true
					out.tasks += n
					out.stopsAt[k]++
				case k == len(d.Looks)-1:
					out.tasks += n
					out.stopsAt[k]++
				}
			}
			if decided || k == len(d.Looks)-1 {
				out.tasksNoFutility += n
				if isDifference(verdict) {
					out.differencesNoFutility++
				} else if verdict == Equivalent {
					out.equivalentNoFutility++
				}
				break
			}
		}
	}
	return out
}

func (o *seqOutcome) add(p seqOutcome) {
	o.reps += p.reps
	o.differences += p.differences
	o.improved += p.improved
	o.equivalent += p.equivalent
	o.tasks += p.tasks
	o.differencesNoFutility += p.differencesNoFutility
	o.equivalentNoFutility += p.equivalentNoFutility
	o.tasksNoFutility += p.tasksNoFutility
	o.differencesTOnly += p.differencesTOnly
	if o.stopsAt == nil {
		o.stopsAt = make([]int, len(p.stopsAt))
	}
	for i, s := range p.stopsAt {
		o.stopsAt[i] += s
	}
}

// rate is a share with its 95% Wilson interval, in percent, for logs.
func rate(k, n int) string {
	lo, hi := Wilson(k, n)
	return fmt.Sprintf("%.2f%% [%.2f, %.2f]", 100*float64(k)/float64(n), 100*lo, 100*hi)
}

// runCells simulates cells in parallel, each on its own seed (from base), so counts do not depend on scheduling.
func runCells(d seqDesign, cells []seqCell, reps, draws int, base uint64) []seqOutcome {
	out := make([]seqOutcome, len(cells))
	var wg sync.WaitGroup
	for i, c := range cells {
		wg.Go(func() { out[i] = simulateSequential(d, c, reps, draws, base+uint64(i)) })
	}
	wg.Wait()
	return out
}

// TestGroupSequentialFalseVerdicts is the trust guard's evidence for the note's design: looks after 8, 12 and 16
// tasks × 1 run per arm, O'Brien–Fleming-type spending of a two-sided 4.5% (efficacy: seqAlpha) and of a one-sided 5%
// per side (equivalence), Decide at each look's nominal levels (bootstrap and t-interval must agree), non-binding
// futility at conditional power below 10%. With no true mean difference (an A/A, τ = 0; and a zero mean with τ = 0.10
// or 0.25), for σ = 0.19 and 0.35, under normal, skewed and heavy-tailed noise, false differences (improved,
// regressed) are counted ignoring futility stops (the non-binding worst case). It requires:
//   - always: at most 5.0% pooled over every null scenario, and at most 6.0% per noise shape (a regression check: the
//     default run has 2,400 experiments a shape, about ±0.9 points at 95%);
//   - in the long run (AGENTIUM_LONG_SIM ≥ 10): per noise shape, at most 5.0% with its Wilson 95% upper bound at most
//     5.0% too. This is the wave-3 exit gate's simulation half.
//
// It also checks "equivalent" at a true +10% (the margin): at most 5%. Bootstrap draws are 200 a look, not the
// analysis's 10,000: with one run per cell the bootstrap resamples tasks only and its percentile interval is narrower
// than the t-interval at these sizes, so the widest-interval rule is decided by the t-interval almost always; the
// t-only count, an upper bound on false differences, is logged too.
func TestGroupSequentialFalseVerdicts(t *testing.T) {
	factor := longFactor()
	reps, draws := 400*factor, 200
	d := newSeqDesign([]int{8, 12, 16}, seqAlpha, 0.05, 0.10)
	var cells []seqCell
	for _, shape := range []noiseShape{shapeNormal, shapeSkewed, shapeHeavy} {
		for _, sigma := range []float64{0.19, 0.35} {
			for _, tau := range []float64{0, 0.10, 0.25} {
				cells = append(cells, seqCell{Sigma: sigma, Tau: tau, Shape: shape})
			}
		}
	}
	results := runCells(d, cells, reps, draws, 1000)
	pooled := map[noiseShape]*seqOutcome{}
	aa := map[noiseShape]*seqOutcome{}
	for i, c := range cells {
		o := results[i]
		t.Logf("null, %s, σ = %.2f, τ = %.2f: false differences %s (futility obeyed %s; t alone %s); tasks used %.1f (%.1f without futility)",
			c.Shape, c.Sigma, c.Tau, rate(o.differencesNoFutility, o.reps), rate(o.differences, o.reps), rate(o.differencesTOnly, o.reps),
			float64(o.tasks)/float64(o.reps), float64(o.tasksNoFutility)/float64(o.reps))
		if pooled[c.Shape] == nil {
			pooled[c.Shape], aa[c.Shape] = &seqOutcome{}, &seqOutcome{}
		}
		pooled[c.Shape].add(o)
		if c.Tau == 0 {
			aa[c.Shape].add(o)
		}
	}
	all, allAA := seqOutcome{}, seqOutcome{}
	for _, shape := range []noiseShape{shapeNormal, shapeSkewed, shapeHeavy} {
		p := pooled[shape]
		all.add(*p)
		allAA.add(*aa[shape])
		t.Logf("null, %s, pooled over %d experiments: false differences %s ignoring futility, %s obeying it, %s on the t-interval alone; A/A only (τ = 0): %s",
			shape, p.reps, rate(p.differencesNoFutility, p.reps), rate(p.differences, p.reps), rate(p.differencesTOnly, p.reps),
			rate(aa[shape].differencesNoFutility, aa[shape].reps))
		limit := 0.06
		if factor >= 10 {
			limit = 0.05
		}
		if r := float64(p.differencesNoFutility) / float64(p.reps); r > limit {
			t.Errorf("%s: false differences in %.2f%% of experiments without a true difference, above %.0f%%", shape, 100*r, 100*limit)
		}
		if _, hi := Wilson(p.differencesNoFutility, p.reps); factor >= 10 && hi > 0.05 {
			t.Errorf("%s: the false-difference rate's 95%% upper bound is %.2f%%, above 5%%", shape, 100*hi)
		}
	}
	t.Logf("null, all shapes, pooled over %d experiments: false differences %s ignoring futility; A/A only: %s; stops at looks %v (futility obeyed)",
		all.reps, rate(all.differencesNoFutility, all.reps), rate(allAA.differencesNoFutility, allAA.reps), all.stopsAt)
	if r := float64(all.differencesNoFutility) / float64(all.reps); r > 0.05 {
		t.Errorf("false differences in %.2f%% of all experiments without a true difference, above 5%%", 100*r)
	}

	// Equivalence at the margin: a true +10% (log 1.1) must be called equivalent at most 5% of the time.
	var edge []seqCell
	for _, shape := range []noiseShape{shapeNormal, shapeSkewed} {
		for _, tau := range []float64{0, 0.10} {
			edge = append(edge, seqCell{Sigma: 0.19, Tau: tau, Effect: math.Log(1.1), Shape: shape})
		}
	}
	edgeOut := runCells(d, edge, reps, draws, 2000)
	var eq seqOutcome
	for _, o := range edgeOut {
		eq.add(o)
	}
	t.Logf("a true +10%% (the margin), pooled over %d experiments: equivalent %s (futility ignored)", eq.reps, rate(eq.equivalentNoFutility, eq.reps))
	if r := float64(eq.equivalentNoFutility) / float64(eq.reps); r > 0.05 {
		t.Errorf("equivalent at the margin in %.2f%% of experiments, above 5%%", 100*r)
	}
}

// TestGroupSequentialPower logs power and the tasks used at plausible effects, for the sequential design and for the
// fixed designs it replaces (8 tasks, 16 tasks), at σ = 0.19 and τ = 0.10 and 0.25 (normal noise; skewed at τ = 0.25).
// It asserts only what the design is for: at a true 20% reduction the sequential design decides more often than the
// fixed 8-task design, and uses fewer tasks on average than the fixed 16-task design.
func TestGroupSequentialPower(t *testing.T) {
	factor := longFactor()
	reps, draws := 100*factor, 200
	designs := []struct {
		name string
		d    seqDesign
	}{
		{"sequential 8/12/16", newSeqDesign([]int{8, 12, 16}, seqAlpha, 0.05, 0.10)},
		{"fixed 8", newSeqDesign([]int{8}, 0.05, 0.05, 0)},
		{"fixed 16", newSeqDesign([]int{16}, 0.05, 0.05, 0)},
	}
	type key struct {
		design string
		effect float64
		tau    float64
		shape  noiseShape
	}
	got := map[key]seqOutcome{}
	seed := uint64(3000)
	for _, ds := range designs {
		var cells []seqCell
		for _, effect := range []float64{0, 0.10, 0.20, 0.35, 0.63} {
			for _, tau := range []float64{0.10, 0.25} {
				cells = append(cells, seqCell{Sigma: 0.19, Tau: tau, Effect: math.Log(1 - effect)})
			}
			cells = append(cells, seqCell{Sigma: 0.19, Tau: 0.25, Effect: math.Log(1 - effect), Shape: shapeSkewed})
		}
		results := runCells(ds.d, cells, reps, draws, seed)
		seed += uint64(len(cells))
		for i, c := range cells {
			o := results[i]
			effect := 1 - math.Exp(c.Effect)
			got[key{ds.name, math.Round(effect * 100), c.Tau, c.Shape}] = o
			t.Logf("%s, true reduction %.0f%%, τ = %.2f, %s: improved %s; any verdict %s; tasks used %.1f (futility obeyed), %.1f (ignored); stops at looks %v",
				ds.name, 100*effect, c.Tau, c.Shape, rate(o.improved, o.reps), rate(o.differences+o.equivalent, o.reps),
				float64(o.tasks)/float64(o.reps), float64(o.tasksNoFutility)/float64(o.reps), o.stopsAt)
		}
	}
	for _, tau := range []float64{0.10, 0.25} {
		seq, fixed8, fixed16 := got[key{"sequential 8/12/16", 20, tau, shapeNormal}], got[key{"fixed 8", 20, tau, shapeNormal}], got[key{"fixed 16", 20, tau, shapeNormal}]
		if seq.improved <= fixed8.improved {
			t.Errorf("τ = %.2f: the sequential design found a 20%% reduction %d times, the fixed 8-task design %d", tau, seq.improved, fixed8.improved)
		}
		if seq.tasks >= fixed16.tasks {
			t.Errorf("τ = %.2f: the sequential design used %d tasks in all, the fixed 16-task design %d", tau, seq.tasks, fixed16.tasks)
		}
	}
}

// TestReuseValidationAndAllowance simulates the note's reuse rules end to end, at σ = 0.19 (normal and skewed noise):
//  1. the reused-against-fresh A/A: 16 tasks, 4 stored runs a task against 4 fresh ones, log cost; it passes when the
//     widest 95% interval of log(fresh/stored) contains zero and its farther end is within log 1.15; the allowance is
//     that farther end;
//  2. after a pass, a reuse experiment with the same true bias: arm A 4 stored runs a task, arm B 1 fresh run, the
//     sequential looks (no futility), and a difference only beyond ±allowance; no true effect, then true reductions.
//
// It requires, for true biases within ±5%, false differences at most 5% among experiments that ran (after a pass),
// and logs the pass rate and power.
func TestReuseValidationAndAllowance(t *testing.T) {
	factor := longFactor()
	reps, draws := 100*factor, 200
	d := newSeqDesign([]int{8, 12, 16}, seqAlpha, 0.05, 0)
	limit := math.Log(1.15)
	type res struct {
		pass, falseDiff, improved20, improved35, improved63, runs int
		tasks20, tasks35, tasks63                                 int
		allowance                                                 float64
	}
	type scenario struct {
		bias  float64
		shape noiseShape
	}
	var scenarios []scenario
	for _, shape := range []noiseShape{shapeNormal, shapeSkewed} {
		for _, bias := range []float64{0, -0.05, 0.05, 0.10} {
			scenarios = append(scenarios, scenario{bias, shape})
		}
	}
	results := make([]res, len(scenarios))
	var wg sync.WaitGroup
	for si, sc := range scenarios {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(4000+uint64(si), 5))
			var out res
			for range reps {
				// The validation: stored (biased) against fresh, both 4 runs a task.
				tb := NewTable()
				for i := range 16 {
					name := fmt.Sprintf("t%02d", i)
					level := r.NormFloat64()
					for range 4 {
						tb.Add(name, "A", math.Exp(level+sc.bias+0.19*sc.shape.draw(r)))
						tb.Add(name, "B", math.Exp(level+0.19*sc.shape.draw(r)))
					}
				}
				boot, _ := NewBootstrap(tb, "A", "B", math.Log, draws, r)
				t95, _ := TInterval(tb.Paired("A", "B", math.Log), 0.95)
				u := widest(boot.Percentile(0.95), t95)
				allowance := math.Max(math.Abs(u.Low), math.Abs(u.High))
				if u.Low > 0 || u.High < 0 || allowance > limit {
					continue
				}
				out.pass++
				out.allowance += allowance
				// Reuse experiments under the same bias, with this allowance.
				for _, effect := range []float64{0, 0.20, 0.35, 0.63} {
					c := seqCell{Sigma: 0.19, Tau: 0.10, Effect: math.Log(1 - effect), Shape: sc.shape, Stored: 4, Bias: sc.bias, Allowance: allowance}
					o := simulateSequential(d, c, 1, draws, r.Uint64())
					switch effect {
					case 0:
						out.falseDiff += o.differencesNoFutility
						out.runs += o.tasksNoFutility
					case 0.20:
						out.improved20 += o.improved
						out.tasks20 += o.tasksNoFutility
					case 0.35:
						out.improved35 += o.improved
						out.tasks35 += o.tasksNoFutility
					case 0.63:
						out.improved63 += o.improved
						out.tasks63 += o.tasksNoFutility
					}
				}
			}
			results[si] = out
		})
	}
	wg.Wait()
	for si, sc := range scenarios {
		o := results[si]
		if o.pass == 0 {
			t.Logf("bias %+.0f%%, %s: the validation never passed in %d tries", 100*sc.bias, sc.shape, reps)
			continue
		}
		per := func(n int) float64 { return float64(n) / float64(o.pass) }
		t.Logf("bias %+.0f%% (stored against fresh), %s: validation passed %s; mean allowance %.3f; after a pass, with no true effect, false differences %s, fresh runs %.1f; improved at a true 20%% %s (fresh runs %.1f), 35%% %s (%.1f), 63%% %s (%.1f)",
			100*sc.bias, sc.shape, rate(o.pass, reps), o.allowance/float64(o.pass), rate(o.falseDiff, o.pass), per(o.runs),
			rate(o.improved20, o.pass), per(o.tasks20), rate(o.improved35, o.pass), per(o.tasks35), rate(o.improved63, o.pass), per(o.tasks63))
		if math.Abs(sc.bias) <= 0.05 && float64(o.falseDiff)/float64(o.pass) > 0.05 {
			t.Errorf("bias %+.2f, %s: false differences in %.2f%% of reuse experiments, above 5%%", sc.bias, sc.shape, 100*float64(o.falseDiff)/float64(o.pass))
		}
	}
}
