package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

// stepTitles are the steps' names in the boxes: in full, and short for a narrow terminal or a one-line box.
var stepTitles = [2][stepCount]string{
	{"fresh copy", "Claude works", "hidden tests", "result"},
	{"copy", "Claude", "tests", "result"},
}

// rowLayout places an arm's four step boxes: their width, the column of each, and the row's width.
type rowLayout struct {
	box    int
	x      [stepCount]int
	total  int
	titles [stepCount]string
}

// layoutMargin is the dashboard's left margin; layoutMinGap the fewest cells between two boxes (room for the sandbox's
// edges and a stretch of dotted line); layoutMaxGap the most: a wide terminal gets more space, not longer lines.
const (
	layoutMargin = 2
	layoutMinGap = 5
	layoutMaxGap = 10
)

// layoutFor fits the boxes in width cells: 14-cell boxes with the steps' full names where they fit with the
// smallest gaps (73 cells), else 10-cell boxes with short names.
func layoutFor(width int) rowLayout {
	l := rowLayout{box: 14, titles: stepTitles[0]}
	if width-layoutMargin-4*l.box < 3*layoutMinGap {
		l = rowLayout{box: 10, titles: stepTitles[1]}
	}
	return l.place(width, layoutMinGap)
}

// oneLineLayout fits boxes of one line in width cells: 16 cells, room for a step's short name and its time, where
// they fit (78 cells: the sandbox's edges then touch), else as layoutFor's.
func oneLineLayout(width int) rowLayout {
	l := rowLayout{box: 16, titles: stepTitles[1]}
	if width-layoutMargin-4*l.box < 3*(layoutMinGap-1) {
		return layoutFor(width)
	}
	return l.place(width, layoutMinGap-1)
}

// place puts l's boxes in width cells, at least minGap apart.
func (l rowLayout) place(width, minGap int) rowLayout {
	width = min(width, term.MaxContentWidth)
	gap := min(max((width-layoutMargin-4*l.box)/3, minGap), layoutMaxGap)
	for i := range l.x {
		l.x[i] = layoutMargin + i*(l.box+gap)
	}
	l.total = l.x[stepCount-1] + l.box
	return l
}

// boxState is how a step's box looks: done, in progress, still to come, or the result.
type boxState struct {
	border, title term.Role
	bold          bool
	mark          string // ✓, the spinner, –, or the result's words
	markRole      term.Role
	text          string // after the mark: the time, "judging", or nothing
	noTitle       bool   // on one line, the mark and text say it all: the result's words, or the judge at work
}

// dashboardFrame draws the dashboard from a copy of the state: the header, each arm's current run as a row of step
// boxes joined by dotted lines, the answer so far and the latest log lines. It fits height by dropping the legend,
// then drawing each box on one line, then each arm on one line.
func dashboardFrame(v stateView, sh term.Shapes, now time.Time, width, height, tick int) []string {
	var lines []string
	for level := range 4 {
		lay := layoutFor(width)
		if level >= 2 {
			lay = oneLineLayout(width)
		}
		lines = append(v.panel(sh, lay, level, now, tick), v.logSection(sh, lay.total, min(width, term.MaxContentWidth), level)...)
		if len(lines) <= height {
			break
		}
	}
	for i, line := range lines {
		lines[i] = term.Truncate(line, width, sh.Ellipsis())
	}
	return lines
}

