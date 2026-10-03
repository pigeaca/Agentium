package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

var updateGoldens = flag.Bool("update", false, "rewrite the dashboard's golden files")

// screenLock is a made-up experiment's lock: base against "trimmed", 16 tasks, one run per task and arm, the pairs in
// order (A then B), graded in the sandbox; seq adds checks after 8, 12 and 16 tasks.
func screenLock(template, goal string, sandbox, seq bool) experiment.Lock {
	d := experiment.Design{Template: template, Goal: goal, Repeats: 1, BudgetUSD: 5, CostMargin: 0.10, SuccessMargin: 0.15,
		Concurrency: 2, RunBudgetUSD: 3, Timeout: 20 * time.Minute,
		Arms: []experiment.Arm{{Name: "A", Context: experiment.BaseContext}, {Name: "B", Context: "trimmed", Snapshot: "abc"}}}
	if template == experiment.TemplateAA {
		d.Arms[1] = experiment.Arm{Name: "B", Context: experiment.BaseContext}
	}
	if template == experiment.TemplateModelAB {
		d.Arms[0].Model, d.Arms[1] = "claude-sonnet-5", experiment.Arm{Name: "B", Context: experiment.BaseContext, Model: "claude-opus-5", Effort: "high"}
	}
	l := experiment.Lock{Design: d, Grader: task.GraderHost}
	if sandbox {
		l.Grader = task.GraderSandbox
	}
	names := []string{"map-keys", "chunk", "uniq-by", "keyby", "zip", "flatten-deep", "debounce", "throttle", "pick", "omit",
		"merge-with", "group-by", "partition", "sort-by", "range-step", "sum-by"}
	for p, n := range names {
		l.Tasks = append(l.Tasks, experiment.LockedTask{Name: n})
		l.Schedule = append(l.Schedule, experiment.Slot{Position: 2 * p, Pair: p, Task: n, Arm: "A", Repeat: 1},
			experiment.Slot{Position: 2*p + 1, Pair: p, Task: n, Arm: "B", Repeat: 1})
	}
	if seq {
		l.Method, l.Sequential = experiment.MethodSeq, &experiment.Sequential{Looks: []int{8, 12, 16}}
	}
	return l
}

// clock is a test's time: it moves only when told.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 3, 15, 34, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// scene drives a state through events at the clock's time.
type scene struct {
	t     *testing.T
	lock  experiment.Lock
	clock *clock
	state *runState
	spent float64
}

func newScene(t *testing.T, lock experiment.Lock, standing experiment.Standing) *scene {
	c := newClock()
	if standing.Settled == nil {
		standing.Settled = map[int]bool{}
	}
	return &scene{t: t, lock: lock, clock: c, state: newRunState(factsOf(lock, experiment.DefaultUsageLimit), standing, c.Now)}
}

func (s *scene) event(e experiment.Event) { s.state.apply(e) }

func (s *scene) start(pos int) {
	s.event(experiment.Event{Kind: "start", Slot: s.lock.Schedule[pos], Attempt: 1, SpentUSD: s.spent})
}

func (s *scene) step(pos int, step string) {
	s.event(experiment.Event{Kind: "step", Slot: s.lock.Schedule[pos], Attempt: 1, Step: step})
}

func (s *scene) finish(pos int, outcome string, passed *bool, cost float64) {
	s.spent += cost
	s.event(experiment.Event{Kind: "finish", Slot: s.lock.Schedule[pos], Attempt: 1, SpentUSD: s.spent,
		Result: experiment.Result{Outcome: outcome, Passed: passed, CostUSD: cost}})
}

// whole runs the slot at pos through every step, in the given times, and finishes it.
func (s *scene) whole(pos int, passed bool) {
	s.start(pos)
	s.clock.add(3 * time.Second)
	s.step(pos, run.StepAgent)
	s.clock.add(2*time.Minute + 14*time.Second)
	s.step(pos, run.StepGrading)
	s.clock.add(38 * time.Second)
	s.finish(pos, claude.OutcomeOK, &passed, 0.09)
}

func (s *scene) frame(sh term.Shapes, width, height, tick int) string {
	return strings.Join(dashboardFrame(s.state.view(), sh, s.clock.Now(), width, height, tick), "\n")
}

