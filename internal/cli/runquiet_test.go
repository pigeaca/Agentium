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

// quietScene is a scene for the quiet view.
func quietScene(t *testing.T, lock experiment.Lock, standing experiment.Standing) *scene {
	return newScene(t, lock, standing)
}

func (s *scene) quiet(sh term.Shapes, width, height int) string {
	return strings.Join(quietFrame(s.state.view(), sh, s.clock.Now(), width, height), "\n")
}

// quietSizes are the terminals the quiet view's goldens cover: the roomy, the narrow, and the short ones (the hint goes,
// then the last results, then the blank rows).
var quietSizes = [][2]int{{79, 40}, {99, 40}, {119, 40}, {64, 40}, {79, 14}, {79, 11}, {79, 9}, {60, 40}}

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

// quietSeq is a seq-v1 scene on a sandbox lock with 32 runs and checks after 8, 12 and 16 tasks.
func quietSeq(t *testing.T) *scene {
	return quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
}

func look(s *scene, n int, verdict string, ratio float64, decision string, planned int) {
	s.event(experiment.Event{Kind: "look", Look: &experiment.Look{Look: n, Planned: planned, Counted: planned, Analysed: true, Verdict: verdict,
		Interval: &stats.Interval{Estimate: ratio, Low: ratio - 0.1, High: ratio + 0.1}, Decision: decision}, Looks: 3})
}

func TestQuietGoldens(t *testing.T) {
	t.Parallel()
	start := quietSeq(t)
	start.start(0)
	start.start(1)
	start.clock.add(time.Second)

	mid, host := midRun(t, true), midRun(t, false)

	// Two-digit counts: eleven runs settled in each arm, a run of each in flight.
	counts := quietSeq(t)
	for pos := range 22 {
		counts.whole(pos, pos != 4 && pos != 9)
	}
	look(counts, 1, stats.Inconclusive, 0.84, experiment.LookContinue, 8)
	counts.start(22)
	counts.start(23)
	counts.clock.add(2 * time.Second)
	counts.step(22, run.StepAgent)

	// The last live frame of a run every scheduled run of which settled.
	final := quietSeq(t)
	for pos := range 32 {
		final.whole(pos, pos%3 != 0)
	}
	look(final, 3, stats.Inconclusive, 0.93, experiment.LookFinal, 16)
	final.clock.add(time.Second)

	// The last live frame of a run a check stopped early: 16 of 32, a check that is sure.
	stopped := quietSeq(t)
	for pos := range 16 {
		stopped.whole(pos, pos%3 != 0)
	}
	look(stopped, 2, stats.Improved, 0.82, experiment.LookStop, 8)
	stopped.clock.add(time.Second)

	for _, tc := range []struct {
		name  string
		scene *scene
	}{{"start", start}, {"mid", mid}, {"host", host}, {"counts", counts}, {"final", final}, {"stopped", stopped}} {
		checkQuiet(t, tc.name, tc.scene)
	}
	if f := term.Plain(start.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "no grades yet · first check after 8 tasks") || strings.Contains(f, "0 of 0") {
		t.Errorf("before any grade:\n%s", f)
	}
	if f := term.Plain(counts.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "next check after 12 tasks") || !strings.Contains(f, "baseline 10/11 · trimmed 10/11") {
		t.Errorf("two-digit counts at 79 columns lose the next check:\n%s", f)
	}
	if f := term.Plain(final.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "answer ") || strings.Contains(f, "so far") || !strings.Contains(f, "now      finishing") ||
		!strings.Contains(f, "32 of 32") {
		t.Errorf("the last frame of a finished run:\n%s", f)
	}
	if f := term.Plain(stopped.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "16 of 32 · stopped early") || !strings.Contains(f, "now      finishing") {
		t.Errorf("the last frame of a run a check stopped:\n%s", f)
	}
	if f := term.Plain(mid.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "so far") || !strings.Contains(f, "plan 64% used") ||
		!strings.Contains(f, "passed: baseline 1/1 · trimmed 0/1 · next check after 12 tasks") {
		t.Errorf("the mid frame:\n%s", f)
	}
	if f := term.Plain(host.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "hidden tests run on your machine") {
		t.Errorf("the host grader's warning is missing:\n%s", f)
	}
	for _, bad := range []string{"⠋", "⠙", "●", "┌", "╌"} {
		if f := mid.quiet(plainUnicode, 79, 40); strings.Contains(f, bad) {
			t.Errorf("the quiet view draws %q:\n%s", bad, f)
		}
	}
}