// panel is the dashboard without its log at a level of compaction: 0 is everything, 1 drops the legend, 2 draws each
// box on one line and the answer on two, without the line of terms, 3 draws each arm on one line.
func (v stateView) panel(sh term.Shapes, lay rowLayout, level int, now time.Time, tick int) []string {
	m := marksFor(sh)
	w := lay.total
	center := func(line string) string { return centered(w, line) }
	var out []string
	out = append(out, center(questionLine(sh, m, v.facts)))
	if level < 3 {
		out = append(out, " "+sh.Style.Paint(term.Muted, strings.Repeat(m.rule, max(w-1, 1))))
	}
	out = append(out, center(v.statusLine(sh, m, now)))
	out = append(out, v.infoLines(sh, m, w, level, now)...)
	if !v.facts.sandboxed {
		out = append(out, center(hostWarning(sh, m)))
	}
	if level == 0 {
		out = append(out, center(legendLine(sh, m)))
	}
	if level < 3 {
		out = append(out, "")
	}
	for arm := range 2 {
		switch level {
		case 0, 1:
			out = append(out, v.armBoxes(sh, m, lay, arm, now, tick)...)
		case 2:
			out = append(out, v.armOneLine(sh, m, lay, arm, now, tick)...)
		default:
			out = append(out, v.armText(sh, m, arm, now, tick))
		}
	}
	switch level {
	case 0, 1:
		out = append(out, "")
		out = append(out, answerBox(sh, m, v.answer, v.facts, w)...)
	case 2:
		out = append(out, "")
		out = append(out, answerLines(sh, m, v.answer, v.facts, w)...)
	default:
		out = append(out, answerLine(sh, m, v.answer, v.facts))
	}
	return out
}

// infoLines is the status line under the header, in rows that do not change with what it says: the latest pause,
// retry or warning while it applies, else how the runs are run (on two lines where one is too narrow, and cut to one
// on a short terminal).
func (v stateView) infoLines(sh term.Shapes, m marks, w, level int, now time.Time) []string {
	terms := packParts(v.facts.terms, w)
	if level >= 2 {
		terms = []string{v.facts.terms}
	}
	note, role := v.noteNow(now), v.noteRole
	if !v.until.IsZero() {
		note, role = fmt.Sprintf("waiting for your Claude plan's usage to reset at %s %s in %s", experiment.Clock(v.until, now), m.sep,
			term.Elapsed(max(v.until.Sub(now), 0))), term.LevelCaution
	}
	out := make([]string, len(terms))
	for i, line := range terms {
		if note == "" {
			out[i] = centered(w, sh.Style.Paint(term.Muted, sh.Fit(m.words(line), w)))
		}
	}
	if note != "" {
		out[0] = centered(w, sh.Style.Paint(role, sh.Fit(m.words(note), w)))
	}
	return out
}

// centered puts line in the middle of w cells.
func centered(w int, line string) string {
	return strings.Repeat(" ", max((w-term.Width(line))/2, 0)) + line
}

// statusLine is the money spent against the budget, the plan's share used (a subscription's only) and each arm's
// progress: "spent $2.17 of $5 · plan 64% used · baseline 11/16 · trimmed 10/16".
func (v stateView) statusLine(sh term.Shapes, m marks, now time.Time) string {
	st := sh.Style
	sep := st.Paint(term.Muted, " "+m.sep+" ")
	warn := func(role term.Role, text string) string {
		if role == term.LevelCalm {
			return text
		}
		return st.Paint(role, text)
	}
	line := st.Paint(term.Muted, "spent ") + warn(term.Level(v.spent, v.facts.budget), fmt.Sprintf("$%.2f", v.spent)) +
		st.Paint(term.Muted, " of "+money(v.facts.budget)) + sep
	if v.hasUsage {
		used := v.usage.FiveHourAt(now)
		line += st.Paint(term.Muted, "plan ") + warn(term.Level(used, v.facts.limit), fmt.Sprintf("%.0f%%", 100*used)) + st.Paint(term.Muted, " used") + sep
	}
	for arm := range 2 {
		if arm == 1 {
			line += sep
		}
		line += st.Paint(armRole(arm), v.facts.labels[arm]) + fmt.Sprintf(" %d/%d", v.settled[arm], v.facts.perArm[arm])
	}
	return line
}

// subWords are the moments within a step in words, in three lengths for the room a box has: "fetching dependencies",
// "downloads", "deps". A moment without words shows its time alone.
var subWords = map[string][3]string{
	run.StepPreparing:    {"copying", "copying", "copy"},
	run.StepDependencies: {"fetching dependencies", "downloads", "deps"},
	run.StepSetup:        {"running setup", "setup", "setup"},
	run.StepSandbox:      {"starting sandbox", "starting", "start"},
	run.StepTests:        {"running tests", "testing", "test"},
	run.StepCleanup:      {"cleaning up", "cleanup", "clean"},
	run.StepJudging:      {"judging", "judging", "judge"},
}