var (
	plainUnicode = term.Shapes{}
	color256     = term.Shapes{Style: term.Colored().WithDepth(term.Color256)}
	plainASCII   = term.Shapes{ASCII: true}
)

// checkGolden compares got with testdata/name, or writes it with -update.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	// A frame ends with the log's blank rows: the marker keeps them, and a golden never ends with blank lines.
	got = strings.ReplaceAll(got, "\x1b", `\e`) + "=== end\n"
	path := filepath.Join("testdata", name)
	if *updateGoldens {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test ./internal/cli -run %s -update writes it)", err, t.Name())
	}
	if got != string(want) {
		t.Errorf("%s differs (go test ./internal/cli -run %s -update rewrites it):\n%s", name, t.Name(), got)
	}
}

// banned are words the dashboard and the log view never say: they stay in the report.
var banned = regexp.MustCompile(`(?i)\b(look|looks|futility|interval|window|arm [AB])\b|\[\d`)

func checkWords(t *testing.T, what, text string) {
	t.Helper()
	if m := banned.FindString(term.Plain(text)); m != "" {
		t.Errorf("%s says %q:\n%s", what, m, term.Plain(text))
	}
}

// midRun is the dashboard a few runs in: baseline's run in the agent step, trimmed's in its tests, a dot on its way, two
// finished runs in the log and a check made.
func midRun(t *testing.T, sandbox bool) *scene {
	s := newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, sandbox, true),
		experiment.Standing{Spent: 2.17, Usage: claude.UsageReading{FiveHour: 0.64, FiveHourResets: time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)}, HasUsage: true})
	s.spent = 2.17
	s.whole(0, true)
	s.whole(1, false)
	ratio := 1.04
	s.event(experiment.Event{Kind: "look", Look: &experiment.Look{Look: 1, Planned: 8, Counted: 8, Analysed: true, Verdict: stats.Inconclusive,
		Interval: &stats.Interval{Estimate: ratio, Low: 0.8, High: 1.3}, Decision: experiment.LookContinue}, Looks: 3})
	s.start(2)
	s.start(3)
	s.clock.add(2 * time.Second)
	s.step(2, run.StepAgent)
	s.step(3, run.StepAgent)
	s.clock.add(2*time.Minute + 1*time.Second)
	s.step(3, run.StepGrading)
	s.clock.add(20 * time.Second)
	s.step(3, run.StepJudging) // trimmed: its tests done, the judge reading it
	s.clock.add(450 * time.Millisecond)
	return s
}

func TestDashboardGoldens(t *testing.T) {
	t.Parallel()
	start := newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	start.start(0)
	start.start(1)
	start.clock.add(time.Second)

	host := midRun(t, false)
	mid := midRun(t, true)

	final := newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
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
		var b strings.Builder
		for _, size := range [][2]int{{79, 40}, {99, 40}, {119, 40}, {79, 23}, {79, 12}, {64, 40}} {
			fmt.Fprintf(&b, "=== %d columns, %d rows\n%s\n", size[0], size[1], tc.scene.frame(plainUnicode, size[0], size[1], 3))
		}
		fmt.Fprintf(&b, "=== ASCII, 79 columns, 40 rows\n%s\n", tc.scene.frame(plainASCII, 79, 40, 3))
		checkWords(t, tc.name, b.String())
		checkGolden(t, "dashboard-"+tc.name+".golden", b.String())
		checkGolden(t, "dashboard-"+tc.name+"-color.golden", tc.scene.frame(color256, 79, 40, 3)+"\n")
		for _, size := range [][2]int{{79, 40}, {79, 23}, {79, 12}, {59, 40}, {119, 40}} {
			for _, line := range strings.Split(tc.scene.frame(plainUnicode, size[0], size[1], 3), "\n") {
				if w := term.Width(line); w > size[0] {
					t.Errorf("%s at %v: a line of %d cells: %q", tc.name, size, w, line)
				}
			}
			if n := len(strings.Split(tc.scene.frame(plainUnicode, size[0], size[1], 3), "\n")); n > size[1] && size[1] >= 12 {
				t.Errorf("%s at %v: %d lines", tc.name, size, n)
			}
		}
	}
}

