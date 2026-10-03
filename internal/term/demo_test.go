package term

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDemo draws the shapes and a live region for people to look at, not for CI; it is skipped unless
// AGENTIUM_TERM_DEMO is set:
//   - AGENTIUM_TERM_DEMO=tty draws on this terminal (/dev/tty) with its own capabilities: the shapes, then about six
//     seconds of a live experiment flow with lines logged above it (Ctrl-C clears it early). LANG=C shows ASCII and
//     NO_COLOR plain lines.
//   - AGENTIUM_TERM_DEMO=<folder> writes, in 24-bit color at COLS columns (default 100), shapes.ans (every shape) and
//     flow.ans (a still of the live view, then a run as a chain) to the folder, for scripts/readme_images/ansi2svg.py.
//
// go test ./internal/term -run TestDemo -count=1
func TestDemo(t *testing.T) {
	target := os.Getenv("AGENTIUM_TERM_DEMO")
	if target == "" {
		t.Skip("set AGENTIUM_TERM_DEMO=tty or a folder to see the shapes")
	}
	if target != "tty" {
		cols, err := strconv.Atoi(os.Getenv("COLS"))
		if err != nil || cols <= 0 {
			cols = 100
		}
		sh := Shapes{Style: Colored().WithDepth(TrueColor)}
		files := map[string]string{
			"shapes.ans": showcase(sh, cols, false),
			"flow.ans": demoStill(sh, cols) + "\n\x1b[36m$\x1b[39m agentium run show 41\n" +
				strings.Join(sh.Flow(runFlow(sh), min(cols, 100)), "\n") + "\n",
		}
		for name, text := range files {
			if err := os.WriteFile(filepath.Join(target, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	switch {
	case err == nil:
		defer tty.Close()
	case IsTerminal(os.Stdout): // a compiled test binary run on a terminal without a controlling one (a recording)
		tty = os.Stdout
	default:
		t.Skipf("no terminal: %v", err)
	}
	size := func() (int, int) { return Size(tty) }
	caps := DetectCapabilities(IsTerminal(tty), os.Getenv, size)
	fmt.Fprintf(tty, "capabilities: %+v (color %v), designed: %v\n\n", caps, caps.Color, caps.Designed())
	fmt.Fprint(tty, showcase(caps.Shapes(), min(caps.Width, 120), true))
	fmt.Fprintln(tty)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	d := NewDisplay(ctx, tty, caps, DisplayOptions{Size: size})
	defer d.Close()
	demoRun(ctx, d, caps.Shapes())
}

// demoTasks are the made-up experiment's tasks.
var demoTasks = []string{"fix-login-redirect", "parse-iso-weeks", "retry-on-503", "cache-invalidation", "csv-quoting",
	"timezone-rollover", "flaky-port-bind", "unicode-filenames", "pagination-off-by-one", "gzip-streaming",
	"config-reload", "graceful-shutdown"}

// demoExperiment is a made-up experiment's state: runs finish one at a time.
type demoExperiment struct {
	sh    Shapes
	done  int
	arms  [2]struct{ ok, failed int }
	spent float64
}

// step finishes the next run at time now and returns its log line: the detail the dashboard leaves out.
func (e *demoExperiment) step(now time.Time) string {
	s := e.sh.Style
	e.done++
	arm, task := e.done%2, demoTasks[(e.done-1)%len(demoTasks)]
	cost := 0.25 + float64(e.done%5)*0.08
	outcome, role := "ok", OutcomeOK
	switch e.done % 7 {
	case 3:
		outcome, role = "failed", OutcomeFailed
		e.arms[arm].failed++
	case 5:
		outcome, role = "infra", OutcomeInfra
	default:
		e.arms[arm].ok++
	}
	e.spent += cost
	return strings.Join([]string{
		s.Paint(Muted, now.Format("15:04:05")), s.Paint([]Role{ArmA, ArmB}[arm], []string{"A", "B"}[arm]),
		Pad(task, 22), s.Paint(role, Pad(outcome, 6)), s.Paint(Muted, fmt.Sprintf("$%.2f", cost)),
	}, "  ")
}

// frame is the dashboard for the state now. e is a copy, so the display's goroutine reads nothing the caller changes.
func (e demoExperiment) frame(elapsed string) Frame {
	return func(width, tick int) []string {
		width = min(width, 100)
		f := experimentFlow(e.sh, width, e.done, e.spent, e.arms[0].ok, e.arms[0].failed, e.arms[1].ok, e.arms[1].failed, tick,
			elapsed)
		return e.sh.Flow(f, width)
	}
}

// demoStill is the made-up experiment part way, as the live view shows it: the last lines logged and the flow under
// them.
func demoStill(sh Shapes, width int) string {
	e := demoExperiment{sh: sh}
	at := time.Date(2026, 10, 3, 11, 58, 0, 0, time.UTC)
	var lines []string
	for i := 0; i < 14; i++ {
		lines = append(lines, e.step(at.Add(time.Duration(i)*17*time.Second)))
	}
	lines = append(lines[len(lines)-5:], e.frame("4m02s")(width-1, 3)...)
	return strings.Join(lines, "\n") + "\n"
}

// demoRun plays the made-up experiment on d: a run finishes every 250ms.
func demoRun(ctx context.Context, d Display, sh Shapes) {
	const total = 24
	start := time.Now()
	e := demoExperiment{sh: sh}
	d.Update(e.frame(Elapsed(0)))
	for e.done < total {
		select {
		case <-ctx.Done():
			d.Log(sh.Style.Warn("interrupted"))
			return
		case <-time.After(250 * time.Millisecond):
		}
		d.Log(e.step(time.Now()))
		d.Update(e.frame(Elapsed(time.Since(start))))
	}
	d.Log(sh.Style.Good("done") + fmt.Sprintf(" %d runs, $%.2f", total, e.spent))
}
