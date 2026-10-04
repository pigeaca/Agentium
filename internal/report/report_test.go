package report

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/report/reporttest"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/term"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixture is reporttest.LeanAB as a report's input.
func fixture() Input { return inputOf(reporttest.LeanAB()) }

// oneRun is reporttest.OneRun as a report's input.
func oneRun(method, template string) Input { return inputOf(reporttest.OneRun(method, template)) }

// inputOf is a test experiment as a report's input.
func inputOf(e reporttest.Experiment) Input {
	in := Input{Name: e.Name, Lock: e.Lock, Status: e.Status, StatusNote: e.StatusNote, DataDir: e.DataDir, Home: e.Home}
	for _, r := range e.Runs {
		in.Runs = append(in.Runs, Run(r))
	}
	return in
}

// One run per arm: phase1-v2 gives a cost verdict, and its report shows the noise the design can estimate (σ only
// with the planner's help in an A/B, σ and w from the paired differences in an A/A); a phase1-v1 lock keeps its floor.
func TestReportOneRunGolden(t *testing.T) {
	for _, c := range []struct {
		golden, method, template string
		want, not                []string
	}{
		{"lean-ab-1run.md", experiment.MethodV2, experiment.TemplateContextAB,
			[]string{"): improved.", "Cost's verdict rests on tasks with fewer than 3 runs per arm, as method phase1-v2 allows", "| σ, per-run spread of log cost | - | - | 0.19 | not separable",
				"taking σ = 0.19", "| w, per-run variance of success | - |"},
			[]string{"Cost is exploratory"}},
		{"lean-ab-1run-v1.md", experiment.MethodV1, experiment.TemplateContextAB,
			[]string{"Cost is exploratory: 0 of 10 task(s) have 3 or more counted runs in both arms, below the floor of 8 tasks (method phase1-v1).", "(method phase1-v1)"},
			[]string{"verdict rests on tasks"}},
		{"aa-1run.md", experiment.MethodV2, experiment.TemplateAA,
			[]string{"A/A calibration", "this is the noise itself", "τ = 0 in an A/A", "| 0.19: ", "| 0.20: "},
			[]string{"τ, spread of the cost effect"}},
	} {
		in := oneRun(c.method, c.template)
		rep, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		var md bytes.Buffer
		if err := rep.Markdown(&md); err != nil {
			t.Fatal(err)
		}
		golden(t, c.golden, md.Bytes())
		for _, want := range c.want {
			if !strings.Contains(md.String(), want) {
				t.Errorf("%s lacks %q", c.golden, want)
			}
		}
		for _, not := range c.not {
			if strings.Contains(md.String(), not) {
				t.Errorf("%s shows %q", c.golden, not)
			}
		}
	}
}

func TestCompareWithDefaults(t *testing.T) {
	c := &experiment.Component{Estimate: 0.2, Low: 0.15, High: 0.3}
	for _, x := range []struct {
		low, high float64
		want      string
	}{{0.19, 0.19, "within"}, {0.1, 0.1, "below"}, {0.35, 0.35, "above"}, {0.10, 0.25, "overlaps"}, {0.31, 0.4, "above"}, {0.05, 0.1, "below"}} {
		if got := compare(x.low, x.high, c); !strings.HasPrefix(got, x.want) || strings.Contains(got, "hint") {
			t.Errorf("compare(%v, %v) = %q, want %s", x.low, x.high, got, x.want)
		}
	}
	if got := compare(0.10, 0.25, &experiment.Component{}); !strings.Contains(got, "no spread detected (range truncated at zero)") {
		t.Errorf("a range truncated at zero is not compared: %q", got)
	}
	if got := compare(0.20, 0.20, &experiment.Component{Estimate: 0.05, Low: 0.02, High: 0.1, Bootstrap: true}); !strings.HasPrefix(got, "above") ||
		!strings.Contains(got, "a hint, not a finding") {
		t.Errorf("a bootstrap range gives a hint: %q", got)
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update after checking):\n%s", name, got)
	}
}

