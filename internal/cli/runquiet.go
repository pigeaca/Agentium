package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

// The quiet view of a running experiment (the dashboard, the default on a terminal): what a person needs while the runs
// go, in fixed rows with nothing that moves. It reads the same stateView as the flow view.
//
//	BASELINE vs LEAN · does lean save money?
//	─────────────────────────────────────────
//	runs     ████████░░░░░░  9 of 16 · 4m05s
//	spent    $1.64 of $54 · plan 20% used
//
//	now      baseline  task-a    Claude works    50s
//	so far   lean may be cheaper: about 16% less · not sure yet
//	         baseline passed 4 of 4 · lean passed 4 of 5 · next check after 12 tasks
//
//	last     ✓ lean  task-b  $0.31  3s
//
//	Ctrl-C stops · run it again to go on

// quietRows is the "last" area: the latest results, newest first.
const quietRows = 3

// quietIndent is where a row's content starts: two cells, then a label of nine.
const (
	quietMargin = 2
	quietLabel  = 9
	quietIndent = quietMargin + quietLabel
)

// quietHint is the line under the frame.
const quietHint = "Ctrl-C stops · run it again to go on"

// quietFrame draws the quiet view from a copy of the state. It fits height by dropping the hint, then the "last" rows,
// then every blank row; the rows it keeps never change with what the runs do, so the frame does not jump.
func quietFrame(v stateView, sh term.Shapes, now time.Time, width, height int) []string {
	var lines []string
	for level := range 4 {
		lines = v.quietLines(sh, now, width, level)
		if len(lines) <= height {
			break
		}
	}
	for i, line := range lines {
		lines[i] = term.Truncate(line, width, sh.Ellipsis())
	}
	return lines
}

// quietWaiting is the frame before the experiment's lock is read: the checks, a calibration or a task validated again
// as the latest line printed, with no spinner.
func quietWaiting(sh term.Shapes, m marks, latest string, width int) []string {
	text := "getting ready"
	if latest != "" {
		text += " " + m.sep + " " + latest
	}
	return []string{quietRow(sh, "now", sh.Style.Paint(term.Muted, sh.Fit(text, max(width-quietIndent, 1))))}
}

// quietRow is a row's label, muted, and its content.
func quietRow(sh term.Shapes, label, content string) string {
	return strings.Repeat(" ", quietMargin) + term.Pad(sh.Style.Paint(term.Muted, label), quietLabel) + content
}

// quietCont is a row under a label: the content alone, at the same column.
func quietCont(content string) string {
	if content == "" { // a blank row holds no trailing spaces
		return ""
	}
	return strings.Repeat(" ", quietIndent) + content
}

// quietLines is the frame at a level of compaction: 0 is everything, 1 drops the hint, 2 the "last" rows, 3 the blank rows.
func (v stateView) quietLines(sh term.Shapes, now time.Time, width, level int) []string {
	m := marksFor(sh)
	w := min(width, term.MaxContentWidth)
	blank := func() []string {
		if level >= 3 {
			return nil
		}
		return []string{""}
	}
	out := []string{centered(w, questionLine(sh, m, v.facts))}
	out = append(out, " "+sh.Style.Paint(term.Muted, strings.Repeat(m.rule, max(w-1, 1))))
	out = append(out, v.quietRuns(sh, m, now, w), v.quietSpent(sh, m, now))
	if v.facts.host {
		out = append(out, quietCont(hostWarning(sh, m)))
	}
	out = append(out, blank()...)
	out = append(out, v.quietNow(sh, m, now, w, level)...)
	out = append(out, v.quietAnswer(sh, m, w)...)
	if level < 2 {
		out = append(out, blank()...)
		out = append(out, v.quietLast(sh, m, w)...)
	}
	if level < 1 {
		hint := sh.Style.Paint(term.Muted, m.words(quietHint))
		if v.answer.Final() && v.facts.report != "" { // the end: the report is what comes next
			hint = sh.Style.Paint(term.Muted, "report: ") + sh.Style.Command("agentium experiment report "+v.facts.report)
		}
		out = append(out, "", quietCont(hint))
	}
	return out
}

// settledAll is the runs settled, of all.
func (v stateView) settledAll() (done, all int) {
	return v.settled[0] + v.settled[1], v.facts.perArm[0] + v.facts.perArm[1]
}