// TestQuietMomentsGoldens draws the quiet view at the moments the flow view's moments golden draws (copying, fetching
// dependencies, setup, the sandbox starting, the tests, cleaning up, a folder quarantined, passed, the host, the
// sandbox unavailable with and without a retry, flagged denials), as the whole frame at 79 columns.
func TestQuietMomentsGoldens(t *testing.T) {
	t.Parallel()
	var plain, color strings.Builder
	draw := func(name string, s *scene) {
		fmt.Fprintf(&plain, "=== %s\n%s\n", name, s.quiet(plainUnicode, 79, 40))
		fmt.Fprintf(&color, "=== %s\n%s\n", name, s.quiet(color256, 79, 40))
	}
	moments := func(sandbox bool) *scene {
		return quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, sandbox, true), experiment.Standing{})
	}
	s := moments(true)
	s.start(0)
	s.step(0, run.StepPreparing)
	s.clock.add(2 * time.Second)
	draw("copying", s)
	s.step(0, run.StepDependencies)
	s.clock.add(72 * time.Second)
	draw("fetching dependencies", s)
	s.step(0, run.StepSetup)
	s.clock.add(4 * time.Second)
	draw("running the setup", s)
	s.step(0, run.StepAgent)
	s.clock.add(2 * time.Minute)
	s.step(0, run.StepGrading)
	s.step(0, run.StepSandbox)
	s.clock.add(1500 * time.Millisecond)
	draw("starting the sandbox", s)
	s.step(0, run.StepTests)
	s.clock.add(38 * time.Second)
	draw("running the tests", s)
	s.step(0, run.StepCleanup)
	s.clock.add(1500 * time.Millisecond)
	draw("cleaning up", s)
	s.step(0, run.StepQuarantined)
	draw("a folder quarantined", s)
	yes := true
	s.finish(0, agent.OutcomeOK, &yes, 0.09)
	draw("passed", s)

	host := moments(false)
	host.start(0)
	host.step(0, run.StepAgent)
	host.step(0, run.StepGrading)
	host.step(0, run.StepTests)
	host.clock.add(9 * time.Second)
	draw("running the tests on the host", host)

	down := moments(true)
	down.start(0)
	down.step(0, run.StepAgent)
	down.step(0, run.StepGrading)
	down.step(0, run.StepSandbox)
	down.step(0, run.StepSandboxDown)
	draw("the sandbox unavailable, running", down)
	down.step(0, run.StepCleanup)
	down.finish(0, agent.OutcomeInfra, nil, 0.08)
	down.event(experiment.Event{Kind: "retry", Slot: down.lock.Schedule[0], Attempt: 1, RetryIn: 30 * time.Second})
	draw("the sandbox unavailable, a retry", down)

	spent := moments(true)
	spent.start(0)
	spent.step(0, run.StepAgent)
	spent.step(0, run.StepGrading)
	spent.step(0, run.StepSandbox)
	spent.step(0, run.StepSandboxDown)
	spent.finish(0, agent.OutcomeInfra, nil, 0.08)
	draw("the sandbox unavailable, no retry", spent)

	flagged := moments(true)
	flagged.start(0)
	flagged.step(0, run.StepAgent)
	flagged.step(0, run.StepGrading)
	flagged.step(0, run.StepSandbox)
	flagged.step(0, run.StepTests)
	flagged.event(experiment.Event{Kind: "finish", Slot: flagged.lock.Schedule[0], Attempt: 1, SpentUSD: 0.1,
		Result: experiment.Result{Outcome: run.OutcomeSandboxFlagged, CostUSD: 0.1, SandboxFlagged: "mach-lookup, file-read-data, mach-register"}})
	draw("flagged denials", flagged)
	fmt.Fprintf(&plain, "=== ASCII, copying\n%s\n", s2ASCII(t))
	checkWords(t, "the moments", plain.String())
	checkGolden(t, "quiet-moments.golden", plain.String())
	checkGolden(t, "quiet-moments-color.golden", color.String())
	for _, want := range []string{"fetching dependencies", "cleaning up", "sandbox unavailable · not counted", "blocked: a system service"} {
		if !strings.Contains(plain.String(), want) {
			t.Errorf("the moments lack %q", want)
		}
	}
}

// s2ASCII is a run in its first moment, in ASCII.
func s2ASCII(t *testing.T) string {
	s := quietScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	s.start(0)
	s.step(0, run.StepPreparing)
	s.clock.add(2 * time.Second)
	return s.quiet(plainASCII, 79, 40)
}

