package stats

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
)

// Simulation of the drift chart of the wave-3 statistics note, on production code (drift.go): a self-starting two-sided CUSUM (Hawkins) on the
// mean log cost of a fixed panel of tasks, one run per task at each check, instead of a fresh 5% test per Claude Code
// version.
//
// Model: a panel of tasks; each check runs every task once, on its own day (a common day effect of spread gamma moves
// every run of a check); a cost change of delta (log ratio) from some check on. Because the panel is fixed, the tasks'
// levels add up to a constant and the check's mean log cost y_j carries only noise, the day, and the change. The first
// DriftRefChecks checks are the reference; from then on each y_j is standardized against all earlier in-control checks,
//
//	U_j = Φ⁻¹(F_{t, m−1}((y_j − ȳ) / (s·√(1 + 1/m))))   (m earlier checks, mean ȳ, spread s),
//
// which is exactly standard normal under normal noise whatever the panel's spread and the day effects, so no noise
// parameter is assumed. The chart runs S⁺ = max(0, S⁺ + U − k), S⁻ = max(0, S⁻ − U − k) and alarms when either
// passes h. A first version that standardized against a fixed four-check reference alarmed falsely within 52 checks in
// about 30–45% of charts at h = 5 and 6 (the reference's own error persists in every check), which is why the chart is
// self-starting.

type driftCell struct {
	tasks        int
	sigma, gamma float64 // per-run spread of log cost; the true day effect's spread
	shape        noiseShape
	delta        float64 // the cost change (log ratio), from check onset+1 after the reference on
	onset        int     // in-control checks after the reference before the change
	h            float64
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
		for range DriftRefChecks {
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
			up, down = DriftStep(up, down, DriftScore(series, y))
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
		if share := float64(within) / float64(len(lengths)); c.h == 6 && factor >= gateFactor && share > 0.05 {
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
			100*(math.Exp(c.delta)-1), DriftRefChecks+c.onset, DriftRefChecks, c.onset, c.h, c.tasks, c.sigma, c.gamma, rate(within3, len(lengths)), rate(within8, len(lengths)), sorted[len(sorted)/2])
	}
}

// noisySeries is n points of N(level, sd) from a fixed seed.
func noisySeries(seed uint64, n int, level, sd float64) []float64 {
	r := rand.New(rand.NewPCG(seed, 7))
	out := make([]float64, n)
	for i := range out {
		out[i] = level + sd*r.NormFloat64()
	}
	return out
}

func TestDriftScore(t *testing.T) {
	base := []float64{1, 2, 3, 4, 5}
	if u := DriftScore(base, 3); math.Abs(u) > 1e-9 {
		t.Errorf("a point at the mean scores %v, want 0", u)
	}
	hi, lo := DriftScore(base, 6), DriftScore(base, 0)
	if hi <= 0 || math.Abs(hi+lo) > 1e-9 {
		t.Errorf("scores %v and %v for mirrored points: want opposite signs and equal size", hi, lo)
	}
	// y = mean + s·√(1+1/m)·t with t the 95th percentile of t(4) (2.1318; m = 5 gives 4 degrees of freedom) has
	// probability 0.95, so its score is Φ⁻¹(0.95).
	s := math.Sqrt(Variance(base))
	if u := DriftScore(base, 3+s*math.Sqrt(1.2)*2.1318); math.Abs(u-NormalQuantile(0.95)) > 1e-3 {
		t.Errorf("score at the t(4) 95th percentile = %v, want %v", u, NormalQuantile(0.95))
	}
}

func TestDriftScoreConstantBaseline(t *testing.T) {
	flat := []float64{2, 2, 2, 2}
	if u := DriftScore(flat, 2); u != 0 {
		t.Errorf("same value on a constant baseline scores %v, want 0", u)
	}
	if u := DriftScore(flat, 2.001); u < 7 {
		t.Errorf("a move off a constant baseline scores %v, want the clamp's extreme (above 7)", u)
	}
	if u := DriftScore(flat, 1.9); u > -7 {
		t.Errorf("a drop off a constant baseline scores %v, want below -7", u)
	}
}

func TestDriftChartConstantAndShort(t *testing.T) {
	res, err := DriftChart([]float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	if err != nil || res.Alarm != 0 || res.AlarmAt != -1 || res.ClimbStart != -1 || res.WarmingUp || res.Points != 14 {
		t.Errorf("constant points: %+v, %v; want no alarm, past warm-up", res, err)
	}
	for _, n := range []int{0, 1, DriftRefChecks} {
		res, err := DriftChart(make([]float64, n))
		if err != nil || res.Alarm != 0 || len(res.Scores) != 0 || !res.WarmingUp {
			t.Errorf("%d points: %+v, %v; want warming up, nothing scored", n, res, err)
		}
	}
	res, err = DriftChart([]float64{1, 1, 1, 1, 1.01})
	if err != nil || res.Alarm != 1 || res.AlarmAt != 4 {
		t.Errorf("a move off constant points: %+v, %v; want an upward alarm at point 4", res, err)
	}
}

func TestDriftChartRefusesNonFinite(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		pts := noisySeries(1, 10, 0, 0.1)
		pts[6] = bad
		if _, err := DriftChart(pts); !errors.Is(err, ErrNonFinite) {
			t.Errorf("point %v: error %v, want ErrNonFinite", bad, err)
		}
	}
}