// oneTask is a fixed cost design's answer when one task has cost in both arms: the analysis stops before its
// intervals, so there is no estimate to show (not "-100%").
func oneTask() answerState {
	one := 1.0
	an := experiment.Analysis{Results: []experiment.MetricResult{{Metric: experiment.MetricCost, Role: experiment.RolePrimary, Ratio: true, Tasks: 1,
		A: &one, B: &one, Verdict: stats.Exploratory, Note: "fewer than two tasks have counted runs in both arms"}}}
	a, ok := answerOfAnalysis(an, 12)
	if !ok {
		panic("no primary result")
	}
	return a
}

// TestAnswerWords is the verdict-to-words table: every verdict, check and stop reason in plain words.
func TestAnswerWords(t *testing.T) {
	t.Parallel()
	labels := [2]string{"baseline", "trimmed"}
	rate := func(v float64) *float64 { return &v }
	cost := func(verdict string, ratio float64, decision string, next int) answerState {
		return answerState{Metric: experiment.MetricCost, Verdict: verdict, Estimate: ratio, HasEstimate: true, Margin: 0.10, Decision: decision,
			Seq: true, Next: next, All: 16}
	}
	success := func(verdict string, a, b float64) answerState {
		return answerState{Metric: experiment.MetricSuccess, Verdict: verdict, Estimate: b - a, HasEstimate: true, A: rate(a), B: rate(b),
			Margin: 0.15, All: 12, Ended: experiment.StatusDone}
	}
	ended := func(a answerState, status string) answerState { a.Ended = status; return a }
	settle := func(a answerState, tasks int) answerState { a.Settle = tasks; return a }
	for _, tc := range []struct {
		name string
		in   answerState
		aa   bool
		want string
	}{
		{"before the first check", answerState{Metric: experiment.MetricCost, Seq: true, Next: 8, All: 16}, false,
			"the answer so far: too early to tell · first check after 8 tasks"},
		{"a check with too few tasks", answerState{Metric: experiment.MetricCost, Seq: true, Decision: experiment.LookContinue, Next: 12, All: 16}, false,
			"the answer so far: too early to tell · too few tasks finished in both · next check after 12 tasks"},
		{"inconclusive, close", cost(stats.Inconclusive, 1.04, experiment.LookContinue, 12), false,
			"the answer so far: about the same cost (+4%) · not sure yet · next check after 12 tasks"},
		{"inconclusive, cheaper", cost(stats.Inconclusive, 0.82, experiment.LookContinue, 12), false,
			"the answer so far: trimmed may be cheaper (18% less) · not sure yet · next check after 12 tasks"},
		{"inconclusive, dearer", cost(stats.Inconclusive, 1.25, experiment.LookContinue, 16), false,
			"the answer so far: trimmed may cost more (25% more) · not sure yet · next check after 16 tasks"},
		{"improved, stopped early", cost(stats.Improved, 0.82, experiment.LookStop, 12), false,
			"the answer: trimmed is cheaper: 18% less · stopped early: sure enough"},
		{"improved but small", cost(stats.ImprovedSmall, 0.96, experiment.LookStop, 12), false,
			"the answer: trimmed is a little cheaper: 4% less · stopped early: sure enough"},
		{"regressed", cost(stats.Regressed, 1.31, experiment.LookStop, 12), false,
			"the answer: trimmed costs more: 31% more · stopped early: sure enough"},
		{"equivalent", cost(stats.Equivalent, 1.02, experiment.LookStop, 16), false,
			"the answer: the same cost, within 10% (+2%) · stopped early: sure enough"},
		{"futility", cost(stats.Inconclusive, 1.03, experiment.LookFutility, 16), false,
			"the answer: no clear difference in cost · stopped early · more tasks are unlikely to settle it"},
		{"the last check, inconclusive", cost(stats.Inconclusive, 0.93, experiment.LookFinal, 0), false,
			"the answer: no clear difference in cost · after all 16 tasks · not sure"},
		{"the last check, improved", cost(stats.Improved, 0.70, experiment.LookFinal, 0), false,
			"the answer: trimmed is cheaper: 30% less · after all 16 tasks · sure enough"},
		{"exploratory", cost(stats.Exploratory, 1.04, experiment.LookFinal, 0), false,
			"the answer: too few tasks to tell (+4%) · after all 16 tasks · not sure"},
		{"stopped at the budget between checks", ended(cost(stats.Inconclusive, 1.04, experiment.LookContinue, 12), experiment.StatusBudget), false,
			"the answer so far: about the same cost (+4%) · not sure yet · stopped at the budget"},
		{"paused at the usage limit", ended(answerState{Metric: experiment.MetricCost, Seq: true, Next: 8, All: 16}, experiment.StatusUsage), false,
			"the answer so far: too early to tell · paused at your Claude plan's usage limit"},
		{"interrupted", ended(cost(stats.Inconclusive, 0.9, experiment.LookContinue, 12), experiment.StatusStopped), false,
			"the answer so far: about the same cost (-10%) · not sure yet · stopped · run it again to go on"},
		{"a fixed design, running", answerState{Metric: experiment.MetricSuccess, All: 12}, false,
			"the answer so far: too early to tell · the answer comes once all 12 tasks are done"},
		{"a fixed design, done without an answer", answerState{Metric: experiment.MetricSuccess, All: 12, Ended: experiment.StatusDone}, false,
			"the answer so far: too early to tell · every run is done: the report has the answer"},
		{"success improved", success(stats.Improved, 0.60, 0.75), false,
			"the answer: trimmed passes more tasks (trimmed 75%, baseline 60%) · after all 12 tasks · sure enough"},
		{"success small", success(stats.ImprovedSmall, 0.60, 0.65), false,
			"the answer: trimmed passes a few more tasks (trimmed 65%, baseline 60%) · after all 12 tasks · sure enough"},
		{"success regressed", success(stats.Regressed, 0.60, 0.45), false,
			"the answer: trimmed passes fewer tasks (trimmed 45%, baseline 60%) · after all 12 tasks · sure enough"},
		{"success no loss", success(stats.NoLoss, 0.60, 0.62), false,
			"the answer: trimmed passes no fewer tasks, within 15 points (trimmed 62%, baseline 60%) · after all 12 tasks · sure enough"},
		{"success equivalent", success(stats.Equivalent, 0.60, 0.62), false,
			"the answer: they pass about as many tasks, within 15 points (trimmed 62%, baseline 60%) · after all 12 tasks · sure enough"},
		{"the last check, not sure, with the tasks that would settle it", settle(cost(stats.Inconclusive, 0.93, experiment.LookFinal, 0), 30), false,
			"the answer: no clear difference in cost · after all 16 tasks · not sure yet · about 30 tasks would settle it"},
		{"an A/A has nothing to settle", settle(cost(stats.Inconclusive, 0.93, experiment.LookFinal, 0), 30), true,
			"the answer: no clear difference in cost · after all 16 tasks · not sure"},
		{"success inconclusive", success(stats.Inconclusive, 0.60, 0.70), false,
			"the answer: no clear difference in passed tasks (trimmed 70%, baseline 60%) · after all 12 tasks · not sure"},
		{"an A/A that differs", cost(stats.Improved, 0.80, experiment.LookStop, 12), true,
			"the answer: the two differ by 20% with nothing changed: a false alarm, or something besides the context differs · stopped early: sure enough"},
		{"the last check, too few tasks", answerState{Metric: experiment.MetricCost, Seq: true, Decision: experiment.LookFinal, All: 16}, false,
			"the answer: no answer: too few tasks finished in both · after all 16 tasks"},
		{"a fixed cost design with one paired task", oneTask(), false,
			"the answer: too few tasks to tell · after all 12 tasks · not sure"},
		{"an A/A as expected", cost(stats.Equivalent, 1.01, experiment.LookStop, 12), true,
			"the answer: no difference in cost, as expected (+1%) · stopped early: sure enough"},
	} {
		headline, status := answerWords(tc.in, labels, tc.aa)
		got := tc.in.Title() + ": " + headline + " · " + status
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		checkWords(t, tc.name, got)
	}
}

