package stats

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestQuantiles(t *testing.T) {
	for _, c := range []struct {
		p    float64
		df   int
		want float64
	}{{0.975, 1, 12.706205}, {0.975, 5, 2.570582}, {0.975, 29, 2.045230}, {0.95, 10, 1.812461}, {0.95, 4, 2.131847}, {0.975, 100000, 1.959988}} {
		if got := TQuantile(c.p, c.df); !near(got, c.want, 1e-6) {
			t.Errorf("TQuantile(%v, %d) = %.6f, want %.6f", c.p, c.df, got, c.want)
		}
	}
	if got := TQuantile(0.025, 5); !near(got, -2.570582, 1e-6) {
		t.Errorf("lower tail: %v", got)
	}
	if !math.IsNaN(TQuantile(0.975, 0)) || !math.IsNaN(TQuantile(1, 5)) {
		t.Error("invalid arguments give NaN")
	}
	for _, c := range []struct{ z, p float64 }{{zPower, 0.80}, {zTwoSided, 0.975}, {zOneSided, 0.95}} {
		if !near(c.z, NormalQuantile(c.p), 1e-12) {
			t.Errorf("z(%v) = %v, NormalQuantile says %v", c.p, c.z, NormalQuantile(c.p))
		}
	}
}

// The test's generator matches Python's random.Random(7), bit for bit.
func TestPyRandomMatchesPython(t *testing.T) {
	r := newPyRandom(7)
	for i, want := range []uint32{1390851128, 4071050724, 647892279} { // random.Random(7).getrandbits(32)
		if got := r.uint32(); got != want {
			t.Fatalf("draw %d = %d, Python gives %d", i, got, want)
		}
	}
	r = newPyRandom(7)
	var got []int
	for _, n := range []int{6, 5, 5, 3, 1000} { // random.Random(7).choice(range(n))
		got = append(got, r.IntN(n))
	}
	if !slices.Equal(got, []int{2, 1, 3, 2, 49}) {
		t.Errorf("choices %v, Python gives [2 1 3 2 49]", got)
	}
}

func TestTablePairedAndVariance(t *testing.T) {
	tb := NewTable()
	for _, o := range []struct {
		task, arm string
		v         float64
	}{{"b", "A", 1}, {"a", "A", 2}, {"a", "B", 4}, {"b", "B", 3}, {"b", "B", 5}, {"c", "A", 9}} {
		tb.Add(o.task, o.arm, o.v)
	}
	if !slices.Equal(tb.Tasks(), []string{"b", "a", "c"}) {
		t.Errorf("tasks in first-added order: %v", tb.Tasks())
	}
	if d := tb.Paired("A", "B", Identity); !slices.Equal(d, []float64{3, 2}) { // c has no B
		t.Errorf("Paired = %v", d)
	}
	if v, ok := WithinVariance(tb, Identity); !ok || v != 2 { // only cell b/B has two runs: variance of {3, 5}
		t.Errorf("WithinVariance = %v, %v", v, ok)
	}
	if !math.IsNaN(Variance([]float64{1})) || Variance([]float64{1, 3}) != 2 {
		t.Error("Variance")
	}
	if Heterogeneity([]float64{3, 2}, 2, 1) != 0 || !near(Heterogeneity([]float64{0, 4}, 1, 2), 7, 1e-12) {
		t.Error("Heterogeneity: variance of differences less 2·within/repeats, floored at zero")
	}
}

