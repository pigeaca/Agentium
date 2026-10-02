package stats

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
)

// The drift chart of the wave-3 statistics note (research, test-only): a self-starting two-sided CUSUM (Hawkins) on the
// mean log cost of a fixed panel of tasks, one run per task at each check, instead of a fresh 5% test per Claude Code
// version.
//
// Model: a panel of tasks; each check runs every task once, on its own day (a common day effect of spread gamma moves
// every run of a check); a cost change of delta (log ratio) from some check on. Because the panel is fixed, the tasks'
// levels add up to a constant and the check's mean log cost y_j carries only noise, the day, and the change. The first
// refChecks checks are the reference; from then on each y_j is standardized against all earlier in-control checks,
//
//	U_j = Φ⁻¹(F_{t, m−1}((y_j − ȳ) / (s·√(1 + 1/m))))   (m earlier checks, mean ȳ, spread s),
//
// which is exactly standard normal under normal noise whatever the panel's spread and the day effects, so no noise
// parameter is assumed. The chart runs S⁺ = max(0, S⁺ + U − k), S⁻ = max(0, S⁻ − U − k) and alarms when either
// passes h. A first version that standardized against a fixed four-check reference alarmed falsely within 52 checks in
// about 30–45% of charts at h = 5 and 6 (the reference's own error persists in every check), which is why the chart is
// self-starting.

const (
	refChecks = 4
	cusumK    = 0.5
)

type driftCell struct {
	tasks        int
	sigma, gamma float64 // per-run spread of log cost; the true day effect's spread
	shape        noiseShape
	delta        float64 // the cost change (log ratio), from check onset+1 after the reference on
	onset        int     // in-control checks after the reference before the change
	h            float64
}

// tCDFSigned is Student's t distribution function on the whole line.
func tCDFSigned(x float64, df int) float64 {
	if x < 0 {
		return 1 - tCDF(-x, df)
	}
	return tCDF(x, df)
}

// simulateDrift returns, over reps charts, the run lengths: checks from the change (or, without one, from the end of the
// reference) to the first alarm, horizon+1 for none, and 0 for a false alarm before the change.
func simulateDrift(c driftCell, reps, horizon int, seed uint64) []int {
	r := rand.New(rand.NewPCG(seed, 13))
	lengths := make([]int, reps)
	check := func(delta float64) float64 {
		day := c.gamma * r.NormFloat64()
		sum := 0.0
		for range c.tasks {
			sum += day + delta + c.sigma*c.shape.draw(r) // the task's level is a constant of the panel: left out
		}
		return sum / float64(c.tasks)
	}
	for rep := range reps {
		var series []float64
		for range refChecks {
			series = append(series, check(0))
		}
		up, down := 0.0, 0.0
		lengths[rep] = horizon + 1
		for j := 1 - c.onset; j <= horizon; j++ {
			delta := c.delta
			if j < 1 {
				delta = 0
			}
			y := check(delta)
			m := len(series)
			mean, s := Mean(series), math.Sqrt(Variance(series))
			p := tCDFSigned((y-mean)/(s*math.Sqrt(1+1/float64(m))), m-1)
			u := NormalQuantile(math.Min(math.Max(p, 1e-15), 1-1e-15))
			up, down = math.Max(0, up+u-cusumK), math.Max(0, down-u-cusumK)
			if up > c.h || down > c.h {
				lengths[rep] = max(j, 0)
				break
			}
			series = append(series, y) // in control so far: it joins the baseline
		}
	}
	return lengths
}

func runDrift(cells []driftCell, reps, horizon int, base uint64) [][]int {
	out := make([][]int, len(cells))
	var wg sync.WaitGroup
	for i, c := range cells {
		wg.Go(func() { out[i] = simulateDrift(c, reps, horizon, base+uint64(i)) })
	}
	wg.Wait()
	return out
}