func TestArmLabelsAndQuestion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		lock experiment.Lock
		want string
	}{
		{screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), "BASELINE vs TRIMMED · does trimmed save money?"},
		{screenLock(experiment.TemplateContextAB, experiment.GoalBetter, true, false), "BASELINE vs TRIMMED · does trimmed pass more tasks?"},
		{screenLock(experiment.TemplateAA, experiment.GoalCheaper, true, true), "BASELINE 1 vs BASELINE 2 · checking how much results vary"},
		{screenLock(experiment.TemplateModelAB, experiment.GoalCheaper, true, true), "SONNET-5 vs OPUS-5:HIGH · does opus-5:high save money?"},
	} {
		f := factsOf(tc.lock, 85)
		got := strings.Join(strings.Fields(term.Plain(questionLine(plainUnicode, unicodeMarks, f))), " ")
		if got != tc.want {
			t.Errorf("question %q, want %q", got, tc.want)
		}
	}
	// A name from outside Agentium cannot move the cursor or reorder the screen.
	l := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
	l.Design.Arms[1].Context = "evil\x1b[2J\u202e"
	if f := factsOf(l, 85); f.labels[1] != "evil" {
		t.Errorf("label %q, want it sanitized", f.labels[1])
	}
}

// Task names and the agent's words (the judge's verdict) are sanitized before they reach the screen.
func TestScreenSanitizesOutsideText(t *testing.T) {
	t.Parallel()
	l := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
	l.Schedule[0].Task = "bad\x1b]0;title\x07name\u202e"
	s := newScene(t, l, experiment.Standing{})
	s.start(0)
	passed := true
	s.event(experiment.Event{Kind: "finish", Slot: l.Schedule[0], Attempt: 1, SpentUSD: 0.1,
		Result: experiment.Result{Outcome: claude.OutcomeOK, Passed: &passed, CostUSD: 0.1, Judge: "fixed\x1b[31m (3 of 3)\r\x1b[2K"}})
	out := s.frame(color256, 99, 40, 0)
	for _, bad := range []string{"\x1b]", "\x07", "\u202e", "\r", "\x1b[2K", "\x1b[31m"} {
		if strings.Contains(out, bad) {
			t.Errorf("the frame holds %q:\n%q", bad, out)
		}
	}
	if !strings.Contains(term.Plain(out), "badname") || !strings.Contains(term.Plain(out), "judge: fixed (3 of 3)") {
		t.Errorf("the names are lost:\n%s", term.Plain(out))
	}
}