// TestQuietJudgeGraded is the flow view's judge scene in the quiet view: a judge-graded run's grading step is the judge's,
// and its result shows in the last results in the judge's words, beside a test-graded run.
func TestQuietJudgeGraded(t *testing.T) {
	t.Parallel()
	lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
	s := quietScene(t, lock, experiment.Standing{})
	yes, no := true, false
	judged := func(pos int, passed *bool, votes string) {
		s.start(pos)
		s.clock.add(3 * time.Second)
		s.step(pos, run.StepAgent)
		s.clock.add(2 * time.Minute)
		s.step(pos, run.StepGrading)
		s.clock.add(time.Second)
		s.step(pos, run.StepJudgeGrading)
		s.clock.add(40 * time.Second)
		s.spent += 0.4
		s.event(experiment.Event{Kind: "finish", Slot: lock.Schedule[pos], Attempt: 1, SpentUSD: s.spent,
			Result: experiment.Result{Outcome: agent.OutcomeOK, Passed: passed, CostUSD: 0.4, JudgeUSD: 0.3, JudgeGraded: true, JudgeVotes: votes,
				Judge: "fixed (" + votes + ")"}})
	}
	judged(0, &yes, "4 of 5")
	judged(1, &no, "3 of 5")
	s.clock.add(5 * time.Second)
	var b strings.Builder
	sizes := [][2]int{{79, 40}, {79, 14}, {79, 11}, {64, 40}}
	for _, size := range sizes {
		fmt.Fprintf(&b, "=== finished, %d columns, %d rows\n%s\n", size[0], size[1], s.quiet(plainUnicode, size[0], size[1]))
	}
	done := term.Plain(s.quiet(plainUnicode, 79, 40))
	for _, want := range []string{"✓ baseline", "judge: fixed (4 of 5)", "✗ trimmed", "judge: not fixed (3 of 5)", "no grades yet"} {
		if !strings.Contains(done, want) {
			t.Errorf("the finished frame lacks %q:\n%s", want, done)
		}
	}
	checkGolden(t, "quiet-judged-color.golden", s.quiet(color256, 79, 40)+"\n")
	s.whole(2, true)
	s.start(3)
	s.clock.add(2 * time.Second)
	s.step(3, run.StepAgent)
	s.clock.add(time.Minute)
	s.step(3, run.StepGrading)
	s.step(3, run.StepJudgeGrading)
	s.clock.add(12 * time.Second)
	for _, size := range sizes {
		fmt.Fprintf(&b, "=== judging, %d columns, %d rows\n%s\n", size[0], size[1], s.quiet(plainUnicode, size[0], size[1]))
	}
	fmt.Fprintf(&b, "=== judging, ASCII\n%s\n", s.quiet(plainASCII, 79, 40))
	checkWords(t, "judged", b.String())
	checkGolden(t, "quiet-judged.golden", b.String())
	// The test-graded run counts; the judge's two grades do not.
	if f := term.Plain(s.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "baseline 1/1 · trimmed 0/0") || !strings.Contains(f, "judge grading") {
		t.Errorf("the judging frame:\n%s", f)
	}
}

// The quiet view's rows never change with what the runs do: the same frame height from before the first run to the
// last, at every terminal height, through retries, checks, pauses and results; and a frame never holds more rows than
// the terminal (so the answer's rows are not the ones cut).
func TestQuietFrameDoesNotJump(t *testing.T) {
	t.Parallel()
	for _, conc := range []int{2, 8} {
		lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
		lock.Design.Concurrency = conc
		s := quietScene(t, lock, experiment.Standing{})
		heights := map[int]int{}
		check := func(when string) {
			t.Helper()
			for h := 7; h <= 40; h++ {
				lines := strings.Split(s.quiet(plainUnicode, 79, h), "\n")
				if h >= 9 && len(lines) > h {
					t.Errorf("%d at once, %s at %d rows: %d rows", conc, when, h, len(lines))
				}
				if was, ok := heights[h]; ok && was != len(lines) {
					t.Errorf("%d at once, %s at %d rows: the frame went from %d to %d rows", conc, when, h, was, len(lines))
				}
				heights[h] = len(lines)
			}
		}
		check("before any run")
		s.start(0)
		check("one run")
		for pos := 1; pos < conc && pos < 8; pos++ {
			s.start(pos)
		}
		check("every slot busy")
		s.event(experiment.Event{Kind: "retry", Slot: s.lock.Schedule[20], Attempt: 1, RetryIn: 30 * time.Second})
		check("a retry")
		s.event(experiment.Event{Kind: "wait", Until: s.clock.Now().Add(20 * time.Minute), Usage: 0.86})
		check("a pause")
		for pos := range min(conc, 8) {
			s.finish(pos, agent.OutcomeOK, &[]bool{true}[0], 0.1)
			check(fmt.Sprintf("after run %d", pos))
		}
	}
}

