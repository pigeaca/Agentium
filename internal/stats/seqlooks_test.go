package stats

import (
	"math"
	"slices"
	"testing"
)

// The note's table (§3): seq-v1's boundaries and nominal levels at 8, 12 and 16 tasks, from the production code.
func TestSeqLooksMatchTheNote(t *testing.T) {
	looks, err := SequentialLooks(SeqLooks(SeqMaxTasks), SeqMaxTasks, true, SeqAlpha, SeqEquivalenceAlpha)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ eff, effLevel, eq, eqLevel float64 }{
		{3.164, 0.9984, 2.538, 0.9889},
		{2.523, 0.9884, 2.016, 0.9562},
		{2.155, 0.9688, 1.720, 0.9146},
	}
	for k, w := range want {
		l := looks[k]
		if math.Abs(l.EffBound-w.eff) > 0.002 || math.Abs(l.EqBound-w.eq) > 0.002 || math.Abs(l.EffLevel-w.effLevel) > 0.0001 || math.Abs(l.EqLevel-w.eqLevel) > 0.0001 {
			t.Errorf("look %d: efficacy z %.4f (level %.5f), equivalence z %.4f (level %.5f); want %v", k+1, l.EffBound, l.EffLevel, l.EqBound, l.EqLevel, w)
		}
	}
	if f := []float64{looks[0].Fraction, looks[1].Fraction, looks[2].Fraction}; !slices.Equal(f, []float64{0.5, 0.75, 1}) {
		t.Errorf("fractions %v", f)
	}
}

func TestSeqLooksBySize(t *testing.T) {
	for _, c := range []struct {
		tasks int
		want  []int
	}{{16, []int{8, 12, 16}}, {20, []int{8, 12, 16}}, {15, []int{8, 15}}, {12, []int{8, 12}}, {11, []int{11}}, {8, []int{8}}, {3, []int{3}}} {
		if got := SeqLooks(c.tasks); !slices.Equal(got, c.want) {
			t.Errorf("SeqLooks(%d) = %v, want %v", c.tasks, got, c.want)
		}
	}
	// One look at 8 to 11 tasks is a fixed design at the sequential level: a 96.5% efficacy interval, and equivalence's
	// one-sided 5% a side (90%).
	one, err := SequentialLooks([]int{10}, 10, true, SeqAlpha, SeqEquivalenceAlpha)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(one[0].EffLevel-0.965) > 1e-6 || math.Abs(one[0].EqLevel-0.90) > 1e-6 {
		t.Errorf("one look: levels %.5f and %.5f, want 0.965 and 0.90", one[0].EffLevel, one[0].EqLevel)
	}
}

// Lost tasks (the note's §3): an interim look's fraction is its counted tasks over the planned maximum; earlier
// boundaries stay as they were used; the final look spends the remainder whatever its count.
func TestSeqLooksWithLostTasks(t *testing.T) {
	planned, err := SequentialLooks([]int{8, 12, 16}, 16, true, SeqAlpha, SeqEquivalenceAlpha)
	if err != nil {
		t.Fatal(err)
	}
	lost, err := SequentialLooks([]int{8, 11, 14}, 16, true, SeqAlpha, SeqEquivalenceAlpha)
	if err != nil {
		t.Fatal(err)
	}
	if lost[0] != planned[0] {
		t.Errorf("the first look changed with later counts: %+v, was %+v", lost[0], planned[0])
	}
	if lost[1].Fraction != 11.0/16 {
		t.Errorf("an interim look's fraction is %.4f, want 11/16", lost[1].Fraction)
	}
	interim, err := SequentialLooks([]int{8, 11}, 16, false, SeqAlpha, SeqEquivalenceAlpha)
	if err != nil {
		t.Fatal(err)
	}
	if interim[1] != lost[1] {
		t.Errorf("look 2 depends on look 3: %+v against %+v", interim[1], lost[1])
	}
	// The final look spends the remainder over the information it has, which is more correlated with the earlier looks
	// than planned: its boundary drops below the planned one.
	if final := lost[len(lost)-1].EffBound; final >= planned[2].EffBound {
		t.Errorf("the final look with 14 of 16 tasks has boundary %.4f, not below the planned %.4f", final, planned[2].EffBound)
	}
	// A skipped interim look (too few tasks) leaves spending to the next analysed look: a first analysed look at 12 of
	// 16 spends what the spending function has spent by 0.75.
	skipped, err := SequentialLooks([]int{12, 16}, 16, true, SeqAlpha, SeqEquivalenceAlpha)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := 1-skipped[0].EffLevel, 2*OBFSpending(SeqAlpha/2, 0.75); math.Abs(got-want) > 1e-6 {
		t.Errorf("a first analysed look at 12 of 16 spends %.6f, want %.6f", got, want)
	}
	for _, bad := range [][]int{{8, 8}, {12, 8}, {0}, {17}} {
		if _, err := SequentialLooks(bad, 16, true, SeqAlpha, SeqEquivalenceAlpha); err == nil {
			t.Errorf("counts %v were accepted", bad)
		}
	}
}