func TestDriftAlarm(t *testing.T) {
	for _, c := range []struct {
		up, down float64
		want     int
	}{{0, 0, 0}, {DriftH, 0, 0}, {6.1, 0, 1}, {0, 6.1, -1}, {7, 6.5, 1}, {6.5, 7, -1}} {
		if got := DriftAlarm(c.up, c.down); got != c.want {
			t.Errorf("DriftAlarm(%v, %v) = %d, want %d", c.up, c.down, got, c.want)
		}
	}
}

// Accumulation counts during warm-up: an outlier right after the reference alarms, and the result says the chart is
// still warming up.
func TestDriftChartAlarmsDuringWarmUp(t *testing.T) {
	// One outlier scores at most about 6, so its sum (5.5) stays under h; a second rise on top of it alarms, at point 5.
	res, err := DriftChart([]float64{0, 0.1, -0.1, 0.05, 100, 1e4})
	if err != nil || res.Alarm != 1 || res.AlarmAt != 5 || res.ClimbStart != 4 || !res.WarmingUp || res.Points != 6 {
		t.Errorf("outliers after the reference: %+v, %v; want an upward alarm at point 5, climbing since 4, while warming up", res, err)
	}
	// Twelve quiet points: no alarm, and warm-up is over (no claim before that).
	for n, want := range map[int]bool{DriftClaimChecks - 1: true, DriftClaimChecks: false} {
		res, err := DriftChart(noisySeries(3, n, 0, 0.1))
		if err != nil || res.Alarm != 0 || res.WarmingUp != want {
			t.Errorf("%d quiet points: %+v, %v; want no alarm and WarmingUp = %v", n, res, err, want)
		}
	}
}

func TestDriftChartStepChange(t *testing.T) {
	for _, shift := range []float64{1.5, -1.5} { // ±15 standard deviations of the noise
		series := append(noisySeries(11, 14, 0, 0.1), noisySeries(12, 8, shift, 0.1)...)
		res, err := DriftChart(series)
		want := 1
		if shift < 0 {
			want = -1
		}
		if err != nil || res.Alarm != want {
			t.Fatalf("shift %v: %+v, %v; want alarm %d", shift, res, err, want)
		}
		if res.AlarmAt < 14 || res.AlarmAt > 16 {
			t.Errorf("shift %v: alarm at point %d, want within 2 points of the change at 14", shift, res.AlarmAt)
		}
		// Chance in the quiet stretch may have started the climb earlier, but the sum was zero just before it began.
		sums := res.Up
		if shift < 0 {
			sums = res.Down
		}
		if k := res.ClimbStart; k < DriftRefChecks || k > res.AlarmAt || (k > DriftRefChecks && sums[k-DriftRefChecks-1] != 0) || sums[k-DriftRefChecks] == 0 {
			t.Errorf("shift %v: climb began at %d (alarm %d), not at a zero-to-positive step", shift, k, res.AlarmAt)
		}
		if res.WarmingUp || res.Points != res.AlarmAt+1 || len(res.Scores) != res.Points-DriftRefChecks {
			t.Errorf("shift %v: points %d, scores %d, warming up %v", shift, res.Points, len(res.Scores), res.WarmingUp)
		}
	}
	// Points after the alarm are ignored.
	short, _ := DriftChart(append(noisySeries(11, 14, 0, 0.1), noisySeries(12, 3, 1.5, 0.1)...))
	long, _ := DriftChart(append(noisySeries(11, 14, 0, 0.1), noisySeries(12, 30, 1.5, 0.1)...))
	if short.AlarmAt != long.AlarmAt || short.ClimbStart != long.ClimbStart {
		t.Errorf("later points changed the alarm: %+v vs %+v", short, long)
	}
}

func TestDriftClimbStart(t *testing.T) {
	// S⁺ was last zero at scored index 3 (point 7); the climb began at scored index 4 (point 8). An earlier, abandoned
	// climb and the other sum do not matter.
	up := []float64{0.4, 0, 0.2, 0, 0.3, 2, 4, 7}
	down := []float64{0, 3, 3, 3, 3, 3, 3, 3}
	if got := driftClimbStart(up, down, 1); got != 4+DriftRefChecks {
		t.Errorf("upward climb start = %d, want %d", got, 4+DriftRefChecks)
	}
	if got := driftClimbStart(up, down, -1); got != 1+DriftRefChecks {
		t.Errorf("downward climb start = %d, want %d", got, 1+DriftRefChecks)
	}
	if got := driftClimbStart([]float64{7}, []float64{0}, 1); got != DriftRefChecks {
		t.Errorf("a first-point alarm starts at %d, want %d", got, DriftRefChecks)
	}
}
