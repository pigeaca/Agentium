package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/term"
)

// runScreen is a designed view of a running experiment, fed by the scheduler's events: the dashboard (a live region
// redrawn in place, the log under it) or the log view (styled lines, nothing redrawn). The plain view does not use it.
type runScreen struct {
	view  string
	sh    term.Shapes
	m     marks
	out   io.Writer    // where the log view's lines, and the dashboard's last lines, go
	disp  term.Display // the dashboard's; nil in the log view
	now   func() time.Time
	width func() int
	limit float64 // the usage limit, in percent

	state   *runState // from Begin on
	lock    experiment.Lock
	fixed   func(experiment.Lock) *answerState // a fixed design's answer once every run is done
	printed string                             // the answer box printed last, in words: the end does not print the same box again
	ended   bool                               // end has left the execution in the scrollback

	// The dashboard keeps what is printed until the execution's end (held), so nothing moves the screen while it runs:
	// the checks, calibrations and revalidation before the first run show as the status line (latest), and print, in
	// order, above the run log at the end. After the end, lines print as they come.
	mu      sync.Mutex
	held    []string
	partial []byte
	latest  string
}

// heldWriter is the dashboard's standard output: it holds whole lines until the execution's end (runScreen.held).
type heldWriter struct{ s *runScreen }

func (w heldWriter) Write(p []byte) (int, error) {
	s := w.s
	s.mu.Lock()
	s.partial = append(s.partial, p...)
	var through []string
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		line := string(s.partial[:i])
		s.partial = s.partial[i+1:]
		if s.ended {
			through = append(through, line)
			continue
		}
		s.held = append(s.held, line)
		if plain := strings.TrimSpace(term.Plain(term.Sanitize(line))); plain != "" {
			s.latest = plain
		}
	}
	waiting := !s.ended && s.state == nil
	s.mu.Unlock()
	for _, line := range through {
		s.disp.Log(line)
	}
	if waiting {
		s.redrawWaiting()
	}
	return len(p), nil
}

// newRunScreen makes the screen for view and returns env with its output going through it. On the dashboard, what is
// printed is held until the execution ends (heldWriter) and errors print above the live region, so nothing interleaves
// with a redraw or moves the screen. Close it when done (defer): that prints what is held, clears the region and shows
// the cursor again.
func newRunScreen(ctx context.Context, env Env, view string, caps term.Capabilities, usageLimit float64) (Env, *runScreen) {
	size := envSize(env)
	s := &runScreen{view: view, sh: caps.Shapes(), now: env.Now, limit: usageLimit,
		width: func() int {
			cols, _ := size()
			if cols <= 0 {
				cols = caps.Width
			}
			return min(max(cols-1, 1), term.MaxContentWidth)
		}}
	s.m = marksFor(s.sh)
	if view == viewDashboard {
		// Force: chooseView picked the dashboard for a terminal wide enough; NO_COLOR only takes its color away. Errors
		// (standard error) print above the region at once.
		s.disp = term.NewDisplay(ctx, env.Stdout, caps, term.DisplayOptions{Force: true, Size: size})
		env.Stdout, env.Stderr = heldWriter{s}, s.disp.Over(env.Stderr)
		s.redrawWaiting()
	}
	s.out = env.Stdout
	return env, s
}

// redrawWaiting shows, before the first run, a spinner and the latest line printed: the checks, a calibration, a
// task validated again.
func (s *runScreen) redrawWaiting() {
	s.mu.Lock()
	latest := s.latest
	s.mu.Unlock()
	sh, m := s.sh, s.m
	s.disp.Update(func(width, _, tick int) []string {
		text := "getting ready"
		if latest != "" {
			text += " " + m.sep + " " + latest
		}
		return []string{" " + term.Plain(sh.Spinner(tick)) + " " + sh.Style.Paint(term.Muted, sh.Fit(text, max(width-3, 1)))}
	})
}

// Close clears the live region, if any.
func (s *runScreen) Close() error {
	s.end(nil) // an error, a panic or an early return before the execution's end: what it ran still stays
	if s.disp == nil {
		return nil
	}
	s.mu.Lock()
	rest := s.partial // an unfinished last line printed after the end
	s.partial = nil
	s.mu.Unlock()
	if len(rest) > 0 {
		s.disp.Log(string(rest))
	}
	return s.disp.Close()
}