// fitStatus is what follows a box's mark in inner cells: for each form of the words, longest first, the words and the
// time with a cell to spare, then the words alone (with a cell to spare, then without); else the time alone.
func fitStatus(mark string, forms [3]string, since string, inner int) string {
	room := inner - term.Width(mark) - 1 // after the mark and its space
	for _, f := range forms {
		switch {
		case f == "":
		case since != "" && term.Width(f)+1+term.Width(since) <= room-1:
			return f + " " + since
		case term.Width(f) <= room:
			return f
		}
	}
	return since
}

// boxes is how each of an arm's step boxes looks now, for boxes whose status has inner cells.
func (v stateView) boxes(sh term.Shapes, m marks, arm int, now time.Time, tick, inner int) [stepCount]boxState {
	r, have := v.shown[arm], v.have[arm]
	spin := term.Plain(sh.Spinner(tick))
	role := armRole(arm)
	var out [stepCount]boxState
	for i := range out {
		later := boxState{border: term.Muted, title: term.Muted, mark: m.none, markRole: term.Muted}
		switch {
		case !have:
			out[i] = later
		case i == stepTests && r.sandboxDown: // never shown as if it ran
			out[i] = boxState{border: term.LevelCaution, title: term.Default, mark: m.warn, markRole: term.LevelCaution,
				text: fitStatus(m.warn, [3]string{"sandbox unavailable", "no sandbox", "down"}, "", inner)}
		case i == stepResult && r.finished:
			words, _, outcome := outcomeWords(r.result, r.requeued, r.sandboxDown, m)
			out[i] = boxState{border: outcome, title: term.Default, mark: words, markRole: outcome, noTitle: true}
		case i < r.step && r.began[i].IsZero(): // a step the run never began: it was not graded
			out[i] = later
		case i < r.step:
			out[i] = boxState{border: term.OutcomeOK, title: term.Default, mark: m.ok, markRole: term.OutcomeOK, text: term.Elapsed(r.took[i])}
		case i == r.step:
			b := boxState{border: role, title: role, bold: true, mark: spin, markRole: role}
			since := r.began[i]
			if words, ok := subWords[r.sub]; ok {
				b.text, b.noTitle = fitStatus(spin, words, term.Elapsed(now.Sub(r.subAt)), inner), true
			} else {
				b.text = term.Elapsed(now.Sub(since))
			}
			out[i] = b
		default:
			out[i] = later
		}
	}
	return out
}

// outlineRole is the color of the sandbox's outline around step i of an arm's row: the caution color when the
// grading sandbox could not start for the run shown.
func (v stateView) outlineRole(arm, i int) term.Role {
	if i == stepTests && v.have[arm] && v.shown[arm].sandboxDown {
		return term.LevelCaution
	}
	return term.Sandbox
}

// rowNote is what the sandbox did to the run shown, in plain words, for its name line: in full, and without what the
// result box says already for a narrow line; "" when nothing.
func (v stateView) rowNote(arm int, m marks) (full, short string) {
	r := v.shown[arm]
	switch {
	case !v.have[arm]:
		return "", ""
	case r.sandboxDown && r.retrying:
		return "sandbox unavailable " + m.sep + " retrying", "sandbox unavailable"
	case r.sandboxDown:
		return "sandbox unavailable " + m.sep + " not counted", "sandbox unavailable"
	case r.finished && r.result.Outcome == run.OutcomeSandboxFlagged:
		blocked := "blocked: " + blockedWords(r.result.SandboxFlagged)
		return blocked + " " + m.sep + " this run doesn't count", blocked
	}
	return "", ""
}