// quietRuns is the progress: a bar of the runs settled out of all, the count and the time since the start.
func (v stateView) quietRuns(sh term.Shapes, m marks, now time.Time, w int) string {
	done, all := v.settledAll()
	text := fmt.Sprintf("%d of %d %s %s", done, all, m.sep, term.Elapsed(now.Sub(v.began)))
	barWidth := min(max(w-quietIndent-term.Width(text)-3, 6), 37)
	frac := 0.0
	if all > 0 {
		frac = float64(done) / float64(all)
	}
	bar := sh.Bar(term.Bar{Fraction: frac, Role: term.Default}, barWidth)
	return quietRow(sh, "runs", bar+"   "+text)
}

// quietSpent is the money spent against the budget, and the plan's share used (a subscription's only), in the colors of
// the limits' nearing.
func (v stateView) quietSpent(sh term.Shapes, m marks, now time.Time) string {
	st := sh.Style
	warn := func(role term.Role, text string) string {
		if role == term.LevelCalm {
			return text
		}
		return st.Paint(role, text)
	}
	line := warn(term.Level(v.spent, v.facts.budget), fmt.Sprintf("$%.2f", v.spent)) + st.Paint(term.Muted, " of "+money(v.facts.budget))
	if v.hasUsage {
		used := v.usage.FiveHourAt(now)
		line += st.Paint(term.Muted, " "+m.sep+" plan ") + warn(term.Level(used, v.facts.limit), fmt.Sprintf("%.0f%%", 100*used)) + st.Paint(term.Muted, " used")
	}
	return quietRow(sh, "spent", line)
}

// quietStatus is what the "now" area says when it must say something besides the runs: a pause at the plan's limit with
// the time it resets, a retry, a warning or a comparison (until it fades), or how the execution ended. "" when nothing.
func (v stateView) quietStatus(m marks, now time.Time) (string, term.Role) {
	switch note := v.noteNow(now); {
	case !v.until.IsZero():
		return fmt.Sprintf("waiting for your Claude plan's usage to reset at %s %s in %s", experiment.Clock(v.until, now), m.sep,
			term.Elapsed(max(v.until.Sub(now), 0))), term.LevelCaution
	case note != "":
		return note, v.noteRole
	}
	switch v.answer.Ended {
	case experiment.StatusBudget, experiment.StatusUsage:
		return stoppedWords(v.answer.Ended), term.LevelCaution
	case "":
	default:
		return stoppedWords(v.answer.Ended), term.Muted
	}
	return "", term.Default
}

// quietNow is the "now" area: a row for each run in flight, as many rows as runs go at once, then one row that is
// blank or the status's, so the area keeps its height. A status that does not fit a row goes on over the rows no run
// uses (all of them, with no run in flight), and is cut with an ellipsis after that.
func (v stateView) quietNow(sh term.Shapes, m marks, now time.Time, w, level int) []string {
	st := sh.Style
	rows := max(v.facts.concurrency, len(v.running), 1)
	room := max(w-quietIndent, 1)
	status, role := v.quietStatus(m, now)
	contents := make([]string, rows+1) // the last is the row under the runs
	for i, r := range v.running {
		contents[i] = v.quietRun(sh, m, r, now, w)
	}
	if status == "" && len(v.running) == 0 {
		contents[0] = st.Paint(term.Muted, "getting ready")
		if v.settledSoFar() || v.have[0] || v.have[1] {
			contents[0] = st.Paint(term.Muted, "starting the next run")
		}
	}
	if status != "" {
		free := contents[len(v.running):] // the rows no run uses, and the one under them
		lines := wrapWords(m.words(status), room)
		if len(lines) > len(free) {
			lines = append(lines[:len(free)-1], sh.Fit(strings.Join(lines[len(free)-1:], " "), room))
		}
		for i, line := range lines {
			free[i] = st.Paint(role, line)
		}
	}
	out := []string{quietRow(sh, "now", contents[0])}
	for _, c := range contents[1:rows] {
		out = append(out, quietCont(c))
	}
	switch {
	case contents[rows] != "":
		out = append(out, quietCont(contents[rows]))
	case level < 3:
		out = append(out, "")
	}
	return out
}