// TestDriftChart gives the chart's false alarms (no change: the share of charts alarming within 52 checks, about a
// year of weekly checks, and in the long run the median run length over 2,000 checks) and its detection (a ±10%, ±20%
// or +35% change after 4 reference checks and 0, 8 or 20 more: the share alarming within 3 and 8 checks of the change,
// and the median delay), for h = 5 and 6, panels of 8 and 16 tasks, σ = 0.19 and 0.35, day effects of 0, 0.03 and
// 0.05, and normal, skewed and heavy-tailed run noise. It requires the note's h = 6 to alarm falsely within 52 checks in
// at most 5% of charts, pooled (and, in the long run, in every scenario), against about 93% (1 − 0.95⁵²) for a fresh
// 5% test per check. The default run keeps the detection scenarios to the note's (h = 6, σ = 0.19, 8 more checks).
func TestDriftChart(t *testing.T) {
	factor := longFactor()
	reps := 100 * factor
	horizon, long := 52, 52
	if factor >= 10 {
		long = 2000
	}
	var nullCells []driftCell
	for _, h := range []float64{5, 6} {
		for _, shape := range []noiseShape{shapeNormal, shapeSkewed, shapeHeavy} {
			for _, sigma := range []float64{0.19, 0.35} {
				for _, gamma := range []float64{0, 0.03, 0.05} {
					nullCells = append(nullCells, driftCell{tasks: 8, sigma: sigma, gamma: gamma, shape: shape, h: h})
				}
			}
		}
	}
	nullLengths := runDrift(nullCells, reps, long, 5000)
	pooled := map[float64][2]int{}
	for i, c := range nullCells {
		lengths := nullLengths[i]
		within := 0
		for _, l := range lengths {
			if l <= horizon {
				within++
			}
		}
		p := pooled[c.h]
		pooled[c.h] = [2]int{p[0] + within, p[1] + len(lengths)}
		sorted := slices.Sorted(slices.Values(lengths))
		median := fmt.Sprint(sorted[len(sorted)/2])
		if sorted[len(sorted)/2] > long {
			median = fmt.Sprintf("over %d", long)
		}
		t.Logf("no change, h = %.0f, %s, σ = %.2f, day effect %.2f: false alarm within %d checks %s; median run length %s",
			c.h, c.shape, c.sigma, c.gamma, horizon, rate(within, len(lengths)), median)
		if share := float64(within) / float64(len(lengths)); c.h == 6 && factor >= 10 && share > 0.05 {
			t.Errorf("h = 6, %s, σ = %.2f, day effect %.2f: false alarms within %d checks in %.1f%% of charts, above 5%%", c.shape, c.sigma, c.gamma, horizon, 100*share)
		}
	}
	for _, h := range []float64{5, 6} {
		t.Logf("no change, h = %.0f, pooled over %d charts: false alarm within %d checks %s", h, pooled[h][1], horizon, rate(pooled[h][0], pooled[h][1]))
		if share := float64(pooled[6][0]) / float64(pooled[6][1]); h == 6 && share > 0.05 {
			t.Errorf("h = 6: false alarms within %d checks in %.2f%% of charts, above 5%%", horizon, 100*share)
		}
	}
	hs, onsets, sigmas := []float64{6}, []int{8}, []float64{0.19}
	if factor >= 10 {
		hs, onsets, sigmas = []float64{5, 6}, []int{0, 8, 20}, []float64{0.19, 0.35}
	}
	var shiftCells []driftCell
	for _, h := range hs {
		for _, onset := range onsets {
			for _, tasks := range []int{8, 16} {
				for _, change := range []float64{1.10, 0.90, 1.20, 0.80, 1.35} {
					for _, sigma := range sigmas {
						shiftCells = append(shiftCells, driftCell{tasks: tasks, sigma: sigma, gamma: 0.03, delta: math.Log(change), onset: onset, h: h})
					}
				}
			}
		}
	}
	shiftLengths := runDrift(shiftCells, reps, 100, 6000)
	for i, c := range shiftCells {
		var lengths []int // charts without a false alarm before the change
		for _, l := range shiftLengths[i] {
			if l > 0 {
				lengths = append(lengths, l)
			}
		}
		within3, within8 := 0, 0
		for _, l := range lengths {
			if l <= 3 {
				within3++
			}
			if l <= 8 {
				within8++
			}
		}
		sorted := slices.Sorted(slices.Values(lengths))
		t.Logf("a %+.0f%% change after %d checks (%d reference, %d more), h = %.0f, %d tasks, σ = %.2f, day effect %.2f: alarm within 3 checks %s, within 8 %s; median delay %d checks",
			100*(math.Exp(c.delta)-1), refChecks+c.onset, refChecks, c.onset, c.h, c.tasks, c.sigma, c.gamma, rate(within3, len(lengths)), rate(within8, len(lengths)), sorted[len(sorted)/2])
	}
}
