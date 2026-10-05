package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/report/reporttest"
	"github.com/pigeaca/agentium/internal/term"
)

var planNow = time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)

// planScenes are reviews as plan and start draw them: a seq-v1 experiment that is ready, a fixed design that is not
// (a missing task, a warning and a calibration to make), and one whose costs are unknown.
func planScenes(t *testing.T) map[string]experiment.Review {
	t.Helper()
	seq, err := reporttest.Seq(0.5, 0, true, experiment.StatusDone)
	if err != nil {
		t.Fatal(err)
	}
	sd := seq.Lock.Design
	est := experiment.EstimateRun(sd.Model, []experiment.PastRun{{Task: "task-00", CostUSD: 0.30}, {Task: "task-01", CostUSD: 0.34}, {Task: "task-02", CostUSD: 0.38}})
	est.CapUSD = sd.RunBudgetUSD
	ready := experiment.Review{Design: sd, Estimates: experiment.Same(est), Rows: experiment.PreviewFor(sd, sd.Tasks, experiment.Same(est)),
		Readiness: experiment.Readiness{Ready: true, Checks: []experiment.Check{{Status: "ok", Text: "Claude Code 2.1.281"}, {Status: "ok", Text: "16 task(s), each valid in every arm's context"}}}}

	fixed := reporttest.OneRun(experiment.MethodV2, experiment.TemplateContextAB).Lock.Design
	fe := experiment.EstimateRun(fixed.Model, nil)
	fe.CapUSD = fixed.RunBudgetUSD
	notReady := experiment.Review{Design: fixed, Estimates: experiment.Same(fe), Rows: experiment.PreviewFor(fixed, fixed.Tasks, experiment.Same(fe)),
		Readiness: experiment.Readiness{Checks: []experiment.Check{{Status: "ok", Text: "Claude Code 2.1.281"},
			{Status: "MISSING", Text: "task value: its tests do not fail on the base commit"},
			{Status: "WARNING", Text: "the budget $30.00 is below the estimated $41.00 plus $12.00 held for runs in flight: expect it to stop the experiment early"}},
			Calibrations: []experiment.CalibrationNeed{{Arm: fixed.Arms[1], Model: fixed.Model, Why: "none yet"}}}}

	unknown := notReady
	unknown.Estimates = experiment.Same(experiment.Estimate{})
	unknown.Rows = experiment.PreviewFor(fixed, fixed.Tasks, unknown.Estimates)
	unknown.Readiness = ready.Readiness
	return map[string]experiment.Review{"seq": ready, "not-ready": notReady, "unknown": unknown}
}

var planSceneNames = []string{"seq", "not-ready", "unknown"}