func TestConditionalPower(t *testing.T) {
	looks, _ := SequentialLooks([]int{8, 12, 16}, 16, true, SeqAlpha, SeqEquivalenceAlpha)
	final := looks[2].EffBound
	// At the first look, |z| below √0.5 (c₃ − z(0.90)√0.5) stops for futility at the 10% threshold.
	edge := math.Sqrt(0.5) * (final - NormalQuantile(0.90)*math.Sqrt(0.5))
	if cp := ConditionalPower(edge, 0.5, final); math.Abs(cp-0.10) > 1e-9 {
		t.Errorf("conditional power at the futility edge %.4f is %.6f, want 0.10", edge, cp)
	}
	if ConditionalPower(-edge-0.01, 0.5, final) <= 0.10 || ConditionalPower(edge-0.01, 0.5, final) >= 0.10 {
		t.Error("conditional power is not symmetric in z, or not increasing in |z|")
	}
	if ConditionalPower(3, 1, final) != 1 || ConditionalPower(1, 1, final) != 0 {
		t.Error("at the final fraction, conditional power is the crossing itself")
	}
}

// The preview's expected tasks: the note's simulations used 10.3 tasks with no effect and 13.2 at a true 20% cut
// (σ = 0.19, τ = 0.10, futility obeyed).
func TestSequentialExpectedTasks(t *testing.T) {
	looks, _ := SequentialLooks([]int{8, 12, 16}, 16, true, SeqAlpha, SeqEquivalenceAlpha)
	sd := math.Sqrt(2*0.19*0.19 + 0.10*0.10)
	none := SequentialExpectedTasks(looks, SeqFutility, 0, 20000, 1)
	cut := SequentialExpectedTasks(looks, SeqFutility, math.Log(0.8)/sd, 20000, 1)
	t.Logf("expected tasks: %.2f with no effect, %.2f at a 20%% cut", none, cut)
	if math.Abs(none-10.3) > 0.3 || math.Abs(cut-13.2) > 0.3 {
		t.Errorf("expected tasks %.2f and %.2f, want about 10.3 and 13.2", none, cut)
	}
	if again := SequentialExpectedTasks(looks, SeqFutility, 0, 20000, 1); again != none {
		t.Error("the estimate is not reproducible")
	}
	never := slices.Clone(looks)
	for k := range never {
		never[k].EffLevel = 1 - 1e-12
	}
	if all := SequentialExpectedTasks(never, 0, 0, 100, 1); all != 16 {
		t.Errorf("without stops every path runs to 16 tasks, got %.2f", all)
	}
}

// seqGolden is every seq-v1 design size's looks as this code computes them (tasks, look's tasks, efficacy z,
// equivalence z, efficacy level, equivalence level). Locks record the levels, and a resume refuses a lock whose levels
// this build computes more than 1e-6 apart (experiment's checkSequential): a change that moves them that far is a new
// method (seq-v2), not a fix, and this test fails first.
var seqGolden = [][6]float64{
	{1, 1, 2.108358399, 1.644853627, 0.965000000, 0.900000000},
	{8, 8, 2.108358399, 1.644853627, 0.965000000, 0.900000000},
	{11, 11, 2.108358399, 1.644853627, 0.965000000, 0.900000000},
	{12, 8, 2.686160103, 2.135142979, 0.992772155, 0.967250667},
	{12, 12, 2.135246880, 1.694201380, 0.967259151, 0.909772977},
	{13, 8, 2.812904795, 2.242239726, 0.995090382, 0.975054117},
	{13, 13, 2.126835611, 1.682687952, 0.966566256, 0.907564485},
	{14, 8, 2.934365088, 2.344741967, 0.996657692, 0.980959749},
	{14, 14, 2.121081853, 1.673921395, 0.966085085, 0.905853951},
	{15, 8, 3.051140279, 2.443179116, 0.997720260, 0.985441490},
	{15, 15, 2.117133251, 1.667229439, 0.965751460, 0.904531209},
	{16, 8, 3.163725458, 2.537987603, 0.998442363, 0.988850807},
	{16, 12, 2.522640501, 2.015923197, 0.988352259, 0.956191992},
	{16, 16, 2.154460000, 1.720132483, 0.968795884, 0.914591638},
}

func TestSeqLevelsGolden(t *testing.T) {
	byTasks := map[int][][6]float64{}
	for _, g := range seqGolden {
		byTasks[int(g[0])] = append(byTasks[int(g[0])], g)
	}
	for n, want := range byTasks {
		looks, err := SequentialLooks(SeqLooks(n), n, true, SeqAlpha, SeqEquivalenceAlpha)
		if err != nil || len(looks) != len(want) {
			t.Fatalf("%d tasks: %d looks, %v", n, len(looks), err)
		}
		for k, w := range want {
			got := []float64{float64(looks[k].Tasks), looks[k].EffBound, looks[k].EqBound, looks[k].EffLevel, looks[k].EqLevel}
			for i, g := range got {
				if math.Abs(g-w[i+1]) > 1e-6 {
					t.Errorf("%d tasks, look %d: field %d is %.9f, the golden value %.9f", n, k+1, i, g, w[i+1])
				}
			}
		}
	}
}
