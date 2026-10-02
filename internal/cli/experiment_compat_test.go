package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/store"
)

// Designs as experiment new stored them before its flags were simplified, with the old flags each was made with,
// recorded from that Agentium on experimentFixture. Each has a fixed seed and no snapshot commit, so it is the same
// on every run of the fixture.
var designsBefore = []struct {
	name, template, design string
	oldArgs, newArgs       []string
}{
	{
		name: "ctx", template: "aa",
		design: `{"version":3,"template":"aa","arms":[{"name":"A","context":"base"},{"name":"B","context":"base"}],"tasks":["value"],"repeats":1,` +
			`"model":"claude-opus-5-5","effort":"high","goal":"cheaper","cost_margin":0.1,"success_margin":0.15,"run_budget_usd":3,"budget_usd":42,` +
			`"timeout":1200000000000,"verify_timeout":600000000000,"concurrency":2,"seed":7,"judge":{"model":"claude-opus-5-5","effort":"max","repeats":3},` +
			`"judge_pairs":{"model":"claude-opus-5-5","effort":"max","repeats":1},"method":"seq-v1"}`,
		oldArgs: []string{"--template", "aa", "--task", "value", "--model", "claude-opus-5-5", "--effort", "high", "--judge", "--judge-pairs",
			"--judge-model", "claude-opus-5-5", "--judge-effort", "max", "--seed", "7"},
		newArgs: []string{"--task", "value", "--model", "claude-opus-5-5:high", "--judge=claude-opus-5-5:max", "--judge-pairs=claude-opus-5-5:max", "--seed", "7"},
	},
	{
		name: "mab", template: "model-ab",
		design: `{"version":3,"template":"model-ab","arms":[{"name":"A","context":"base","requested_model":"claude-sonnet-5-5","effort":"low"},` +
			`{"name":"B","context":"base","requested_model":"claude-opus-5-5","effort":"high"}],"tasks":["value"],"repeats":1,"model":"claude-sonnet-5-5",` +
			`"effort":"low","goal":"cheaper","cost_margin":0.1,"success_margin":0.15,"run_budget_usd":3,"budget_usd":28,"timeout":1200000000000,` +
			`"verify_timeout":600000000000,"concurrency":2,"seed":7,"judge":{"model":"claude-sonnet-5-5","effort":"medium","repeats":3},"method":"seq-v1"}`,
		oldArgs: []string{"--template", "model-ab", "--a", "claude-sonnet-5-5:low", "--b", "claude-opus-5-5:high", "--task", "value", "--judge",
			"--judge-model", "claude-sonnet-5-5", "--judge-effort", "medium", "--seed", "7"},
		newArgs: []string{"--a", "claude-sonnet-5-5:low", "--b", "claude-opus-5-5:high", "--task", "value", "--judge=claude-sonnet-5-5:medium", "--seed", "7"},
	},
	{
		name: "def", template: "aa",
		design: `{"version":3,"template":"aa","arms":[{"name":"A","context":"base"},{"name":"B","context":"base"}],"tasks":["value"],"repeats":1,` +
			`"model":"claude-sonnet-5-5","goal":"cheaper","cost_margin":0.1,"success_margin":0.15,"run_budget_usd":3,"budget_usd":14,` +
			`"timeout":1200000000000,"verify_timeout":600000000000,"concurrency":2,"seed":7,"method":"seq-v1"}`,
		oldArgs: []string{"--template", "aa", "--task", "value", "--seed", "7"},
		newArgs: []string{"--task", "value", "--seed", "7"},
	},
}