func TestReportGolden(t *testing.T) {
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	var md, js bytes.Buffer
	if err := rep.Markdown(&md); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&js); err != nil {
		t.Fatal(err)
	}
	golden(t, "lean-ab.md", md.Bytes())
	goldenJSON(t, "lean-ab.json", js.Bytes())
	var plain bytes.Buffer
	if err := rep.Terminal(&plain, term.Style{}); err != nil {
		t.Fatal(err)
	}
	golden(t, "lean-ab.txt", plain.Bytes())
	assertTerminalColors(t, rep, plain.String())
}

// assertTerminalColors checks that the colored rendering is the plain one plus escape codes, and that it colors.
func assertTerminalColors(t *testing.T, rep Report, plain string) {
	t.Helper()
	var colored bytes.Buffer
	if err := rep.Terminal(&colored, term.Colored()); err != nil {
		t.Fatal(err)
	}
	if got := term.Plain(colored.String()); got != plain {
		t.Errorf("the colored rendering without its escape codes differs from the plain one:\n%s", got)
	}
	if colored.String() == plain || strings.Contains(plain, "\x1b") {
		t.Errorf("only the colored rendering should carry escape codes")
	}
}

// The A/A case (with noise and a budget stop) and the one-run case (no intervals, exploratory) in words.
func TestReportTerminalGolden(t *testing.T) {
	in := fixture()
	in.Lock.Design.Template = experiment.TemplateAA
	in.Lock.Arms[1].Context, in.Lock.Arms[1].Snapshot = "base", ""
	in.Status, in.StatusNote = experiment.StatusBudget, "the next run would not fit the $60.00 budget"
	for name, in := range map[string]Input{"aa.txt": in, "lean-ab-1run.txt": oneRun(experiment.MethodV2, experiment.TemplateContextAB)} {
		rep, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		var plain bytes.Buffer
		if err := rep.Terminal(&plain, term.Style{}); err != nil {
			t.Fatal(err)
		}
		golden(t, name, plain.Bytes())
		assertTerminalColors(t, rep, plain.String())
	}
}

// Verdicts take their colors: improved and no loss green, regressed red, exploratory and inconclusive yellow; and no
// markup is left in the terminal rendering.
func TestReportTerminalVerdictColorsAndNoMarkup(t *testing.T) {
	st := term.Colored()
	for verdict, want := range map[string]string{stats.Improved: st.Good("x"), stats.NoLoss: st.Good("x"), stats.Regressed: st.Bad("x"),
		stats.Exploratory: st.Warn("x"), stats.Inconclusive: st.Warn("x")} {
		if got := verdictStyle(st, verdict, "x"); got != want {
			t.Errorf("%s: %q, want %q", verdict, got, want)
		}
	}
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := rep.Terminal(&out, term.Style{}); err != nil {
		t.Fatal(err)
	}
	for _, markup := range []string{"**", "`", "| ", "## ", "# "} {
		if strings.Contains(out.String(), markup) {
			t.Errorf("the terminal rendering has markup %q", markup)
		}
	}
}

// goldenJSON compares JSON with the golden file, numbers within 1e-9 relative: the last digits of floating-point
// results differ between machines (arm64 fuses multiply-adds, amd64 does not).
func goldenJSON(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		golden(t, name, got)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	var want, have any
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if where := jsonDiff("", want, have); where != "" {
		t.Errorf("%s differs from the golden file at %s (run with -update after checking):\n%s", name, where, got)
	}
}

func jsonDiff(at string, want, have any) string {
	switch w := want.(type) {
	case float64:
		h, ok := have.(float64)
		if !ok || math.Abs(w-h) > 1e-9*math.Max(1, math.Abs(w)) {
			return at
		}
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok || len(h) != len(w) {
			return at
		}
		for k, v := range w {
			if d := jsonDiff(at+"."+k, v, h[k]); d != "" {
				return d
			}
		}
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return at
		}
		for i := range w {
			if d := jsonDiff(fmt.Sprintf("%s[%d]", at, i), w[i], h[i]); d != "" {
				return d
			}
		}
	default:
		if want != have {
			return at
		}
	}
	return ""
}

