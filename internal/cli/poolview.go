package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/term"
)

// writeHealth prints the pool's health: on a terminal that shows the designed console, as bars (healthView); everywhere
// else (a pipe, NO_COLOR, TERM=dumb, a narrow terminal) the lines of printHealth, byte for byte.
func writeHealth(env Env, h pool.Health) error {
	caps := term.DetectCapabilities(env.Terminal, env.Getenv, envSize(env))
	if env.JSON || env.Plain || !caps.Designed() {
		printHealth(env, h)
		return nil
	}
	_, err := io.WriteString(env.Stdout, strings.Join(healthView(h, caps.Shapes(), caps.Width, env.style().Command("agentium pool update")), "\n")+"\n")
	return err
}

// healthView draws the pool's health: one bar per state that has tasks (valid always), each against all the pool's
// tasks, green for valid, yellow for weak or flaky ones, red for invalid and grey for the rest, then the last pass and the
// oldest valid base in dim words. update is the command that mines, validates and retires, shown while no pass ran.
func healthView(h pool.Health, sh term.Shapes, width int, update string) []string {
	st := sh.Style
	m := marksFor(sh)
	w := min(width, term.MaxContentWidth)
	inner := w - 4
	type state struct {
		label string
		n     int
		role  term.Role
		show  bool
	}
	states := []state{
		{"valid", h.Valid, term.OutcomeOK, true},
		{"  weak tests", h.Weak, term.OutcomeInfra, h.Weak > 0},
		{"flaky", h.Flaky, term.OutcomeInfra, h.Flaky > 0},
		{"invalid", h.Invalid, term.OutcomeFailed, h.Invalid > 0},
		{"unchecked", h.Unchecked, term.OutcomeLeftOut, h.Unchecked > 0},
		{"not validated", h.Unvalidated, term.OutcomeLeftOut, h.Unvalidated > 0},
		{"awaiting review", h.AwaitingReview, term.OutcomeLeftOut, h.AwaitingReview > 0},
		{"retired", h.Retired, term.OutcomeLeftOut, h.Retired > 0},
	}
	var p term.Panel
	p.Title = fmt.Sprintf("task pool %s %s", m.sep, taskCount(h.Total))
	for _, s := range states {
		if !s.show {
			continue
		}
		p.Lines = append(p.Lines, sh.Bar(term.Bar{Label: s.label, LabelWidth: 15, Fraction: float64(s.n) / float64(max(h.Total, 1)),
			Value: fmt.Sprint(s.n), ValueWidth: 4, Role: s.role}, inner))
	}
	out := sh.Panel(p, w)
	last, oldest := "never", "none"
	if !h.LastPass.IsZero() {
		last = h.LastPass.UTC().Format("2006-01-02 15:04 UTC")
	}
	if !h.OldestValidBase.IsZero() {
		oldest = h.OldestValidBase.UTC().Format(time.DateOnly)
	}
	out = append(out, wrapped(st, " ", fmt.Sprintf("last pass %s %s oldest valid base %s", last, m.sep, oldest), term.Muted, w)...)
	if h.LastPass.IsZero() {
		out = append(out, " "+update+st.Paint(term.Muted, " mines, validates and retires (no agent runs)"))
	}
	for i, line := range out {
		out[i] = term.Truncate(line, width, sh.Ellipsis())
	}
	return out
}
