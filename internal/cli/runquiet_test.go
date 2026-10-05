package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/term"
)

// quietScene is a scene for the quiet view: the report command is set, as the screen sets it.
func quietScene(t *testing.T, lock experiment.Lock, standing experiment.Standing) *scene {
	s := newScene(t, lock, standing)
	s.state.facts.report = "trimmed-ab"
	return s
}

func (s *scene) quiet(sh term.Shapes, width, height int) string {
	return strings.Join(quietFrame(s.state.view(), sh, s.clock.Now(), width, height), "\n")
}

// quietSizes are the terminals the quiet view's goldens cover: the roomy, the narrow, and the short ones (the hint goes,
// then the last results, then the blank rows).
var quietSizes = [][2]int{{79, 40}, {99, 40}, {119, 40}, {64, 40}, {79, 14}, {79, 11}, {79, 9}}

// drawQuiet is a scene in every size, then in ASCII, as plain text.
func drawQuiet(s *scene) string {
	var b strings.Builder
	for _, size := range quietSizes {
		fmt.Fprintf(&b, "=== %d columns, %d rows\n%s\n", size[0], size[1], s.quiet(plainUnicode, size[0], size[1]))
	}
	fmt.Fprintf(&b, "=== ASCII, 79 columns, 40 rows\n%s\n", s.quiet(plainASCII, 79, 40))
	return b.String()
}

// checkQuiet writes or compares a scene's plain and color goldens, and checks that no line is wider than its terminal
// and that no frame is taller than a terminal of 9 rows or more.
func checkQuiet(t *testing.T, name string, s *scene) {
	t.Helper()
	plain := drawQuiet(s)
	checkWords(t, name, plain)
	checkGolden(t, "quiet-"+name+".golden", plain)
	checkGolden(t, "quiet-"+name+"-color.golden", s.quiet(color256, 79, 40)+"\n")
	for _, size := range quietSizes {
		lines := strings.Split(s.quiet(plainUnicode, size[0], size[1]), "\n")
		for _, line := range lines {
			if w := term.Width(line); w > size[0] {
				t.Errorf("%s at %v: a line of %d cells: %q", name, size, w, line)
			}
		}
		if len(lines) > size[1] {
			t.Errorf("%s at %v: %d rows", name, size, len(lines))
		}
	}
}

// quietMid is the quiet view a few runs in: baseline's run in the agent step, trimmed's in the judge, two results and
// a check made (midRun's moment).
func TestQuietGoldens(t *testing.T) {
	t.Parallel()
	start := quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	start.start(0)
	start.start(1)
	start.clock.add(time.Second)

	mid, host := midRun(t, true), midRun(t, false)
	mid.state.facts.report, host.state.facts.report = "trimmed-ab", "trimmed-ab"

	final := quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	for pos := range 16 {
		final.whole(pos, pos%3 != 0)
	}
	final.event(experiment.Event{Kind: "look", Look: &experiment.Look{Look: 1, Planned: 8, Counted: 8, Analysed: true, Verdict: stats.Improved,
		Interval: &stats.Interval{Estimate: 0.82, Low: 0.7, High: 0.95}, Decision: experiment.LookStop}, Looks: 3})
	final.state.end(experiment.Summary{Status: experiment.StatusDone}, nil)
	final.clock.add(5 * time.Second)

	for _, tc := range []struct {
		name  string
		scene *scene
	}{{"start", start}, {"mid", mid}, {"host", host}, {"final", final}} {
		checkQuiet(t, tc.name, tc.scene)
	}
	if f := final.quiet(plainUnicode, 79, 40); !strings.Contains(f, "answer ") || !strings.Contains(f, "agentium experiment report trimmed-ab") ||
		strings.Contains(f, "so far") {
		t.Errorf("the last frame is not the answer with its report command:\n%s", f)
	}
	if f := mid.quiet(plainUnicode, 79, 40); !strings.Contains(f, "so far") || !strings.Contains(f, "plan 64% used") || !strings.Contains(f, "passed: baseline 1 of 1 · trimmed 0 of 1 · next check after 12 tasks") {
		t.Errorf("the mid frame:\n%s", f)
	}
	if f := host.quiet(plainUnicode, 79, 40); !strings.Contains(f, "hidden tests run on your machine") {
		t.Errorf("the host grader's warning is missing:\n%s", f)
	}
	for _, bad := range []string{"⠋", "⠙", "●", "┌", "╌"} {
		if f := mid.quiet(plainUnicode, 79, 40); strings.Contains(f, bad) {
			t.Errorf("the quiet view draws %q:\n%s", bad, f)
		}
	}
}

