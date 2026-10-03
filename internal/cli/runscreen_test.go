package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/term"
)

// vtScreen replays a live display's output the way a terminal shows it, understanding what the display writes: cursor
// up ("\x1b[nA"), erase to the end of the screen ("\r\x1b[J"), newlines, and modes and styles, which it drops. It
// returns the lines on screen (and in the scrollback) at the end, and whether the cursor is visible.
func vtScreen(raw string) (lines []string, cursorShown bool) {
	rows, cur := []string{""}, 0
	cursorShown = true
	csi := regexp.MustCompile(`^\x1b\[([0-9;?]*)([A-Za-z])`)
	for raw != "" {
		if m := csi.FindStringSubmatch(raw); m != nil {
			raw = raw[len(m[0]):]
			switch {
			case m[2] == "A":
				n, _ := strconv.Atoi(m[1])
				cur = max(cur-n, 0)
			case m[2] == "J":
				rows = append(rows[:cur], "")
			case m[1] == "?25" && m[2] == "l":
				cursorShown = false
			case m[1] == "?25" && m[2] == "h":
				cursorShown = true
			}
			continue
		}
		switch raw[0] {
		case '\r':
			raw = raw[1:]
			if strings.HasPrefix(raw, "\x1b[J") {
				continue
			}
			rows[cur] = ""
		case '\n':
			raw = raw[1:]
			cur++
			if cur == len(rows) {
				rows = append(rows, "")
			}
		default:
			end := strings.IndexAny(raw, "\x1b\r\n")
			if end < 0 {
				end = len(raw)
			}
			rows[cur] += raw[:end]
			raw = raw[end:]
		}
	}
	for len(rows) > 0 && rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}
	return rows, cursorShown
}

// terminalVars makes the fixture's stdout a 100 by 40 UTF-8 color terminal.
func terminalVars(f runFixture) {
	*f.terminal = true
	f.vars["TERM"] = "xterm-256color"
	f.vars["LANG"] = "en_US.UTF-8"
	f.vars["COLUMNS"] = "100"
	f.vars["LINES"] = "40"
}

// On a terminal, experiment run draws the dashboard: the region is redrawn in place and cleared at the end, and what
// stays is the question, a line per run, the answer and the summary; never the plain view's event lines.
func TestExperimentRunDrawsTheDashboardOnATerminal(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--seed", "5"), ExitOK)
	terminalVars(f)
	r := f.run(ctx, "experiment", "run", "lean-ab")
	expect(t, r, ExitOK)
	for _, want := range []string{"\x1b[?25l", "\x1b[38;5;75m", "Claude works", "hidden tests", "fresh copy", "the answer so far"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the dashboard never drew %q", want)
		}
	}
	lines, shown := vtScreen(r.stdout)
	screen := term.Plain(strings.Join(lines, "\n"))
	if !shown {
		t.Error("the cursor stays hidden")
	}
	for _, want := range []string{"BASELINE  vs  LEAN", "does lean pass more tasks?", "Every run is done.", "Experiment lean-ab: done", "the answer"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the screen lacks %q:\n%s", want, screen)
		}
	}
	if n := strings.Count(screen, "✓ passed"); n != 4 {
		t.Errorf("%d runs passed on screen, want 4:\n%s", n, screen)
	}
	for _, gone := range []string{"Claude works", ": started", "[1/4]", "in flight"} {
		if strings.Contains(screen, gone) {
			t.Errorf("the screen keeps %q after the run:\n%s", gone, screen)
		}
	}
	answer := screen[strings.Index(screen, "BASELINE"):strings.Index(screen, "Experiment lean-ab:")]
	checkWords(t, "the dashboard's last lines", answer)
}

