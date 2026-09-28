package experiment

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/task"
)

// The formula reproduces the study's table (§5.6), which assumed τ = 0.05 for success and σ = 0.35, τ = 0.10 for cost.
func TestMDEReproducesTheStudysTable(t *testing.T) {
	for _, c := range []struct {
		tasks, repeats int
		success, cost  float64 // pp and %, as the study printed them
	}{{8, 3, 36.5, 25.9}, {12, 3, 29.8, 21.7}, {20, 3, 23.1, 17.3}, {20, 5, 18.0, 14.1}, {40, 5, 12.7, 10.2}, {65, 5, 10.0, 8.1}} {
		success := 100 * mde(zTwoSided+zPower, 0.05, WSuccess, c.tasks, c.repeats)
		cost := 100 * (1 - math.Exp(-mde(zTwoSided+zPower, 0.10, 0.35*0.35, c.tasks, c.repeats)))
		if math.Abs(success-c.success) > 0.05 || math.Abs(cost-c.cost) > 0.05 {
			t.Errorf("%d × %d: success %.1f pp (study %.1f), cost %.1f%% (study %.1f)", c.tasks, c.repeats, success, c.success, cost, c.cost)
		}
	}
	// The Confident tier's guard: 23 tasks × 5 runs certify no success loss beyond about 15 pp.
	if guard := 100 * mde(zOneSided+zPower, 0.05, WSuccess, 23, 5); math.Abs(guard-14.9) > 0.05 {
		t.Errorf("23 × 5 guard = %.1f pp, the study sized it at 15", guard)
	}
}

// With the planning defaults, the Quick tier detects the 12–21% cost changes the Phase 0 results reported (there with
// τ from 0.05; here from 0.10, so the low end is 14%).
func TestDetectWithPlanningDefaults(t *testing.T) {
	d := Detect(12, 3)
	want := Detectable{Cost: [2]float64{0.1386, 0.2117}, Success: [2]float64{0.3060, 0.3577}, Guard: [2]float64{0.2716, 0.3175}}
	for i := range 2 {
		for _, pair := range [][2]float64{{d.Cost[i], want.Cost[i]}, {d.Success[i], want.Success[i]}, {d.Guard[i], want.Guard[i]}} {
			if math.Abs(pair[0]-pair[1]) > 0.0001 {
				t.Fatalf("Detect(12, 3) = %+v, want about %+v", d, want)
			}
		}
	}
	if d.Cost[0] >= d.Cost[1] {
		t.Error("a wider spread across tasks must need a larger effect")
	}
	if more := Detect(23, 5); more.Cost[1] >= d.Cost[1] || more.Success[1] >= d.Success[1] {
		t.Error("more tasks and runs must detect smaller effects")
	}
	if (Detect(0, 3) != Detectable{}) {
		t.Error("no tasks detect nothing")
	}
}

func TestExploratoryFloors(t *testing.T) {
	for _, c := range []struct {
		tasks, repeats int
		want           []string
	}{{23, 5, nil}, {12, 3, []string{"success"}}, {7, 5, []string{"cost", "success"}}, {30, 2, []string{"cost", "success"}}} {
		if got := Exploratory(c.tasks, c.repeats); !slices.Equal(got, c.want) {
			t.Errorf("Exploratory(%d, %d) = %v, want %v", c.tasks, c.repeats, got, c.want)
		}
	}
}

func TestEstimateRun(t *testing.T) {
	past := EstimateRun("claude-sonnet-5", []float64{0.9, 0.2, 0.4, 0.3})
	if !past.Known || past.PerRunUSD != 0.35 || !strings.Contains(past.Basis, "median of this project's 4") {
		t.Errorf("past runs: %+v", past)
	}
	// Too few earlier runs: the default profile, with Claude Code's one-hour cache writes.
	profile := EstimateRun("claude-sonnet-5", []float64{0.3})
	if !profile.Known || math.Abs(profile.PerRunUSD-1.612) > 1e-9 || !strings.Contains(profile.Basis, "list prices") {
		t.Errorf("profile: %+v", profile)
	}
	if unknown := EstimateRun("sonnet", nil); unknown.Known || !strings.Contains(unknown.Basis, "no list price") {
		t.Errorf("an alias has no price: %+v", unknown)
	}
	if got := DefaultBudget(72, profile); got != 146 {
		t.Errorf("DefaultBudget = %v, want 146 (1.25 × 72 × $1.612, rounded up)", got)
	}
	if DefaultBudget(72, Estimate{}) != 0 {
		t.Error("an unknown estimate has no default budget")
	}
}

func TestPreviewLimitsTiersToEligibleTasks(t *testing.T) {
	d := validDesign()
	d.Tasks, d.Repeats = []string{"a", "b", "c", "d", "e"}, 4
	rows := Preview(d, 17, Estimate{PerRunUSD: 0.5, Known: true})
	if len(rows) != 3 || rows[0].Name != "Quick" || rows[1].Name != "Confident" || rows[2].Name != "This experiment" {
		t.Fatalf("rows = %+v", rows)
	}
	quick, confident, own := rows[0], rows[1], rows[2]
	if quick.Tasks != 12 || quick.Short || quick.Runs != 72 || quick.CostUSD != 36 || quick.WorstUSD != 216 {
		t.Errorf("quick = %+v", quick)
	}
	if confident.Tasks != 17 || !confident.Short || confident.Runs != 170 {
		t.Errorf("confident, with only 17 eligible tasks = %+v", confident)
	}
	if own.Tasks != 5 || own.Repeats != 4 || own.Runs != 40 || !slices.Equal(own.Exploratory, []string{"cost", "success"}) {
		t.Errorf("own = %+v", own)
	}
	if unknown := Preview(d, 17, Estimate{}); unknown[0].CostUSD != 0 || unknown[0].WorstUSD != 216 {
		t.Errorf("an unknown estimate still has a worst case: %+v", unknown[0])
	}
}