// end leaves the execution in the scrollback, once: on the dashboard the region goes, and the question and every run's
// line print above it; in both views the answer follows in its box. sum is how the execution ended; nil when it did not
// say (an error or a panic stopped it), which reads as stopped. Only after Begin.
func (s *runScreen) end(sum *experiment.Summary) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	held := s.held
	if len(s.partial) > 0 {
		held, s.partial = append(held, string(s.partial)), nil
	}
	s.held = nil
	s.mu.Unlock()
	if s.disp != nil {
		s.disp.Update(nil)
		for _, line := range held {
			s.disp.Log(line)
		}
	}
	if s.state == nil {
		return
	}
	ended := experiment.Summary{Status: experiment.StatusStopped}
	if sum != nil {
		ended = *sum
	}
	var final *answerState
	if sum != nil && s.lock.Method != experiment.MethodSeq && sum.Status == experiment.StatusDone && s.fixed != nil {
		final = s.fixed(s.lock)
	}
	a := s.state.end(ended, final)
	if s.disp != nil {
		lines := s.header()[:2]
		for _, entry := range s.state.entries() {
			lines = append(lines, entry.format(s.sh, s.m, s.state.facts, noWidth))
		}
		for _, line := range lines {
			s.disp.Log(line)
		}
	}
	s.printAnswer(a)
}

// observer follows the execution: the screen begins with the lock, takes in each event, and at the end prints the
// log (dashboard) and the answer (end). answer reads a fixed design's answer once every run is done (nil: none).
func (s *runScreen) observer(answer func(experiment.Lock) *answerState) experiment.Observer {
	s.fixed = answer
	return experiment.Observer{
		Steps: s.disp != nil, // the dashboard draws them; its Event never waits (Display.Update returns at once)
		Begin: func(lock experiment.Lock, standing experiment.Standing) {
			s.lock = lock
			state := newRunState(factsOf(lock, s.limit), standing, s.now)
			s.mu.Lock()
			s.state = state
			s.mu.Unlock()
			if s.disp != nil {
				s.redraw()
				return
			}
			for _, line := range s.header() {
				fmt.Fprintln(s.out, line)
			}
		},
		Event: func(e experiment.Event) {
			if s.state == nil {
				return
			}
			added, checked := s.state.apply(e)
			if s.disp != nil {
				s.redraw()
				return
			}
			for _, entry := range added {
				if !entry.check {
					fmt.Fprintln(s.out, entry.format(s.sh, s.m, s.state.facts, noWidth))
				}
			}
			if checked {
				s.printAnswer(s.state.view().answer)
			}
		},
		Finish: func(sum experiment.Summary) { s.end(&sum) },
	}
}

// redraw gives the display a frame of the state now: a copy, which the frame reads on the display's goroutine.
func (s *runScreen) redraw() {
	v := s.state.view()
	sh, now := s.sh, s.now
	s.disp.Update(func(width, height, tick int) []string { return dashboardFrame(v, sh, now(), width, height, tick) })
}

// header is the log view's first lines: a blank line, the question, the budget, the tasks and the sandbox's legend, and
// how the runs are run.
func (s *runScreen) header() []string {
	f, st, m := s.state.facts, s.sh.Style, s.m
	lines := []string{"", " " + questionLine(s.sh, m, f),
		" " + st.Paint(term.Muted, fmt.Sprintf("budget %s %s %s %s ", money(f.budget), m.sep, taskCount(f.tasks), m.sep)) + legendLine(s.sh, m),
		" " + st.Paint(term.Muted, m.words(f.terms))}
	if !f.sandboxed {
		lines = append(lines, " "+hostWarning(s.sh, m))
	}
	return lines
}

// printAnswer prints the answer in its box: at each check (log view) and at the end, unless the box just printed says
// the same (a check that ended the experiment).
func (s *runScreen) printAnswer(a answerState) {
	headline, status := answerWords(a, s.state.facts.labels, s.state.facts.aa)
	words := a.Title() + "\n" + headline + "\n" + status
	if words == s.printed {
		return
	}
	s.printed = words
	box := answerBox(s.sh, s.m, a, s.state.facts, min(s.width(), answerBoxWidth(a, s.state.facts)+2))
	if s.disp != nil {
		for _, line := range box {
			s.disp.Log(line)
		}
		return
	}
	fmt.Fprintln(s.out, strings.Join(box, "\n"))
}