// With 8 runs at a time on a short terminal the "now" rows give way, not the answer's: 80 by 13 shows the passes and the
// next check, and "+ N more" stands for the runs left out.
func TestQuietManyAtOnceOnAShortTerminal(t *testing.T) {
	t.Parallel()
	lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
	lock.Design.Concurrency = 8
	s := quietScene(t, lock, experiment.Standing{})
	for pos := range 8 {
		s.start(pos)
	}
	s.finish(8, agent.OutcomeOK, &[]bool{true}[0], 0.1)
	f := term.Plain(s.quiet(plainUnicode, 80, 13))
	if n := len(strings.Split(f, "\n")); n > 13 || !strings.Contains(f, "passed: baseline 1/1 · trimmed 0/0 · first check after 8 tasks") || !strings.Contains(f, "+ ") {
		t.Errorf("%d rows:\n%s", n, f)
	}
	if full := term.Plain(s.quiet(plainUnicode, 80, 40)); strings.Contains(full, "more") || strings.Count(full, "fresh copy") != 8 {
		t.Errorf("a roomy terminal shows every run:\n%s", full)
	}
}

// Nothing in flight and nothing coming is "finishing", not "starting the next run".
func TestQuietFinishing(t *testing.T) {
	t.Parallel()
	s := quietSeq(t)
	s.whole(0, true)
	if f := term.Plain(s.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "starting the next run") {
		t.Errorf("between runs:\n%s", f)
	}
	look(s, 1, stats.Improved, 0.8, experiment.LookStop, 8)
	if f := term.Plain(s.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "now      finishing") {
		t.Errorf("after a check that stops:\n%s", f)
	}
	b := quietSeq(t)
	b.spent = 5
	b.whole(0, true)
	b.state.spent = 5
	if f := term.Plain(b.quiet(plainUnicode, 79, 40)); !strings.Contains(f, "now      finishing") {
		t.Errorf("at the budget:\n%s", f)
	}
}

// A long version label on a narrow terminal shrinks the task and step columns, never the time.
func TestQuietKeepsTheClock(t *testing.T) {
	t.Parallel()
	lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
	lock.Design.Arms[1].Context = "a-very-long-snapshot-name"
	s := quietScene(t, lock, experiment.Standing{})
	s.start(1)
	s.clock.add(2 * time.Second)
	s.step(1, run.StepAgent)
	s.clock.add(63 * time.Second)
	for _, width := range []int{60, 64, 80} {
		f := term.Plain(s.quiet(plainUnicode, width, 40))
		var row string
		for _, l := range strings.Split(f, "\n") {
			if strings.Contains(l, "Claude") || strings.Contains(l, "Cla…") {
				row = l
			}
		}
		if !strings.HasSuffix(row, " 1m03s") || term.Width(row) > width {
			t.Errorf("at %d columns the run's row lacks its clock: %q", width, row)
		}
	}
}

// A resumed run counts the passes the earlier execution stored: the settled, test-graded, fair runs; not a judge's, not
// an unsettled slot.
func TestQuietCountsPassesOfAResume(t *testing.T) {
	t.Parallel()
	lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
	yes, no := true, false
	data := []experiment.RunData{
		{Slot: 0, Arm: "A", Outcome: agent.OutcomeOK, Passed: &yes},
		{Slot: 1, Arm: "B", Outcome: agent.OutcomeOK, Passed: &no},
		{Slot: 2, Arm: "A", Outcome: agent.OutcomeOK, Passed: &yes, Judged: true},
		{Slot: 3, Arm: "B", Outcome: agent.OutcomeInfra},
		{Slot: 4, Arm: "A", Outcome: agent.OutcomeOK, Passed: &yes}, // not settled in the standing
	}
	// Through the screen's own Begin, as a resumed run starts: before the first frame.
	out := &syncBuffer{}
	_, screen := newRunScreen(context.Background(), Env{Stdout: out, Stderr: out, Now: time.Now}, viewDashboard,
		term.Capabilities{Terminal: true, Color: term.Color256, UTF8: true, Width: 100, Height: 40}, 85)
	defer screen.Close()
	screen.stored = func(experiment.Lock) []experiment.RunData { return data }
	standing := experiment.Standing{Settled: map[int]bool{0: true, 1: true, 2: true}}
	screen.observer(nil).Begin(lock, standing)
	s := newScene(t, lock, standing)
	s.state = screen.state
	f := term.Plain(s.quiet(plainUnicode, 79, 40))
	if !strings.Contains(f, "passed: baseline 1/1 · trimmed 0/1") || !strings.Contains(f, "3 of 32") {
		t.Errorf("after a resume:\n%s", f)
	}
	if strings.Contains(f, "no grades yet") {
		t.Errorf("a resume says nothing was graded:\n%s", f)
	}
	// Counted once: the settled slot finishing again (a run graded again) does not add.
	s.start(0)
	s.finish(0, agent.OutcomeOK, &yes, 0.1)
	if v := s.state.view(); v.passed[0] != 1 || v.graded[0] != 1 {
		t.Errorf("counted twice: %d of %d", v.passed[0], v.graded[0])
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
	if p := term.Plain(g); !strings.Contains(p, "blocked: a system service lookup, a file read") || !strings.Contains(p, "badna") {
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
