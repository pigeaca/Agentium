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

// Evidence for the wave-3 statistics note (docs/research/2026-10-02-wave3-statistics-note.md): a group-sequential cost
// design (method seq-v1: looks after 8, 12 and 16 tasks, O'Brien–Fleming-type Lan–DeMets spending, non-binding
// futility), run reuse with a bias allowance, the reused-against-fresh A/A, and the drift chart. The sequential design's
// spending, boundaries, nominal levels, look evidence and conditional power are the production code's (sequential.go),
// and each look's verdict is the production Decide; only the simulated data and the reuse rules are test-only.
//
// The default run is sized for CI under the race detector, and asserts with margins of several Monte Carlo standard
// errors. AGENTIUM_LONG_SIM=<factor> multiplies every replicate count; the note's figures and its exit gate come from
// AGENTIUM_LONG_SIM=50 (gateFactor), and smaller factors are exploratory. Run it with -v to read the figures.

// gateFactor is the replicate multiplier at which the strict thresholds of the note's exit gate apply.
const gateFactor = 50

// simDraws is the bootstrap draws a look in the simulations: 200 for the figures (factor ≥ 10), 50 in the default run
// to keep it fast under the race detector. The default run's assertions on false differences use the t-interval alone,
// which does not depend on the draws.
func simDraws(factor int) int {
	if factor >= 10 {
		return 200
	}
	return 50
}

// longFactor is the replicate multiplier from AGENTIUM_LONG_SIM (1 when unset or invalid).
func longFactor() int {
	if f, err := strconv.Atoi(os.Getenv("AGENTIUM_LONG_SIM")); err == nil && f > 1 {
		return f
	}
	return 1
}

// seqAlpha is the production method's efficacy alpha: the gate below tests exactly it.
const seqAlpha = SeqAlpha

// seqDesign is a group-sequential cost design as the simulations use it: looks after Looks[k] tasks (the last is the
// maximum), with the production boundaries and nominal levels (SequentialLooks) of O'Brien–Fleming-type spending of a
// two-sided Alpha for efficacy and a one-sided EqAlpha per side for equivalence.
type seqDesign struct {
	Looks     []int
	Fractions []float64
	Alpha     float64
	EqAlpha   float64
	EffBounds []float64 // z-boundaries, two-sided
	EqBounds  []float64 // z-boundaries, one-sided
	EffLevel  []float64 // the efficacy interval's two-sided level
	EqLevel   []float64 // the equivalence interval's two-sided level
	// Futility: an interim look with no verdict stops when the conditional power to cross the final efficacy boundary,
	// under the trend observed so far, is below Futility (0: never). Non-binding: the efficacy boundaries ignore it.
	Futility float64
}

func newSeqDesign(looks []int, alpha, eqAlpha, futility float64) seqDesign {
	d := seqDesign{Looks: looks, Alpha: alpha, EqAlpha: eqAlpha, Futility: futility}
	computed, err := SequentialLooks(looks, looks[len(looks)-1], true, alpha, eqAlpha)
	if err != nil {
		panic(err) // the designs here are fixed
	}
	for _, l := range computed {
		d.Fractions = append(d.Fractions, l.Fraction)
		d.EffBounds, d.EqBounds = append(d.EffBounds, l.EffBound), append(d.EqBounds, l.EqBound)
		d.EffLevel, d.EqLevel = append(d.EffLevel, l.EffLevel), append(d.EqLevel, l.EqLevel)
	}
	return d
}

// conditionalPower is the production ConditionalPower at look k.
func (d seqDesign) conditionalPower(k int, z float64) float64 {
	return ConditionalPower(z, d.Fractions[k], d.EffBounds[len(d.EffBounds)-1])
}

// groupSequentialBounds is SequentialBounds with the error spent given as a function of the fractions.
func groupSequentialBounds(fractions []float64, spend func(float64) float64, twoSided bool) []float64 {
	spent := make([]float64, len(fractions))
	for i, f := range fractions {
		spent[i] = spend(f)
	}
	b, err := SequentialBounds(fractions, spent, twoSided)
	if err != nil {
		panic(err)
	}
	return b
}