func TestReportContents(t *testing.T) {
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	var md, js bytes.Buffer
	rep.Markdown(&md)
	rep.JSON(&js)
	text := md.String()
	for _, want := range []string{"# Experiment lean-ab", "Context A/B: A = `base`, B = `lean`. Goal: cheaper, without losing success.",
		"**Cost -20%** (95%: ", "): improved.", "**Success ", "exploratory", "60 of 60 runs settled (done)", "| B | `lean` |", "(-3000)",
		"Runs not counted: 1 unfair (the environment drifted), 1 infrastructure failure, 1 cancelled.",
		"Environment drift in unfair runs: tools differ (added Monitor; missing none); 1 file tool call(s) reached <agentium data>/projects.",
		"1 run(s) ended without Claude Code's cost", "1 run(s) were cut short when Agentium stopped", "Arm ", "passed with test-runner configuration changed",
		"Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory.",
		"Success is exploratory: 9 of 10 task(s) have 3 or more counted runs in both arms, below the floor of 20 tasks (method phase1-v2).",
		"## Noise", "| σ, per-run spread of log cost | 0.04 |",
		"Cold-cache cost reprices every cached read", "| task-0 |"} {
		if !strings.Contains(text, want) {
			t.Errorf("Markdown lacks %q", want)
		}
	}
	if !strings.Contains(js.String(), "the hidden tests could not be added: open <agentium data>/records/r05") {
		t.Error("run notes are kept, with local paths replaced")
	}
	for _, private := range []string{"personal-looking-skill", "/home/someone", "\"review\"", "/usr/local/bin", "sk-ant-api03", "Done in"} {
		if strings.Contains(js.String(), private) || strings.Contains(text, private) {
			t.Errorf("the report shows %q", private)
		}
	}
	if !strings.Contains(js.String(), "(1 skill)") || !strings.Contains(js.String(), "(2 slash commands)") || !strings.Contains(js.String(), `"claude_path": "claude"`) {
		t.Error("the JSON lock should count skills and slash commands")
	}
	if rep.Arms[0].Behavior.ConfigPasses+rep.Arms[1].Behavior.ConfigPasses != 1 {
		t.Errorf("config passes %d + %d, want the one that passed", rep.Arms[0].Behavior.ConfigPasses, rep.Arms[1].Behavior.ConfigPasses)
	}
	// Counted runs: 60 slots, one unfair; the infra and cancelled tries of slot 10 are extra runs, not counted.
	if rep.Arms[0].Counted+rep.Arms[1].Counted != 59 || len(rep.Runs) != 62 || rep.Settled != 60 {
		t.Errorf("counted %d+%d, runs %d, settled %d", rep.Arms[0].Counted, rep.Arms[1].Counted, len(rep.Runs), rep.Settled)
	}
}

func TestReportAA(t *testing.T) {
	in := fixture()
	in.Lock.Design.Template = experiment.TemplateAA
	in.Lock.Arms[1].Context, in.Lock.Arms[1].Snapshot = "base", ""
	in.Status, in.StatusNote = experiment.StatusBudget, "the next run would not fit the $60.00 budget"
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var md bytes.Buffer
	rep.Markdown(&md)
	for _, want := range []string{"A/A calibration of context `base`", "any difference is noise",
		"The experiment is not finished (budget: the next run would not fit the $60.00 budget)", "## Noise", "this is the noise itself"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("A/A report lacks %q", want)
		}
	}
}

// A guard loss the intervals can tell from none is "regressed", even within the margin (the study's order of rules);
// the headline says so.
func TestHeadlineGuardLossWithinTheMargin(t *testing.T) {
	a, b := 0.80, 0.77
	res := experiment.MetricResult{Metric: experiment.MetricSuccess, Role: experiment.RoleGuard, Tasks: 40, A: &a, B: &b,
		Boot95: stats.Interval{Estimate: -0.03, Low: -0.05, High: -0.01}, T95: stats.Interval{Estimate: -0.03, Low: -0.05, High: -0.01},
		Verdict: stats.Regressed}
	got := headline(res, experiment.Design{SuccessMargin: 0.15})
	if !strings.Contains(got, "regressed (a loss the intervals can tell from none, though within the 15 pp margin)") || !strings.Contains(got, "80% → 77%") {
		t.Errorf("headline %q", got)
	}
}