// wrapWords breaks text into lines of at most width cells at its spaces (a word wider than a line has one to itself).
func wrapWords(text string, width int) []string {
	var lines []string
	for _, word := range strings.Fields(text) {
		if n := len(lines); n > 0 && term.Width(lines[n-1])+1+term.Width(word) <= width {
			lines[n-1] += " " + word
			continue
		}
		lines = append(lines, word)
	}
	return lines
}

// settledSoFar reports whether any run has settled.
func (v stateView) settledSoFar() bool {
	done, _ := v.settledAll()
	return done > 0
}

// quietStep is the words of a run's step and its time: the dashboard's box words, the moment's where it is a long one
// (fetching dependencies, cleaning up, judging) and the judge's for a judge-graded run's grading.
func quietStep(r stateRun, now time.Time) (words string, since time.Time) {
	since = r.began[r.step]
	sub, hasSub := subWords[r.sub]
	switch {
	case r.sandboxDown && r.step == stepTests:
		return "sandbox unavailable", since
	case hasSub && (r.step == stepCopy || r.step == stepResult || r.sub == run.StepJudgeGrading):
		return sub[0], r.subAt
	case r.judged && r.step == stepTests:
		return judgeTitles[0][stepTests], since
	}
	return stepTitles[0][r.step], since
}

// quietRun is a run in flight: its version in its color, the task, the step and the time in the step.
func (v stateView) quietRun(sh term.Shapes, m marks, r stateRun, now time.Time, w int) string {
	st := sh.Style
	labelW := max(term.Width(v.facts.labels[0]), term.Width(v.facts.labels[1]))
	words, since := quietStep(r, now)
	elapsed := term.Elapsed(now.Sub(since))
	const stepW = 21 // "fetching dependencies"
	taskW := min(v.facts.taskWidth, max(w-quietIndent-labelW-stepW-term.Width(elapsed)-6, 6))
	role := term.Default
	if r.sandboxDown {
		role = term.LevelCaution
	}
	line := term.Pad(st.Paint(armRole(r.arm), v.facts.labels[r.arm]), labelW) + "  " +
		term.Pad(st.Paint(term.Muted, sh.Fit(r.task, taskW)), taskW) + "  " +
		term.Pad(st.Paint(role, sh.Fit(words, stepW)), stepW) + "  " + elapsed
	if r.attempt > 1 {
		line += st.Paint(term.Muted, fmt.Sprintf(" %s try %d", m.sep, r.attempt))
	}
	return line
}

// quietAnswer is the answer so far (or the answer, at the end) on two rows: what the runs show and how sure it is, then
// each version's passes and the next check, or at the end the command for the report. Words from answerWords.
func (v stateView) quietAnswer(sh term.Shapes, m marks, w int) []string {
	st := sh.Style
	room := max(w-quietIndent, 1)
	headline, status := answerWords(v.answer, v.facts.labels, v.facts.aa)
	headline, status = m.words(headline), m.words(status)
	var rest, next []string
	for _, p := range strings.Split(status, " "+m.sep+" ") {
		switch {
		case strings.HasPrefix(p, "next check"), strings.HasPrefix(p, "first check"), strings.HasPrefix(p, "the answer comes"):
			next = append(next, p)
		case p != "":
			rest = append(rest, p)
		}
	}
	headline = sh.Fit(headline, room)
	first := st.Paint(v.answer.Role(), headline)
	if avail := room - term.Width(headline) - 3; avail >= 8 && len(rest) > 0 {
		first += st.Paint(term.Muted, " "+m.sep+" "+fitParts(strings.Join(rest, " "+m.sep+" "), avail, m))
	}
	label := "so far"
	if v.answer.Final() {
		label = "answer"
	}
	sep := st.Paint(term.Muted, " "+m.sep+" ")
	tail := ""
	switch {
	case len(next) > 0 && !v.answer.Final():
		tail = st.Paint(term.Muted, next[0])
	}
	// Each version's passes in the longest form that fits with the tail, then shorter, then without the tail.
	var second string
	for _, form := range []struct {
		words   string
		hasTail bool
	}{{" passed", true}, {"", true}, {" passed", false}, {"", false}} {
		var passes []string
		for arm := range 2 {
			passes = append(passes, st.Paint(armRole(arm), v.facts.labels[arm])+st.Paint(term.Muted, fmt.Sprintf("%s %d of %d", form.words, v.passed[arm], v.graded[arm])))
		}
		second = strings.Join(passes, sep)
		if form.words == "" {
			second = st.Paint(term.Muted, "passed: ") + second
		}
		if form.hasTail && tail != "" {
			second += sep + tail
		}
		if term.Width(second) <= room || !form.hasTail && form.words == "" {
			break
		}
	}
	return []string{quietRow(sh, label, first), quietCont(second)}
}

