package experiment

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/stats"
)

// synthetic makes runs for tasks × repeats × both arms: cost per arm (with a little task and run spread) and success
// per arm from pass(task, repeat, arm).
func synthetic(tasks, repeats int, costA, costB float64, pass func(task, repeat int, arm string) bool) []RunData {
	var runs []RunData
	yes, no := true, false
	for r := range repeats {
		for i := range tasks {
			for _, arm := range []string{"A", "B"} {
				cost := costA
				if arm == "B" {
					cost = costB
				}
				cost *= 1 + 0.3*float64(i%4) + 0.05*float64(r%3)
				passed := &no
				if pass(i, r, arm) {
					passed = &yes
				}
				runs = append(runs, RunData{Slot: len(runs), Task: fmt.Sprintf("t%02d", i), Arm: arm, Outcome: claude.OutcomeOK, Passed: passed,
					CostUSD: cost, DurationS: 60 * cost, OutputTokens: 1000})
			}
		}
	}
	return runs
}

func lockFor(goal string, tasks, repeats int) Lock {
	d := validDesign()
	d.Goal, d.Repeats, d.Seed = goal, repeats, 11
	d.Tasks = nil
	for i := range tasks {
		d.Tasks = append(d.Tasks, fmt.Sprintf("t%02d", i))
	}
	return Lock{Design: d}
}

func result(t *testing.T, a Analysis, name string) MetricResult {
	t.Helper()
	i := slices.IndexFunc(a.Results, func(r MetricResult) bool { return r.Metric == name })
	if i < 0 {
		t.Fatalf("no %s result in %+v", name, a.Results)
	}
	return a.Results[i]
}

func TestAnalyzeCheaperContext(t *testing.T) {
	// B costs 30% less; success is equal (and mixed, so it discriminates) on 24 tasks × 3 runs.
	runs := synthetic(24, 3, 1.0, 0.7, func(task, repeat int, _ string) bool { return (task+repeat)%3 != 0 })
	a, err := Analyze(lockFor(GoalCheaper, 24, 3), runs)
	if err != nil {
		t.Fatal(err)
	}
	cost := result(t, a, MetricCost)
	if cost.Role != RolePrimary || !cost.Ratio || cost.Verdict != stats.Improved || math.Abs(cost.Boot95.Estimate-0.7) > 1e-9 ||
		cost.Tasks != 24 || cost.Repeats != 3 || !(cost.Boot95.High < 1 && cost.T95.High < 1) {
		t.Errorf("cost = %+v", cost)
	}
	success := result(t, a, MetricSuccess)
	if success.Role != RoleGuard || success.Verdict != stats.Equivalent || success.Boot95.Estimate != 0 {
		t.Errorf("success = %+v: equal success on 24 tasks is equivalent within 15 pp", success)
	}
	if time := result(t, a, MetricTime); time.Role != RoleSecondary || time.Verdict != stats.Exploratory {
		t.Errorf("time = %+v: secondary metrics get no verdict", time)
	}
	if a.Counted["A"] != 72 || a.Counted["B"] != 72 || len(a.Excluded) != 0 || math.Abs(a.PassAt1["A"]-a.PassAt1["B"]) > 1e-12 {
		t.Errorf("counts %+v, pass@1 %+v", a.Counted, a.PassAt1)
	}
	if a.Variance == nil || a.Variance.Repeats != 3 || a.Variance.SigmaLogCost <= 0 {
		t.Errorf("variance %+v", a.Variance)
	}
	again, _ := Analyze(lockFor(GoalCheaper, 24, 3), runs)
	if again.Results[0].Boot95 != a.Results[0].Boot95 || again.Results[1].Boot95 != a.Results[1].Boot95 {
		t.Error("the same runs and seed must give the same intervals")
	}
}

