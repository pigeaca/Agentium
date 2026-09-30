package stats

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestChiSquareQuantile(t *testing.T) {
	for _, c := range []struct {
		p    float64
		df   int
		want float64
	}{{0.975, 1, 5.023886}, {0.025, 1, 0.000982}, {0.975, 5, 12.832502}, {0.025, 5, 0.831212}, {0.975, 11, 21.920049},
		{0.025, 11, 3.815748}, {0.5, 2, 1.386294}, {0.99, 29, 49.587884}, {0.95, 2, 5.991465}, {0.025, 100, 74.221927}} {
		if got := ChiSquareQuantile(c.p, c.df); !near(got, c.want, 1e-5*math.Max(1, c.want)) {
			t.Errorf("ChiSquareQuantile(%v, %d) = %.6f, want %.6f", c.p, c.df, got, c.want)
		}
	}
	if !math.IsNaN(ChiSquareQuantile(0.5, 0)) || !math.IsNaN(ChiSquareQuantile(1, 3)) {
		t.Error("invalid arguments give NaN")
	}
}

func TestVarianceAndHeterogeneityRanges(t *testing.T) {
	// s² = 0.04 on 5 degrees of freedom: [5·0.04/12.8325, 5·0.04/0.8312].
	low, high := VarianceInterval(0.04, 5, 0.95)
	if !near(low, 0.015585, 1e-5) || !near(high, 0.240612, 1e-5) {
		t.Errorf("VarianceInterval = [%v, %v]", low, high)
	}
	// Known within (dfW = 0): var(d)'s interval at the level, less 2·within/R.
	low, high = HeterogeneityRange(0.1, 11, 0.0361, 0, 1, 0.95)
	dLow, dHigh := VarianceInterval(0.1, 11, 0.95)
	if !near(low, math.Max(0, dLow-0.0722), 1e-12) || !near(high, dHigh-0.0722, 1e-12) {
		t.Errorf("HeterogeneityRange with σ known = [%v, %v]", low, high)
	}
	// Measured within: both intervals at 97.5%, bounds from the rectangle's corners.
	low, high = HeterogeneityRange(0.1, 11, 0.0361, 40, 3, 0.95)
	dLow, dHigh = VarianceInterval(0.1, 11, 0.975)
	wLow, wHigh := VarianceInterval(0.0361, 40, 0.975)
	if !near(low, math.Max(0, dLow-2*wHigh/3), 1e-12) || !near(high, dHigh-2*wLow/3, 1e-12) || low > high {
		t.Errorf("HeterogeneityRange = [%v, %v]", low, high)
	}
	if low, _ := HeterogeneityRange(0.01, 5, 0.0361, 0, 1, 0.95); low != 0 {
		t.Errorf("τ² is floored at zero: %v", low)
	}
}

func TestPooledWithinAndResample(t *testing.T) {
	tb := NewTable()
	for _, o := range []struct {
		task, arm string
		v         float64
	}{{"a", "A", 1}, {"a", "A", 3}, {"a", "B", 2}, {"b", "A", 5}, {"b", "B", 4}, {"b", "B", 8}, {"b", "B", 6}} {
		tb.Add(o.task, o.arm, o.v)
	}
	if v, dof := PooledWithin(tb, Identity); dof != 3 || !near(v, (2*1+4*2)/3.0, 1e-12) {
		t.Errorf("PooledWithin = %v on %d", v, dof)
	}
	if _, dof := PooledWithin(NewTable(), Identity); dof != 0 {
		t.Error("an empty table has no degrees of freedom")
	}
	r := Resample(tb, rand.New(rand.NewPCG(1, 2)))
	if len(r.Tasks()) != 2 || len(r.Paired("A", "B", Identity)) != 2 {
		t.Errorf("a resample keeps the task count: %v", r.Tasks())
	}
	// A statistic that does not depend on which tasks were drawn has a degenerate range.
	low, high := BootstrapRange(tb, func(t *Table) float64 { return float64(len(t.Tasks())) }, 50, 0.95, rand.New(rand.NewPCG(3, 4)))
	if low != 2 || high != 2 {
		t.Errorf("BootstrapRange = [%v, %v]", low, high)
	}
}