// TestPlanViewPreview writes the plan view of each scene (and the pool's health) for ansi2svg.py: AGENTIUM_PLAN_DEMO
// names the folder. Skipped unless asked.
func TestPlanViewPreview(t *testing.T) {
	dir := os.Getenv("AGENTIUM_PLAN_DEMO")
	if dir == "" {
		t.Skip("set AGENTIUM_PLAN_DEMO to a folder to write the plan view's previews")
	}
	sh := term.Shapes{Style: term.Colored().WithDepth(term.Color256)}
	for name, r := range planScenes(t) {
		text := "\x1b[36m$\x1b[39m agentium experiment plan lean-seq\n" + strings.Join(planView(r, "lean-seq", "login", planNow, sh, 80), "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "plan-"+name+".ans"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, h := range healthScenes() {
		text := "\x1b[36m$\x1b[39m agentium pool status\n" + strings.Join(healthView(h, healthTells[name], sh, 80, sh.Style.Command("agentium pool update")), "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "pool-"+name+".ans"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlanViewGoldens(t *testing.T) {
	scenes := planScenes(t)
	for _, name := range planSceneNames {
		var b strings.Builder
		for _, width := range []int{term.MinWidth, 80, 120} {
			fmt.Fprintf(&b, "=== %d columns\n", width)
			for _, line := range planView(scenes[name], "lean-seq", "login", planNow, plainUnicode, width) {
				if w := term.Width(line); w > width {
					t.Errorf("%s at %d columns: a line of %d cells: %q", name, width, w, line)
				}
				b.WriteString(line + "\n")
			}
		}
		checkGolden(t, "plan-"+name+".golden", b.String())
	}
	checkGolden(t, "plan-seq-color.golden", strings.Join(planView(scenes["seq"], "lean-seq", "login", planNow, color256, 80), "\n")+"\n")
	checkGolden(t, "plan-seq-ascii.golden", strings.Join(planView(scenes["seq"], "lean-seq", "login", planNow, plainASCII, 80), "\n")+"\n")
}

// The view says what matters in plain words, never shows a table or the jargon of --details, and keeps a missing
// piece and a warning in sight.
func TestPlanViewWords(t *testing.T) {
	scenes := planScenes(t)
	view := func(name string) string {
		return strings.Join(planView(scenes[name], "lean-seq", "login", planNow, plainUnicode, 100), "\n")
	}
	for name, want := range map[string][]string{
		"seq":       {"BASELINE", "LEAN", "does lean save money?", "everything is in place (2 checks)", "what it may spend", "likely", "all tasks run", "budget", "when it checks the answer", "check 1: 8 tasks", "agentium experiment plan lean-seq --details"},
		"not-ready": {"✗ task value: its tests do not fail", "! the budget $30.00 is below", "✓ 1 check ok", "plus about", "calibrate 1 context", "not ready to run"},
		"unknown":   {"cost unknown until runs measure it"},
	} {
		v := view(name)
		for _, w := range want {
			if !strings.Contains(v, w) {
				t.Errorf("%s: want %q in:\n%s", name, w, v)
			}
		}
	}
	for name := range scenes {
		for _, jargon := range []string{"EXPLORATORY", "NO-LOSS GUARD", "σ", "Floors", "look"} {
			if strings.Contains(view(name), jargon) {
				t.Errorf("%s: the picture has %q:\n%s", name, jargon, view(name))
			}
		}
	}
	if v := view("seq"); strings.Contains(v, "not ready") {
		t.Errorf("a ready plan says it is not:\n%s", v)
	}
	evil := scenes["not-ready"]
	evil.Readiness.Checks = []experiment.Check{{Status: "MISSING", Text: "a\x1b[2Jb"}}
	if v := strings.Join(planView(evil, "x\x1b[31m", "login", planNow, plainUnicode, 80), "\n"); strings.Contains(v, "\x1b") {
		t.Errorf("an escape reached the screen: %q", v)
	}
}

func healthScenes() map[string]pool.Health {
	return map[string]pool.Health{
		"mixed": {Total: 20, Valid: 12, Weak: 3, Flaky: 2, Invalid: 1, AwaitingReview: 2, Retired: 3, LastPass: time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC),
			OldestValidBase: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)},
		"fresh": {Total: 4, Valid: 0, Unvalidated: 4},
		"empty": {},
	}
}

// healthTells are the scenes' counts of what the tasks tell, as poolStatus gathers them.
var healthTells = map[string]tellCounts{
	"mixed": {Separate: 6, TooEasy: 2, ToCheck: 1, FewRuns: 3, NotRun: 2, NotReady: 3, Retired: 3},
	"fresh": {NotReady: 4},
	"empty": {},
}

func TestHealthViewGoldens(t *testing.T) {
	scenes := healthScenes()
	for _, name := range []string{"mixed", "fresh", "empty"} {
		var b strings.Builder
		for _, width := range []int{term.MinWidth, 80, 120} {
			fmt.Fprintf(&b, "=== %d columns\n", width)
			for _, line := range healthView(scenes[name], healthTells[name], plainUnicode, width, "agentium pool update") {
				if w := term.Width(line); w > width {
					t.Errorf("%s at %d columns: a line of %d cells: %q", name, width, w, line)
				}
				b.WriteString(line + "\n")
			}
		}
		checkGolden(t, "pool-"+name+".golden", b.String())
	}
	checkGolden(t, "pool-mixed-color.golden", strings.Join(healthView(scenes["mixed"], healthTells["mixed"], color256, 80, "agentium pool update"), "\n")+"\n")
	checkGolden(t, "pool-mixed-ascii.golden", strings.Join(healthView(scenes["mixed"], healthTells["mixed"], plainASCII, 80, "agentium pool update"), "\n")+"\n")
	// Only states with tasks have a bar, valid always; the others are in --json and the plain lines.
	v := strings.Join(healthView(scenes["fresh"], healthTells["fresh"], plainUnicode, 80, "agentium pool update"), "\n")
	for _, absent := range []string{"flaky", "invalid", "retired", "awaiting review"} {
		if strings.Contains(v, absent) {
			t.Errorf("a state with no tasks has a bar (%s):\n%s", absent, v)
		}
	}
}

// On a terminal plan and pool status are pictures; --details, NO_COLOR, TERM=dumb, a narrow terminal, a pipe and
// --json give the lines as before.
func TestPlanAndPoolStatusOnATerminal(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "1", "--seed", "5"), ExitOK)
	piped := f.run(ctx, "experiment", "plan", "lean-ab")
	expect(t, piped, ExitOK, "Experiment lean-ab:", "Before it runs:", "Sizes (runs count both arms):")
	pool := f.run(ctx, "pool", "status")
	expect(t, pool, ExitOK, "Task pool: ", "  valid ")

	*f.terminal = true
	designed := f.run(ctx, "experiment", "plan", "lean-ab")
	expect(t, cliResult{designed.code, term.Plain(designed.stdout), designed.stderr}, ExitOK, "what it may spend", "before it runs", "agentium experiment plan lean-ab --details")
	if strings.Contains(term.Plain(designed.stdout), "Sizes (runs count both arms)") {
		t.Errorf("the picture carries the sizes table:\n%s", designed.stdout)
	}
	details := f.run(ctx, "experiment", "plan", "lean-ab", "--details")
	if details.code != ExitOK || term.Plain(details.stdout) != piped.stdout {
		t.Errorf("--details on a terminal differs from the piped review:\n%s", details.stdout)
	}
	bars := f.run(ctx, "pool", "status")
	expect(t, cliResult{bars.code, term.Plain(bars.stdout), bars.stderr}, ExitOK, "task pool", "valid")
	f.vars["NO_COLOR"] = "1"
	if got := f.run(ctx, "experiment", "plan", "lean-ab"); got.stdout != piped.stdout {
		t.Errorf("NO_COLOR plan differs from the piped review:\n%s", got.stdout)
	}
	if got := f.run(ctx, "pool", "status"); got.stdout != pool.stdout {
		t.Errorf("NO_COLOR pool status differs from the piped lines:\n%s", got.stdout)
	}
	delete(f.vars, "NO_COLOR")
	for name, value := range map[string]string{"TERM": "dumb", "COLUMNS": "40"} {
		f.vars[name] = value
		if got := f.run(ctx, "experiment", "plan", "lean-ab"); term.Plain(got.stdout) != piped.stdout {
			t.Errorf("%s=%s plan differs from the piped review:\n%s", name, value, got.stdout)
		}
		if got := f.run(ctx, "pool", "status"); term.Plain(got.stdout) != pool.stdout {
			t.Errorf("%s=%s pool status differs from the piped lines:\n%s", name, value, got.stdout)
		}
		delete(f.vars, name)
	}
	for _, args := range [][]string{{"experiment", "plan", "lean-ab", "--json"}, {"pool", "status", "--json"}} {
		if js := f.run(ctx, args...); js.code != ExitOK || strings.Contains(js.stdout, "\x1b") || !strings.HasPrefix(strings.TrimSpace(js.stdout), "{") {
			t.Errorf("%v on a terminal:\n%s", args, js.stdout)
		}
	}
}
