package experiment

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
)

// modelAB is a valid model-ab design: Sonnet against Opus at high effort, on the base's context.
func modelAB() Design {
	d := validDesign()
	d.Template = TemplateModelAB
	d.Arms = []Arm{{Name: "A", Context: BaseContext, Model: "claude-sonnet-5"}, {Name: "B", Context: BaseContext, Model: "claude-opus-5-5", Effort: "high"}}
	d.Model = "claude-sonnet-5"
	d.Version = DesignVersionModelAB
	return d
}

// A model-ab design is stored as version 2, so an older Agentium refuses it; context designs stay at version 1.
func TestModelABDesignVersion(t *testing.T) {
	if modelAB().WantVersion() != 2 || validDesign().WantVersion() != 1 {
		t.Error("versions")
	}
	d := modelAB()
	d.Version = DesignVersion
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "design version 1 (this Agentium writes 2 for a model-ab experiment)") {
		t.Errorf("a model-ab design at version 1: %v", err)
	}
	c := validDesign()
	c.Version = DesignVersionModelAB
	if err := c.Validate(); err == nil {
		t.Error("a context design at version 2 validates")
	}
}

func TestEffortsIsAFreshSlice(t *testing.T) {
	e := Efforts()
	e[0] = "changed"
	if Efforts()[0] != "low" {
		t.Error("Efforts shares its slice")
	}
}

// Arm caps that are equal but differ from the design's run budget are what the runs reserve.
func TestEqualArmCapsBelowTheRunBudget(t *testing.T) {
	d := modelAB()
	d.Arms[0].RunBudgetUSD, d.Arms[1].RunBudgetUSD, d.BudgetUSD = 1, 1, 4
	// Each $1 cap and its overshoot: $0.15 on Sonnet (arm A), $0.30 on Opus 5.5 (arm B), whose output costs twice as much.
	if !near(d.RunCapUSD(), 1.3) || !near(d.PairCapUSD(), 2.45) || !near(Reserve(d), 3.9) {
		t.Errorf("run cap %v, pair %v, reserve %v; want 1.30, 2.45, 3.90", d.RunCapUSD(), d.PairCapUSD(), Reserve(d))
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if caps := armCaps(d); !near(caps["A"], 1.15) || !near(caps["B"], 1.3) {
		t.Errorf("armCaps = %v", caps)
	}
	if armCaps(validDesign()) != nil {
		t.Error("a context design has no arm caps")
	}
	d.Tasks, d.Repeats = []string{"a", "b"}, 1
	slots := Schedule(d)
	f := &fake{}
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 1, RunCapUSD: d.RunCapUSD(), ArmCapUSD: armCaps(d), BudgetUSD: 4, MaxAttempts: 3}, f.run)
	if err != nil || sum.Status != StatusDone || sum.Settled != 4 {
		t.Errorf("summary %+v, %v", sum, err)
	}
}

func TestParseProfile(t *testing.T) {
	for in, want := range map[string][2]string{"claude-sonnet-5": {"claude-sonnet-5", ""}, "claude-opus-5-5:high": {"claude-opus-5-5", "high"}, "m:xhigh": {"m", "xhigh"}} {
		if model, effort, err := ParseProfile(in); err != nil || model != want[0] || effort != want[1] {
			t.Errorf("ParseProfile(%q) = %q, %q, %v", in, model, effort, err)
		}
	}
	for in, want := range map[string]string{"": "names no model", ":high": "names no model", "m:": "no effort after the colon", "m:huge": `unknown effort "huge"`} {
		if _, _, err := ParseProfile(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseProfile(%q) = %v, want %q", in, err, want)
		}
	}
	if Profile("m", "") != "m" || Profile("m", "low") != "m:low" {
		t.Error("Profile")
	}
}