// The dot travels the whole dotted line from a finished step's box to the next in about a second, over the sandbox's
// edge too, and then is gone; the lines themselves never change.
func TestDotTravelsAlongTheLine(t *testing.T) {
	t.Parallel()
	s := newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	s.start(0)
	s.step(0, run.StepAgent)
	lay := layoutFor(79)
	row := func() string { // the title row of baseline's boxes, at the connector from fresh copy to Claude works
		lines := strings.Split(s.frame(plainUnicode, 79, 40, 0), "\n")
		var r []rune
		for _, l := range lines {
			if strings.Contains(l, "│ fresh copy │") && r == nil {
				r = []rune(l)
			}
		}
		return string(r[lay.x[0]+lay.box : lay.x[1]])
	}
	var seen []string
	for range 10 {
		seen = append(seen, row())
		s.clock.add(100 * time.Millisecond)
	}
	if got := row(); got != "╌╌╌╌╌┆╌" {
		t.Errorf("after the dot: %q", got)
	}
	want := []string{"●╌╌╌╌┆╌", "●╌╌╌╌┆╌", "╌●╌╌╌┆╌", "╌╌●╌╌┆╌", "╌╌●╌╌┆╌", "╌╌╌●╌┆╌", "╌╌╌╌●┆╌", "╌╌╌╌●┆╌", "╌╌╌╌╌●╌", "╌╌╌╌╌┆●"}
	if strings.Join(seen, " ") != strings.Join(want, " ") {
		t.Errorf("the dot's way, every 100ms:\n got %q\nwant %q", seen, want)
	}
}