// --view log, and AGENTIUM_VIEW=log, print styled lines and redraw nothing; NO_COLOR keeps the view without color.
func TestExperimentRunLogView(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	terminalVars(f)
	for i, args := range [][]string{{"--view", "log"}, nil} {
		name := fmt.Sprintf("log-%d", i)
		expect(t, f.run(ctx, "experiment", "new", name, "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "2", "--seed", "5"), ExitOK)
		if args == nil {
			f.vars["AGENTIUM_VIEW"] = "log"
			f.vars["NO_COLOR"] = "1"
		}
		r := f.run(ctx, append([]string{"experiment", "run", name}, args...)...)
		expect(t, r, ExitOK, "does lean pass more tasks?", "Every run is done.")
		if regexp.MustCompile(`\x1b\[[0-9]*[AJK]|\r|\x1b\[\?25`).MatchString(r.stdout) {
			t.Errorf("the log view moved the cursor:\n%q", r.stdout)
		}
		if strings.Contains(r.stdout, "\x1b[") == (args == nil) {
			t.Errorf("styled = %v with NO_COLOR = %v:\n%q", strings.Contains(r.stdout, "\x1b["), args == nil, r.stdout)
		}
		plain := term.Plain(r.stdout)
		if n := regexp.MustCompile(`(?m)^ \d\d:\d\d:\d\d  (baseline|lean) +value +✓ passed  \$0\.30  `).FindAllString(plain, -1); len(n) != 4 {
			t.Errorf("%d run lines, want 4:\n%s", len(n), plain)
		}
		if strings.Contains(plain, ": started") || strings.Contains(plain, "[1/4]") {
			t.Errorf("the log view printed the plain lines:\n%s", plain)
		}
		if !strings.Contains(plain, "│                the answer                │") && !strings.Contains(plain, "the answer") {
			t.Errorf("no answer box:\n%s", plain)
		}
		checkWords(t, "the log view", plain[strings.Index(plain, "BASELINE"):strings.Index(plain, "Experiment "+name+":")])
	}
}

// Off a terminal, --view and AGENTIUM_VIEW change nothing: the plain lines, without an escape code.
func TestExperimentRunViewsArePlainOffATerminal(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	f.vars["AGENTIUM_VIEW"] = "dashboard"
	for i, args := range [][]string{{"--view", "log"}, {"--view", "dashboard"}, nil} {
		name := fmt.Sprintf("plain-%d", i)
		expect(t, f.run(ctx, "experiment", "new", name, "--b", "lean", "--task", "value", "--repeats", "1", "--seed", "5"), ExitOK)
		r := f.run(ctx, append([]string{"experiment", "run", name}, args...)...)
		expect(t, r, ExitOK, ": started", "Every run is done.")
		if strings.Contains(r.stdout+r.stderr, "\x1b") || strings.Contains(r.stdout, "\r") {
			t.Errorf("%v off a terminal printed escape codes:\n%q", args, r.stdout)
		}
	}
	// A dumb terminal, and JSON, are plain too.
	*f.terminal = true
	f.vars["TERM"] = "dumb"
	expect(t, f.run(ctx, "experiment", "new", "dumb", "--b", "lean", "--task", "value", "--repeats", "1", "--seed", "6"), ExitOK)
	if r := f.run(ctx, "experiment", "run", "dumb", "--view", "dashboard"); strings.Contains(r.stdout, "\x1b") || !strings.Contains(r.stdout, ": started") {
		t.Errorf("a dumb terminal got a designed view:\n%q", r.stdout)
	}
}

func TestViewFlagAndVariable(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "run", "x", "--view", "fancy"), ExitUsage, `--view is dashboard or log, not "fancy"`)
	f.vars["AGENTIUM_VIEW"] = "fancy"
	expect(t, f.run(ctx, "experiment", "run", "x"), ExitUsage, `AGENTIUM_VIEW is dashboard or log, not "fancy"`)
	expect(t, f.run(ctx, "start", "--view", "fancy"), ExitUsage, `--view is dashboard or log, not "fancy"`)

	vars := func(kv ...string) func(string) string {
		m := map[string]string{"TERM": "xterm", "COLUMNS": "100"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		name     string
		terminal bool
		getenv   func(string) string
		asked    string
		json     bool
		want     string
	}{
		{"a terminal", true, vars(), "", false, viewDashboard},
		{"a terminal, log asked", true, vars(), viewLog, false, viewLog},
		{"a pipe", false, vars(), viewDashboard, false, viewPlain},
		{"FORCE_COLOR into a pipe", false, vars("FORCE_COLOR", "1"), "", false, viewPlain},
		{"a dumb terminal", true, vars("TERM", "dumb"), viewLog, false, viewPlain},
		{"NO_COLOR", true, vars("NO_COLOR", "1"), "", false, viewPlain},
		{"NO_COLOR, dashboard asked", true, vars("NO_COLOR", "1"), viewDashboard, false, viewDashboard},
		{"NO_COLOR, log asked", true, vars("NO_COLOR", "1"), viewLog, false, viewLog},
		{"a narrow terminal", true, vars("COLUMNS", "50"), viewDashboard, false, viewPlain},
		{"JSON", true, vars(), viewDashboard, true, viewPlain},
	} {
		got, _ := chooseView(Env{Terminal: tc.terminal, Getenv: tc.getenv}, tc.asked, tc.json)
		if got != tc.want {
			t.Errorf("%s: view %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		flag viewFlag
		env  string
		want string
	}{{"", "", ""}, {"", "log", "log"}, {"", " dashboard ", "dashboard"}, {"dashboard", "log", "dashboard"}} {
		if got, err := askedView(tc.flag, func(string) string { return tc.env }); err != nil || got != tc.want {
			t.Errorf("askedView(%q, %q) = %q, %v; want %q", tc.flag, tc.env, got, err, tc.want)
		}
	}
}

// syncBuffer is a writer safe for the display's goroutine and the test's.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The dashboard's feed under -race: events from several goroutines (as the scheduler's runs report their steps)
// while the display draws frames from copies, then the end; the screen is left clean.
func TestDashboardFeedIsRaceFree(t *testing.T) {
	t.Parallel()
	out := &syncBuffer{}
	caps := term.Capabilities{Terminal: true, Color: term.Color256, UTF8: true, Width: 100, Height: 40}
	env := Env{Stdout: out, Stderr: out, Now: time.Now}
	ctx := context.Background()
	env, screen := newRunScreen(ctx, env, viewDashboard, caps, 85)
	lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
	obs := screen.observer(nil)
	obs.Begin(lock, experiment.Standing{Settled: map[int]bool{}})
	var mu sync.Mutex // the scheduler's events come one at a time
	emit := func(e experiment.Event) {
		mu.Lock()
		defer mu.Unlock()
		obs.Event(e)
	}
	var wg sync.WaitGroup
	for pos := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot := lock.Schedule[pos]
			emit(experiment.Event{Kind: "start", Slot: slot, Attempt: 1})
			for _, step := range []string{run.StepPreparing, run.StepAgent, run.StepGrading} {
				time.Sleep(time.Millisecond)
				emit(experiment.Event{Kind: "step", Slot: slot, Attempt: 1, Step: step})
			}
			passed := pos%2 == 0
			emit(experiment.Event{Kind: "finish", Slot: slot, Attempt: 1, Result: experiment.Result{Outcome: claude.OutcomeOK, Passed: &passed, CostUSD: 0.1}})
			fmt.Fprintf(env.Stdout, "a line printed meanwhile %d\n", pos)
		}()
	}
	wg.Wait()
	obs.Finish(experiment.Summary{Status: experiment.StatusStopped})
	if err := screen.Close(); err != nil {
		t.Fatal(err)
	}
	lines, shown := vtScreen(out.String())
	plain := term.Plain(strings.Join(lines, "\n"))
	if !shown || strings.Contains(plain, "fresh copy") || strings.Count(plain, "a line printed meanwhile") != 8 || !strings.Contains(plain, "stopped · run it again") {
		t.Errorf("the screen after the run (cursor shown %v):\n%s", shown, plain)
	}
}

// logViewScene runs a made-up experiment through the log view at the clock's time: every kind of run and sentence,
// a check of the answer and a stop at the budget.
func logViewScene(t *testing.T, caps term.Capabilities) string {
	t.Helper()
	c := newClock()
	var out bytes.Buffer
	_, screen := newRunScreen(context.Background(), Env{Stdout: &out, Stderr: &out, Now: c.Now}, viewLog, caps, 85)
	lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, false, true)
	obs := screen.observer(nil)
	obs.Begin(lock, experiment.Standing{Settled: map[int]bool{}})
	yes, no := true, false
	spent := 0.0
	finish := func(pos int, r experiment.Result, requeued bool) {
		obs.Event(experiment.Event{Kind: "start", Slot: lock.Schedule[pos], Attempt: 1, SpentUSD: spent})
		c.add(2*time.Minute + 5*time.Second)
		spent += r.CostUSD
		obs.Event(experiment.Event{Kind: "finish", Slot: lock.Schedule[pos], Attempt: 1, SpentUSD: spent, Result: r, Requeued: requeued})
	}
	finish(0, experiment.Result{Outcome: claude.OutcomeOK, Passed: &yes, CostUSD: 0.12, JudgeUSD: 0.03, Judge: "fixed (3 of 3)"}, false)
	finish(1, experiment.Result{Outcome: claude.OutcomeOK, Passed: &no, CostUSD: 0.09}, false)
	finish(2, experiment.Result{Outcome: claude.OutcomeInfra, CostUSD: 0.01}, false)
	obs.Event(experiment.Event{Kind: "retry", Slot: lock.Schedule[2], Attempt: 1, RetryIn: 30 * time.Second, SpentUSD: spent})
	finish(3, experiment.Result{Outcome: "unfair", CostUSD: 0.08}, false)
	finish(4, experiment.Result{Outcome: run.OutcomeSandboxFlagged, CostUSD: 0.07}, false)
	finish(5, experiment.Result{Outcome: claude.OutcomeCapped, Passed: &yes, CostUSD: 0.31,
		Overshoot: "it passed its $0.30 cost cap by $0.010, more than the $0.00 allowance budgets hold for that"}, false)
	finish(6, experiment.Result{}, true)
	finish(7, experiment.Result{Outcome: claude.OutcomeCancelled, CostUSD: 0.02}, false)
	obs.Event(experiment.Event{Kind: "pair", Slot: lock.Schedule[1], Result: experiment.Result{Judge: "prefers B", JudgeUSD: 0.09, CostUSD: 0.09}})
	obs.Event(experiment.Event{Kind: "wait", Until: c.Now().Add(23 * time.Minute), Usage: 0.86, SpentUSD: spent})
	obs.Event(experiment.Event{Kind: "look", Look: &experiment.Look{Look: 1, Planned: 8, Counted: 8, Analysed: true, Verdict: "inconclusive",
		Interval: &stats.Interval{Estimate: 0.82, Low: 0.6, High: 1.1}, Decision: experiment.LookContinue}, Looks: 3})
	obs.Finish(experiment.Summary{Status: experiment.StatusBudget})
	return out.String()
}