// quietLast is the last results in a fixed area of quietRows rows, newest first, blank until they come: a mark, the
// version, the task, the cost, the time, and a note for a grade that does not count or that the judge gave.
func (v stateView) quietLast(sh term.Shapes, m marks, w int) []string {
	labelW := max(term.Width(v.facts.labels[0]), term.Width(v.facts.labels[1]))
	taskW := min(v.facts.taskWidth, max(w-quietIndent-labelW-30, 6))
	out := make([]string, quietRows)
	for i := range out {
		n := len(v.log) - 1 - i
		switch {
		case n < 0:
		case i == 0:
			out[i] = quietRow(sh, "last", v.quietResult(sh, m, v.log[n], labelW, taskW, w-quietIndent))
		default:
			out[i] = quietCont(v.quietResult(sh, m, v.log[n], labelW, taskW, w-quietIndent))
		}
	}
	return out
}

// quietResult is one finished run's row in room cells, without its label.
func (v stateView) quietResult(sh term.Shapes, m marks, e logEntry, labelW, taskW, room int) string {
	st := sh.Style
	r := e.result
	_, words, role := outcomeWords(r, e.requeued, e.sandboxDown, m)
	mark := m.none
	switch {
	case experiment.Fair(r.Outcome) && r.Passed != nil && *r.Passed:
		mark = m.ok
	case experiment.Fair(r.Outcome) && r.Passed != nil:
		mark = m.fail
	}
	// The note after the time, and the room it leaves: a long note takes room from the task's name before it is cut.
	note, noteRole := "", term.Muted
	switch {
	case !experiment.Fair(r.Outcome) || r.Passed == nil:
		note, noteRole = m.words(words), quietNoteRole(r, e.sandboxDown, role)
		switch {
		case r.Outcome == run.OutcomeSandboxFlagged:
			note = "blocked: " + blockedWords(r.SandboxFlagged) + " " + m.sep + " this run doesn't count"
		case e.sandboxDown:
			note = "sandbox unavailable " + m.sep + " not counted"
		}
	case r.JudgeGraded:
		note = "judge: fixed"
		if !*r.Passed {
			note = "judge: not fixed"
		}
		if e.votes != "" {
			note += " (" + e.votes + ")"
		}
	case e.judge != "":
		note = "judge: " + e.judge
	}
	fixed := 2 + labelW + 2 + 2 + 6 + 2 + 5 // the mark, the version, the cost and the time, and their gaps
	if note != "" {
		taskW = min(taskW, max(room-fixed-2-term.Width(note), 8))
		if r.Outcome == run.OutcomeSandboxFlagged && fixed+taskW+2+term.Width(note) > room { // the short form
			note = "blocked: " + blockedWords(r.SandboxFlagged)
		}
	}
	cost := ""
	if c := r.AgentUSD(); c > 0 || experiment.Fair(r.Outcome) {
		cost = fmt.Sprintf("$%.2f", c)
	}
	line := st.Paint(role, mark) + " " + term.Pad(st.Paint(armRole(e.arm), v.facts.labels[e.arm]), labelW) + "  " +
		term.Pad(st.Paint(term.Muted, sh.Fit(e.task, taskW)), taskW) + "  " + term.PadLeft(cost, 6) + "  " +
		term.PadLeft(st.Paint(term.Muted, term.Elapsed(e.took)), 5)
	if note != "" {
		line += "  " + st.Paint(noteRole, note)
	}
	return line
}

// quietNoteRole is the color of a left-out or ungraded run's note: the caution color for what the sandbox did, else the
// outcome's own.
func quietNoteRole(r experiment.Result, sandboxDown bool, outcome term.Role) term.Role {
	if sandboxDown || r.Outcome == run.OutcomeSandboxFlagged {
		return term.LevelCaution
	}
	return outcome
}