// A check that counted no task since the last one keeps the last answer, as the report does; a last check that never
// had one says so.
func TestUnanalysedCheckKeepsTheLastAnswer(t *testing.T) {
	t.Parallel()
	s := newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	s.event(experiment.Event{Kind: "look", Look: &experiment.Look{Look: 1, Planned: 8, Counted: 8, Analysed: true, Verdict: stats.Inconclusive,
		Interval: &stats.Interval{Estimate: 0.84, Low: 0.6, High: 1.1}, Decision: experiment.LookContinue}, Looks: 3})
	s.event(experiment.Event{Kind: "look", Look: &experiment.Look{Look: 2, Planned: 12, Counted: 8, Decision: experiment.LookContinue}, Looks: 3})
	a := s.state.view().answer
	headline, status := answerWords(a, s.state.facts.labels, false)
	if got := headline + " · " + status; got != "trimmed may be cheaper (16% less) · not sure yet · next check after 16 tasks" {
		t.Errorf("after a check with no new task: %q", got)
	}
	s.event(experiment.Event{Kind: "look", Look: &experiment.Look{Look: 3, Planned: 16, Counted: 8, Decision: experiment.LookFinal}, Looks: 3})
	headline, status = answerWords(s.state.view().answer, s.state.facts.labels, false)
	if got := headline + " · " + status; got != "no clear difference in cost · after all 16 tasks · not sure" {
		t.Errorf("the last check: %q", got)
	}
}

// A step reported for another attempt of the slot (a run that was retried) does not move the run on screen.
func TestStepOfAnotherAttemptIsIgnored(t *testing.T) {
	t.Parallel()
	s := newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	s.event(experiment.Event{Kind: "start", Slot: s.lock.Schedule[0], Attempt: 2})
	s.event(experiment.Event{Kind: "step", Slot: s.lock.Schedule[0], Attempt: 1, Step: run.StepAgent})
	if r := s.state.view().shown[0]; r.step != stepCopy || r.attempt != 2 {
		t.Errorf("a stale step moved the run: step %d, attempt %d", r.step, r.attempt)
	}
	s.event(experiment.Event{Kind: "step", Slot: s.lock.Schedule[0], Attempt: 2, Step: run.StepAgent})
	if r := s.state.view().shown[0]; r.step != stepAgent {
		t.Errorf("the run's own step: step %d", r.step)
	}
}

// The dashboard's log is a fixed area of the last 4 runs' results, from the first frame to the last: the frame never
// grows, shrinks or jumps. Pauses, retries and warnings take the status line instead, and the checks the answer box.
func TestDashboardLogIsFourFixedRows(t *testing.T) {
	t.Parallel()
	s := newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	type size struct{ w, h int }
	heights := map[size]int{}
	check := func(when string) {
		t.Helper()
		for _, sz := range []size{{79, 40}, {79, 23}, {99, 30}} {
			lines := strings.Split(s.frame(plainUnicode, sz.w, sz.h, 0), "\n")
			head := -1
			for i, l := range lines {
				if strings.Contains(l, "─ log ─") {
					head = i
				}
			}
			if head < 0 || len(lines)-head-1 != logRows {
				t.Errorf("%s at %v: the log area is %d rows:\n%s", when, sz, len(lines)-head-1, strings.Join(lines, "\n"))
				continue
			}
			for _, l := range lines[head+1:] {
				if strings.Contains(l, "checked") || strings.Contains(l, "retrying") || strings.Contains(l, "waiting") {
					t.Errorf("%s at %v: a sentence in the log area: %q", when, sz, l)
				}
			}
			if h, ok := heights[sz]; ok && h != len(lines) {
				t.Errorf("%s at %v: the frame went from %d to %d rows", when, sz, h, len(lines))
			}
			heights[sz] = len(lines)
		}
	}
	check("before any run")
	for pos := range 12 {
		s.whole(pos, pos%2 == 0)
		check(fmt.Sprintf("after run %d", pos))
		if pos == 3 {
			s.event(experiment.Event{Kind: "retry", Slot: s.lock.Schedule[4], Attempt: 1, RetryIn: 30 * time.Second})
			check("a retry")
			if !strings.Contains(s.frame(plainUnicode, 79, 40, 0), "retrying uniq-by for baseline in 30s: it failed, not by Claude's doing") {
				t.Errorf("the retry is not in the status line:\n%s", s.frame(plainUnicode, 79, 40, 0))
			}
		}
		if pos == 7 {
			s.event(experiment.Event{Kind: "look", Look: &experiment.Look{Look: 1, Planned: 8, Counted: 8, Analysed: true, Verdict: stats.Inconclusive,
				Interval: &stats.Interval{Estimate: 0.9, Low: 0.7, High: 1.1}, Decision: experiment.LookContinue}, Looks: 3})
			s.event(experiment.Event{Kind: "wait", Until: s.clock.Now().Add(20 * time.Minute), Usage: 0.86})
			check("a check and a pause")
			if !strings.Contains(s.frame(plainUnicode, 79, 40, 0), "waiting for your Claude plan's usage to reset at 16:") {
				t.Errorf("the pause is not in the status line:\n%s", s.frame(plainUnicode, 79, 40, 0))
			}
		}
	}
	// The last 4 runs, oldest first.
	frame := s.frame(plainUnicode, 79, 40, 0)
	tail := strings.Split(frame, "\n")
	got := strings.Join(tail[len(tail)-4:], "\n")
	for _, task := range []string{"zip", "flatten-deep"} {
		if !strings.Contains(got, task) {
			t.Errorf("the log area lacks %s:\n%s", task, got)
		}
	}
	// The pause ended with the next run: the status line says how runs are run again.
	s.start(12)
	if f := s.frame(plainUnicode, 79, 40, 0); strings.Contains(f, "waiting for") || !strings.Contains(f, "2 at a time") {
		t.Errorf("after the pause:\n%s", f)
	}
}

