package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/term"
)

// writeReview prints an experiment's review (experiment plan, and start's preview): on a terminal that shows the designed
// console, the picture of planView unless details asks for every line; everywhere else (a pipe, NO_COLOR, TERM=dumb,
// a narrow terminal, --json) the review as before, byte for byte.
func writeReview(ctx context.Context, env Env, r experiment.Review, name, signIn string, details bool) error {
	caps := term.DetectCapabilities(env.Terminal, env.Getenv, envSize(env))
	if details || env.JSON || env.Plain || !caps.Designed() {
		return r.Write(ctx, env.Stdout, env.style(), name, signIn, env.Now())
	}
	if err := ctx.Err(); err != nil { // as Write: the readiness checks may have been cut short
		return err
	}
	_, err := io.WriteString(env.Stdout, strings.Join(planView(r, name, signIn, env.Now(), caps.Shapes(), caps.Width), "\n")+"\n")
	return err
}

// planView draws an experiment's review for reading on a terminal, in plain words: the question, whether everything
// is in place, what it may spend as bars against the budget and, for a seq-v1 design, when it checks the answer, each
// check's spend as a bar. The detectable effects, floors and the long notes are in --details. Every name from outside
// Agentium (contexts, check texts) is sanitized before it is drawn.
func planView(r experiment.Review, name, signIn string, now time.Time, sh term.Shapes, width int) []string {
	st := sh.Style
	m := marksFor(sh)
	w := min(width, term.MaxContentWidth)
	d := r.Design
	labels := armLabels(d)
	f := runFacts{labels: labels, question: runQuestion(d, labels), aa: d.Template == experiment.TemplateAA}
	var out []string
	out = append(out, centered(w, questionLine(sh, m, f)))
	out = append(out, " "+st.Paint(term.Muted, strings.Repeat(m.rule, max(w-1, 1))))
	out = append(out, centered(w, st.Paint(term.Muted, m.words(planFacts(name, r)))))
	out = append(out, "")
	out = append(out, sh.Panel(readinessPanel(r, sh, m, w), w)...)
	var seq *experiment.SeqPreview
	if d.Sequential() && len(d.Tasks) > 0 {
		if p, err := experiment.PreviewSequential(d, r.Estimates); err == nil {
			seq = &p
		}
	}
	out = append(out, "")
	out = append(out, sh.Panel(spendPanel(r, seq, sh, m, w), w)...)
	if seq != nil && seq.Known && len(seq.Looks) > 1 { // one check is the end: the bars above say it
		out = append(out, "")
		out = append(out, sh.Panel(checksPanel(d, seq, sh, m, w), w)...)
	}
	if notes := planNotes(r, name, signIn, now, sh, m, w); len(notes) > 0 {
		out = append(out, "")
		out = append(out, notes...)
	}
	for i, line := range out {
		out[i] = term.Truncate(line, width, sh.Ellipsis())
	}
	return out
}

// planFacts is the line under the question: "lean-ab · 12 tasks · 24 runs · claude-sonnet-5 · 2 at a time".
func planFacts(name string, r experiment.Review) string {
	d := r.Design
	runs := len(d.Tasks) * d.Repeats * len(d.Arms)
	parts := []string{term.Sanitize(name), taskCount(len(d.Tasks)), fmt.Sprintf("%d %s", runs, plural(runs, "run", "runs"))}
	if !d.PerArmProfiles() {
		parts = append(parts, term.Sanitize(strings.TrimPrefix(d.Model, "claude-")))
	}
	if d.Concurrency == 1 {
		parts = append(parts, "one at a time")
	} else {
		parts = append(parts, fmt.Sprintf("%d at a time", d.Concurrency))
	}
	return strings.Join(parts, " · ")
}