// nameLine is an arm's name and its current task.
func (v stateView) nameLine(c *term.Canvas, m marks, x, y, arm int) int {
	x = c.Put(x, y, v.facts.labels[arm], armRole(arm), true)
	if !v.have[arm] {
		return c.Put(x, y, " "+m.sep+" waiting for its first run", term.Muted, false)
	}
	r := v.shown[arm]
	x = c.Put(x, y, " "+m.sep+" "+term.Truncate(r.task, maxNameWidth, m.ellipsis), term.Muted, false)
	if r.attempt > 1 {
		x = c.Put(x, y, fmt.Sprintf(" %s try %d", m.sep, r.attempt), term.Muted, false)
	}
	if full, short := v.rowNote(arm, m); full != "" { // on the name line, so the frame keeps its height
		room := max(c.Width()-x-3, 0)
		note := full
		if term.Width(full) > room {
			note = term.Truncate(short, room, m.ellipsis)
		}
		x = c.Put(x, y, "   "+note, term.LevelCaution, false)
	}
	return x
}

// sandboxedStep reports whether step i runs in the sandbox: Claude Code always does, the hidden tests in sandbox mode.
func (v stateView) sandboxedStep(i int) bool {
	return i == stepAgent || i == stepTests && v.facts.sandboxed
}

// connectors draws the dotted lines on row y between the boxes, the sandbox's edges where they cross them, and the dot
// on its way from a finished step to the next.
func (v stateView) connectors(c *term.Canvas, m marks, lay rowLayout, arm, y int, now time.Time) {
	for i := range stepCount - 1 {
		c.HLine(lay.x[i]+lay.box, y, lay.x[i+1]-lay.x[i]-lay.box, m.line, term.Muted)
	}
	for i := range stepCount {
		if v.sandboxedStep(i) {
			c.Put(lay.x[i]-2, y, m.outV, v.outlineRole(arm, i), false)
			c.Put(lay.x[i]+lay.box+1, y, m.outV, v.outlineRole(arm, i), false)
		}
	}
	r := v.shown[arm]
	if !v.have[arm] || r.step == 0 || r.moved.IsZero() {
		return
	}
	since := now.Sub(r.moved)
	if since < 0 || since >= dotTime {
		return
	}
	from := r.step - 1
	start, n := lay.x[from]+lay.box, lay.x[from+1]-lay.x[from]-lay.box
	at := min(int(float64(n)*float64(since)/float64(dotTime)), n-1)
	c.Put(start+at, y, m.dot, armRole(arm), false)
}

// armBoxes is an arm's block: its name and task, then four boxes three rows high inside their outlines.
func (v stateView) armBoxes(sh term.Shapes, m marks, lay rowLayout, arm int, now time.Time, tick int) []string {
	c := term.NewCanvas(lay.total, 7)
	v.nameLine(c, m, lay.x[0], 0, arm)
	states := v.boxes(sh, m, arm, now, tick, lay.box-2)
	inner := lay.box - 2
	for i, b := range states {
		x := lay.x[i]
		box(c, m, x, 2, lay.box, 4, b.border)
		title := term.Truncate(lay.titles[i], inner, "")
		c.Put(x+1+(inner-term.Width(title))/2, 3, title, b.title, b.bold)
		status := []part{{b.mark, b.markRole, false}}
		if b.text != "" && term.Width(b.mark)+1+term.Width(b.text) <= inner { // else the mark alone: the judge's spinner
			status = append(status, part{" " + b.text, term.Default, false})
		}
		putParts(c, x+1+max((inner-partsWidth(status))/2, 0), 4, inner, status)
	}
	for i := range stepCount {
		if v.sandboxedStep(i) {
			outline(c, m, lay.x[i]-2, 1, lay.box+4, 6, v.outlineRole(arm, i))
		}
	}
	v.connectors(c, m, lay, arm, 3, now) // last: the dot passes over the outline's edge
	return c.Lines(sh.Style)
}

// outline draws the sandbox's dashed outline, labelled, w cells wide and h rows high at x, y, in role.
func outline(c *term.Canvas, m marks, x, y, w, h int, role term.Role) {
	label := " sandbox "
	c.Put(x, y, m.outTL, role, false)
	c.Put(x+1, y, m.outH, role, false)
	end := c.Put(x+2, y, label, role, false)
	c.HLine(end, y, x+w-1-end, m.outH, role)
	c.Put(x+w-1, y, m.outTR, role, false)
	c.VLine(x, y+1, h-2, m.outV, role)
	c.VLine(x+w-1, y+1, h-2, m.outV, role)
	c.Put(x, y+h-1, m.outBL, role, false)
	c.HLine(x+1, y+h-1, w-2, m.outH, role)
	c.Put(x+w-1, y+h-1, m.outBR, role, false)
}