// storedExperiment reads an experiment's stored row.
func storedExperiment(t *testing.T, f runFixture, name string) store.Experiment {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	e, err := db.ExperimentByName(ctx, projects[0].ID, name)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// saveStoredDesign stores design under name, as an experiment that has not run, in place of any of that name.
func saveStoredDesign(t *testing.T, f runFixture, name, template string, design []byte) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(f.data, "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	if _, err := db.ExperimentByName(ctx, projects[0].ID, name); err == nil {
		if err := db.DeleteExperiment(ctx, projects[0].ID, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: projects[0].ID, Name: name, Template: template, Design: design,
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// storeAsBefore rewrites an experiment that has not run as experiment new stored it before its flags were simplified:
// edit sets what only the removed flags could (an arm's own run cap, the judge's repeats, another budget).
func storeAsBefore(t *testing.T, f runFixture, name string, edit func(*experiment.Design)) {
	t.Helper()
	e := storedExperiment(t, f, name)
	var d experiment.Design
	if err := json.Unmarshal(e.Design, &d); err != nil {
		t.Fatal(err)
	}
	edit(&d)
	if err := d.Validate(); err != nil {
		t.Fatalf("the old design: %v", err)
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	saveStoredDesign(t, f, name, e.Template, encoded)
}

// --model M:E and --judge=M:E (and --judge-pairs=M:E) store the very design the old flag pairs did, byte for byte, and
// the old flags now fail with a usage error that names the new form.
func TestExperimentNewMatchesTheOldFlags(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	for _, c := range designsBefore {
		expect(t, f.run(ctx, append([]string{"experiment", "new", c.name}, c.newArgs...)...), ExitOK)
		e := storedExperiment(t, f, c.name)
		if string(e.Design) != c.design || e.Template != c.template {
			t.Errorf("%s: %v stored template %s, design\n%s\nwant %s, the old flags' (%v)\n%s", c.name, c.newArgs, e.Template, e.Design, c.template, c.oldArgs, c.design)
		}
		old := f.run(ctx, append([]string{"experiment", "new", c.name + "-old"}, c.oldArgs...)...)
		expect(t, old, ExitUsage, "was removed: ")
		if !strings.Contains(old.stderr, "--model MODEL:EFFORT") && !strings.Contains(old.stderr, "--b decides it") {
			t.Errorf("%s: the old flags' error names no replacement:\n%s", c.name, old.stderr)
		}
	}
}

// --b decides the template: none an A/A, a snapshot a context A/B, a model (with or without an effort) a model A/B;
// one that names both a snapshot and a model is refused, saying how it was read. The JSON keeps the template.
func TestExperimentNewInfersTheTemplate(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		args     []string
		template string
		arms     [2]string // each arm's context, model and effort
		says     string
	}{
		{"aa", nil, experiment.TemplateAA, [2]string{"base claude-sonnet-5-5 ", "base claude-sonnet-5-5 "}, "A/A calibration of context base"},
		{"aa-lean", []string{"--a", "lean", "--model", "claude-opus-5-5:high"}, experiment.TemplateAA,
			[2]string{"lean claude-opus-5-5 high", "lean claude-opus-5-5 high"}, "A/A calibration of context lean"},
		{"ctx", []string{"--b", "lean"}, experiment.TemplateContextAB, [2]string{"base claude-sonnet-5-5 ", "lean claude-sonnet-5-5 "}, "context A/B, A = base, B = lean"},
		{"model", []string{"--b", "claude-opus-5-5"}, experiment.TemplateModelAB, [2]string{"base claude-sonnet-5-5 ", "base claude-opus-5-5 "},
			"model A/B on context base, A = claude-sonnet-5-5, B = claude-opus-5-5"},
		{"effort", []string{"--b", "claude-sonnet-5-5:high"}, experiment.TemplateModelAB, [2]string{"base claude-sonnet-5-5 ", "base claude-sonnet-5-5 high"},
			"A = claude-sonnet-5-5, B = claude-sonnet-5-5:high"},
		{"both", []string{"--a", "claude-opus-5-5:low", "--b", "claude-opus-5-5:max", "--context", "lean"}, experiment.TemplateModelAB,
			[2]string{"lean claude-opus-5-5 low", "lean claude-opus-5-5 max"}, "model A/B on context lean, A = claude-opus-5-5:low, B = claude-opus-5-5:max"},
		{"from-model", []string{"--model", "claude-opus-5-5:low", "--b", "claude-opus-5-5"}, experiment.TemplateModelAB,
			[2]string{"base claude-opus-5-5 low", "base claude-opus-5-5 "}, "A = claude-opus-5-5:low, B = claude-opus-5-5"},
	} {
		args := append([]string{"experiment", "new", c.name, "--task", "value", "--budget", "40"}, c.args...)
		expect(t, f.run(ctx, args...), ExitOK, c.says)
		d := loadDesign(t, f, c.name)
		if e := storedExperiment(t, f, c.name); e.Template != c.template || d.Template != c.template {
			t.Errorf("%s: stored template %s, design's %s, want %s", c.name, e.Template, d.Template, c.template)
		}
		for i, a := range d.Arms {
			if got := a.Context + " " + d.ArmModel(a) + " " + d.ArmEffort(a); got != c.arms[i] {
				t.Errorf("%s: arm %s is %q, want %q", c.name, a.Name, got, c.arms[i])
			}
		}
		doc := jsonRun(t, f, ExitOK, "experiment", "show", c.name)
		if got := doc.get("experiment", "template"); got != c.template {
			t.Errorf("%s: the JSON template %v, want %s", c.name, got, c.template)
		}
	}

	// A snapshot named as a model is ambiguous as --b: refused, saying it was read as the model. Its MODEL:EFFORT
	// form cannot be a snapshot's name, so it stays a model A/B.
	writeFile(t, f.repo, "CLAUDE.md", "# Rules\nBe brief.\n")
	expect(t, f.run(ctx, "context", "snapshot", "claude-opus-5-5", "--working-tree"), ExitOK)
	gitIn(t, f.repo, "checkout", "--", "CLAUDE.md")
	expect(t, f.run(ctx, "experiment", "new", "ambiguous", "--b", "claude-opus-5-5", "--task", "value", "--budget", "40"), ExitUsage,
		"agentium experiment new: --b claude-opus-5-5 names both a model and a snapshot: it was read as the model (a model A/B), and is refused")
	expect(t, f.run(ctx, "experiment", "show", "ambiguous"), ExitError, "not found")
	expect(t, f.run(ctx, "experiment", "new", "colon", "--b", "claude-opus-5-5:high", "--task", "value", "--budget", "40"), ExitOK,
		"A = claude-sonnet-5-5, B = claude-opus-5-5:high")
}

// A design stored before the flags were simplified, byte for byte as the old Agentium wrote it, with what only the
// removed flags could set (each arm's own run cap, 2 judge calls per run), still previews, locks, pauses, resumes and
// reports, with its lock keeping those values.
func TestExperimentFromBeforeResumesAndReports(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	calibrateOn(t, f, ctrl, opus)
	design := designsBefore[1].design // the model A/B
	for old, with := range map[string]string{
		`"effort":"low"}`:  `"effort":"low","run_budget_usd":2}`,  // --run-budget-a 2
		`"effort":"high"}`: `"effort":"high","run_budget_usd":4}`, // --run-budget-b 4
		`"repeats":3}`:     `"repeats":2}`,                        // --judge-repeats 2
		`"concurrency":2`:  `"concurrency":1`,                     // --concurrency 1
	} {
		if strings.Count(design, old) != 1 {
			t.Fatalf("the old design has %q %d times", old, strings.Count(design, old))
		}
		design = strings.Replace(design, old, with, 1)
	}
	saveStoredDesign(t, f, "before", experiment.TemplateModelAB, []byte(design))

	expect(t, f.run(ctx, "experiment", "plan", "before"), ExitOK, "arm A: model claude-sonnet-5-5, effort low, context base; each run up to $2.00",
		"arm B: model claude-opus-5-5, effort high, context base; each run up to $4.00", "judge: claude-sonnet-5-5 at effort medium, 2 call(s) per run")
	writeFile(t, ctrl, "judge-limit", "")
	expect(t, f.run(ctx, "experiment", "run", "before"), ExitOK, "Locked: Claude Code 2.1.281, A = claude-sonnet-5-5:low, B = claude-opus-5-5:high",
		"each run up to $2.00 (arm A) or $4.00 (arm B)", "Paused: the judge hit a usage limit or a sign-in failure.")
	var lock experiment.Lock
	if err := json.Unmarshal(storedLock(t, f, "before"), &lock); err != nil {
		t.Fatal(err)
	}
	if d := lock.Design; d.Template != experiment.TemplateModelAB || d.Arms[0].RunBudgetUSD != 2 || d.Arms[1].RunBudgetUSD != 4 || d.Judge == nil ||
		d.Judge.Repeats != 2 || d.Judge.Model != "claude-sonnet-5-5" || d.Judge.Effort != "medium" {
		t.Errorf("the lock's design %+v, judge %+v", d, d.Judge)
	}
	os.Remove(filepath.Join(ctrl, "judge-limit"))
	expect(t, f.run(ctx, "experiment", "run", "before"), ExitOK, "Experiment before: done", "2 of 2 runs settled")
	if runs := experimentRuns(t, f, "before"); len(runs) != 2 {
		t.Errorf("%d runs, want the 2 of one pair", len(runs))
	}
	for _, r := range records(t, experimentRuns(t, f, "before")) {
		if r.Judge == nil || r.Judge.Requested != 2 || r.Judge.Model != "claude-sonnet-5-5" || r.Judge.Effort != "medium" {
			t.Errorf("run %s's verdict %+v", r.ID, r.Judge)
		}
	}
	expect(t, f.run(ctx, "experiment", "show", "before"), ExitOK, "A = claude-sonnet-5-5:low, B = claude-opus-5-5:high", "2 call(s) per run")
	expect(t, f.run(ctx, "experiment", "report", "before"), ExitOK, "Model A/B on context `base`: A = `claude-sonnet-5-5:low`, B = `claude-opus-5-5:high`.",
		"## Judge")
	list, _ := jsonRun(t, f, ExitOK, "experiment", "list").get("experiments").([]any)
	if len(list) != 1 || list[0].(map[string]any)["template"] != experiment.TemplateModelAB {
		t.Errorf("experiment list --json: %v", list)
	}
}

// The help of experiment new, run once and run calibrate lists no expert flag (each still parses: the guide's
// "Advanced flags" lists them), no removed flag, and at most 12 flags for experiment new; every flag it lists parses.
func TestUsageListsNoHiddenOrRemovedFlag(t *testing.T) {
	t.Parallel()
	flagName := regexp.MustCompile(`--([a-z][a-z-]*)`)
	hidden := append(slices.Clone(experimentHidden), runHidden...)
	for _, usage := range []string{experimentUsage, runUsage} {
		for _, m := range flagName.FindAllStringSubmatch(usage, -1) {
			if slices.Contains(hidden, m[1]) {
				t.Errorf("the help lists the expert flag --%s", m[1])
			}
			if _, gone := experimentRemoved[m[1]]; gone {
				t.Errorf("the help lists the removed flag --%s", m[1])
			}
		}
	}
	if strings.Contains(runUsage, "run calibrate") {
		t.Error("run's help lists run calibrate")
	}
	newSection := experimentUsage[:strings.Index(experimentUsage, "agentium experiment plan")]
	visible := map[string]bool{}
	for _, m := range flagName.FindAllStringSubmatch(newSection, -1) {
		visible[m[1]] = true
	}
	if len(visible) > 12 {
		t.Errorf("experiment new's help lists %d flags, want at most 12: %v", len(visible), visible)
	}
	onceSection := runUsage[:strings.Index(runUsage, "agentium run list")]
	once := map[string]bool{}
	for _, m := range flagName.FindAllStringSubmatch(onceSection, -1) {
		once[m[1]] = true
	}
	t.Logf("visible flags: experiment new %d %v, run once %d %v", len(visible), slices.Sorted(mapKeys(visible)), len(once), slices.Sorted(mapKeys(once)))
}

func mapKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// Expert flags still parse though the help leaves them out, and run once's removed --effort names its replacement.
func TestHiddenFlagsStillWork(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "expert", "--b", "lean", "--goal", "better", "--tier", "confident", "--repeats", "2",
		"--concurrency", "1", "--timeout", "5m", "--verify-timeout", "2m", "--seed", "9", "--budget", "40"), ExitOK)
	if d := loadDesign(t, f, "expert"); d.Repeats != 2 || d.Concurrency != 1 || d.Timeout != 5*time.Minute || d.VerifyTimeout != 2*time.Minute || d.Seed != 9 {
		t.Errorf("the expert flags' design %+v", d)
	}
	expect(t, f.run(ctx, "experiment", "new", "nofut", "--b", "lean", "--task", "value", "--no-futility", "--budget", "40"), ExitOK)
	if d := loadDesign(t, f, "nofut"); !d.NoFutility {
		t.Error("--no-futility was not stored")
	}
	expect(t, f.run(ctx, "run", "once", "value", "--effort", "high"), ExitUsage, "agentium run once: --effort was removed: put the effort in --model: --model MODEL:EFFORT")
	expect(t, f.run(ctx, "run", "once", "value", "--model", "claude-opus-5-5:huge"), ExitUsage, `agentium run once: --model "claude-opus-5-5:huge": unknown effort "huge"`)
	expect(t, f.run(ctx, "run", "calibrate", "--model", ":high"), ExitUsage, "agentium run calibrate: --model")

	// run once's --model MODEL:EFFORT runs the model at the effort; --keep, hidden, keeps the workspace.
	once := f.run(ctx, "run", "once", "value", "--model", opus+":high", "--keep", "--timeout", "5m")
	expect(t, once, ExitOK, "Starting a real Claude Code run (claude-opus-5-5:high,", "outcome      ok")
	args, err := filepath.Glob(filepath.Join(ctrl, "args-*"))
	if err != nil || len(args) != 1 {
		t.Fatalf("agent runs %v, %v", args, err)
	}
	if got, _ := os.ReadFile(args[0]); strings.TrimSpace(string(got)) != opus+" high" {
		t.Errorf("run once ran with %q, want %s high", got, opus)
	}
	// run calibrate, out of the help, still works; an effort in --model is accepted, and the calibration is the model's.
	f.vars["AGENTIUM_CLAUDE"] = modelCalibrator(t, opus)
	expect(t, f.run(ctx, "run", "calibrate", "--model", opus+":high"), ExitOK, "Calibrating 1 arm(s)", "(claude-opus-5-5, sign-in")
	if models := storedCalibrations(t, f, "base", ""); !slices.Contains(models, opus) {
		t.Errorf("calibrations of base on %v, want one on %s", models, opus)
	}
}