// An arm with no counted runs shows no numbers, rather than zeros that read as measured.
func TestReportArmWithoutCountedRuns(t *testing.T) {
	in := fixture()
	for i := range in.Runs {
		if in.Runs[i].Record.Arm == "B" {
			in.Runs[i].Record.Outcome, in.Runs[i].Record.Passed = claude.OutcomeUnfair, nil
		}
	}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var md, js bytes.Buffer
	rep.Markdown(&md)
	if err := rep.JSON(&js); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| B | `lean` | 0 | - | - | - | - | - |", "→ - |", "**Cost**: no result"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("Markdown lacks %q", want)
		}
	}
	if strings.Contains(md.String(), "## Noise") || strings.Contains(md.String(), "$0.000") {
		t.Error("no paired tasks: no noise estimate, and no zero costs")
	}
	if !strings.Contains(js.String(), `"cost_usd": null`) {
		t.Error("JSON: an arm without runs has null means")
	}
}

func TestReportBetterGoalAndNoLossHeadline(t *testing.T) {
	in := fixture()
	in.Lock.Design.Goal = experiment.GoalBetter
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var md bytes.Buffer
	rep.Markdown(&md)
	if !strings.Contains(md.String(), "Verdicts are given for success (primary); cost and time and output tokens are exploratory.") {
		t.Errorf("the verdict note follows the goal:\n%s", md.String())
	}
	// "No loss" rests on the 90% interval, and the headline shows that one.
	a, b := 0.80, 0.78
	res := experiment.MetricResult{Metric: experiment.MetricSuccess, Role: experiment.RoleGuard, Tasks: 30, A: &a, B: &b, Verdict: stats.NoLoss,
		Boot95: stats.Interval{Estimate: -0.02, Low: -0.17, High: 0.10}, T95: stats.Interval{Estimate: -0.02, Low: -0.16, High: 0.09},
		Boot90: stats.Interval{Estimate: -0.02, Low: -0.14, High: 0.08}, T90: stats.Interval{Estimate: -0.02, Low: -0.13, High: 0.07}}
	if got := headline(res, experiment.Design{SuccessMargin: 0.15}); !strings.Contains(got, "Δ -2 pp (90%: -14 to +8): no loss beyond 15 pp.") {
		t.Errorf("headline %q", got)
	}
}

func TestScrubReplacesWholePaths(t *testing.T) {
	data := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(data)
	in := Input{DataDir: data, Home: "/Users/a"}
	for text, want := range map[string]string{
		"open /Users/a/.claude/x: denied":             "open ~/.claude/x: denied",
		"/Users/alex/project stays":                   "/Users/alex/project stays",
		"at /Users/a":                                 "at ~",
		"reached " + data + "/projects":               "reached <agentium data>/projects",
		"reached " + resolved + "/records/r1":         "reached <agentium data>/records/r1",
		"key sk-ant-api03-" + strings.Repeat("z", 40): "key [REDACTED]", // secret-scan: allow
	} {
		got := in.scrub(text)
		if strings.Contains(want, "[REDACTED]") {
			if strings.Contains(got, "sk-ant-api03") {
				t.Errorf("scrub(%q) = %q: the key stayed", text, got)
			}
			continue
		}
		if got != want {
			t.Errorf("scrub(%q) = %q, want %q", text, got, want)
		}
	}
}

// The one-run note names the interval that decides: the t-interval usually, the bootstrap when a skewed task reaches
// past it, or each on its own side.
func TestWiderNamesTheDecidingInterval(t *testing.T) {
	i := func(lo, hi float64) stats.Interval { return stats.Interval{Low: lo, High: hi} }
	for _, c := range []struct {
		t, boot stats.Interval
		want    string
	}{{i(-0.3, 0.1), i(-0.2, 0.05), "the t-interval"}, {i(-0.2, 0.05), i(-0.3, 0.1), "the bootstrap"}, {i(-0.3, 0.05), i(-0.2, 0.1), "on each side"}} {
		if got := wider(c.t, c.boot); !strings.Contains(got, c.want) {
			t.Errorf("wider(%v, %v) = %q, want %q", c.t, c.boot, got, c.want)
		}
	}
}