func TestLogViewGoldens(t *testing.T) {
	t.Parallel()
	color := logViewScene(t, term.Capabilities{Terminal: true, Color: term.Color256, UTF8: true, Width: 100, Height: 40})
	checkGolden(t, "logview-color.golden", color)
	plain := logViewScene(t, term.Capabilities{Terminal: true, UTF8: true, Width: 100, Height: 40})
	checkGolden(t, "logview.golden", plain)
	if term.Plain(color) != plain {
		t.Errorf("color changed the words:\n%s\nwithout color:\n%s", term.Plain(color), plain)
	}
	checkGolden(t, "logview-ascii.golden", logViewScene(t, term.Capabilities{Terminal: true, Width: 80, Height: 24}))
	checkWords(t, "the log view", plain)
	if strings.Contains(color, "\r") || regexp.MustCompile(`\x1b\[[0-9;]*[A-HJK]`).MatchString(color) {
		t.Errorf("the log view moves the cursor:\n%q", color)
	}
}

// Ctrl-C on the dashboard: the region is cleared at once, the cursor comes back, and the question, every run of the
// execution (the interrupted one as stopped) and the answer so far stay in the scrollback, above the plain summary.
func TestDashboardCtrlC(t *testing.T) {
	t.Parallel()
	f, ctrl := experimentFixture(t)
	expect(t, f.run(context.Background(), "experiment", "new", "stop", "--b", "lean", "--task", "value", "--repeats", "1", "--concurrency", "1"), ExitOK)
	writeFile(t, ctrl, "hang", "s1-t1\n")
	terminalVars(f)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, "the second run's agent", func() bool {
			_, err := os.Stat(filepath.Join(ctrl, "hanging-e1-s1-t1"))
			return err == nil
		})
		cancel()
	}()
	r := f.run(ctx, "experiment", "run", "stop")
	expect(t, r, ExitError)
	if !strings.Contains(r.stdout, "\x1b[?25l") {
		t.Fatal("the dashboard was never drawn")
	}
	lines, shown := vtScreen(r.stdout)
	screen := term.Plain(strings.Join(lines, "\n"))
	if !shown {
		t.Error("the cursor stays hidden after Ctrl-C")
	}
	for _, gone := range []string{"Claude works", "fresh copy", "─ log ─"} {
		if strings.Contains(screen, gone) {
			t.Errorf("the region was left on screen (%q):\n%s", gone, screen)
		}
	}
	tail := screen[strings.LastIndex(screen, "BASELINE  vs  LEAN"):]
	for _, want := range []string{"✓ passed", "stopped (not counted)", "the answer so far", "stopped · run it again to go on",
		"Experiment stop: stopped: interrupted", "To continue: agentium experiment run stop"} {
		if !strings.Contains(tail, want) {
			t.Errorf("the scrollback lacks %q:\n%s", want, tail)
		}
	}
}

