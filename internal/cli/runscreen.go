package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
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
	printed string // the answer box printed last, in words: the end does not print the same box again
}

// newRunScreen makes the screen for view and returns env with its output going through it: on the dashboard, every
// line printed (the checks, calibrations, errors) goes above the live region, so nothing interleaves with a redraw.
// Close it when done (defer): that clears the region and shows the cursor again.
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
		// Force: chooseView picked the dashboard for a terminal wide enough; NO_COLOR only takes its color away.
		s.disp = term.NewDisplay(ctx, env.Stdout, caps, term.DisplayOptions{Force: true, Size: size})
		env.Stdout, env.Stderr = s.disp.Writer(), s.disp.Over(env.Stderr)
	}
	s.out = env.Stdout
	return env, s
}

// Close clears the live region, if any.
func (s *runScreen) Close() error {
	if s.disp == nil {
		return nil
	}
	return s.disp.Close()
}

// observer follows the execution: the screen begins with the lock, takes in each event, and at the end prints the
// log (dashboard) and the answer. answer reads a fixed design's answer once every run is done (nil: none).
func (s *runScreen) observer(answer func(experiment.Lock) *answerState) experiment.Observer {
	return experiment.Observer{
		Begin: func(lock experiment.Lock, standing experiment.Standing) {
			s.lock = lock
			s.state = newRunState(factsOf(lock, s.limit), standing, s.now)
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
		Finish: func(sum experiment.Summary) {
			if s.state == nil {
				return
			}
			var final *answerState
			if s.lock.Method != experiment.MethodSeq && sum.Status == experiment.StatusDone && answer != nil {
				final = answer(s.lock)
			}
			a := s.state.end(sum, final)
			if s.disp != nil {
				// The region goes; what stays in the scrollback is the question, every run of this execution and the answer.
				s.disp.Update(nil)
				lines := s.header()[:2]
				for _, entry := range s.state.entries() {
					lines = append(lines, entry.format(s.sh, s.m, s.state.facts, noWidth))
				}
				for _, line := range lines {
					s.disp.Log(line)
				}
			}
			s.printAnswer(a)
		},
	}
}

// redraw gives the display a frame of the state now: a copy, which the frame reads on the display's goroutine.
func (s *runScreen) redraw() {
	v := s.state.view()
	sh, now := s.sh, s.now
	s.disp.Update(func(width, height, tick int) []string { return dashboardFrame(v, sh, now(), width, height, tick) })
}

// header is the log view's first lines: a blank line, the question, the budget and the tasks, and the sandbox's legend.
func (s *runScreen) header() []string {
	f, st, m := s.state.facts, s.sh.Style, s.m
	lines := []string{"", " " + questionLine(s.sh, m, f),
		" " + st.Paint(term.Muted, fmt.Sprintf("budget %s %s %s %s ", money(f.budget), m.sep, taskCount(f.tasks), m.sep)) + legendLine(s.sh, m)}
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