// The shared report names only the subagent types the run's context use names; the rest share "other".
func TestReportSubagentModelsAreShareable(t *testing.T) {
	in := fixture()
	rec := &in.Runs[0].Record
	rec.Metrics.SubagentModels = map[string][]string{"Explore": {"claude-haiku-4-5"}, "my-personal-agent": {"claude-sonnet-5"},
		"unknown": {"claude-sonnet-5"}}
	rec.ContextUse = &run.ContextUse{Start: []string{"CLAUDE.md"}, Subagents: []string{"Explore"}, OtherSubagents: 1}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Runs[0].Metrics.SubagentModels
	want := map[string][]string{"Explore": {"claude-haiku-4-5"}, "other": {"claude-sonnet-5"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("subagent models = %v, want %v", got, want)
	}
	if in.Runs[0].Record.Metrics.SubagentModels["my-personal-agent"] == nil {
		t.Error("the input's record was changed")
	}
}

// An experiment whose agents had the sandbox's local binding says so in the report, in every format: the grant is wider
// than its name (any local port, and localhost services), so the reader must be able to see it.
func TestReportShowsLocalBinding(t *testing.T) {
	in := fixture()
	in.Lock.LocalBinding = true
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var md, termOut, js bytes.Buffer
	rep.Markdown(&md)
	rep.Terminal(&termOut, term.Style{})
	rep.JSON(&js)
	for name, out := range map[string]string{"Markdown": md.String(), "terminal": termOut.String()} {
		if !strings.Contains(out, "allowed local binding") {
			t.Errorf("%s lacks the local binding note", name)
		}
	}
	if !strings.Contains(js.String(), `"local_binding": true`) {
		t.Error("the JSON lock lacks local_binding")
	}
	plain, _ := Build(fixture())
	var out bytes.Buffer
	plain.Markdown(&out)
	if strings.Contains(out.String(), "local binding") {
		t.Error("a note without local binding")
	}
}

// isolatedCells returns the Isolated-run cost cell of each arm's row in the three renderings.
func isolatedCells(t *testing.T, rep Report) (md, txt, js []string) {
	t.Helper()
	var m, p, j bytes.Buffer
	if err := rep.Markdown(&m); err != nil {
		t.Fatal(err)
	}
	if err := rep.Terminal(&p, term.Style{}); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&j); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(m.String(), "\n") {
		if cells := strings.Split(line, " | "); strings.HasPrefix(line, "| A |") || strings.HasPrefix(line, "| B |") {
			if len(cells) == 8 { // the cost table: arm, context, counted, first, cost, isolated, cold, share
				md = append(md, cells[5])
			}
		}
	}
	lines := strings.Split(p.String(), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "Arm  Context  Runs counted") {
			continue
		}
		for _, row := range lines[i+1:] {
			if strings.TrimSpace(row) == "" {
				break
			}
			// Columns are separated by two spaces; the cost columns are the 5th to 7th of the row ("27000 (-3000)" has one).
			cells := regexp.MustCompile(`  +`).Split(strings.TrimSpace(row), -1)
			txt = append(txt, cells[5])
		}
		break
	}
	var out struct {
		Arms []struct {
			Isolated *float64 `json:"isolated_cost_usd"`
		} `json:"arms"`
	}
	if err := json.Unmarshal(j.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, a := range out.Arms {
		if a.Isolated == nil {
			js = append(js, "null")
		} else {
			js = append(js, fmt.Sprintf("$%.3f", *a.Isolated))
		}
	}
	return
}

// The isolated-run cost is the mean over counted runs when every one has it, beside the actual cost; the table and the
// JSON agree, and the verdicts do not move.
func TestReportIsolatedCost(t *testing.T) {
	base, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range base.Arms {
		if a.IsolatedCostUSD == nil || a.CostUSD == nil || math.Abs(*a.IsolatedCostUSD-*a.CostUSD-0.02) > 1e-9 {
			t.Fatalf("arm %s: isolated %v, actual %v; want actual + 0.02", a.Name, a.IsolatedCostUSD, a.CostUSD)
		}
	}
	md, txt, js := isolatedCells(t, base)
	if len(md) != 2 || !reflect.DeepEqual(md, js) || !reflect.DeepEqual(txt, js) {
		t.Errorf("renderings disagree: markdown %v, terminal %v, JSON %v", md, txt, js)
	}
	var out bytes.Buffer
	base.Markdown(&out)
	if !strings.Contains(out.String(), "Isolated-run cost is each run's cost had no other run warmed the prompt cache") ||
		!strings.Contains(out.String(), "Verdicts use the actual cost.") || strings.Contains(out.String(), "have no isolated-run cost") {
		t.Error("the note is missing, or it names missing values when there are none")
	}
}

