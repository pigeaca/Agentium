package experiment

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/task"
)

// The formula reproduces the study's table (§5.6), which assumed τ = 0.05 for success and σ = 0.35, τ = 0.10 for cost.
func TestMDEReproducesTheStudysTable(t *testing.T) {
	for _, c := range []struct {
		tasks, repeats int
		success, cost  float64 // pp and %, as the study printed them
	}{{8, 3, 36.5, 25.9}, {12, 3, 29.8, 21.7}, {20, 3, 23.1, 17.3}, {20, 5, 18.0, 14.1}, {40, 5, 12.7, 10.2}, {65, 5, 10.0, 8.1}} {
		success := 100 * stats.MDE(0.05*0.05, WSuccess, c.repeats, c.tasks)
		cost := 100 * (1 - math.Exp(-stats.MDE(0.10*0.10, 0.35*0.35, c.repeats, c.tasks)))
		if math.Abs(success-c.success) > 0.05 || math.Abs(cost-c.cost) > 0.05 {
			t.Errorf("%d × %d: success %.1f pp (study %.1f), cost %.1f%% (study %.1f)", c.tasks, c.repeats, success, c.success, cost, c.cost)
		}
	}
	// The Confident tier's guard: 23 tasks × 5 runs certify no success loss beyond about 15 pp.
	if guard := 100 * stats.GuardMargin(0.05*0.05, WSuccess, 5, 23); math.Abs(guard-14.9) > 0.05 {
		t.Errorf("23 × 5 guard = %.1f pp, the study sized it at 15", guard)
	}
}

// With the planning defaults, the Quick tier detects the 12–21% cost changes the Phase 0 results reported (there with
// τ from 0.05; here from 0.10, so the low end is 14%).
func TestDetectWithPlanningDefaults(t *testing.T) {
	d := Detect(12, 3)
	want := Detectable{Cost: [2]float64{0.1387, 0.2118}, Success: [2]float64{0.3062, 0.3579}, Guard: [2]float64{0.2717, 0.3176}}
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
	}{{23, 5, nil}, {12, 3, []string{"success"}}, {7, 5, []string{"cost", "success"}}, {30, 2, []string{"success"}},
		{12, 1, []string{"success"}}, {7, 1, []string{"cost", "success"}}} { // phase1-v2: cost needs one run per arm
		if got := Exploratory(c.tasks, c.repeats); !slices.Equal(got, c.want) {
			t.Errorf("Exploratory(%d, %d) = %v, want %v", c.tasks, c.repeats, got, c.want)
		}
	}
	for method, want := range map[string]Floors{MethodV1: {8, 3, 20, 3}, MethodV2: {8, 1, 20, 3}, "phase0": {8, 3, 20, 3}} {
		if got := FloorsFor(method); got != want {
			t.Errorf("FloorsFor(%s) = %+v, want %+v", method, got, want)
		}
	}
	for method, want := range map[string]bool{MethodV1: true, MethodV2: true, "phase0": false, "": false} {
		if Resumable(method) != want {
			t.Errorf("Resumable(%q) = %v", method, !want)
		}
	}
}

func TestEstimateRun(t *testing.T) {
	past := EstimateRun("claude-sonnet-5", []PastRun{{"a", 0.9}, {"b", 0.2}, {"c", 0.4}, {"d", 0.3}})
	if !past.Known || past.PerRunUSD != 0.35 || !strings.Contains(past.Basis, "median of this project's 4") {
		t.Errorf("past runs: %+v", past)
	}
	// Too few earlier runs: the default profile, with Claude Code's one-hour cache writes.
	profile := EstimateRun("claude-sonnet-5", []PastRun{{"a", 0.3}})
	if !profile.Known || math.Abs(profile.PerRunUSD-1.612) > 1e-9 || !strings.Contains(profile.Basis, "list prices") {
		t.Errorf("profile: %+v", profile)
	}
	if unknown := EstimateRun("sonnet", nil); unknown.Known || !strings.Contains(unknown.Basis, "no list price") {
		t.Errorf("an alias has no price: %+v", unknown)
	}
	d := validDesign()
	d.Tasks = []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11", "t12"} // 72 runs, none with runs of its own
	if got := DefaultBudget(d, profile); got != 155 {
		t.Errorf("DefaultBudget = %v, want 155 (1.25 × 72 × $1.612 + 3 caps of $3 held for runs in flight, rounded up)", got)
	}
	// Cheap runs: the reserve keeps the budget above what a pair needs to start.
	d.Tasks = d.Tasks[:1]
	if got := DefaultBudget(d, Estimate{PerRunUSD: 0.25, Known: true}); got != 11 || got < 2*d.RunBudgetUSD {
		t.Errorf("DefaultBudget for 6 runs at $0.25 = %v, want 11", got)
	}
	if DefaultBudget(d, Estimate{}) != 0 {
		t.Error("an unknown estimate has no default budget")
	}
	if Reserve(d) != 9 {
		t.Errorf("Reserve = %v, want 3 caps of $3 at concurrency 2", Reserve(d))
	}
}

