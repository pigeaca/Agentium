package stats

import (
	"errors"
	"fmt"
	"math"
)

// The drift chart of the wave-3 statistics note (§4): a self-starting two-sided CUSUM (Hawkins) on the mean log cost of
// a fixed panel of tasks, one point per check. The first DriftRefChecks points are the reference; from then on each
// point is standardized against all earlier in-control points,
//
//	U_j = Φ⁻¹(F_{t, m−1}((y_j − ȳ) / (s·√(1 + 1/m)))),
//
// exactly standard normal under normal noise whatever the panel's spread and the day effects. The chart accumulates
// S⁺ = max(0, S⁺ + U − k) and S⁻ = max(0, S⁻ − U − k) and alarms when either passes h. It alarms from the first point
// after the reference, warm-up included; "warming up" only means that no alarm is not yet evidence of no change. A
// point that does not alarm joins the baseline; the chart closes at its first alarm. All functions are pure.

const (
	DriftRefChecks   = 4   // points that form the first baseline; none is standardized
	DriftK           = 0.5 // the CUSUM's allowance, in standard deviations
	DriftH           = 6.0 // the alarm threshold; at most 5% of charts alarm falsely within 52 checks
	DriftClaimChecks = 12  // before this many points, "no alarm" is not evidence of no change
)

// ErrNonFinite is returned for a NaN or infinite point.
var ErrNonFinite = errors.New("stats: drift point is NaN or infinite")

// DriftScore is U for the point y against the in-control baseline. DriftChart guarantees at least DriftRefChecks
// finite points; a direct caller passing fewer than 2 gets NaN (no spread to standardize by).
//
// A baseline whose spread is within driftEps of zero counts as constant, whatever float rounding left of it (the mean of
// identical floats is often a hair off them): y within driftEps of the mean scores 0, and any other y the most extreme
// score the clamp allows (about ±7.9), so a cost that never varied and then moved still alarms. The epsilon suppresses
// variation below 1e-9 (relative, for costs above 1): without it a repeated cost, such as zero or a cached one, would
// score a stable ±0.8–0.95 at every check and false-alarm in most charts.
func DriftScore(baseline []float64, y float64) float64 {
	m := len(baseline)
	if m < 2 {
		return math.NaN()
	}
	mean, s := Mean(baseline), math.Sqrt(Variance(baseline))
	eps := driftEps * math.Max(1, math.Abs(mean))
	var p float64
	switch {
	case s <= eps && math.Abs(y-mean) <= eps:
		return 0
	case s <= eps && y > mean:
		p = 1
	case s <= eps:
		p = 0
	default:
		p = tCDFSigned((y-mean)/(s*math.Sqrt(1+1/float64(m))), m-1)
	}
	return NormalQuantile(math.Min(math.Max(p, driftClamp), 1-driftClamp))
}

const (
	driftEps   = 1e-9  // scale of "no variation", relative to max(1, |mean|)
	driftClamp = 1e-15 // probabilities are kept this far from 0 and 1
)

// tCDFSigned is Student's t distribution function on the whole line.
func tCDFSigned(x float64, df int) float64 {
	if x < 0 {
		return 1 - tCDF(-x, df)
	}
	return tCDF(x, df)
}

// DriftStep advances the sums by one score: it returns the new S⁺ and S⁻.
func DriftStep(up, down, u float64) (newUp, newDown float64) {
	return math.Max(0, up+u-DriftK), math.Max(0, down-u-DriftK)
}

// DriftAlarm is the alarm decision: +1 when S⁺ passes h (cost rose), -1 when S⁻ does (cost fell), else 0. If both pass,
// the larger sum names the direction.
func DriftAlarm(up, down float64) int {
	switch {
	case up > DriftH && up >= down:
		return 1
	case down > DriftH:
		return -1
	}
	return 0
}

// DriftResult is a chart read over a series of points.
type DriftResult struct {
	Points    int       // points read
	WarmingUp bool      // fewer than DriftClaimChecks points: no alarm is not yet evidence of no change
	Scores    []float64 // U_j for each point from index DriftRefChecks on (Scores[i] is point i+DriftRefChecks)
	Up, Down  []float64 // S⁺ and S⁻ after each scored point, same indexing as Scores
	Alarm     int       // +1 up, -1 down, 0 none
	AlarmAt   int       // index of the alarming point; -1 without an alarm
	// ClimbStart is the index of the point where the run of increments that led to the alarm began: where the CUSUM
	// started climbing, not an estimate of the change point; -1 without one.
	ClimbStart int
}

// DriftChart reads the points in order, stopping at the first alarm (later points are ignored and not counted in
// Points). Fewer than DriftRefChecks points give no alarm. NaN or infinite points are refused with ErrNonFinite.
func DriftChart(points []float64) (DriftResult, error) {
	for i, y := range points {
		if math.IsNaN(y) || math.IsInf(y, 0) {
			return DriftResult{}, fmt.Errorf("drift point %d: %w", i, ErrNonFinite)
		}
	}
	res := DriftResult{AlarmAt: -1, ClimbStart: -1}
	up, down := 0.0, 0.0
	for j := DriftRefChecks; j < len(points); j++ {
		u := DriftScore(points[:j], points[j])
		up, down = DriftStep(up, down, u)
		res.Scores, res.Up, res.Down = append(res.Scores, u), append(res.Up, up), append(res.Down, down)
		if dir := DriftAlarm(up, down); dir != 0 {
			res.Alarm, res.AlarmAt = dir, j
			res.Points = j + 1
			res.ClimbStart = driftClimbStart(res.Up, res.Down, dir)
			res.WarmingUp = res.Points < DriftClaimChecks
			return res, nil
		}
	}
	res.Points = len(points)
	res.WarmingUp = res.Points < DriftClaimChecks
	return res, nil
}

// driftClimbStart walks back from the alarming (last) scored point while the alarming sum was positive before it; the
// point after the last zero (or the first scored point) is where the climb began.
func driftClimbStart(up, down []float64, dir int) int {
	sums := up
	if dir < 0 {
		sums = down
	}
	i := len(sums) - 1
	for i > 0 && sums[i-1] > 0 {
		i--
	}
	return i + DriftRefChecks
}
