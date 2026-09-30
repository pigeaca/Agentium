package stats

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"testing"
)

// oneRunCell is one simulated scenario of the one-run cost design: tasks × 1 run per arm, per-run log-cost spread
// sigma, a true effect across tasks of mean effect and spread tau.
type oneRunCell struct {
	tasks       int
	tau, effect float64
}

// oneRunOutcome counts, over reps experiments, the verdicts that claim a difference and the verdict intervals (the
// wider of the bootstrap and the t-interval at 95%, as Decide uses them) that cover the true mean effect.
type oneRunOutcome struct{ reps, differences, covered int }

// simulateOneRun runs one scenario through the analysis the cost verdict uses: the two-stage bootstrap (which, with one
// run per cell, resamples tasks only), the t-interval, and Decide with a 10% margin.
func simulateOneRun(c oneRunCell, sigma float64, reps, draws int, seed uint64) oneRunOutcome {
	r := rand.New(rand.NewPCG(seed, 7))
	margin := RatioMargin(0.10, LowerIsBetter)
	out := oneRunOutcome{reps: reps}
	names := make([]string, c.tasks)
	for i := range names {
		names[i] = fmt.Sprintf("t%02d", i)
	}
	for range reps {
		tb := NewTable()
		for _, name := range names {
			level := r.NormFloat64() // task difficulty: differencing removes it
			effect := c.effect + c.tau*r.NormFloat64()
			tb.Add(name, "A", math.Exp(level+sigma*r.NormFloat64()))
			tb.Add(name, "B", math.Exp(level+effect+sigma*r.NormFloat64()))
		}
		boot, _ := NewBootstrap(tb, "A", "B", math.Log, draws, r)
		diffs := tb.Paired("A", "B", math.Log)
		t95, _ := TInterval(diffs, 0.95)
		t90, _ := TInterval(diffs, 0.90)
		e := Evidence{Boot95: boot.Percentile(0.95), T95: t95, Boot90: boot.Percentile(0.90), T90: t90}
		verdict, _ := Decide(e, LowerIsBetter, margin, false, false)
		if verdict == Improved || verdict == ImprovedSmall || verdict == Regressed {
			out.differences++
		}
		if u := widest(e.Boot95, e.T95); u.Low <= c.effect && c.effect <= u.High {
			out.covered++
		}
	}
	return out
}

// TestOneRunCostVerdictsSimulation is the gate for method phase1-v2's cost floor: tasks with at least one counted run
// in both arms, instead of three. It simulates 8, 10 and 12 tasks × 1 run per arm with Phase 0's σ = 0.19 and the
// planner's τ range, normal noise, and requires, for each τ pooled over the task counts:
//   - no true difference (an A/A, τ = 0; and a zero mean effect with τ = 0.10 or 0.25): verdicts that claim a
//     difference in at most 6% of experiments;
//   - every scenario, including a true 20% reduction: 95% verdict intervals that cover the true mean effect in at
//     least 93% of experiments.
//
// Seeded (one seed per scenario, run in parallel), so it gives the same counts every time; at the time of writing:
// differences 5.2–5.3%, coverage 94.5–95.1%. To stay fast in CI under the race detector it draws 400 bootstrap samples
// per experiment instead of the analysis's 10,000: with one run per cell the bootstrap resamples tasks only, its
// percentile interval is narrower than the t-interval for 8–12 tasks, and Decide takes the wider, so fewer draws hardly
// change a verdict. 3,000 experiments per τ put a rate within about ±0.4 points (one standard error).
func TestOneRunCostVerdictsSimulation(t *testing.T) {
	const sigma, reps, draws = 0.19, 1000, 400
	reduction := math.Log(0.8)
	type group struct {
		name  string
		cells []oneRunCell
	}
	var null, effect group
	null.name, effect.name = "no true difference", "a true 20% reduction"
	for _, n := range []int{8, 10, 12} {
		for _, tau := range []float64{0, 0.10, 0.25} {
			null.cells = append(null.cells, oneRunCell{tasks: n, tau: tau})
			if tau > 0 {
				effect.cells = append(effect.cells, oneRunCell{tasks: n, tau: tau, effect: reduction})
			}
		}
	}
	// Scenarios run in parallel, each on its own seed, so the counts do not depend on scheduling.
	groups := []group{null, effect}
	results := make([][]oneRunOutcome, len(groups))
	var wg sync.WaitGroup
	seed := uint64(1)
	for gi, g := range groups {
		results[gi] = make([]oneRunOutcome, len(g.cells))
		for ci, c := range g.cells {
			cellSeed := seed
			wg.Go(func() { results[gi][ci] = simulateOneRun(c, sigma, reps, draws, cellSeed) })
			seed++
		}
	}
	wg.Wait()
	for gi, g := range groups {
		byTau := map[float64]*oneRunOutcome{}
		var taus []float64
		for ci, c := range g.cells {
			o := results[gi][ci]
			t.Logf("%s, %d tasks, τ = %.2f: differences %.1f%%, coverage %.1f%%", g.name, c.tasks, c.tau,
				100*float64(o.differences)/float64(o.reps), 100*float64(o.covered)/float64(o.reps))
			if byTau[c.tau] == nil {
				byTau[c.tau] = &oneRunOutcome{}
				taus = append(taus, c.tau)
			}
			total := byTau[c.tau]
			total.reps += o.reps
			total.differences += o.differences
			total.covered += o.covered
		}
		// Each τ is judged on its own, pooled over 8, 10 and 12 tasks.
		for _, tau := range taus {
			total := byTau[tau]
			differences, coverage := float64(total.differences)/float64(total.reps), float64(total.covered)/float64(total.reps)
			t.Logf("%s, τ = %.2f, pooled over %d experiments: differences %.2f%%, coverage %.2f%%", g.name, tau, total.reps, 100*differences, 100*coverage)
			if g.name == null.name && differences > 0.06 {
				t.Errorf("%s, τ = %.2f: %.2f%% of experiments claim a difference, above 6%%", g.name, tau, 100*differences)
			}
			if coverage < 0.93 {
				t.Errorf("%s, τ = %.2f: 95%% intervals cover the true effect in %.2f%% of experiments, below 93%%", g.name, tau, 100*coverage)
			}
		}
	}
}