func TestBootstrapAndTInterval(t *testing.T) {
	tb := NewTable()
	for i, task := range []string{"a", "b", "c", "d", "e", "f"} {
		for r := range 3 {
			tb.Add(task, "A", float64(i+r))
			tb.Add(task, "B", float64(i+r)+0.5+0.1*float64(r))
		}
	}
	boot, err := NewBootstrap(tb, "A", "B", Identity, 2000, rand.New(rand.NewPCG(1, 2)))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := NewBootstrap(tb, "A", "B", Identity, 2000, rand.New(rand.NewPCG(1, 2)))
	if !slices.Equal(boot.Draws, again.Draws) || !slices.IsSorted(boot.Draws) || len(boot.Draws) != 2000 {
		t.Fatal("draws must be sorted and reproducible for a seed")
	}
	i95, i90 := boot.Percentile(0.95), boot.Percentile(0.90)
	if !near(i95.Estimate, 0.6, 1e-12) || i95.Low > i90.Low || i95.High < i90.High || i95.Low > 0.6 || i95.High < 0.6 {
		t.Errorf("95%% %+v, 90%% %+v", i95, i90)
	}
	if i95.Low != boot.Draws[50] || i95.High != boot.Draws[1949] {
		t.Error("percentile indices differ from the spike's")
	}
	draws := make([]float64, 10000)
	for i := range draws {
		draws[i] = float64(i)
	}
	big := Bootstrap{Draws: draws}
	if p95, p90 := big.Percentile(0.95), big.Percentile(0.90); p95.Low != 250 || p95.High != 9749 || p90.Low != 500 || p90.High != 9499 {
		t.Errorf("10,000 draws: 95%% [%v, %v], 90%% [%v, %v]; want the 251st/9,750th and 501st/9,500th", p95.Low, p95.High, p90.Low, p90.High)
	}
	ti, err := TInterval([]float64{1, 2, 3, 4}, 0.95)
	half := TQuantile(0.975, 3) * math.Sqrt(Variance([]float64{1, 2, 3, 4})/4)
	if err != nil || ti.Estimate != 2.5 || !near(ti.Low, 2.5-half, 1e-12) || !near(ti.High, 2.5+half, 1e-12) {
		t.Errorf("TInterval = %+v, %v", ti, err)
	}
	if _, err := TInterval([]float64{1}, 0.95); err == nil {
		t.Error("one task has no t-interval")
	}
	if _, err := NewBootstrap(NewTable(), "A", "B", Identity, 10, rand.New(rand.NewPCG(1, 2))); !errors.Is(err, ErrNoPairs) {
		t.Errorf("empty table: %v", err)
	}
	if r := (Interval{Estimate: 0, Low: -1, High: 1}).Map(math.Exp); r.Estimate != 1 || !near(r.Low, 1/math.E, 1e-15) {
		t.Errorf("Map = %+v", r)
	}
}

func TestPlanningFormulas(t *testing.T) {
	// The Confident tier: 23 tasks × 5 runs certify no success loss beyond about 15 pp (τ = 0.05, w = 0.20).
	if g := GuardMargin(0.05*0.05, 0.20, 5, 23); !near(g, 0.1493, 0.0005) {
		t.Errorf("GuardMargin = %v", g)
	}
	if n := NonInferiorityTasks(0.05*0.05, 0.20, 5, 0.15); n != 23 {
		t.Errorf("NonInferiorityTasks = %d, want 23", n)
	}
	if m := MDE(0.05*0.05, 0.20, 3, 12); !near(m, 0.298, 0.001) {
		t.Errorf("MDE = %v, the study's 29.8 pp", m)
	}
	// A 0.12 half-width at 10 tasks; a 0.0953 target needs 0.0667 (0.0953 × 1.96 / 2.80): 10 × (0.12 / 0.0667)² = 32.4.
	if n, err := TasksToResolve(10, 0.12, math.Log(1.1)); err != nil || n != 33 {
		t.Errorf("TasksToResolve = %d, %v", n, err)
	}
	// Narrow enough already, yet inconclusive (an estimate between zero and the margin): more than it has.
	if n, _ := TasksToResolve(20, 0.1, 0.15); n != 21 {
		t.Errorf("TasksToResolve(20, 0.1, 0.15) = %d, want 21", n)
	}
	if _, err := TasksToResolve(1, 0.1, 0.05); err == nil {
		t.Error("one task cannot say how many more")
	}
}

func interval(est, lo, hi float64) Interval { return Interval{Estimate: est, Low: lo, High: hi} }