// A task's own earlier runs estimate it; tasks without any fall back to the project's median. The budget and the
// design's cost sum the tasks' own estimates.
func TestEstimateFromEachTasksOwnRuns(t *testing.T) {
	// The 16-run A/B's case: cheap tasks set the project's median, and a new task cost far more.
	// A run without a task (its task was removed) counts toward the project's median only.
	past := []PastRun{{"cheap", 0.5}, {"cheap", 0.7}, {"cheap", 0.6}, {"mid", 0.8}, {"costly", 1.6}, {"costly", 2.0}, {"costly", 1.9}, {"", 0.8}}
	est := EstimateRun("claude-sonnet-5", past)
	if !est.Known || est.PerRunUSD != 0.8 || !strings.Contains(est.Basis, "median of this project's 8 earlier task runs on claude-sonnet-5") {
		t.Errorf("fallback: %+v", est)
	}
	want := map[string]TaskCost{"cheap": {0.6, 3}, "mid": {0.8, 1}, "costly": {1.9, 3}}
	if len(est.Tasks) != len(want) {
		t.Errorf("tasks = %+v, want %+v", est.Tasks, want)
	}
	for task, w := range want {
		if got := est.Tasks[task]; math.Abs(got.PerRunUSD-w.PerRunUSD) > 1e-9 || got.Runs != w.Runs {
			t.Errorf("task %s = %+v, want %+v", task, got, w)
		}
	}
	if got, ok := est.TaskUSD("new"); !ok || got != 0.8 {
		t.Errorf("a task without runs = %v, %v; want the project's median", got, ok)
	}

	d := validDesign()
	d.Tasks, d.Repeats = []string{"cheap", "costly", "new"}, 1 // 2 runs each
	if got, ok := est.DesignUSD(d); !ok || math.Abs(got-2*(0.6+1.9+0.8)) > 1e-9 {
		t.Errorf("DesignUSD = %v, %v; want 6.60", got, ok)
	}
	if got := DefaultBudget(d, est); got != 18 { // 1.25 × $6.60 + 3 caps of $3, rounded up; the project's median gave 15
		t.Errorf("DefaultBudget = %v, want 18", got)
	}
	rows := Preview(d, []string{"cheap", "costly", "mid", "new"}, est)
	own := rows[len(rows)-1]
	if !own.CostKnown || math.Abs(own.CostUSD-6.6) > 1e-9 || own.WorstUSD != 18 {
		t.Errorf("own row = %+v; the worst case stays every run at its cap", own)
	}
	// A tier would draw from the eligible tasks: it costs their average run, (0.6 + 1.9 + 0.8 + 0.8) / 4.
	if quick := rows[0]; quick.Tasks != 4 || !quick.CostKnown || math.Abs(quick.CostUSD-24*1.025) > 1e-9 {
		t.Errorf("quick = %+v, want 24 runs at $1.025", quick)
	}

	// No price and too few runs: tasks with their own runs still have an estimate; the others make the total unknown.
	alias := EstimateRun("sonnet", []PastRun{{"cheap", 0.5}, {"cheap", 0.7}})
	if alias.Known || alias.Tasks["cheap"].PerRunUSD != 0.6 {
		t.Errorf("alias = %+v", alias)
	}
	d.Tasks = []string{"cheap"}
	if got, ok := alias.DesignUSD(d); !ok || math.Abs(got-1.2) > 1e-9 || DefaultBudget(d, alias) != 11 {
		t.Errorf("a design of tasks with their own runs = %v, %v, budget %v", got, ok, DefaultBudget(d, alias))
	}
	d.Tasks = []string{"cheap", "new"}
	if _, ok := alias.DesignUSD(d); ok || DefaultBudget(d, alias) != 0 {
		t.Error("a task without an estimate makes the design's unknown")
	}
	if rows := Preview(d, []string{"cheap", "new"}, alias); rows[0].CostKnown || rows[0].CostUSD != 0 || rows[len(rows)-1].CostKnown {
		t.Errorf("rows with an unknown task = %+v", rows)
	}
}

func TestPreviewLimitsTiersToEligibleTasks(t *testing.T) {
	d := validDesign()
	d.Tasks, d.Repeats = []string{"a", "b", "c", "d", "e"}, 4
	eligible := make([]string, 17)
	for i := range eligible {
		eligible[i] = fmt.Sprintf("t%d", i)
	}
	rows := Preview(d, eligible, Estimate{PerRunUSD: 0.5, Known: true})
	if len(rows) != len(Tiers())+1 || rows[0].Name != "Quick" || rows[1].Name != "Confident" || rows[2].Name != "This experiment" {
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
	if own.CostUSD != 20 || !own.CostKnown {
		t.Errorf("own cost = %v, want 40 runs at $0.50", own.CostUSD)
	}
	if unknown := Preview(d, eligible, Estimate{}); unknown[0].CostUSD != 0 || unknown[0].CostKnown || unknown[0].WorstUSD != 216 {
		t.Errorf("an unknown estimate still has a worst case: %+v", unknown[0])
	}
}

func validDesign() Design {
	return Design{Version: DesignVersion, Template: TemplateContextAB, Arms: []Arm{{Name: "A", Context: BaseContext}, {Name: "B", Context: "lean", Snapshot: "abc"}},
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
		"below one pair":    {func(d *Design) { d.RunBudgetUSD = 60 }, "below one pair of runs at their caps ($120.00)"},
		"version":           {func(d *Design) { d.Version = 0 }, "design version 0"},
		"seed":              {func(d *Design) { d.Seed = MaxSeed + 1 }, "the seed must be at most"},
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

func TestIneligibleRejectsFlakyTasks(t *testing.T) {
	flaky := &task.Validation{Status: task.StatusFlaky, Repeats: 3, Arms: []task.Arm{{Name: "base"}, {Name: "lean", Snapshot: "abc"}},
		Stages: []task.Stage{{Arm: "minimal", Stage: "reference", Runs: 3, PassedRuns: 2, Flaky: true}}}
	why := Ineligible(Candidate{Name: "t", Validation: flaky}, validDesign().Arms)
	if !strings.Contains(why, "its validation is flaky: minimal/reference passed 2 of 3 times") ||
		!strings.Contains(why, "make the check deterministic, then revalidate: agentium task validate t --snapshot lean --repeat 3)") {
		t.Errorf("flaky: %q", why)
	}
}