func TestGroupSequentialBounds(t *testing.T) {
	// Published values (gsDesign, three equally spaced looks, one-sided 0.025, O'Brien–Fleming-type Lan–DeMets
	// spending): 3.7103, 2.5114, 1.9930.
	got := groupSequentialBounds([]float64{1.0 / 3, 2.0 / 3, 1}, func(t float64) float64 { return OBFSpending(0.025, t) }, false)
	for i, want := range []float64{3.7103, 2.5114, 1.9930} {
		if math.Abs(got[i]-want) > 0.002 {
			t.Errorf("look %d: boundary %.4f, want %.4f", i+1, got[i], want)
		}
	}
	// One look spends everything: the fixed test's 1.96.
	if one := groupSequentialBounds([]float64{1}, func(t float64) float64 { return 2 * OBFSpending(0.025, t) }, true); math.Abs(one[0]-1.95996) > 1e-3 {
		t.Errorf("one look: %.4f, want 1.96", one[0])
	}
	// The note's design, checked by a Brownian simulation: the crossing probabilities are the spending function's.
	d := newSeqDesign(SeqLooks(SeqMaxTasks), seqAlpha, SeqEquivalenceAlpha, 0)
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
		got, want := float64(cumulative)/paths, 2*OBFSpending(seqAlpha/2, f)
		if se := math.Sqrt(want * (1 - want) / paths); math.Abs(got-want) > 4*se {
			t.Errorf("look %d: crossed by then in %.4f%% of paths, want %.4f%%", k+1, 100*got, 100*want)
		}
	}
}

// noiseShape is how simulated noise and task effects are distributed, each standardized to mean 0 and variance 1.
type noiseShape int

const (
	shapeNormal     noiseShape = iota
	shapeSkewed                // the standardized lognormal of TestOneRunCostVerdictsSimulation (skewness about 0.9)
	shapeHeavy                 // Student's t with 5 degrees of freedom, scaled to variance 1 (kurtosis 9)
	shapeSkewedLeft            // shapeSkewed mirrored (skewness about −0.9)
	shapeStrong                // a standardized lognormal of shape 0.6 (skewness about 2.3)
	shapeStrongLeft            // shapeStrong mirrored
)

func (s noiseShape) String() string {
	return [...]string{"normal", "skewed", "heavy-tailed", "left-skewed", "strongly skewed", "strongly left-skewed"}[s]
}

// standardLognormal draws from a lognormal of the given shape, shifted and scaled to mean 0 and variance 1.
func standardLognormal(r *rand.Rand, shape float64) float64 {
	v := shape * shape
	mean, sd := math.Exp(v/2), math.Sqrt((math.Exp(v)-1)*math.Exp(v))
	return (math.Exp(shape*r.NormFloat64()) - mean) / sd
}