func TestDecide(t *testing.T) {
	same := func(est, lo95, hi95, lo90, hi90 float64) Evidence {
		return Evidence{Boot95: interval(est, lo95, hi95), T95: interval(est, lo95, hi95), Boot90: interval(est, lo90, hi90), T90: interval(est, lo90, hi90)}
	}
	for _, c := range []struct {
		name       string
		e          Evidence
		dir        Direction
		margin     float64
		guard      bool
		belowFloor bool
		want       string
		warn       bool
	}{
		{"cost down", same(-0.2, -0.3, -0.1, -0.28, -0.12), LowerIsBetter, 0.0953, false, false, Improved, false},
		{"cost down, small", same(-0.05, -0.08, -0.02, -0.07, -0.03), LowerIsBetter, 0.0953, false, false, ImprovedSmall, false},
		// A 9.5% reduction is within a 10% margin: small, though log 0.905 reaches past log 1.1.
		{"cost down 9.5%", same(math.Log(0.905), math.Log(0.88), math.Log(0.93), math.Log(0.89), math.Log(0.92)), LowerIsBetter, 0, false, false, ImprovedSmall, false},
		// Equivalence for cost is a ratio within [0.90, 1.10].
		{"cost equivalent", same(math.Log(0.97), math.Log(0.905), math.Log(1.04), math.Log(0.91), math.Log(1.03)), LowerIsBetter, 0, false, false, Equivalent, false},
		{"cost not equivalent", same(math.Log(0.97), math.Log(0.88), math.Log(1.04), math.Log(0.895), math.Log(1.03)), LowerIsBetter, 0, false, false, Inconclusive, false},
		{"cost up", same(0.2, 0.1, 0.3, 0.12, 0.28), LowerIsBetter, 0.0953, false, false, Regressed, false},
		{"success up", same(0.2, 0.05, 0.35, 0.08, 0.32), HigherIsBetter, 0.15, false, false, Improved, false},
		{"success down", same(-0.2, -0.35, -0.05, -0.32, -0.08), HigherIsBetter, 0.15, true, false, Regressed, false},
		{"equivalent", same(0.01, -0.12, 0.14, -0.1, 0.12), HigherIsBetter, 0.15, true, false, Equivalent, false},
		{"no loss", same(0.1, -0.12, 0.3, -0.1, 0.28), HigherIsBetter, 0.15, true, false, NoLoss, false},
		{"no loss is the guard's only", same(0.1, -0.12, 0.3, -0.1, 0.28), HigherIsBetter, 0.15, false, false, Inconclusive, false},
		{"inconclusive", same(0, -0.3, 0.3, -0.25, 0.25), HigherIsBetter, 0.15, true, false, Inconclusive, false},
		{"below the floor", same(-0.2, -0.3, -0.1, -0.28, -0.12), LowerIsBetter, 0.0953, false, true, Exploratory, false},
		{"a regression below the floor warns", same(0.2, 0.1, 0.3, 0.12, 0.28), LowerIsBetter, 0.0953, false, true, Exploratory, true},
		// The bootstrap excludes zero but the t-interval does not: no verdict either way.
		{"intervals disagree", Evidence{Boot95: interval(-0.2, -0.3, -0.1), T95: interval(-0.2, -0.35, 0.02), Boot90: interval(-0.2, -0.28, -0.12),
			T90: interval(-0.2, -0.31, -0.05)}, LowerIsBetter, 0.0953, false, false, Inconclusive, false},
	} {
		margin := Symmetric(c.margin)
		if c.dir == LowerIsBetter {
			margin = RatioMargin(0.10, LowerIsBetter)
		}
		verdict, warning := Decide(c.e, c.dir, margin, c.guard, c.belowFloor)
		if verdict != c.want || (warning != "") != c.warn {
			t.Errorf("%s: %q, warning %q; want %q (warning %v)", c.name, verdict, warning, c.want, c.warn)
		}
	}
}

func TestConsistencyAndDiscrimination(t *testing.T) {
	tb := NewTable()
	for _, o := range []struct {
		task, arm string
		v         float64
	}{{"easy", "A", 1}, {"easy", "A", 1}, {"easy", "B", 1}, {"easy", "B", 1}, {"mixed", "A", 1}, {"mixed", "A", 0}, {"mixed", "B", 1},
		{"mixed", "B", 1}, {"hard", "A", 0}, {"hard", "B", 0}} {
		tb.Add(o.task, o.arm, o.v)
	}
	if all, n := Consistency(tb, "A"); n != 3 || !near(all, 1.0/3, 1e-12) {
		t.Errorf("pass^k A = %v of %d", all, n)
	}
	if all, _ := Consistency(tb, "B"); !near(all, 2.0/3, 1e-12) {
		t.Errorf("pass^k B = %v", all)
	}
	if got := NotDiscriminating(tb, "A", "B"); !slices.Equal(got, []string{"easy", "hard"}) {
		t.Errorf("NotDiscriminating = %v", got)
	}
}