func validDesign() Design {
	return Design{Template: TemplateContextAB, Arms: []Arm{{Name: "A", Context: BaseContext}, {Name: "B", Context: "lean", Snapshot: "abc"}},
		Tasks: []string{"t1"}, Repeats: 3, Model: "claude-sonnet-5", Goal: GoalCheaper, CostMargin: DefaultCostMargin,
		SuccessMargin: DefaultSuccessMargin, RunBudgetUSD: 3, BudgetUSD: 100, Timeout: time.Minute, VerifyTimeout: time.Minute, Concurrency: 2}
}

func TestValidate(t *testing.T) {
	if err := validDesign().Validate(); err != nil {
		t.Fatalf("valid design: %v", err)
	}
	aa := validDesign()
	aa.Template, aa.Arms[1] = TemplateAA, aa.Arms[0]
	aa.Arms[1].Name = "B"
	if err := aa.Validate(); err != nil {
		t.Errorf("A/A design: %v", err)
	}
	for name, c := range map[string]struct {
		change func(*Design)
		want   string
	}{
		"same context":      {func(d *Design) { d.Arms[1] = Arm{Name: "B", Context: BaseContext} }, "aa template"},
		"aa differs":        {func(d *Design) { d.Template = TemplateAA }, "one context in both arms"},
		"template":          {func(d *Design) { d.Template = "ab" }, "unknown template"},
		"arm names":         {func(d *Design) { d.Arms[0].Name = "X" }, "two arms, A and B"},
		"snapshot mismatch": {func(d *Design) { d.Arms[1].Snapshot = "" }, "do not match"},
		"no tasks":          {func(d *Design) { d.Tasks = nil }, "no tasks"},
		"repeated task":     {func(d *Design) { d.Tasks = []string{"t1", "t1"} }, "listed twice"},
		"repeats":           {func(d *Design) { d.Repeats = 0 }, "repeats"},
		"model":             {func(d *Design) { d.Model = "" }, "no model"},
		"goal":              {func(d *Design) { d.Goal = "faster" }, "unknown goal"},
		"margin":            {func(d *Design) { d.SuccessMargin = 1.5 }, "margins"},
		"budget":            {func(d *Design) { d.BudgetUSD = 0 }, "budgets"},
		"cap above budget":  {func(d *Design) { d.RunBudgetUSD = 200 }, "above the budget"},
		"timeout":           {func(d *Design) { d.Timeout = 0 }, "timeouts"},
		"concurrency":       {func(d *Design) { d.Concurrency = MaxConcurrency + 1 }, "concurrency"},
	} {
		d := validDesign()
		d.Arms = slices.Clone(d.Arms)
		c.change(&d)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestIneligible(t *testing.T) {
	arms := validDesign().Arms
	valid := &task.Validation{Status: task.StatusValid, Arms: []task.Arm{{Name: "base"}, {Name: "lean", Snapshot: "abc"}}}
	if why := Ineligible(Candidate{Name: "t", Validation: valid}, arms); why != "" {
		t.Errorf("a task valid in both contexts: %s", why)
	}
	for name, c := range map[string]struct {
		candidate Candidate
		want      string
	}{
		"review":      {Candidate{Name: "t", NeedsReview: true, Validation: valid}, "agentium task edit t --reviewed"},
		"never":       {Candidate{Name: "t"}, "not validated (agentium task validate t --snapshot lean)"},
		"unchecked":   {Candidate{Name: "t", Validation: &task.Validation{Status: task.StatusUnchecked}}, "no solution"},
		"invalid":     {Candidate{Name: "t", Validation: &task.Validation{Status: task.StatusInvalid}}, "validation failed"},
		"other arm":   {Candidate{Name: "t", Validation: &task.Validation{Status: task.StatusValid, Arms: []task.Arm{{Name: "base"}}}}, "not validated in context lean"},
		"only a snap": {Candidate{Name: "t", Validation: &task.Validation{Status: task.StatusValid, Arms: []task.Arm{{Name: "lean", Snapshot: "abc"}}}}, "not validated in context base"},
	} {
		if why := Ineligible(c.candidate, arms); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q, want %q", name, why, c.want)
		}
	}
}

func TestSample(t *testing.T) {
	names := []string{"e", "a", "d", "b", "c", "f"}
	one, again := Sample(names, 3, 7), Sample(names, 3, 7)
	if len(one) != 3 || !slices.Equal(one, again) || !slices.IsSorted(one) {
		t.Errorf("Sample = %v then %v: want 3 sorted names, the same for the same seed", one, again)
	}
	differs := false
	for seed := range uint64(20) {
		differs = differs || !slices.Equal(Sample(names, 3, seed), one)
	}
	if !differs {
		t.Error("the seed never changes the sample")
	}
	if all := Sample(names, 10, 1); !slices.Equal(all, []string{"a", "b", "c", "d", "e", "f"}) {
		t.Errorf("fewer names than asked: %v", all)
	}
	if names[0] != "e" {
		t.Error("Sample reordered its input")
	}
}