// readinessPanel says whether everything is in place: one line when it is, else each check that is not ok.
func readinessPanel(r experiment.Review, sh term.Shapes, m marks, width int) term.Panel {
	st := sh.Style
	p := term.Panel{Title: "before it runs"}
	var rest []string
	for _, c := range r.Readiness.Checks {
		switch c.Status {
		case "ok":
		case "MISSING":
			rest = append(rest, checkLines(st.Paint(term.OutcomeFailed, m.fail), c.Text, sh, width)...)
		default:
			rest = append(rest, checkLines(st.Paint(term.OutcomeInfra, m.warn), c.Text, sh, width)...)
		}
	}
	ok := 0
	for _, c := range r.Readiness.Checks {
		if c.Status == "ok" {
			ok++
		}
	}
	switch {
	case len(rest) == 0:
		p.Border = term.OutcomeOK
		p.Lines = []string{st.Paint(term.OutcomeOK, m.ok) + " everything is in place " + st.Paint(term.Muted, fmt.Sprintf("(%d checks)", ok))}
	default:
		if !r.Readiness.Ready {
			p.Border = term.OutcomeFailed
		}
		p.Lines = rest
		if ok > 0 {
			p.Lines = append(p.Lines, st.Paint(term.Muted, fmt.Sprintf("%s %d %s ok", m.ok, ok, plural(ok, "check", "checks"))))
		}
	}
	return p
}

// checkLines is a check's text after its mark, on two lines at most (the rest is in --details): the second ends in an
// ellipsis when text is longer. A long list of tasks does not bury the lines that matter.
func checkLines(mark, text string, sh term.Shapes, width int) []string {
	room := min(width, term.MaxContentWidth) - 4
	lines := term.Wrap(term.Sanitize(text), room-2)
	if len(lines) > 2 {
		lines = []string{lines[0], sh.Fit(lines[1]+" "+strings.Join(lines[2:], " "), room-2)}
	}
	for i, l := range lines {
		if i == 0 {
			lines[i] = mark + " " + l
		} else {
			lines[i] = "  " + l
		}
	}
	return lines
}

// spendPanel is what the experiment may spend, as bars against the budget: the likely spend, the most, and the budget.
func spendPanel(r experiment.Review, seq *experiment.SeqPreview, sh term.Shapes, m marks, width int) term.Panel {
	st := sh.Style
	d := r.Design
	type row struct {
		label string
		usd   float64
	}
	var rows []row
	var dim []string
	known := false
	switch {
	case seq != nil:
		known = seq.Known
		rows = []row{{"likely", seq.NoneUSD}, {"all tasks run", seq.MaxUSD}}
		if seq.TasksNone < float64(len(d.Tasks))-0.05 {
			dim = append(dim, fmt.Sprintf("likely: if nothing changes, about %s of %d tasks are used", tasksUsed(seq.TasksNone), len(d.Tasks)))
		}
		if known {
			dim = append(dim, "if every run hit its cap: "+money(seq.WorstUSD))
		}
	case len(r.Rows) > 0:
		own := r.Rows[len(r.Rows)-1]
		known = own.CostKnown
		rows = []row{{"likely", own.CostUSD + own.JudgeUSD}, {"worst case", own.WorstUSD}}
		dim = dim[:0]
		dim = append(dim, "worst case: every run hits its cap")
	}
	if needs := r.Readiness.Calibrations; len(needs) > 0 {
		estimate, _ := experiment.CalibrationCosts(needs)
		dim = append(dim, fmt.Sprintf("plus about %s to calibrate %d %s first (from the budget)", money(estimate), len(needs), plural(len(needs), "context", "contexts")))
	}
	if len(rows) == 2 && math.Abs(rows[0].usd-rows[1].usd) < 0.005 { // one amount is one bar
		rows = rows[1:]
		if seq != nil {
			rows[0].label = "all tasks run"
		}
	}
	p := term.Panel{Title: "what it may spend", Overflow: term.WrapText}
	if !known {
		p.Lines = []string{st.Paint(term.Muted, "cost unknown until runs measure it "+m.sep+" budget ") + money(d.BudgetUSD)}
		return p
	}
	scale := d.BudgetUSD
	for _, rw := range rows {
		scale = max(scale, rw.usd)
	}
	inner := min(width, term.MaxContentWidth) - 4
	bar := func(label string, usd float64, role term.Role) string {
		return sh.Bar(term.Bar{Label: label, LabelWidth: 13, Fraction: usd / scale, Value: money(usd), ValueWidth: 8, Role: role}, inner)
	}
	for _, rw := range rows {
		p.Lines = append(p.Lines, bar(rw.label, rw.usd, term.Level(rw.usd, d.BudgetUSD)))
	}
	p.Lines = append(p.Lines, bar("budget", d.BudgetUSD, term.OutcomeLeftOut)) // the limit, in grey: the bars above are what matters
	for _, line := range dim {
		p.Lines = append(p.Lines, st.Paint(term.Muted, m.words(line)))
	}
	return p
}

