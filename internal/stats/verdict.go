package stats

import "math"

// Direction says which way a metric is better.
type Direction int

// Directions.
const (
	HigherIsBetter Direction = iota // success
	LowerIsBetter                   // cost, time, tokens
)

// Verdicts (the study's §5.6).
const (
	Improved      = "improved"
	ImprovedSmall = "improved, but small"
	Regressed     = "regressed"
	NoLoss        = "no loss beyond the margin"
	Equivalent    = "equivalent"
	Inconclusive  = "inconclusive"
	Exploratory   = "exploratory"
)

// Margin is a metric's decision margin in the evidence's scale, per side.
type Margin struct {
	Better float64 // how far the better side reaches before an improvement stops being small
	Worse  float64 // how far the worse side may reach for "no loss beyond the margin"
}

// Symmetric is the same margin on both sides: a difference, such as 0.15 for 15 pp of success.
func Symmetric(m float64) Margin { return Margin{Better: m, Worse: m} }

// RatioMargin is a relative margin m (0.10 for 10%) in log scale: a 10% cost margin is a 10% reduction on the better
// side (−log 0.9) and a 10% increase on the worse side (log 1.1), so equivalence is a ratio within [0.90, 1.10].
func RatioMargin(m float64, dir Direction) Margin {
	if dir == LowerIsBetter {
		return Margin{Better: -math.Log(1 - m), Worse: math.Log(1 + m)}
	}
	return Margin{Better: math.Log(1 + m), Worse: -math.Log(1 - m)}
}

// Evidence is a metric's intervals, in the scale its verdict is made in: a difference, or a log ratio.
type Evidence struct {
	Boot95, T95, Boot90, T90 Interval
}

// widest spans two intervals: a bound holds only when both intervals agree on it.
func widest(a, b Interval) Interval {
	return Interval{Estimate: a.Estimate, Low: math.Min(a.Low, b.Low), High: math.Max(a.High, b.High)}
}

// Decide gives a metric's verdict. margin is in the evidence's scale; guard asks for the no-loss verdict (the success
// guard of a cheaper-context goal); belowFloor marks a design too small for verdicts on this metric. The bootstrap and
// the t-interval must agree: each bound is the wider of the two.
//   - regressed: the 95% interval excludes zero on the worse side (even a loss within the margin, as the study orders
//     the rules: a guard's "no loss beyond the margin" is for losses that cannot be told from none);
//   - improved: it excludes zero on the better side ("improved, but small" when the estimate is within the margin);
//   - equivalent: the 90% interval lies within the margins (two one-sided tests);
//   - no loss beyond the margin (guard only): the worse end of the 90% interval stays within the margin;
//   - inconclusive otherwise.
//
// Below the floor the verdict is exploratory, but a regression is still returned as a warning.
func Decide(e Evidence, dir Direction, margin Margin, guard, belowFloor bool) (verdict, warning string) {
	sign := 1.0
	if dir == LowerIsBetter {
		sign = -1
	}
	u95, u90 := widest(e.Boot95, e.T95), widest(e.Boot90, e.T90)
	better := func(i Interval) bool { return math.Min(sign*i.Low, sign*i.High) > 0 }
	worse := func(i Interval) bool { return math.Max(sign*i.Low, sign*i.High) < 0 }
	worstEnd := math.Min(sign*u90.Low, sign*u90.High) // the 90% interval's worse end, positive when better
	switch {
	case worse(u95):
		verdict = Regressed
	case better(u95) && sign*e.Boot95.Estimate < margin.Better:
		verdict = ImprovedSmall
	case better(u95):
		verdict = Improved
	case worstEnd >= -margin.Worse && math.Max(sign*u90.Low, sign*u90.High) <= margin.Better:
		verdict = Equivalent
	case guard && worstEnd >= -margin.Worse:
		verdict = NoLoss
	default:
		verdict = Inconclusive
	}
	if belowFloor {
		if verdict == Regressed {
			warning = "regressed: both 95% intervals exclude no difference, though the design is too small for a verdict"
		}
		verdict = Exploratory
	}
	return verdict, warning
}

// Consistency is pass^k: the share of tasks whose every run in the arm succeeded, next to pass@1 (the success rate).
func Consistency(t *Table, arm string) (passAll float64, tasks int) {
	all := 0
	for _, task := range t.tasks {
		values := t.cells[task][arm]
		if len(values) == 0 {
			continue
		}
		tasks++
		if Mean(values) == 1 {
			all++
		}
	}
	if tasks == 0 {
		return math.NaN(), 0
	}
	return float64(all) / float64(tasks), tasks
}

// NotDiscriminating lists the tasks whose runs in both arms all succeeded or all failed: they carry no information on
// success (they stay in for cost).
func NotDiscriminating(t *Table, a, b string) []string {
	var out []string
	for _, task := range t.paired(a, b) {
		values := append(t.Cell(task, a), t.Cell(task, b)...)
		if m := Mean(values); m == 0 || m == 1 {
			out = append(out, task)
		}
	}
	return out
}
