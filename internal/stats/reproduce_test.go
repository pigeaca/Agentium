package stats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
)

type spikeRun struct {
	ID, Task, Arm, Status string
	Repeat                int
	Success               *bool
	CostUSD               float64 `json:"cost_usd"`
	WallS                 float64 `json:"wall_s"`
	OutputTokens          float64 `json:"output_tokens"`
	Turns                 float64
	SawImportRuleFailure  bool `json:"saw_import_rule_failure"`
}

type spikeSummary struct {
	Effects                map[string]map[string][3]float64 `json:"effects"`
	EffectsExcludingImport map[string]json.RawMessage       `json:"effects_excluding_import_rule_pairs"`
	Variance               map[string]float64               `json:"variance"`
	Designs                map[string]map[string]float64    `json:"designs"`
	NonInferiorityTasks    int                              `json:"noninferiority_15pp_tasks_at_5_runs"`
}

func loadSpike(t *testing.T) ([]spikeRun, spikeSummary) {
	t.Helper()
	f, err := os.Open("testdata/phase0/runs.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var runs []spikeRun
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r spikeRun
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	data, err := os.ReadFile("testdata/phase0/summary.json")
	if err != nil {
		t.Fatal(err)
	}
	var s spikeSummary
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return runs, s
}

// spikeTables builds the spike's tables: success as 0/1, and the positive values of each other metric.
func spikeTables(runs []spikeRun) map[string]*Table {
	tables := map[string]*Table{"success": NewTable(), "cost_usd": NewTable(), "wall_s": NewTable(), "output_tokens": NewTable(), "turns": NewTable()}
	for _, r := range runs {
		if r.Status == "infra" {
			continue
		}
		if r.Success != nil {
			v := 0.0
			if *r.Success {
				v = 1
			}
			tables["success"].Add(r.Task, r.Arm, v)
		}
		for key, v := range map[string]float64{"cost_usd": r.CostUSD, "wall_s": r.WallS, "output_tokens": r.OutputTokens, "turns": r.Turns} {
			if v > 0 {
				tables[key].Add(r.Task, r.Arm, v)
			}
		}
	}
	return tables
}

// round4 rounds as the spike's summary did.
func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

// The spike's t-intervals used 2.571 for t(0.975, 5), which is 2.570582: half-widths differ by about 0.02%.
const tTolerance = 0.0005

func checkEffects(t *testing.T, label string, runs []spikeRun, want map[string][3]float64) {
	t.Helper()
	tables := spikeTables(runs)
	for key, name := range map[string]string{"success": "success_diff_b_minus_a", "cost_usd": "cost_usd_geomean_ratio_b_over_a",
		"wall_s": "wall_s_geomean_ratio_b_over_a", "output_tokens": "output_tokens_geomean_ratio_b_over_a", "turns": "turns_geomean_ratio_b_over_a"} {
		transform, back := Identity, Identity
		if key != "success" {
			transform, back = math.Log, math.Exp
		}
		// The spike seeds a fresh random.Random(7) for every bootstrap.
		boot, err := NewBootstrap(tables[key], "full", "minimal", transform, 10000, newPyRandom(7))
		if err != nil {
			t.Fatal(err)
		}
		got := boot.Percentile(0.95).Map(back)
		wantBoot := want[name+"/bootstrap"]
		if round4(got.Estimate) != wantBoot[0] || round4(got.Low) != wantBoot[1] || round4(got.High) != wantBoot[2] {
			t.Errorf("%s %s bootstrap = %.4f [%.4f, %.4f], spike %v", label, name, got.Estimate, got.Low, got.High, wantBoot)
		}
		ti, err := TInterval(tables[key].Paired("full", "minimal", transform), 0.95)
		if err != nil {
			t.Fatal(err)
		}
		gotT, wantT := ti.Map(back), want[name+"/t"]
		if round4(gotT.Estimate) != wantT[0] || !near(gotT.Low, wantT[1], tTolerance*math.Max(1, wantT[1])) || !near(gotT.High, wantT[2], tTolerance*math.Max(1, wantT[2])) {
			t.Errorf("%s %s t = %.4f [%.4f, %.4f], spike %v", label, name, gotT.Estimate, gotT.Low, gotT.High, wantT)
		}
	}
}

func flatten(effects map[string]map[string][3]float64) map[string][3]float64 {
	out := map[string][3]float64{}
	for name, kinds := range effects {
		for kind, v := range kinds {
			out[name+"/"+kind] = v
		}
	}
	return out
}

// The Go port reproduces the Phase 0 spike's committed numbers from its 60 runs: the bootstrap draw for draw (with
// Python's generator), the t-intervals within the rounding of the spike's t table, and the variance components.
func TestReproducesThePhase0Spike(t *testing.T) {
	runs, summary := loadSpike(t)
	if len(runs) != 60 {
		t.Fatalf("%d runs, want 60", len(runs))
	}
	checkEffects(t, "all pairs", runs, flatten(summary.Effects))

	// The sensitivity check: without the task/repeat pairs whose minimal run hit the docs-check confound.
	confounded := map[string]bool{}
	for _, r := range runs {
		if r.Arm == "minimal" && r.SawImportRuleFailure {
			confounded[fmt.Sprint(r.Task, r.Repeat)] = true
		}
	}
	var clean []spikeRun
	for _, r := range runs {
		if !confounded[fmt.Sprint(r.Task, r.Repeat)] {
			clean = append(clean, r)
		}
	}
	excluding := map[string]map[string][3]float64{}
	for name, raw := range summary.EffectsExcludingImport {
		if name == "excluded_pairs" {
			var n int
			if err := json.Unmarshal(raw, &n); err != nil || n != len(confounded) {
				t.Errorf("excluded pairs %d, spike %s", len(confounded), raw)
			}
			continue
		}
		var kinds map[string][3]float64
		if err := json.Unmarshal(raw, &kinds); err != nil {
			t.Fatal(err)
		}
		excluding[name] = kinds
	}
	checkEffects(t, "without confounded pairs", clean, flatten(excluding))

	// Variance components, the designs they imply, and the tasks a 15 pp guard needs.
	tables := spikeTables(runs)
	repeats := 5.0
	sigma2, _ := WithinVariance(tables["cost_usd"], math.Log)
	tau2Cost := Heterogeneity(tables["cost_usd"].Paired("full", "minimal", math.Log), sigma2, repeats)
	w, _ := WithinVariance(tables["success"], Identity)
	tau2Success := Heterogeneity(tables["success"].Paired("full", "minimal", Identity), w, repeats)
	for name, got := range map[string]float64{"sigma_log_cost": math.Sqrt(sigma2), "tau_log_cost": math.Sqrt(tau2Cost), "w_success": w,
		"tau_success": math.Sqrt(tau2Success)} {
		if round4(got) != summary.Variance[name] {
			t.Errorf("%s = %.4f, spike %v", name, got, summary.Variance[name])
		}
	}
	// The spike's z values were 1.96 and 0.8416; exact quantiles move a detectable effect by at most a rounding step.
	for _, d := range []struct{ tasks, repeats int }{{12, 3}, {20, 3}, {20, 5}, {23, 5}, {40, 5}, {65, 5}} {
		want := summary.Designs[fmt.Sprintf("%dx%d", d.tasks, d.repeats)]
		if got := 100 * MDE(tau2Success, w, d.repeats, d.tasks); !near(got, want["success_mde_pp"], 0.1) {
			t.Errorf("%d×%d success MDE %.2f pp, spike %v", d.tasks, d.repeats, got, want["success_mde_pp"])
		}
		for label, tau := range map[string]float64{"measured": math.Sqrt(tau2Cost), "0.10": 0.10, "0.25": 0.25} {
			got := 100 * (1 - math.Exp(-MDE(tau*tau, sigma2, d.repeats, d.tasks)))
			if w := want["cost_mde_pct_tau_"+label]; !near(got, w, 0.1) {
				t.Errorf("%d×%d cost MDE (τ %s) %.2f%%, spike %v", d.tasks, d.repeats, label, got, w)
			}
		}
	}
	if got := NonInferiorityTasks(tau2Success, w, 5, 0.15); got != summary.NonInferiorityTasks {
		t.Errorf("tasks for a 15 pp guard at 5 runs = %d, spike %d", got, summary.NonInferiorityTasks)
	}
}