// tasksUsed is a count of tasks that may be fractional (an average): whole numbers have no ".0".
func tasksUsed(n float64) string {
	if math.Abs(n-math.Round(n)) < 0.05 {
		return fmt.Sprint(int(math.Round(n)))
	}
	return fmt.Sprintf("%.1f", n)
}

// checksPanel lists a seq-v1 design's checks of the answer: after how many tasks, and what has been spent by then, as a
// bar against the most it may spend.
func checksPanel(d experiment.Design, seq *experiment.SeqPreview, sh term.Shapes, m marks, width int) term.Panel {
	p := term.Panel{Title: "when it checks the answer"}
	inner := min(width, term.MaxContentWidth) - 4
	for k, l := range seq.Looks {
		label := fmt.Sprintf("check %d: %s", k+1, taskCount(l.Tasks))
		p.Lines = append(p.Lines, sh.Bar(term.Bar{Label: label, LabelWidth: 18, Fraction: l.CostUSD / max(seq.MaxUSD, 1e-9),
			Value: money(l.CostUSD), ValueWidth: 8, Role: term.Level(l.CostUSD, d.BudgetUSD)}, inner))
	}
	p.Lines = append(p.Lines, sh.Style.Paint(term.Muted, "it stops at the first check with a clear answer"))
	return p
}

// planNotes are the dim lines under the panels: the plan's usage, a spend that assumes every run hits its cap, whether
// it is ready, and the command for every line.
func planNotes(r experiment.Review, name, signIn string, now time.Time, sh term.Shapes, m marks, width int) []string {
	st := sh.Style
	var out []string
	if u := r.Usage(signIn, now); !u.APIKey && len(u.Models) > 0 {
		windows := fmt.Sprintf("%.1f", u.Windows)
		text := fmt.Sprintf("on your plan: %d runs need about %s %s of the five-hour limit", u.Runs, windows, map[bool]string{true: "window", false: "windows"}[windows == "1.0"])
		if l := u.Latest; l != nil && l.Current && l.Fits < u.Runs {
			text += fmt.Sprintf(" %s about %d more fit now: it pauses at the limit (add --wait to wait)", m.sep, l.Fits)
		}
		out = append(out, wrapped(st, " ", text, term.Muted, width)...)
	}
	for _, e := range r.Estimates {
		if e.FewRuns() && e.EstimateBasis() == experiment.BasisCap {
			out = append(out, wrapped(st, " ", m.warn+" the spend assumes every run hits its cap, so it is likely high: the first runs measure the real cost", term.OutcomeInfra, width)...)
			break
		}
	}
	if !r.Readiness.Ready {
		out = append(out, " "+st.Paint(term.OutcomeFailed, "not ready to run: fix what is missing above"))
	}
	command := strings.Join([]string{"agentium", "experiment", "plan", term.Sanitize(name), "--details"}, nbsp)
	return append(out, wrapped(st, " ", "everything: "+command, term.Muted, width)...)
}