// The quiet view's rows never change with what the runs do: the same frame height from before the first run to the
// last, at every height, through retries, checks, pauses and results.
func TestQuietFrameDoesNotJump(t *testing.T) {
	t.Parallel()
	s := quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	heights := map[int]int{}
	check := func(when string) {
		t.Helper()
		for _, h := range []int{40, 14, 11} {
			n := len(strings.Split(s.quiet(plainUnicode, 79, h), "\n"))
			if was, ok := heights[h]; ok && was != n {
				t.Errorf("%s at %d rows: the frame went from %d to %d rows", when, h, was, n)
			}
			heights[h] = n
		}
	}
	check("before any run")
	s.start(0)
	check("one run")
	s.start(1)
	check("two runs")
	s.event(experiment.Event{Kind: "retry", Slot: s.lock.Schedule[4], Attempt: 1, RetryIn: 30 * time.Second})
	check("a retry")
	s.event(experiment.Event{Kind: "wait", Until: s.clock.Now().Add(20 * time.Minute), Usage: 0.86})
	check("a pause")
	for pos := range 8 {
		s.whole(pos, pos%2 == 0)
		check(fmt.Sprintf("after run %d", pos))
	}
}

// Every status the flow view's status line has is in the quiet view's "now" area, in its words, and a pause ends with
// the next run.
func TestQuietStatuses(t *testing.T) {
	t.Parallel()
	// a long status wraps over the free rows: read the frame as words
	frame := func(s *scene) string {
		return strings.Join(strings.Fields(term.Plain(s.quiet(plainUnicode, 79, 40))), " ")
	}
	s := quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	if f := frame(s); !strings.Contains(f, "getting ready") {
		t.Errorf("before the first run:\n%s", f)
	}
	s.event(experiment.Event{Kind: "retry", Slot: s.lock.Schedule[0], Attempt: 1, RetryIn: 30 * time.Second})
	if f := frame(s); !strings.Contains(f, "retrying map-keys for baseline in 30s: it failed, not by Claude's doing") {
		t.Errorf("a retry:\n%s", f)
	}
	s.start(0)
	s.clock.add(2 * time.Second)
	s.step(0, run.StepAgent)
	s.event(experiment.Event{Kind: "step", Slot: s.lock.Schedule[0], Attempt: 1, Step: run.StepQuarantined})
	if f := frame(s); !strings.Contains(f, "cleanup quarantined a folder it could not remove") || !strings.Contains(f, "Claude works") {
		t.Errorf("a warning beside a run:\n%s", f)
	}
	s.finish(0, agent.OutcomeInfra, nil, 0.05)
	s.event(experiment.Event{Kind: "wait", Until: s.clock.Now().Add(20 * time.Minute), Usage: 0.86})
	if f := frame(s); !strings.Contains(f, "waiting for your Claude plan's usage to reset at 15:") || !strings.Contains(f, "in 20m00s") {
		t.Errorf("a pause:\n%s", f)
	}
	s.start(1)
	if f := frame(s); strings.Contains(f, "waiting for") || !strings.Contains(f, "fresh copy") {
		t.Errorf("after the pause:\n%s", f)
	}
	s.event(experiment.Event{Kind: "pair", Slot: s.lock.Schedule[1], Result: experiment.Result{Judge: "tie", JudgeUSD: 0.09}})
	if f := frame(s); !strings.Contains(f, "compared the two passing runs") {
		t.Errorf("a comparison:\n%s", f)
	}
	for _, status := range []struct{ ended, want string }{
		{experiment.StatusBudget, "stopped at the budget"}, {experiment.StatusUsage, "paused at your Claude plan's usage limit"},
		{experiment.StatusStopped, "stopped · run it again to go on"},
	} {
		e := quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
		e.state.end(experiment.Summary{Status: status.ended}, nil)
		if f := frame(e); strings.Count(f, status.want) != 2 { // the "now" status and the answer's own words
			t.Errorf("%s:\n%s", status.ended, f)
		}
	}
}