// The boxes keep their full names on a terminal of 74 columns or more (a frame of 73), as the guide says.
func TestLayoutNarrowsBelow74Columns(t *testing.T) {
	t.Parallel()
	if full, narrow := layoutFor(73), layoutFor(72); full.box != 14 || narrow.box != 10 || full.total > 73 {
		t.Errorf("at 73 cells: %+v; at 72: %+v", full, narrow)
	}
}

// A comparison's or a warning's note fades from the status line after noteTime; a retry's stays until its run starts.
func TestNotesFade(t *testing.T) {
	t.Parallel()
	s := newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true), experiment.Standing{})
	s.event(experiment.Event{Kind: "pair", Slot: s.lock.Schedule[1], Result: experiment.Result{Judge: "tie", JudgeUSD: 0.09}})
	if f := s.frame(plainUnicode, 79, 40, 0); !strings.Contains(f, "compared the two passing runs") {
		t.Fatalf("no note:\n%s", f)
	}
	v := s.state.view() // a frame drawn from this copy fades it by its own clock
	s.clock.add(noteTime)
	if f := strings.Join(dashboardFrame(v, plainUnicode, s.clock.Now(), 79, 40, 0), "\n"); strings.Contains(f, "compared") || !strings.Contains(f, "2 at a time") {
		t.Errorf("the note stayed:\n%s", f)
	}
	s.event(experiment.Event{Kind: "retry", Slot: s.lock.Schedule[2], Attempt: 1, RetryIn: 2 * time.Minute})
	s.clock.add(time.Minute)
	if f := s.frame(plainUnicode, 79, 40, 0); !strings.Contains(f, "retrying chunk") {
		t.Errorf("the retry's note faded before its run:\n%s", f)
	}
}