func TestValidateModelAB(t *testing.T) {
	if err := modelAB().Validate(); err != nil {
		t.Fatalf("model-ab design: %v", err)
	}
	effortOnly := modelAB()
	effortOnly.Arms[0].Model, effortOnly.Arms[0].Effort = "claude-opus-5-5", "medium"
	if err := effortOnly.Validate(); err != nil {
		t.Errorf("one model at two efforts: %v", err)
	}
	snap := modelAB()
	snap.Arms[0].Context, snap.Arms[0].Snapshot, snap.Arms[1].Context, snap.Arms[1].Snapshot = "lean", "abc", "lean", "abc"
	if err := snap.Validate(); err != nil {
		t.Errorf("model-ab on a snapshot: %v", err)
	}
	for name, c := range map[string]struct {
		change func(*Design)
		want   string
	}{
		"same profile":       {func(d *Design) { d.Arms[1].Model, d.Arms[1].Effort = d.Arms[0].Model, d.Arms[0].Effort }, "both arms run claude-sonnet-5"},
		"two contexts":       {func(d *Design) { d.Arms[1].Context, d.Arms[1].Snapshot = "lean", "abc" }, "one context in both arms"},
		"no model":           {func(d *Design) { d.Arms[1].Model = "" }, "gives each arm a model"},
		"unknown effort":     {func(d *Design) { d.Arms[1].Effort = "huge" }, `unknown effort "huge"`},
		"negative run cap":   {func(d *Design) { d.Arms[0].RunBudgetUSD = -1 }, "run budget must be positive"},
		"below a pair":       {func(d *Design) { d.Arms[0].RunBudgetUSD, d.Arms[1].RunBudgetUSD, d.BudgetUSD = 1, 5, 5.5 }, "below one pair of runs at their caps ($6.65)"},
		"context ab models":  {func(d *Design) { *d = validDesign(); d.Arms[1].Model = "claude-opus-5-5" }, "compares contexts"},
		"context ab efforts": {func(d *Design) { *d = validDesign(); d.Arms[0].Effort = "high" }, "compares contexts"},
		"aa run budgets": {func(d *Design) {
			*d = validDesign()
			d.Template, d.Arms[1] = TemplateAA, Arm{Name: "B", Context: BaseContext}
			d.Arms[0].RunBudgetUSD = 2
		}, "compares contexts"},
	} {
		d := modelAB()
		d.Arms = append([]Arm(nil), d.Arms...)
		c.change(&d)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestArmProfilesAndCaps(t *testing.T) {
	d := modelAB()
	d.Effort = "low" // a default effort must not leak into an arm with its own model
	a, b := d.Arms[0], d.Arms[1]
	if d.ArmModel(a) != "claude-sonnet-5" || d.ArmEffort(a) != "" || d.ArmModel(b) != "claude-opus-5-5" || d.ArmEffort(b) != "high" {
		t.Errorf("profiles: %q %q, %q %q", d.ArmModel(a), d.ArmEffort(a), d.ArmModel(b), d.ArmEffort(b))
	}
	ctx := validDesign()
	ctx.Effort = "medium"
	if ctx.ArmModel(ctx.Arms[1]) != ctx.Model || ctx.ArmEffort(ctx.Arms[0]) != "medium" || !near(ctx.RunCapUSD(), 3.3) || !near(ctx.PairCapUSD(), 6.6) {
		t.Errorf("a context experiment's arms share the design's profile and cap")
	}
	d.Arms[1].RunBudgetUSD = 5
	if d.ArmRunBudgetUSD(a) != 3 || d.ArmRunBudgetUSD(d.Arms[1]) != 5 || !near(d.RunCapUSD(), 5.5) || !near(d.PairCapUSD(), 8.8) || !near(Reserve(d), 16.5) {
		t.Errorf("caps: %v %v, run cap %v, pair %v, reserve %v", d.ArmRunBudgetUSD(a), d.ArmRunBudgetUSD(d.Arms[1]), d.RunCapUSD(), d.PairCapUSD(), Reserve(d))
	}
	d.Judge = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 1}
	if !near(d.ArmRunCapUSD(a), 4.3) || !near(d.PairCapUSD(), 10.8) { // each arm's judgement ($1) on top of its own cap and overshoot
		t.Errorf("with the judge: arm A cap %v, pair %v", d.ArmRunCapUSD(a), d.PairCapUSD())
	}
	if d.ModelLabel() != "A = claude-sonnet-5, B = claude-opus-5-5:high" || validDesign().ModelLabel() != "claude-sonnet-5" {
		t.Errorf("model label %q", d.ModelLabel())
	}
}

// Designs and locks stored before arms had profiles load as they were, and a context experiment encodes without them.
func TestOlderDesignsAndLocksLoad(t *testing.T) {
	old := `{"version":1,"template":"context-ab","arms":[{"name":"A","context":"base"},{"name":"B","context":"lean","snapshot":"abc"}],"tasks":["t1"],"repeats":3,` +
		`"model":"claude-sonnet-5","goal":"cheaper","cost_margin":0.1,"success_margin":0.15,"run_budget_usd":3,"budget_usd":100,"timeout":60000000000,` +
		`"verify_timeout":60000000000,"concurrency":2,"seed":1}`
	var d Design
	if err := json.Unmarshal([]byte(old), &d); err != nil || d.Validate() != nil {
		t.Fatalf("an older design: %v, %v", err, d.Validate())
	}
	if d.ArmModel(d.Arms[0]) != "claude-sonnet-5" || d.ArmEffort(d.Arms[1]) != "" || !near(d.RunCapUSD(), 3.3) || !near(d.PairCapUSD(), 6.6) {
		t.Errorf("older design's arms: %+v", d.Arms)
	}
	if encoded, _ := json.Marshal(d); strings.Contains(string(encoded), "requested_model") || strings.Contains(string(encoded), "effort") ||
		strings.Count(string(encoded), "run_budget_usd") != 1 {
		t.Errorf("a context design gains fields: %s", encoded)
	}
	var lock Lock
	oldLock := `{"method":"phase1-v2","design":` + old + `,"arms":[{"name":"A","context":"base","calibration":"r1","model":"claude-sonnet-5","tools":["Bash"],"skills":[],"slash_commands":[]}]}`
	if err := json.Unmarshal([]byte(oldLock), &lock); err != nil || lock.Arms[0].Model != "claude-sonnet-5" || lock.Arms[0].Arm.Model != "" {
		t.Fatalf("an older lock: %+v, %v", lock.Arms, err)
	}

	// A model-ab lock keeps both: the profile the arm was asked for, and the model its calibration saw.
	l := Lock{Design: modelAB(), Arms: []LockedArm{{Arm: modelAB().Arms[1], Calibration: "r2", Model: "claude-opus-5-5-20260901"}}}
	encoded, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	var back Lock
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Arms[0]; got.Arm.Model != "claude-opus-5-5" || got.Arm.Effort != "high" || got.Model != "claude-opus-5-5-20260901" {
		t.Errorf("locked arm %+v", got)
	}
}

// The estimates and caps of both arms: each on its own model, the budget over both.
func TestModelABEstimatesAndBudget(t *testing.T) {
	d := modelAB()
	d.Tasks, d.Repeats = []string{"t1", "t2"}, 2
	d.RunBudgetUSD, d.Arms[1].RunBudgetUSD = 3, 6
	sonnet := EstimateRun("claude-sonnet-5", []PastRun{{"t1", 0.5}, {"t1", 0.7}, {"t2", 1}})
	opus := EstimateRun("claude-opus-5-5", []PastRun{{"t1", 2}, {"t2", 3}, {"t2", 5}})
	ests := ArmEstimates{sonnet, opus}
	// t1: 0.6 on Sonnet and 2 on Opus; t2: 1 and 4; two repeats each: 2 × (0.6 + 2 + 1 + 4).
	if got, ok := ests.DesignUSD(d); !ok || math.Abs(got-15.2) > 1e-9 {
		t.Errorf("DesignUSD = %v, %v; want 15.20", got, ok)
	}
	if want := math.Ceil(1.25*15.2 + 3*6.6); DefaultBudgetFor(d, ests) != want {
		t.Errorf("default budget = %v, want %v (1.25 × $15.20 and 3 of the larger cap, $6 and its $0.60 overshoot)", DefaultBudgetFor(d, ests), want)
	}
	rows := PreviewFor(d, []string{"t1", "t2"}, ests)
	own := rows[len(rows)-1]
	if !own.CostKnown || math.Abs(own.CostUSD-15.2) > 1e-9 || !near(own.WorstUSD, 4*(3.3+6.6)) || own.Runs != 8 {
		t.Errorf("this experiment = %+v, want $15.20 and a worst case of 4 pairs × $9.90", own)
	}
	// Quick: 2 eligible tasks × 3 repeats at each arm's mean per run: (0.8 + 3) a pair.
	if quick := rows[0]; math.Abs(quick.CostUSD-6*(0.8+3)) > 1e-9 || !near(quick.WorstUSD, 6*9.9) {
		t.Errorf("quick = %+v", quick)
	}
	// Either arm without an estimate leaves the cost unknown.
	if _, ok := (ArmEstimates{sonnet, {}}).DesignUSD(d); ok || DefaultBudgetFor(d, ArmEstimates{sonnet, {}}) != 0 {
		t.Error("an arm with no estimate must leave the cost unknown")
	}
	// A context experiment's two equal estimates are the old one estimate.
	c := validDesign()
	if a, _ := sonnet.DesignUSD(c); math.Abs(a-2*3*0.6) > 1e-9 { // t1's own estimate, 3 repeats, 2 arms
		t.Errorf("single estimate over both arms = %v", a)
	}
	if DefaultBudget(c, sonnet) != DefaultBudgetFor(c, Same(sonnet)) {
		t.Error("DefaultBudget is the shared-estimate case")
	}
}

// Each arm's own cap is what a run reserves: with caps of $1 and $4, a budget of $15 holds back the caps of the runs in
// flight and of the pair about to start, and never passes it.
func TestExecuteReservesEachArmsCap(t *testing.T) {
	slots := scheduleOf(t, 4, 2) // 16 runs
	caps := map[string]float64{"A": 1, "B": 4}
	f := &fake{outcome: func(s Slot, _ int) Result { return Result{Outcome: claude.OutcomeCapped, CostUSD: caps[s.Arm]} }}
	var mu sync.Mutex
	running := map[int]bool{}
	worst, committed := 0.0, 0.0
	sum, err := Execute(context.Background(), Plan{Schedule: slots, Concurrency: 2, RunCapUSD: 4, ArmCapUSD: caps, BudgetUSD: 15, MaxAttempts: 3,
		Progress: func(e Event) {
			mu.Lock()
			defer mu.Unlock()
			switch e.Kind {
			case "start":
				running[e.Slot.Position] = true
			case "finish":
				delete(running, e.Slot.Position)
			}
			committed = e.SpentUSD
			for pos := range running {
				committed += caps[slots[pos].Arm]
			}
			worst = max(worst, committed)
		}}, f.run)
	if err != nil || sum.Status != StatusBudget || sum.SpentUSD > 15 || sum.Pending == 0 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	if worst > 15 {
		t.Errorf("spend plus the caps of runs in flight reached $%.2f, over the $15 budget", worst)
	}
	// At $5 a pair, with a pair's two caps reserved up front, three pairs fit in $15: the runs stopped for the budget are
	// not those a flat $4 cap would have stopped (two pairs).
	if sum.SpentUSD != 15 || sum.Settled != 6 {
		t.Errorf("settled %d runs, spent $%.2f; want 3 pairs at $5", sum.Settled, sum.SpentUSD)
	}
}