// A grade the sandbox blocked or left out, and the judge's grades, show in the last results with the dashboard's notes;
// names from outside Agentium are sanitized.
func TestQuietLastResults(t *testing.T) {
	t.Parallel()
	lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
	lock.Schedule[0].Task = "bad\x1b]0;title\x07name\u202e"
	s := quietScene(t, lock, experiment.Standing{})
	yes, no := true, false
	s.start(0)
	s.step(0, run.StepAgent)
	s.step(0, run.StepGrading)
	s.step(0, run.StepSandbox)
	s.event(experiment.Event{Kind: "finish", Slot: lock.Schedule[0], Attempt: 1, SpentUSD: 0.1,
		Result: experiment.Result{Outcome: run.OutcomeSandboxFlagged, CostUSD: 0.1, SandboxFlagged: "mach-lookup, file-read-data"}})
	s.start(1)
	s.step(1, run.StepAgent)
	s.step(1, run.StepGrading)
	s.step(1, run.StepSandboxDown)
	s.finish(1, agent.OutcomeInfra, nil, 0.08)
	for i, passed := range []*bool{&yes, &no} {
		pos := 2 + i
		s.start(pos)
		s.step(pos, run.StepAgent)
		s.step(pos, run.StepGrading)
		s.step(pos, run.StepJudgeGrading)
		s.event(experiment.Event{Kind: "finish", Slot: lock.Schedule[pos], Attempt: 1, SpentUSD: 0.5,
			Result: experiment.Result{Outcome: agent.OutcomeOK, Passed: passed, CostUSD: 0.4, JudgeGraded: true, JudgeVotes: "4 of 5", Judge: "fixed (4 of 5)"}})
	}
	f := term.Plain(s.quiet(plainUnicode, 99, 40))
	if strings.Contains(f, "blocked: a system service lookup, a file read") {
		t.Errorf("the oldest result is past the last 3:\n%s", f)
	}
	for _, want := range []string{"✓ baseline", "✗ trimmed", "judge: fixed (4 of 5)", "judge: not fixed (4 of 5)", "sandbox unavailable · not counted"} {
		if !strings.Contains(f, want) {
			t.Errorf("the last results lack %q:\n%s", want, f)
		}
	}
	s2 := quietScene(t, lock, experiment.Standing{})
	s2.start(0)
	s2.event(experiment.Event{Kind: "finish", Slot: lock.Schedule[0], Attempt: 1, SpentUSD: 0.1,
		Result: experiment.Result{Outcome: run.OutcomeSandboxFlagged, CostUSD: 0.1, SandboxFlagged: "mach-lookup, file-read-data"}})
	g := s2.quiet(color256, 119, 40)
	if p := term.Plain(g); !strings.Contains(p, "blocked: a system service lookup, a file read") || !strings.Contains(p, "badname") {
		t.Errorf("a blocked grade:\n%s", p)
	}
	for _, bad := range []string{"\x1b]", "\x07", "\u202e"} {
		if strings.Contains(g, bad) {
			t.Errorf("the frame holds %q:\n%q", bad, g)
		}
	}
	if !strings.Contains(g, "\x1b[38;5;220mblocked") {
		t.Errorf("the blocked grade is not in the caution color:\n%q", g)
	}
}