func (s noiseShape) draw(r *rand.Rand) float64 {
	switch s {
	case shapeSkewed:
		return standardSkewed(r)
	case shapeSkewedLeft:
		return -standardSkewed(r)
	case shapeStrong:
		return standardLognormal(r, 0.6)
	case shapeStrongLeft:
		return -standardLognormal(r, 0.6)
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
//   - ArmShapes gives arm B its own noise shape ShapeB, and SigmaB (when set) its own spread: the arms' mean log costs
//     stay equal, but their difference is no longer symmetric, as two different models or contexts could make it.
//   - Empirical, when set, replaces both runs' noise: each task's paired log difference is Effect plus a value drawn
//     with replacement from Empirical (centered, standardized recorded differences) times Sigma.
//   - Day is the spread of a common log-cost shift per occasion: arm A's runs share one, arm B's another (reuse: the
//     stored runs ran on other days than the fresh ones). Interleaved fresh arms share their days, so they have none.
type seqCell struct {
	Sigma, Tau, Effect float64
	Shape              noiseShape
	Stored             int
	Bias, Allowance    float64
	ArmShapes          bool
	ShapeB             noiseShape
	SigmaB             float64
	Empirical          []float64
	Day                float64
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
	e, z, _ := LookEvidence(tb, "A", "B", math.Log, draws, r, d.EffLevel[k], d.EqLevel[k])
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
		return clear(widest(e.Boot95, e.T95)), clear(e.T95), z
	}
	verdict, _ = Decide(e, LowerIsBetter, margin, false, false)
	tOnly, _ = Decide(Evidence{Boot95: e.T95, T95: e.T95, Boot90: e.T90, T90: e.T90}, LowerIsBetter, margin, false, false)
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
	shapeB, sigmaB := c.Shape, c.Sigma
	if c.ArmShapes {
		shapeB = c.ShapeB
	}
	if c.SigmaB > 0 {
		sigmaB = c.SigmaB
	}
	for range reps {
		tb := NewTable()
		added := 0
		stopped, futile := false, false // futility obeyed: stopped by a verdict or a futility stop
		tDone := false
		dayA, dayB := c.Day*r.NormFloat64(), c.Day*r.NormFloat64()
		for k, n := range d.Looks {
			for ; added < n; added++ {
				level := r.NormFloat64()
				effect := c.Effect + c.Tau*c.Shape.draw(r)
				switch {
				case c.Empirical != nil:
					tb.Add(names[added], "A", math.Exp(level))
					tb.Add(names[added], "B", math.Exp(level+effect+c.Sigma*c.Empirical[r.IntN(len(c.Empirical))]))
					continue
				case c.Stored > 0:
					for range c.Stored {
						tb.Add(names[added], "A", math.Exp(level+dayA+c.Bias+c.Sigma*c.Shape.draw(r)))
					}
				default:
					tb.Add(names[added], "A", math.Exp(level+dayA+c.Sigma*c.Shape.draw(r)))
				}
				tb.Add(names[added], "B", math.Exp(level+dayB+effect+sigmaB*shapeB.draw(r)))
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

// recordedCosts are the per-task mean costs (A, B) of the real experiments in docs/examples: aa-report.md (an A/A, 6
// tasks), context-ab-16-report.md (8) and model-ab-report.md (8).
var recordedCosts = [][][2]float64{
	{{0.510, 0.384}, {1.297, 1.121}, {0.691, 0.703}, {1.526, 1.688}, {0.896, 0.572}, {1.164, 1.662}},
	{{0.876, 0.963}, {2.039, 1.993}, {0.696, 0.575}, {0.981, 1.408}, {0.565, 0.445}, {1.210, 1.463}, {2.010, 2.001}, {1.069, 1.352}},
	{{0.228, 0.100}, {0.415, 0.135}, {0.463, 0.108}, {0.311, 0.111}, {0.219, 0.073}, {0.161, 0.073}, {0.236, 0.133}, {0.239, 0.092}},
}

// recordedDifferences pools the recorded per-task log differences, each experiment's centered on its own mean and
// scaled by its own spread: a real, possibly lopsided, shape with no true difference.
func recordedDifferences() []float64 {
	var out []float64
	for _, experiment := range recordedCosts {
		var d []float64
		for _, pair := range experiment {
			d = append(d, math.Log(pair[1])-math.Log(pair[0]))
		}
		m, sd := Mean(d), math.Sqrt(Variance(d))
		for _, v := range d {
			out = append(out, (v-m)/sd)
		}
	}
	return out
}

// TestGroupSequentialFalseVerdicts is the trust guard's evidence for the note's design: looks after 8, 12 and 16
// tasks × 1 run per arm, O'Brien–Fleming-type spending of a two-sided 3.5% (efficacy: seqAlpha) and of a one-sided 5%
// per side (equivalence), Decide at each look's nominal levels (bootstrap and t-interval must agree), non-binding
// futility at conditional power below 10%. With no true mean difference, false differences (improved, regressed) are
// counted ignoring futility stops (the non-binding worst case), in five groups of scenarios:
//   - normal, skewed and heavy-tailed noise alike in both arms (σ = 0.19 and 0.35; an A/A, τ = 0, and a zero mean with
//     τ = 0.10 or 0.25). With one run per arm and the same noise in both, each task's difference is symmetric whatever
//     the shape, so these test the tails more than the skew;
//   - arm-specific shapes: the arms' noise differs in shape (skewed against normal or mirrored, up to skewness 2.3) and
//     in one scenario in spread (0.19 against 0.35), with equal mean log cost, so the differences are lopsided;
//   - recorded differences: the 22 per-task differences of the three real experiments, centered and resampled.
//
// The assertions use the t-interval-alone count, an upper bound on the widest-interval rule's (that interval contains
// the t-interval) that does not depend on the bootstrap's draws. They require, per group:
//   - always (a regression check, with margins of about 3 Monte Carlo standard errors over the long run's rates): at
//     most 7.0% per group, and at most 5.5% pooled over every group;
//   - at AGENTIUM_LONG_SIM ≥ gateFactor (the wave-3 exit gate's simulation half): at most 5.0% with its Wilson 95%
//     upper bound at most 5.0% too.
//
// At 4.5% the long run failed that gate for the arm-specific shapes (5.66% [5.53, 5.79]); seqAlpha is 3.5% since.
// phase1-v2's fixed 8-task design, logged for comparison, stays above 5% on the arm-specific shapes: a known
// limitation of the shipped method.
//
// It also checks "equivalent" at a true +10% (the margin): at most 5%. Bootstrap draws are 200 a look, not the
// analysis's 10,000: with one run per cell the bootstrap resamples tasks only and its percentile interval is narrower
// than the t-interval at these sizes, so the widest-interval rule is decided by the t-interval almost always.
func TestGroupSequentialFalseVerdicts(t *testing.T) {
	factor := longFactor()
	reps, draws := 400*factor, simDraws(factor)
	d := newSeqDesign(SeqLooks(SeqMaxTasks), seqAlpha, SeqEquivalenceAlpha, SeqFutility)
	type group struct {
		name  string
		cells []seqCell
		base  uint64
	}
	var alike []group
	for i, shape := range []noiseShape{shapeNormal, shapeSkewed, shapeHeavy} {
		g := group{name: shape.String(), base: 1000 + uint64(6*i)}
		for _, sigma := range []float64{0.19, 0.35} {
			for _, tau := range []float64{0, 0.10, 0.25} {
				g.cells = append(g.cells, seqCell{Sigma: sigma, Tau: tau, Shape: shape})
			}
		}
		alike = append(alike, g)
	}
	arms := group{name: "arm-specific shapes", base: 1100, cells: []seqCell{
		{Sigma: 0.19, Shape: shapeSkewed, ArmShapes: true, ShapeB: shapeNormal},
		{Sigma: 0.19, Shape: shapeSkewed, ArmShapes: true, ShapeB: shapeSkewedLeft},
		{Sigma: 0.19, Shape: shapeStrong, ArmShapes: true, ShapeB: shapeNormal},
		{Sigma: 0.19, Shape: shapeStrong, ArmShapes: true, ShapeB: shapeStrongLeft},
		{Sigma: 0.19, Shape: shapeStrong, ArmShapes: true, ShapeB: shapeNormal, SigmaB: 0.35},
		{Sigma: 0.19, Tau: 0.10, Shape: shapeStrong, ArmShapes: true, ShapeB: shapeStrongLeft},
	}}
	recorded := group{name: "recorded differences", base: 1200}
	for range 6 {
		recorded.cells = append(recorded.cells, seqCell{Sigma: 0.25, Empirical: recordedDifferences()})
	}
	groups := append(alike, arms, recorded)
	var all, allAA seqOutcome
	for _, g := range groups {
		results := runCells(d, g.cells, reps, draws, g.base)
		var p, aa seqOutcome
		for i, c := range g.cells {
			o := results[i]
			shape := c.Shape.String()
			switch {
			case c.Empirical != nil:
				shape = "recorded"
			case c.ArmShapes:
				shape = fmt.Sprintf("%s (σ %.2f) against %s (σ %.2f)", c.Shape, c.Sigma, c.ShapeB, math.Max(c.Sigma, c.SigmaB))
			}
			t.Logf("null, %s, σ = %.2f, τ = %.2f: false differences %s (futility obeyed %s; t alone %s); tasks used %.1f (%.1f without futility)",
				shape, c.Sigma, c.Tau, rate(o.differencesNoFutility, o.reps), rate(o.differences, o.reps), rate(o.differencesTOnly, o.reps),
				float64(o.tasks)/float64(o.reps), float64(o.tasksNoFutility)/float64(o.reps))
			p.add(o)
			if c.Tau == 0 && !c.ArmShapes && c.Empirical == nil {
				aa.add(o)
			}
		}
		all.add(p)
		line := fmt.Sprintf("null, %s, pooled over %d experiments: false differences %s ignoring futility, %s obeying it, %s on the t-interval alone",
			g.name, p.reps, rate(p.differencesNoFutility, p.reps), rate(p.differences, p.reps), rate(p.differencesTOnly, p.reps))
		if aa.reps > 0 {
			allAA.add(aa)
			line += "; A/A only (τ = 0): " + rate(aa.differencesNoFutility, aa.reps)
		}
		t.Log(line)
		limit := 0.07
		if factor >= gateFactor {
			limit = 0.05
		}
		if r := float64(p.differencesTOnly) / float64(p.reps); r > limit {
			t.Errorf("%s: false differences (t alone) in %.2f%% of experiments without a true difference, above %.1f%%", g.name, 100*r, 100*limit)
		}
		if _, hi := Wilson(p.differencesTOnly, p.reps); factor >= gateFactor && hi > 0.05 {
			t.Errorf("%s: the false-difference rate's (t alone) 95%% upper bound is %.2f%%, above 5%%", g.name, 100*hi)
		}
	}
	// For comparison, not asserted: phase1-v2's fixed design (one look at 8 tasks, 95% and 90% intervals) on the
	// arm-specific shapes and the recorded differences.
	var fixed seqOutcome
	for _, o := range runCells(newSeqDesign([]int{8}, 0.05, 0.05, 0), arms.cells, reps, draws, 1300) {
		fixed.add(o)
	}
	t.Logf("phase1-v2's fixed 8 tasks on the arm-specific shapes, %d experiments: false differences %s (t alone %s)",
		fixed.reps, rate(fixed.differencesNoFutility, fixed.reps), rate(fixed.differencesTOnly, fixed.reps))
	var fixedRecorded seqOutcome
	for _, o := range runCells(newSeqDesign([]int{8}, 0.05, 0.05, 0), recorded.cells, reps, draws, 1400) {
		fixedRecorded.add(o)
	}
	t.Logf("phase1-v2's fixed 8 tasks on the recorded differences, %d experiments: false differences %s (t alone %s)",
		fixedRecorded.reps, rate(fixedRecorded.differencesNoFutility, fixedRecorded.reps), rate(fixedRecorded.differencesTOnly, fixedRecorded.reps))
	t.Logf("null, all groups, pooled over %d experiments: false differences %s ignoring futility (t alone %s); A/A only: %s; stops at looks %v (futility obeyed)",
		all.reps, rate(all.differencesNoFutility, all.reps), rate(all.differencesTOnly, all.reps), rate(allAA.differencesNoFutility, allAA.reps), all.stopsAt)
	if r := float64(all.differencesTOnly) / float64(all.reps); r > 0.055 {
		t.Errorf("false differences (t alone) in %.2f%% of all experiments without a true difference, above 5.5%%", 100*r)
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
	reps, draws := 100*factor, simDraws(factor)
	designs := []struct {
		name string
		d    seqDesign
	}{
		{"sequential 8/12/16", newSeqDesign(SeqLooks(SeqMaxTasks), seqAlpha, SeqEquivalenceAlpha, SeqFutility)},
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
// Scenarios: true biases of 0, ±5%, +10% and ±12% (log 0.12, about 12.7%), normal and skewed noise, without day
// effects; and, with normal noise, biases of 0 and +5% with a day effect of 0.03 or 0.05: every occasion (the
// validation's stored side, its fresh side, and each experiment's stored and fresh arms) shares a common log-cost shift
// of that spread, which within-experiment intervals do not see. It logs the pass rate, how often a passing key's bias
// exceeds its allowance, false differences and power, and requires false differences at most 5% among experiments
// that ran (after a pass) for biases within ±5% without day effects.
func TestReuseValidationAndAllowance(t *testing.T) {
	factor := longFactor()
	reps, draws := 100*factor, simDraws(factor)
	d := newSeqDesign(SeqLooks(SeqMaxTasks), seqAlpha, SeqEquivalenceAlpha, 0)
	limit := math.Log(1.15)
	type res struct {
		pass, falseDiff, improved20, improved35, improved63, runs int
		tasks20, tasks35, tasks63                                 int
		exceeds                                                   int // passes whose |bias| is above the allowance
		allowance                                                 float64
	}
	type scenario struct {
		bias  float64
		shape noiseShape
		day   float64
	}
	var scenarios []scenario
	for _, shape := range []noiseShape{shapeNormal, shapeSkewed} {
		for _, bias := range []float64{0, -0.05, 0.05, 0.10, -0.12, 0.12} {
			scenarios = append(scenarios, scenario{bias, shape, 0})
		}
	}
	for _, day := range []float64{0.03, 0.05} {
		for _, bias := range []float64{0, 0.05} {
			scenarios = append(scenarios, scenario{bias, shapeNormal, day})
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
				dayStored, dayFresh := sc.day*r.NormFloat64(), sc.day*r.NormFloat64()
				for i := range 16 {
					name := fmt.Sprintf("t%02d", i)
					level := r.NormFloat64()
					for range 4 {
						tb.Add(name, "A", math.Exp(level+dayStored+sc.bias+0.19*sc.shape.draw(r)))
						tb.Add(name, "B", math.Exp(level+dayFresh+0.19*sc.shape.draw(r)))
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
				if math.Abs(sc.bias) > allowance {
					out.exceeds++
				}
				// Reuse experiments under the same bias, with this allowance.
				for _, effect := range []float64{0, 0.20, 0.35, 0.63} {
					c := seqCell{Sigma: 0.19, Tau: 0.10, Effect: math.Log(1 - effect), Shape: sc.shape, Stored: 4, Bias: sc.bias, Allowance: allowance, Day: sc.day}
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
			t.Logf("bias %+.0f%%, %s, day effect %.2f: the validation never passed in %d tries", 100*sc.bias, sc.shape, sc.day, reps)
			continue
		}
		per := func(n int) float64 { return float64(n) / float64(o.pass) }
		t.Logf("bias %+.1f%% (log %+.2f, stored against fresh), %s, day effect %.2f: validation passed %s; mean allowance %.3f; bias beyond the allowance %s of passes; after a pass, with no true effect, false differences %s, fresh runs %.1f; improved at a true 20%% %s (fresh runs %.1f), 35%% %s (%.1f), 63%% %s (%.1f)",
			100*(math.Exp(sc.bias)-1), sc.bias, sc.shape, sc.day, rate(o.pass, reps), o.allowance/float64(o.pass), rate(o.exceeds, o.pass), rate(o.falseDiff, o.pass), per(o.runs),
			rate(o.improved20, o.pass), per(o.tasks20), rate(o.improved35, o.pass), per(o.tasks35), rate(o.improved63, o.pass), per(o.tasks63))
		if sc.day == 0 && math.Abs(sc.bias) <= 0.05 && float64(o.falseDiff)/float64(o.pass) > 0.05 {
			t.Errorf("bias %+.2f, %s: false differences in %.2f%% of reuse experiments, above 5%%", sc.bias, sc.shape, 100*float64(o.falseDiff)/float64(o.pass))
		}
	}
}
