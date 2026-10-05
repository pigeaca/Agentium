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
// then every blank row, then the "now" rows (a "+ N more" line stands for the runs left out); the rows it keeps never
// change with what the runs do, so the frame does not jump and the answer's rows are never the ones cut.
func quietFrame(v stateView, sh term.Shapes, now time.Time, width, height int) []string {
	var lines []string
	reserved := max(v.facts.concurrency, len(v.running), 1)
	done := false
	for level := 0; level < 4 && !done; level++ {
		lines = v.quietLines(sh, now, width, level, reserved)
		done = len(lines) <= height
	}
	for rows := reserved - 1; !done && rows >= 1; rows-- {
		lines = v.quietLines(sh, now, width, 3, rows)
		done = len(lines) <= height
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

// quietLines is the frame at a level of compaction: 0 is everything, 1 drops the hint, 2 the "last" rows, 3 the blank
// rows. nowRows is the most rows the runs in flight take.
func (v stateView) quietLines(sh term.Shapes, now time.Time, width, level, nowRows int) []string {
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
	out = append(out, v.quietNow(sh, m, now, w, level, nowRows)...)
	out = append(out, v.quietAnswer(sh, m, w)...)
	if level < 2 {
		out = append(out, blank()...)
		out = append(out, v.quietLast(sh, m, w)...)
	}
	if level < 1 {
		out = append(out, "", quietCont(sh.Style.Paint(term.Muted, m.words(quietHint))))
	}
	return out
}

// settledAll is the runs settled, of all.
func (v stateView) settledAll() (done, all int) {
	return v.settled[0] + v.settled[1], v.facts.perArm[0] + v.facts.perArm[1]
}

// stoppedEarly reports whether a check ended the experiment before every run was done.
func (v stateView) stoppedEarly() bool {
	done, all := v.settledAll()
	return done < all && (v.answer.Decision == experiment.LookStop || v.answer.Decision == experiment.LookFutility)
}

// quietRuns is the progress: a bar of the runs settled out of all, the count and the time since the start; "stopped
// early" when a check ended the experiment with runs left, so the bar does not read as half done.
func (v stateView) quietRuns(sh term.Shapes, m marks, now time.Time, w int) string {
	done, all := v.settledAll()
	text := fmt.Sprintf("%d of %d %s %s", done, all, m.sep, term.Elapsed(now.Sub(v.began)))
	if v.stoppedEarly() {
		text = fmt.Sprintf("%d of %d %s stopped early %s %s", done, all, m.sep, m.sep, term.Elapsed(now.Sub(v.began)))
	}
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
// the time it resets, or a retry, a warning or a comparison (until it fades). "" when nothing.
func (v stateView) quietStatus(m marks, now time.Time) (string, term.Role) {
	switch note := v.noteNow(now); {
	case !v.until.IsZero():
		return fmt.Sprintf("waiting for your Claude plan's usage to reset at %s %s in %s", experiment.Clock(v.until, now), m.sep,
			term.Elapsed(max(v.until.Sub(now), 0))), term.LevelCaution
	case note != "":
		return note, v.noteRole
	}
	return "", term.Default
}

// idleWords is what "now" says with no run in flight and no status: before the first run, "getting ready"; when no run
// is coming (every run settled, a check stopped the experiment, the budget is spent) "finishing", for the runner is
// grading again or storing; else the next run is starting.
func (v stateView) idleWords() string {
	done, all := v.settledAll()
	switch {
	case v.answer.Final() || done >= all || v.spent >= v.facts.budget:
		return "finishing"
	case done > 0 || v.have[0] || v.have[1]:
		return "starting the next run"
	}
	return "getting ready"
}

// quietNow is the "now" area: a row for each run in flight, as many rows as runs go at once (at most nowRows), then one
// row that is blank or the status's, so the area keeps its height. A status that does not fit a row goes on over the
// rows no run uses (all of them, with no run in flight), and is cut with an ellipsis after that. More runs than rows end
// in "+ N more".
func (v stateView) quietNow(sh term.Shapes, m marks, now time.Time, w, level, nowRows int) []string {
	st := sh.Style
	reserved := max(v.facts.concurrency, len(v.running), 1)
	rows := min(reserved, nowRows)
	room := max(w-quietIndent, 1)
	status, role := v.quietStatus(m, now)
	shown := v.running
	more := 0
	if len(shown) > rows {
		keep := rows - 1
		shown, more = shown[:keep], len(shown)-keep
	}
	contents := make([]string, rows+1) // the last is the row under the runs
	for i, r := range shown {
		contents[i] = v.quietRun(sh, m, r, now, w)
	}
	used := len(shown)
	if more > 0 {
		text := fmt.Sprintf("+ %d more", more)
		if used == 0 {
			text = fmt.Sprintf("%d runs in flight", more)
		}
		contents[used] = st.Paint(term.Muted, text)
		used++
	}
	if status == "" && len(v.running) == 0 {
		contents[0] = st.Paint(term.Muted, v.idleWords())
	}
	if status != "" {
		free := contents[used:] // the rows no run uses, and the one under them
		lines := wrapWords(m.words(status), room)
		if len(lines) > len(free) {
			lines = append(lines[:len(free)-1], sh.Fit(strings.Join(lines[len(free)-1:], " "), room))
		}
		for i, line := range lines {
			free[i] = st.Paint(role, line)
		}
	}
	out := []string{quietRow(sh, "now", contents[0])}
	for _, c := range contents[1:] {
		out = append(out, quietCont(c))
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

// quietCols splits a row's room: the version's column (cut at a third of it), then what is left over after the gaps and
// fixedRight cells (the time, cost and the rest) for the task and, if want is above zero, a second column of at most
// want cells (the step). The task's and the step's columns shrink before the time is cut.
func (v stateView) quietCols(room, fixedRight, want int) (labelW, taskW, stepW int) {
	labelW = min(max(term.Width(v.facts.labels[0]), term.Width(v.facts.labels[1])), max(room/3, 8))
	avail := max(room-labelW-fixedRight-4, 6) // two gaps of two between the version, the task and the rest
	if want > 0 {
		stepW = min(want, max(avail/2, 8))
		avail = max(avail-stepW-2, 4)
	}
	return labelW, min(v.facts.taskWidth, avail), stepW
}

// quietRun is a run in flight: its version in its color, the task, the step and the time in the step.
func (v stateView) quietRun(sh term.Shapes, m marks, r stateRun, now time.Time, w int) string {
	st := sh.Style
	words, since := quietStep(r, now)
	elapsed := term.Elapsed(now.Sub(since))
	try := ""
	if r.attempt > 1 {
		try = fmt.Sprintf(" %s try %d", m.sep, r.attempt)
	}
	labelW, taskW, stepW := v.quietCols(w-quietIndent, term.Width(elapsed+try), 21) // 21: "fetching dependencies"
	role := term.Default
	if r.sandboxDown {
		role = term.LevelCaution
	}
	return term.Pad(st.Paint(armRole(r.arm), sh.Fit(v.facts.labels[r.arm], labelW)), labelW) + "  " +
		term.Pad(st.Paint(term.Muted, sh.Fit(r.task, taskW)), taskW) + "  " +
		term.Pad(st.Paint(role, sh.Fit(words, stepW)), stepW) + "  " + elapsed + st.Paint(term.Muted, try)
}

// quietAnswer is the answer so far (or the answer, at the end) on two rows: what the runs show and how sure it is, then
// each version's passes and the next check. Words from answerWords.
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
	// Each version's passes (test-graded runs; "no grades yet" before one) in the longest form that fits with the next
	// check, then compact, and only then without the next check.
	var second string
	for _, form := range []struct {
		compact, hasTail bool
	}{{false, true}, {true, true}, {true, false}} {
		var passes []string
		for arm := range 2 {
			label := st.Paint(armRole(arm), v.facts.labels[arm])
			if v.graded[arm] == 0 { // no test grade for this version yet, while the other has some
				none := m.none // a dash beside the separator's own dash reads as two: ASCII says the word
				if sh.ASCII {
					none = "none"
				}
				passes = append(passes, label+st.Paint(term.Muted, " "+none))
				continue
			}
			if form.compact {
				passes = append(passes, label+st.Paint(term.Muted, fmt.Sprintf(" %d/%d", v.passed[arm], v.graded[arm])))
			} else {
				passes = append(passes, label+st.Paint(term.Muted, fmt.Sprintf(" passed %d of %d", v.passed[arm], v.graded[arm])))
			}
		}
		second = strings.Join(passes, sep)
		switch {
		case v.graded[0]+v.graded[1] == 0:
			second = st.Paint(term.Muted, "no grades yet")
			if v.judgeGraded > 0 {
				second = st.Paint(term.Muted, "no test grades yet")
			}
		case form.compact:
			second = st.Paint(term.Muted, "passed: ") + second
		}
		if form.hasTail && tail != "" {
			second += sep + tail
		}
		if term.Width(second) <= room || !form.hasTail {
			break
		}
	}
	return []string{quietRow(sh, label, first), quietCont(second)}
}

// quietLast is the last results in a fixed area of quietRows rows, newest first, blank until they come: a mark, the
// version, the task, the cost, the time, and a note for a grade that does not count or that the judge gave.
func (v stateView) quietLast(sh term.Shapes, m marks, w int) []string {
	labelW, taskW, _ := v.quietCols(w-quietIndent, 15, 0) // the mark, the cost and the time
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
		taskW = min(taskW, max(room-fixed-2-term.Width(note), 6))
		if r.Outcome == run.OutcomeSandboxFlagged && fixed+taskW+2+term.Width(note) > room { // the short form
			note = "blocked: " + blockedWords(r.SandboxFlagged)
		}
	}
	cost := ""
	if c := r.AgentUSD(); c > 0 || experiment.Fair(r.Outcome) {
		cost = fmt.Sprintf("$%.2f", c)
	}
	line := st.Paint(role, mark) + " " + term.Pad(st.Paint(armRole(e.arm), sh.Fit(v.facts.labels[e.arm], labelW)), labelW) + "  " +
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