func TestAnalyzeCountsOnlyFairRunsAndStrictSuccess(t *testing.T) {
	runs := synthetic(10, 3, 1.0, 1.0, func(int, int, string) bool { return true })
	runs[0].Outcome, runs[1].Outcome, runs[2].Outcome = claude.OutcomeUnfair, claude.OutcomeInfra, claude.OutcomeCancelled
	runs[3].ConfigChanged = []string{"pytest.ini"} // passed, but with the test runner changed: a failure
	a, err := Analyze(lockFor(GoalCheaper, 10, 3), runs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Excluded[claude.OutcomeUnfair] != 1 || a.Excluded[claude.OutcomeInfra] != 1 || a.Excluded[claude.OutcomeCancelled] != 1 ||
		a.Counted["A"]+a.Counted["B"] != 57 {
		t.Errorf("counted %+v, excluded %+v", a.Counted, a.Excluded)
	}
	if a.PassAt1["B"] == 1 && a.PassAt1["A"] == 1 {
		t.Error("the pass with changed runner configuration counted as a success")
	}
	// The floors count tasks with 3 runs in both arms: 8 of 10 here (two lost a run), enough for cost (8), not success (20).
	if s := result(t, a, MetricSuccess); s.Verdict != stats.Exploratory {
		t.Errorf("success below its floor: %+v", s)
	}
	if c := result(t, a, MetricCost); c.Verdict == stats.Exploratory || c.Verdict == "" || c.Repeats != 3 || c.FullTasks != 8 {
		t.Errorf("cost: %+v; want a verdict (10 tasks, 8 with 3 runs per arm; the floor is 8 tasks)", c)
	}
	fewer := slices.Clone(runs)
	fewer[4].Outcome = claude.OutcomeInfra // a third task loses a run: 7 full tasks
	if c := result(t, mustAnalyze(t, lockFor(GoalCheaper, 10, 3), fewer), MetricCost); c.Verdict != stats.Exploratory || c.FullTasks != 7 {
		t.Errorf("cost with 7 full tasks: %+v", c)
	}
	if !slices.Contains(a.NotDiscriminating, "t02") {
		t.Errorf("tasks every run passed are not discriminating: %v", a.NotDiscriminating)
	}
}

func TestAnalyzeBetterGoalAndThinData(t *testing.T) {
	// B passes where A fails on half the tasks: success improves, and is primary for the "better" goal.
	runs := synthetic(24, 3, 1.0, 1.0, func(task, _ int, arm string) bool { return arm == "B" || task%2 == 0 })
	a, err := Analyze(lockFor(GoalBetter, 24, 3), runs)
	if err != nil {
		t.Fatal(err)
	}
	if s := result(t, a, MetricSuccess); s.Role != RolePrimary || s.Verdict != stats.Improved || math.Abs(s.Boot95.Estimate-0.5) > 1e-9 {
		t.Errorf("success = %+v", s)
	}
	if c := result(t, a, MetricCost); c.Role != RoleSecondary {
		t.Errorf("cost is secondary for the better goal: %+v", c)
	}

	// One task only: no intervals, no verdicts.
	thin, err := Analyze(lockFor(GoalCheaper, 1, 3), synthetic(1, 3, 1, 0.7, func(int, int, string) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	if c := result(t, thin, MetricCost); c.Verdict != stats.Exploratory || !strings.Contains(c.Note, "fewer than two tasks") || c.Warning != "" {
		t.Errorf("one task: %+v", c)
	}

	// Noisy and small: inconclusive, with an estimate of the tasks that would resolve it.
	noisy := synthetic(10, 3, 1.0, 1.0, func(int, int, string) bool { return true })
	k := 0
	for i := range noisy {
		if noisy[i].Arm == "B" { // noise that averages out: the logs of these multipliers sum to about zero
			noisy[i].CostUSD *= []float64{0.6, 1.5, 0.8, 1.3, 0.9, 1.2, 0.7, 1.4, 1.0, 1.1, 1.6}[k%11]
			k++
		}
	}
	n, _ := Analyze(lockFor(GoalCheaper, 10, 3), noisy)
	if c := result(t, n, MetricCost); c.Verdict != stats.Inconclusive || c.TasksToResolve <= 10 {
		t.Errorf("noisy cost: %+v", c)
	}
}

// A/A: both arms from the same distribution. A difference is a false positive, which the 95% level allows in 5% of
// experiments; asking the bootstrap and the t-interval to agree makes it rarer.
func TestAnalyzeAAFindsNoDifference(t *testing.T) {
	differences := 0
	for seed := range uint64(10) {
		r := rand.New(rand.NewPCG(seed, 99))
		runs := synthetic(12, 3, 1, 1, func(int, int, string) bool { return r.Float64() < 0.7 })
		for i := range runs {
			runs[i].CostUSD *= math.Exp(0.19 * r.NormFloat64()) // Phase 0's per-run spread
		}
		a, err := Analyze(lockFor(GoalCheaper, 12, 3), runs)
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range a.Results {
			if res.Verdict == stats.Improved || res.Verdict == stats.ImprovedSmall || res.Verdict == stats.Regressed {
				differences++
				t.Logf("seed %d: %s %s", seed, res.Metric, res.Verdict)
			}
		}
	}
	if differences > 1 {
		t.Errorf("%d verdicts of a difference in 10 A/A experiments", differences)
	}
}

func mustAnalyze(t *testing.T, l Lock, runs []RunData) Analysis {
	t.Helper()
	a, err := Analyze(l, runs)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// An experiment whose every run was excluded, or whose runs report no time or tokens, still has a JSON analysis: no
// NaN, levels are null.
func TestAnalysisAlwaysEncodes(t *testing.T) {
	runs := synthetic(4, 3, 1, 1, func(int, int, string) bool { return true })
	for i := range runs {
		runs[i].Outcome = claude.OutcomeInfra
	}
	for name, rs := range map[string][]RunData{"all excluded": runs, "no tokens": synthetic(4, 3, 1, 1, func(int, int, string) bool { return true })} {
		if name == "no tokens" {
			for i := range rs {
				rs[i].OutputTokens, rs[i].DurationS = 0, 0
			}
		}
		a := mustAnalyze(t, lockFor(GoalCheaper, 4, 3), rs)
		data, err := json.Marshal(a)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if name == "all excluded" && (!strings.Contains(string(data), `"a":null`) || a.Excluded[claude.OutcomeInfra] != 24) {
			t.Errorf("%s: %s", name, data)
		}
	}
}

// Analyze's variance path gives the spike's numbers on the spike's 60 runs (its arms full and minimal as A and B).
func TestAnalyzeVarianceMatchesTheSpike(t *testing.T) {
	f, err := os.Open("../stats/testdata/phase0/runs.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var runs []RunData
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r struct {
			Task, Arm, Status string
			Success           bool
			CostUSD           float64 `json:"cost_usd"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		arm := map[string]string{"full": "A", "minimal": "B"}[r.Arm]
		passed := r.Success
		runs = append(runs, RunData{Task: r.Task, Arm: arm, Outcome: claude.OutcomeOK, Passed: &passed, CostUSD: r.CostUSD})
	}
	data, err := os.ReadFile("../stats/testdata/phase0/summary.json")
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Variance map[string]float64 `json:"variance"`
	}
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	v := mustAnalyze(t, lockFor(GoalCheaper, 6, 5), runs).Variance
	round := func(x float64) float64 { return math.Round(x*1e4) / 1e4 }
	want := summary.Variance
	if v == nil || round(v.SigmaLogCost) != want["sigma_log_cost"] || round(v.TauLogCost) != want["tau_log_cost"] ||
		round(v.WSuccess) != want["w_success"] || round(v.TauSuccess) != want["tau_success"] || v.Repeats != want["repeats"] {
		t.Errorf("variance %+v, spike %v", v, want)
	}
}