// One counted run without a value leaves its arm without a mean (never a mean of the others) and says how many; an
// arm whose runs all lack it shows "-" and null. Verdicts are the same either way.
func TestReportIsolatedCostMissing(t *testing.T) {
	base, _ := Build(fixture())
	in := fixture()
	missing, droppedA := 0, false
	for i := range in.Runs {
		rec := &in.Runs[i].Record
		switch {
		case rec.Arm == "B":
			rec.IsolatedCostUSD = nil
			if experiment.Fair(rec.Outcome) {
				missing++
			}
		case rec.Arm == "A" && !droppedA && experiment.Fair(rec.Outcome): // one counted run of arm A
			rec.IsolatedCostUSD, droppedA = nil, true
			missing++
		}
	}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Arms[0].IsolatedCostUSD != nil || rep.Arms[1].IsolatedCostUSD != nil {
		t.Errorf("an arm with a missing run shows %v and %v", rep.Arms[0].IsolatedCostUSD, rep.Arms[1].IsolatedCostUSD)
	}
	md, txt, js := isolatedCells(t, rep)
	if !reflect.DeepEqual(md, []string{"-", "-"}) || !reflect.DeepEqual(txt, md) || !reflect.DeepEqual(js, []string{"null", "null"}) {
		t.Errorf("cells: markdown %v, terminal %v, JSON %v", md, txt, js)
	}
	var out bytes.Buffer
	rep.Markdown(&out)
	want := fmt.Sprintf("%d counted run(s) have no isolated-run cost (recorded before Agentium kept it, no reported cost, a model without a list price, a subagent request without a model, or a subagent of unknown type), so their arm shows none.", missing)
	if !strings.Contains(out.String(), want) {
		t.Errorf("Markdown lacks %q", want)
	}
	if !reflect.DeepEqual(rep.Analysis, base.Analysis) {
		t.Error("the isolated-run cost changed a verdict")
	}
}

// Only counted runs matter: an uncounted run without a value leaves the arm's mean, and the note, alone.
func TestReportIsolatedCostIgnoresUncountedRuns(t *testing.T) {
	in := fixture()
	dropped := 0
	for i := range in.Runs {
		if rec := &in.Runs[i].Record; !experiment.Fair(rec.Outcome) {
			rec.IsolatedCostUSD = nil
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("the fixture has no uncounted run")
	}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range rep.Arms {
		if a.IsolatedCostUSD == nil {
			t.Errorf("arm %s lost its isolated-run cost because of an uncounted run", a.Name)
		}
	}
	var out bytes.Buffer
	rep.Markdown(&out)
	if strings.Contains(out.String(), "have no isolated-run cost") {
		t.Error("the note counts uncounted runs")
	}
}

// The title names the modules the locked tasks ran in: one shared module, several (the root among them), or none at all
// for an experiment at the repository's root, as before modules.
func TestModulesTitle(t *testing.T) {
	lock := func(modules ...string) experiment.Lock {
		var l experiment.Lock
		for _, m := range modules {
			l.Tasks = append(l.Tasks, experiment.LockedTask{Module: m})
		}
		return l
	}
	for _, c := range []struct {
		lock experiment.Lock
		want string
	}{
		{lock("", ""), ""}, {lock(), ""}, {lock("svc/billing", "svc/billing"), " · svc/billing"},
		{lock("b", "a", ""), " · modules (root), a, b"},
	} {
		if got := modulesTitle(c.lock); got != c.want {
			t.Errorf("modulesTitle(%v) = %q, want %q", c.lock.Tasks, got, c.want)
		}
	}
}