// With a judge-graded run in flight, the grading step is the judge's, as on the flow view.
func TestQuietJudging(t *testing.T) {
	t.Parallel()
	s := quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	s.start(0)
	s.clock.add(2 * time.Second)
	s.step(0, run.StepAgent)
	s.clock.add(time.Minute)
	s.step(0, run.StepGrading)
	s.step(0, run.StepJudgeGrading)
	s.clock.add(12 * time.Second)
	if f := term.Plain(s.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "judge grading") || !strings.Contains(f, "12s") {
		t.Errorf("the judge at work:\n%s", f)
	}
	for _, step := range []struct{ step, want string }{{run.StepPreparing, "copying"}, {run.StepDependencies, "fetching dependencies"}} {
		c := quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
		c.start(0)
		c.step(0, step.step)
		if f := term.Plain(c.quiet(plainUnicode, 79, 40)); !strings.Contains(f, step.want) {
			t.Errorf("%s:\n%s", step.step, f)
		}
	}
}

// Before the lock is read, the quiet view shows what is printed (a check, a calibration) with no spinner.
func TestQuietWaitingHasNoSpinner(t *testing.T) {
	t.Parallel()
	lines := quietWaiting(plainUnicode, unicodeMarks, "calibrating base", 79)
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "now") || !strings.Contains(got, "getting ready · calibrating base") ||
		strings.ContainsAny(got, "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") {
		t.Errorf("the waiting frame: %q", got)
	}
}

// nowRow is the quiet view's "now" row with a run in it.
var nowRow = regexp.MustCompile(`(?m)^  now {6}(baseline|lean) `)

// --view dashboard (and the default on a terminal) is the quiet view, --view flow the step boxes, --view log the log;
// AGENTIUM_VIEW says the same.
func TestExperimentRunViewsOnATerminal(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	writeFile(t, ctrl, "agent-sleep", "0.4")
	terminalVars(f)
	for i, tc := range []struct {
		args, env string
		quiet     bool // draws "so far" and "now", no boxes
		boxes     bool
		log       bool
	}{
		{"", "", true, false, false},
		{"--view dashboard", "", true, false, false},
		{"", "dashboard", true, false, false},
		{"--view flow", "", false, true, false},
		{"", "flow", false, true, false},
		{"--view log", "", false, false, true},
		{"", "log", false, false, true},
	} {
		name := fmt.Sprintf("views-%d", i)
		expect(t, f.run(ctx, "experiment", "new", name, "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "1", "--seed", "5"), ExitOK)
		f.vars["AGENTIUM_VIEW"] = tc.env
		args := []string{"experiment", "run", name}
		if tc.args != "" {
			args = append(args, strings.Fields(tc.args)...)
		}
		r := f.run(ctx, args...)
		expect(t, r, ExitOK)
		plain := term.Plain(r.stdout)
		hasQuiet := nowRow.MatchString(plain) && strings.Contains(plain, "so far")
		hasBoxes := strings.Contains(plain, "╭┄ sandbox") && strings.Contains(plain, "fresh copy")
		hasLog := !strings.Contains(r.stdout, "\x1b[?25l") && strings.Contains(r.stdout, "\x1b[")
		if hasQuiet != tc.quiet || hasBoxes != tc.boxes || hasLog != tc.log {
			t.Errorf("%q with AGENTIUM_VIEW=%q: quiet %v boxes %v log %v, want %v %v %v:\n%s", tc.args, tc.env, hasQuiet, hasBoxes, hasLog,
				tc.quiet, tc.boxes, tc.log, term.Plain(r.stdout))
		}
		if tc.quiet {
			for _, bad := range []string{"⠋", "●", "┆"} {
				if strings.Contains(r.stdout, bad) {
					t.Errorf("the quiet view drew %q", bad)
				}
			}
		}
	}
}