// TestDashboardMomentsGoldens draws the moments inside the boxes (copying, fetching dependencies, setup, starting the
// sandbox, running the tests, cleaning up) and the sandbox's news (unavailable: a caution outline and a row note;
// flagged denials: the row note; a folder quarantined: the status line), as baseline's row at 79 and 64 columns, and in
// color at 79.
func TestDashboardMomentsGoldens(t *testing.T) {
	t.Parallel()
	var plain, color strings.Builder
	draw := func(name string, s *scene) {
		v := s.state.view()
		for _, width := range []int{79, 64} {
			lay := layoutFor(width)
			fmt.Fprintf(&plain, "=== %s, %d columns\n", name, width)
			for _, l := range v.infoLines(plainUnicode, unicodeMarks, lay.total, 0, s.clock.Now()) {
				fmt.Fprintln(&plain, l)
			}
			fmt.Fprintln(&plain, strings.Join(v.armBoxes(plainUnicode, unicodeMarks, lay, 0, s.clock.Now(), 2), "\n"))
		}
		lay := layoutFor(79)
		fmt.Fprintf(&color, "=== %s\n%s\n", name, strings.Join(v.armBoxes(color256, marksFor(color256), lay, 0, s.clock.Now(), 2), "\n"))
	}
	moments := func(sandbox bool) *scene {
		return newScene(t, screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, sandbox, true), experiment.Standing{})
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
	s.finish(0, claude.OutcomeOK, &yes, 0.09)
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
	down.step(0, run.StepCleanup)
	down.finish(0, claude.OutcomeInfra, nil, 0.08)
	down.event(experiment.Event{Kind: "retry", Slot: down.lock.Schedule[0], Attempt: 1, RetryIn: 30 * time.Second})
	down.clock.add(2 * time.Second) // the dot has arrived
	draw("the sandbox unavailable", down)

	// Out of attempts: no retry follows, so the row says the run is not counted.
	spent := moments(true)
	spent.start(0)
	spent.step(0, run.StepAgent)
	spent.step(0, run.StepGrading)
	spent.step(0, run.StepSandbox)
	spent.step(0, run.StepSandboxDown)
	spent.finish(0, claude.OutcomeInfra, nil, 0.08)
	spent.clock.add(2 * time.Second)
	draw("the sandbox unavailable, no retry", spent)

	flagged := moments(true)
	flagged.start(0)
	flagged.step(0, run.StepAgent)
	flagged.step(0, run.StepGrading)
	flagged.step(0, run.StepSandbox)
	flagged.step(0, run.StepTests)
	flagged.event(experiment.Event{Kind: "finish", Slot: flagged.lock.Schedule[0], Attempt: 1, SpentUSD: 0.1,
		Result: experiment.Result{Outcome: run.OutcomeSandboxFlagged, CostUSD: 0.1, SandboxFlagged: "mach-lookup, file-read-data, mach-register"}})
	flagged.clock.add(2 * time.Second)
	draw("flagged denials", flagged)
	if got := flagged.state.entries()[0].format(plainUnicode, unicodeMarks, flagged.state.facts, noWidth); !strings.Contains(got, "left out (sandbox)") {
		t.Errorf("the flagged run's log line: %q", got)
	}
	if got := down.state.entries()[0].format(plainUnicode, unicodeMarks, down.state.facts, noWidth); !strings.Contains(got, "sandbox unavailable (not counted)") {
		t.Errorf("the unavailable run's log line: %q", got)
	}
	if dir := os.Getenv("AGENTIUM_RUN_DEMO"); dir != "" { // a still of the sandbox's news in color, for scripts/readme_images
		var b strings.Builder
		for _, sc := range []struct {
			name string
			s    *scene
		}{{"the sandbox unavailable", down}, {"flagged denials", flagged}} {
			fmt.Fprintf(&b, "\x1b[2m# %s\x1b[22m\n%s\n", sc.name, strings.Join(sc.s.state.view().armBoxes(color256, marksFor(color256), layoutFor(79), 0, sc.s.clock.Now(), 2), "\n"))
		}
		if err := os.WriteFile(filepath.Join(dir, "sandbox-news.ans"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	checkWords(t, "the moments", plain.String())
	checkGolden(t, "dashboard-moments.golden", plain.String())
	checkGolden(t, "dashboard-moments-color.golden", color.String())
	if !strings.Contains(plain.String(), "sandbox unavailable · not counted") || !strings.Contains(plain.String(), "sandbox unavailable · retrying") {
		t.Error("the unavailable sandbox's row notes")
	}
	if !strings.Contains(color.String(), "\x1b[38;5;220m╭┄ sandbox") {
		t.Error("the unavailable sandbox's outline is not in the caution color")
	}
}

func TestBlockedWords(t *testing.T) {
	t.Parallel()
	for ops, want := range map[string]string{
		"mach-lookup": "a system service lookup",
		"mach-lookup, file-read-data, mach-register": "a system service lookup, a file read",
		"network-outbound, file-write-create":        "a network connection, a file write",
		"something-new":                              "an operation the agent's own sandbox allows",
		"":                                           "an operation the agent's own sandbox allows",
		"file-read\x1b[2J-data":                      "a file read",
	} {
		if got := blockedWords(ops); got != want {
			t.Errorf("blockedWords(%q) = %q, want %q", ops, got, want)
		}
	}
}
