package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/term"
)

// screen replays raw terminal output the way a terminal shows it, understanding only what Agentium writes: "\r\x1b[K"
// erases the current line and "\n" ends it. It returns the finished lines and what is left on the current one.
func screen(raw string) (lines []string, current string) {
	for raw != "" {
		switch {
		case strings.HasPrefix(raw, "\r\x1b[K"):
			current, raw = "", raw[len("\r\x1b[K"):]
		case raw[0] == '\n':
			lines, current, raw = append(lines, current), "", raw[1:]
		default:
			end := strings.IndexAny(raw[1:], "\r\n") + 1
			if end == 0 {
				end = len(raw)
			}
			current, raw = current+raw[:end], raw[end:]
		}
	}
	return lines, current
}

// liveChecks verifies a command's output on a fake terminal: the status line was drawn (it says every want), and it
// never reached a finished line or stayed on screen.
func liveChecks(t *testing.T, r cliResult, want ...string) []string {
	t.Helper()
	lines, current := screen(r.stdout)
	if current != "" {
		t.Errorf("text left on the last line: %q", current)
	}
	for _, l := range lines {
		if strings.ContainsAny(term.Plain(l), "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") {
			t.Errorf("a status line stayed in a finished line: %q", l)
		}
	}
	for _, w := range want {
		if !strings.Contains(term.Plain(r.stdout), w) {
			t.Errorf("no status line said %q:\n%q", w, r.stdout)
		}
	}
	if !strings.HasSuffix(r.stdout, "\n") {
		t.Errorf("output ends mid-line: %q", r.stdout[max(len(r.stdout)-40, 0):])
	}
	return lines
}

func TestExperimentRunShowsAStatusLineOnATerminal(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--repeats", "2", "--seed", "5"), ExitOK)
	*f.terminal = true
	f.vars["TERM"] = "xterm"
	r := f.run(ctx, "experiment", "run", "lean-ab")
	expect(t, r, ExitOK, "Every run is done.")
	lines := liveChecks(t, r, "0 of 4 settled", "in flight", "$0.00 of $", "4 of 4 settled")
	var started, finished int
	for _, l := range lines {
		l = term.Plain(l)
		started += strings.Count(l, ": started")
		finished += strings.Count(l, "ok, $0.30")
	}
	if started != 4 || finished != 4 {
		t.Errorf("event lines: %d started, %d finished; want 4 and 4", started, finished)
	}
}

func TestExperimentRunHasNoStatusLineElsewhere(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--repeats", "1", "--seed", "5"), ExitOK)
	r := f.run(ctx, "experiment", "run", "lean-ab")
	expect(t, r, ExitOK, "Every run is done.")
	if strings.ContainsAny(r.stdout, "\r\x1b") || strings.Contains(r.stdout, "in flight") {
		t.Errorf("a status line leaked into non-terminal output:\n%q", r.stdout)
	}
	*f.terminal = true
	f.vars["TERM"] = "dumb"
	expect(t, f.run(ctx, "experiment", "new", "dumb", "--b", "lean", "--task", "value", "--repeats", "1", "--seed", "6"), ExitOK)
	if r := f.run(ctx, "experiment", "run", "dumb"); strings.Contains(r.stdout, "in flight") || strings.Contains(r.stdout, "\r") {
		t.Errorf("a dumb terminal got a status line:\n%q", r.stdout)
	}
}

func TestRunOnceAndValidateShowTheStepInProgress(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	*f.terminal = true
	f.vars["TERM"] = "xterm"
	r := f.run(ctx, "run", "once", "value")
	expect(t, r, ExitOK)
	liveChecks(t, r, "verification: passed", "preparing the workspace", "Claude Code is working", "grading")
	r = f.run(ctx, "task", "validate", "value", "--snapshot", "lean")
	expect(t, r, ExitOK)
	liveChecks(t, r, "validating value: base, hidden-tests", "validating value: lean, reference")
}

func TestRunStatusText(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	s := runStatus{total: 6, budget: 10, settled: map[int]bool{0: true}, spent: 0.3}
	if got, want := s.text(now), "1 of 6 settled; 0 in flight; $0.30 of $10.00"; got != want {
		t.Errorf("text %q, want %q", got, want)
	}
	s.update(experiment.Event{Kind: "start", Slot: experiment.Slot{Position: 1}})
	s.update(experiment.Event{Kind: "start", Slot: experiment.Slot{Position: 2}})
	s.update(experiment.Event{Kind: "finish", Slot: experiment.Slot{Position: 1}, SpentUSD: 0.6, Result: experiment.Result{Outcome: "ok"}})
	if got, want := s.text(now), "2 of 6 settled; 1 in flight; $0.60 of $10.00"; got != want {
		t.Errorf("text %q, want %q", got, want)
	}
	s.update(experiment.Event{Kind: "wait", Usage: 0.9, Until: now.Add(23 * time.Minute)})
	if got, want := s.text(now), "2 of 6 settled; waiting for the usage window to reset at 10:23 (in 23m00s)"; got != want {
		t.Errorf("text %q, want %q", got, want)
	}
	s.update(experiment.Event{Kind: "start", Slot: experiment.Slot{Position: 3}})
	if got := s.text(now); !strings.HasSuffix(got, "; usage 90%") {
		t.Errorf("text %q, want the usage reading", got)
	}
	// An older reading of the same window, reported late, does not replace it.
	s.update(experiment.Event{Kind: "finish", Slot: experiment.Slot{Position: 2}, SpentUSD: 0.9, Result: experiment.Result{Outcome: "ok",
		Usage: &claude.UsageReading{FiveHour: 0.5, FiveHourResets: now.Add(23 * time.Minute)}}})
	if got := s.text(now); !strings.HasSuffix(got, "; usage 90%") {
		t.Errorf("text %q, want the later reading kept", got)
	}
	// Once the window resets, the reading no longer applies: after a --wait the line shows the new window, not 90%.
	if got := s.text(now.Add(24 * time.Minute)); !strings.HasSuffix(got, "; usage 0%") {
		t.Errorf("text %q after the reset, want usage 0%%", got)
	}
	// A reading from the next window replaces it.
	s.update(experiment.Event{Kind: "finish", Slot: experiment.Slot{Position: 3}, SpentUSD: 1.2, Result: experiment.Result{Outcome: "ok",
		Usage: &claude.UsageReading{FiveHour: 0.06, FiveHourResets: now.Add(5 * time.Hour)}}})
	if got := s.text(now.Add(24 * time.Minute)); !strings.HasSuffix(got, "; usage 6%") {
		t.Errorf("text %q, want the next window's reading", got)
	}
}