// An error or a panic that ends the execution before its end still leaves what it ran: closing the screen prints the
// question, every run's line and the answer, as stopped.
func TestScreenCloseLeavesTheRuns(t *testing.T) {
	t.Parallel()
	for _, view := range []string{viewDashboard, viewLog} {
		out := &syncBuffer{}
		caps := term.Capabilities{Terminal: true, Color: term.Color256, UTF8: true, Width: 100, Height: 40}
		_, screen := newRunScreen(context.Background(), Env{Stdout: out, Stderr: out, Now: time.Now}, view, caps, 85)
		lock := screenLock(experiment.TemplateContextAB, experiment.GoalCheaper, true, true)
		obs := screen.observer(nil)
		obs.Begin(lock, experiment.Standing{Settled: map[int]bool{}})
		passed := true
		for pos := range 2 {
			obs.Event(experiment.Event{Kind: "start", Slot: lock.Schedule[pos], Attempt: 1})
			obs.Event(experiment.Event{Kind: "finish", Slot: lock.Schedule[pos], Attempt: 1, Result: experiment.Result{Outcome: claude.OutcomeOK, Passed: &passed, CostUSD: 0.1}})
		}
		if err := screen.Close(); err != nil { // no Finish: the execution failed
			t.Fatal(err)
		}
		lines, shown := vtScreen(out.String())
		screen2 := term.Plain(strings.Join(lines, "\n"))
		if !shown || strings.Count(screen2, "✓ passed") != 2 || !strings.Contains(screen2, "does trimmed save money?") ||
			!strings.Contains(screen2, "stopped · run it again to go on") || strings.Contains(screen2, "fresh copy") {
			t.Errorf("%s after an error:\n%s", view, screen2)
		}
		before := out.String()
		if err := screen.Close(); err != nil || out.String() != before {
			t.Errorf("%s: a second close printed again (%v)", view, err)
		}
	}
}