// armOneLine is an arm's block on two rows: its name, then each box on one line between its borders.
func (v stateView) armOneLine(sh term.Shapes, m marks, lay rowLayout, arm int, now time.Time, tick int) []string {
	c := term.NewCanvas(lay.total, 2)
	v.nameLine(c, m, lay.x[0], 0, arm)
	v.connectors(c, m, lay, arm, 1, now)
	inner := lay.box - 2
	for i, b := range v.boxes(sh, m, arm, now, tick, lay.box-2) {
		x := lay.x[i]
		c.Put(x, 1, m.boxV, b.border, false)
		c.Put(x+lay.box-1, 1, m.boxV, b.border, false)
		parts := stepParts(i, b)
		if partsWidth(parts) > inner && len(parts) > 2 { // too narrow for the name: the place says which step it is
			parts = append(parts[:1], parts[2:]...)
		}
		putParts(c, x+1+max((inner-partsWidth(parts))/2, 0), 1, inner, parts)
	}
	return c.Lines(sh.Style)
}

// armText is an arm on one line: its name and task, then each step in words.
func (v stateView) armText(sh term.Shapes, m marks, arm int, now time.Time, tick int) string {
	taskWidth := min(v.facts.taskWidth, 16)
	nameWidth := max(term.Width(v.facts.labels[0]), term.Width(v.facts.labels[1])) + 3 + taskWidth
	c := term.NewCanvas(nameWidth+72, 1)
	x := c.Put(1, 0, v.facts.labels[arm], armRole(arm), true)
	if v.have[arm] {
		c.Put(x, 0, " "+m.sep+" "+term.Truncate(v.shown[arm].task, taskWidth, m.ellipsis), term.Muted, false)
	}
	x = nameWidth + 3
	for i, b := range v.boxes(sh, m, arm, now, tick, 30) {
		if i > 0 {
			x = c.Put(x, 0, " "+m.line+m.line+" ", term.Muted, false)
		}
		x = putParts(c, x, 0, 30, stepParts(i, b))
	}
	return c.Lines(sh.Style)[0]
}

// part is a piece of a step drawn on one line.
type part struct {
	text string
	role term.Role
	bold bool
}

// stepParts is step i on one line: its mark, its short name and its time, or the result's own words.
func stepParts(i int, b boxState) []part {
	parts := []part{{b.mark, b.markRole, false}}
	if !b.noTitle {
		parts = append(parts, part{" " + stepTitles[1][i], b.title, b.bold})
	}
	if b.text != "" {
		parts = append(parts, part{" " + b.text, term.Default, false})
	}
	return parts
}

func partsWidth(parts []part) int {
	n := 0
	for _, p := range parts {
		n += term.Width(p.text)
	}
	return n
}

// putParts puts parts from x on row y, within room cells, and returns the column after them.
func putParts(c *term.Canvas, x, y, room int, parts []part) int {
	for _, p := range parts {
		text := term.Truncate(p.text, room, "")
		room -= term.Width(text)
		x = c.Put(x, y, text, p.role, p.bold)
	}
	return x
}

// logSection is the log under the panel: a heading, then a fixed area of logRows rows with the latest runs' results,
// blank until they come, so the frame never grows or jumps; a new run's line takes the oldest one's place. Pauses,
// retries and warnings are the status line's, and the answer's checks the answer box's. A blank line comes first
// unless the terminal is short.
func (v stateView) logSection(sh term.Shapes, ruleWidth, width, level int) []string {
	m := marksFor(sh)
	var out []string
	if level < 2 {
		out = append(out, "")
	}
	out = append(out, " "+sh.Style.Paint(term.Muted, m.rule+" log "+strings.Repeat(m.rule, max(ruleWidth-7, 1))))
	for i := range logRows {
		line := ""
		if i < len(v.log) {
			line = v.log[i].format(sh, m, v.facts, width)
		}
		out = append(out, line)
	}
	return out
}
