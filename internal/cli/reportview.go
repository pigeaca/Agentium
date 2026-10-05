package cli

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// reportView draws an experiment's report for reading on a terminal, in plain words: the question, the answer with a
// picture of how sure it is, each version's results side by side, the tasks where the two differ, the judge's opinion
// and the notes that matter. The numbers behind it (intervals, levels, noise) are in --details, the Markdown and the
// JSON. Every name from outside Agentium (contexts, tasks, statuses) is sanitized before it is drawn.
func reportView(rep report.Report, sh term.Shapes, width int) []string {
	f := factsOf(rep.Lock, 0)
	m := marksFor(sh)
	w := min(width, term.MaxContentWidth)
	center := func(line string) string { return centered(w, line) }
	var out []string
	out = append(out, center(questionLine(sh, m, f)))
	out = append(out, " "+sh.Style.Paint(term.Muted, strings.Repeat(m.rule, max(w-1, 1))))
	out = append(out, center(reportFacts(rep, sh, m, f)))
	out = append(out, "")
	out = append(out, reportAnswer(rep, sh, m, f, w)...)
	out = append(out, "")
	out = append(out, armPanels(rep, sh, m, f, w)...)
	if grid := taskGrid(rep, sh, m, f, w); len(grid) > 0 {
		out = append(out, "")
		out = append(out, grid...)
	}
	if judged := judgeLines(rep, sh, m, f, w); len(judged) > 0 {
		out = append(out, "")
		out = append(out, judged...)
	}
	if notes := reportNotes(rep, sh, m, f, w); len(notes) > 0 {
		out = append(out, "")
		out = append(out, notes...)
	}
	for i, line := range out {
		out[i] = term.Truncate(line, width, sh.Ellipsis())
	}
	return out
}

// reportFacts is the line under the question: "10 tasks · 20 runs · $7.12 spent · 3h20m", and how the experiment
// ended when it did not finish.
func reportFacts(rep report.Report, sh term.Shapes, m marks, f runFacts) string {
	st := sh.Style
	runs := 0
	for _, a := range rep.Arms {
		runs += a.Counted
	}
	tasks := taskCount(f.tasks)
	if ran := ranTasks(rep); ran < f.tasks { // a seq-v1 experiment that stopped early, or one not finished
		tasks = fmt.Sprintf("%d of %d tasks", ran, f.tasks)
	}
	parts := []string{tasks, fmt.Sprintf("%d %s", runs, plural(runs, "run", "runs")), fmt.Sprintf("$%.2f spent", rep.SpentUSD)}
	if share, ok := report.PlanShare(rep); ok { // a subscription sign-in with usage readings; never an API key
		parts = append(parts, planShareWords(share))
	}
	if took := runSpan(rep.Runs); took > 0 {
		parts = append(parts, spanWords(took))
	}
	line := st.Paint(term.Muted, strings.Join(parts, " "+m.sep+" "))
	if words := stoppedWords(rep.Status); words != "" && rep.Status != experiment.StatusDone {
		line += st.Paint(term.Muted, " "+m.sep+" ") + st.Paint(term.LevelCaution, m.words(words))
	}
	return line
}

// planShareWords is the experiment's share of the plan's five-hour limit in plain words: "about 4% of your plan's
// limit", "under 1% of your plan's limit".
func planShareWords(share float64) string {
	if share < 0.005 {
		return "under 1% of your plan's limit"
	}
	return fmt.Sprintf("about %.0f%% of your plan's limit", 100*share)
}

// ranTasks counts the tasks with a counted run in either version.
func ranTasks(rep report.Report) int {
	n := 0
	for _, t := range rep.Tasks {
		for _, c := range t.Arms {
			if c.Counted > 0 {
				n++
				break
			}
		}
	}
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// spanWords is a long duration in words: "20 min", "3h 20m".
func spanWords(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d min", max(int(d.Minutes()), 1))
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// runSpan is the time from the first run's start to the last one's end; 0 when unknown.
func runSpan(runs []report.RunRow) time.Duration {
	var first, last time.Time
	for _, r := range runs {
		s, err1 := time.Parse(time.RFC3339, r.Started)
		e, err2 := time.Parse(time.RFC3339, r.Finished)
		if err1 != nil || err2 != nil {
			continue
		}
		if first.IsZero() || s.Before(first) {
			first = s
		}
		if e.After(last) {
			last = e
		}
	}
	if first.IsZero() || !last.After(first) {
		return 0
	}
	return last.Sub(first).Round(time.Minute)
}

// reportAnswerState is the report's answer in the answer box's terms: a seq-v1 experiment's latest check (the last
// analysed one's verdict standing when the latest counted nothing new), else the analysis of a fixed design.
func reportAnswerState(rep report.Report, f runFacts) (answerState, *experiment.MetricResult) {
	var primary *experiment.MetricResult
	for i, r := range rep.Analysis.Results {
		if r.Role == experiment.RolePrimary {
			primary = &rep.Analysis.Results[i]
			break
		}
	}
	var a answerState
	if s := rep.Analysis.Sequential; s != nil && len(s.Looks) > 0 {
		a = answerOfLook(s.Looks[len(s.Looks)-1], f.looks, f.tasks)
		if l := s.ReportedLook(); l != nil && !s.Looks[len(s.Looks)-1].Analysed {
			prev := answerOfLook(*l, f.looks, f.tasks)
			a.Verdict, a.Estimate, a.HasEstimate = prev.Verdict, prev.Estimate, prev.HasEstimate
		}
	} else if fixed, ok := answerOfAnalysis(rep.Analysis, f.tasks); ok {
		a = fixed
	} else {
		a = answerState{Metric: experiment.MetricCost, All: f.tasks, Seq: f.looks != nil}
	}
	a.Margin = f.margin
	a.Ended = rep.Status
	if a.Ended == store.StatusRunning {
		a.Ended = ""
	}
	return a, primary
}

// reportAnswer is the answer in a green box: the title, the headline, how sure the answer is, and a picture of where
// the true difference likely lies; then the other side of the question (success, for a cost question; cost, for a
// success one) the same way, so that an answer is never read as an unqualified yes: its verdict in words and a picture,
// or, when it has no verdict, the words "too few to tell", a muted picture and what would settle it.
func reportAnswer(rep report.Report, sh term.Shapes, m marks, f runFacts, w int) []string {
	a, primary := reportAnswerState(rep, f)
	headline, status := answerWords(a, f.labels, f.aa)
	headline, status = m.words(headline), m.words(status)
	guard, guardRole := guardWords(rep, f)
	var guardScale []string
	var settle string
	if f.aa {
		guard, guardRole = noiseWords(rep), term.Default
	}
	inner := min(max(term.Width(headline), term.Width(status), term.Width(guard), 52)+6, w-6)
	if gr := guardResult(rep, f); gr != nil && !f.aa {
		g, decided := guardState(rep, f, *gr)
		if gr.Tasks >= 2 {
			guardScale = rangePicture(sh, m, *gr, g, inner-4, !decided)
		}
		if !decided || gr.Verdict == stats.Inconclusive { // no verdict, or "not sure": what would settle it
			settle = settleWords(*gr, rep, decided)
		}
	}
	type line struct {
		text string
		role term.Role
		bold bool
		pic  string // a picture row: styled text laid over the box's blank row, text is "" then
	}
	lines := []line{{text: a.Title(), role: term.Default, bold: true}}
	// A line too wide for the box breaks: the status between its parts, the rest at spaces.
	add := func(text string, role term.Role) {
		parts := term.Wrap(text, inner-2)
		if sep := " " + m.sep + " "; strings.Contains(text, sep) {
			parts = nil
			for _, p := range packParts(strings.ReplaceAll(text, sep, " · "), inner-2) {
				parts = append(parts, term.Wrap(m.words(p), inner-2)...)
			}
		}
		for _, p := range parts {
			lines = append(lines, line{text: p, role: role})
		}
	}
	add(headline, a.Role())
	if f.aa { // an A/A has no other side: its second line is the noise
		add(m.words(guard), guardRole)
	}
	if judged := judgeAnswerWords(rep, f); judged != "" {
		add(m.words(judged), term.Muted)
	}
	add(status, term.Muted)
	if primary != nil && primary.Tasks >= 2 && a.HasEstimate {
		if scale := rangePicture(sh, m, *primary, a, inner-4, false); len(scale) > 0 {
			lines = append(lines, line{})
			for _, s := range scale {
				lines = append(lines, line{pic: s})
			}
		}
	}
	if guard != "" && !f.aa {
		if rep.JudgeGrading != nil { // both kinds of success, each named: the tests' and the judge's
			guard = "by the tests: " + guard
		}
		lines = append(lines, line{})
		add(m.words(guard), guardRole)
		for _, s := range guardScale {
			lines = append(lines, line{pic: s})
		}
		if settle != "" {
			add(m.words(settle), term.Muted)
		}
	}
	height := len(lines) + 2
	c := term.NewCanvas(w, height)
	left := max((w-inner-2)/2, 0)
	box(c, m, left, 0, inner+2, height, term.OutcomeOK)
	for i, l := range lines {
		text := term.Truncate(l.text, inner-2, sh.Ellipsis())
		c.Put(left+1+(inner-term.Width(text))/2, 1+i, text, l.role, l.bold)
	}
	out := c.Lines(sh.Style)
	// A picture is styled text, so it is laid over the box's blank rows after the box is painted.
	edge := sh.Style.Paint(term.OutcomeOK, m.boxV)
	for i, l := range lines {
		if l.pic != "" {
			out[1+i] = strings.Repeat(" ", left) + edge + "  " + term.Pad(l.pic, inner-4) + "  " + edge
		}
	}
	return out
}

// guardResult is the analysis' result for the other side of the question: success for a cost question, cost for a
// success one; nil for an A/A or when the analysis has none.
func guardResult(rep report.Report, f runFacts) *experiment.MetricResult {
	if f.aa {
		return nil
	}
	var guard *experiment.MetricResult
	primary := ""
	for i, r := range rep.Analysis.Results {
		switch r.Role {
		case experiment.RolePrimary:
			primary = r.Metric
		case experiment.RoleGuard:
			guard = &rep.Analysis.Results[i]
		}
	}
	if guard == nil { // seq-v1: success is exploratory, but still the question's other side
		for i, r := range rep.Analysis.Results {
			if (primary == experiment.MetricCost && r.Metric == experiment.MetricSuccess) || (primary == experiment.MetricSuccess && r.Metric == experiment.MetricCost) {
				guard = &rep.Analysis.Results[i]
			}
		}
	}
	return guard
}

// guardState is the guard's result in the answer box's terms, and whether it has a verdict to show (too few tasks, an
// exploratory result or none do not).
func guardState(rep report.Report, f runFacts, r experiment.MetricResult) (answerState, bool) {
	g := answerState{Metric: r.Metric, Verdict: r.Verdict, Estimate: r.Boot95.Estimate, HasEstimate: true, A: r.A, B: r.B, All: f.tasks,
		Ended: experiment.StatusDone, Margin: rep.Lock.Design.SuccessMargin}
	if r.Metric == experiment.MetricCost {
		g.Margin = rep.Lock.Design.CostMargin
	}
	return g, r.Tasks >= 2 && r.Verdict != stats.Exploratory && r.Verdict != ""
}

// settleWords is what would settle a metric that has no verdict: the analysis' figure when it has one ("about 40 tasks
// in all could settle it"), else, for passes in a cost experiment, the size of a --goal better experiment, which tests
// them. "" when neither is known.
func settleWords(r experiment.MetricResult, rep report.Report, decided bool) string {
	if r.TasksToResolve > 0 {
		return fmt.Sprintf("about %d tasks in all could settle it", r.TasksToResolve)
	}
	if !decided && r.Metric == experiment.MetricSuccess && rep.Lock.Design.Goal != experiment.GoalBetter {
		t := experiment.Tiers()[0]
		return fmt.Sprintf("--goal better with %d tasks of %d runs would settle it", t.Tasks, t.Repeats)
	}
	return ""
}

// guardWords is what the answer says of the other side of the question, so that an answer is never read as an
// unqualified yes: the guard's verdict in words when it has one ("lean passes no fewer tasks, within 15 points (lean
// 76%, baseline 73%)"), else that there are too few to tell ("whether lean passes as many tasks: too few to tell"),
// muted. "" for an A/A, or without such a metric.
func guardWords(rep report.Report, f runFacts) (string, term.Role) {
	r := guardResult(rep, f)
	if r == nil {
		return "", term.Default
	}
	g, decided := guardState(rep, f, *r)
	if !decided {
		if r.Metric == experiment.MetricCost {
			return "whether " + f.labels[1] + " costs no more: too few to tell", term.Muted
		}
		return "whether " + f.labels[1] + " passes as many tasks: too few to tell", term.Muted
	}
	if r.Metric == experiment.MetricCost {
		return costWords(g, f.labels[1], f.aa), g.Role()
	}
	return successWords(g, f.labels, f.aa), g.Role()
}

// judgeAnswerWords is the answer box's line on judge-graded tasks: "by the judge (unvalidated): baseline 2 of 3 fixed,
// lean 3 of 3"; "" without them.
func judgeAnswerWords(rep report.Report, f runFacts) string {
	g := rep.JudgeGrading
	if g == nil || len(g.Arms) < 2 {
		return ""
	}
	return fmt.Sprintf("by the judge (unvalidated): %s %d of %d fixed, %s %d of %d", f.labels[0], g.Arms[0].Fixed.Count, g.Arms[0].Graded,
		f.labels[1], g.Arms[1].Fixed.Count, g.Arms[1].Graded)
}

// noiseWords is what an A/A shows of the noise in plain words: how much the same setup's cost varies from run to run
// (exp σ − 1 of log cost), "" when it could not be estimated.
func noiseWords(rep report.Report) string {
	n := rep.Analysis.Noise
	if n == nil || n.Sigma == nil || n.Sigma.Estimate <= 0 {
		return ""
	}
	return fmt.Sprintf("the same setup's cost varies by about %.0f%% from run to run", 100*(math.Exp(n.Sigma.Estimate)-1))
}

// rangePicture draws where the true difference likely lies, on a scale from "cheaper" to "costlier" (or from fewer
// passed to more), with "same" under the line of no change, and the range in words under it. muted draws it in grey,
// for a metric without a verdict.
func rangePicture(sh term.Shapes, m marks, res experiment.MetricResult, a answerState, width int, muted bool) []string {
	iv := report.VerdictInterval(res)
	scale := func(x float64) float64 { return 100 * x } // success: points
	left, right := "fewer pass", "more pass"
	if res.Ratio {
		scale = func(x float64) float64 { return 100 * (x - 1) } // cost: percent change
		left, right = "cheaper", "costlier"
		if res.Metric == experiment.MetricTime {
			left, right = "faster", "slower"
		}
	}
	lo, est, hi := scale(iv.Low), scale(iv.Estimate), scale(iv.High)
	if math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(lo, 0) || math.IsInf(hi, 0) {
		return nil
	}
	bound := max(math.Abs(lo), math.Abs(hi), 100*a.Margin, 5) * 1.25
	arrowL, arrowR := "◀", "▶"
	if sh.ASCII {
		arrowL, arrowR = "<", ">"
	}
	role := term.VerdictInconclusive
	if a.decisive() {
		role = term.VerdictRole(a.Verdict)
	}
	if muted { // no verdict: the picture is drawn, in grey
		role = term.Muted
	}
	labelL, labelR := left+" "+arrowL, arrowR+" "+right
	bar := sh.IntervalBar(term.Interval{Label: labelL, Low: lo, Estimate: est, High: hi, Min: -bound, Max: bound, Value: labelR, Role: role}, width)
	// Under the bar: "same" under the line of no change, always, and the range in words under the range; beside
	// "same" when they would overlap, or on a line of their own when there is no room beside it.
	n := width - term.Width(labelL) - term.Width(labelR) - 2
	col := func(v float64) int {
		return term.Width(labelL) + 1 + int(math.Round((min(max(v, -bound), bound)+bound)/(2*bound)*float64(n-1)))
	}
	words := rangeWords(res, lo, hi)
	ww := term.Width(words)
	sx := min(max(col(0)-2, 0), width-4) // "same", centred under the line of no change
	wx := min(max(col((lo+hi)/2)-ww/2, 0), width-ww)
	apart := func(x int) bool { return x+ww <= sx-2 || x >= sx+6 }
	fitsLeft, fitsRight := sx-2-ww >= 0, sx+6+ww <= width
	crosses := lo < 0 && hi > 0
	switch {
	case apart(wx):
	case fitsLeft && (hi <= 0 || crosses): // the range is on the left (or on both sides): the words go left of "same"
		wx = sx - 2 - ww
	case fitsRight && (lo >= 0 || crosses):
		wx = sx + 6
	default: // no room on the range's side of "same": the words go under it
		c := term.NewCanvas(width, 2)
		c.Put(sx, 0, "same", term.Muted, false)
		c.Put(max((width-ww)/2, 0), 1, words, term.Muted, false)
		return append([]string{bar}, c.Lines(sh.Style)...)
	}
	c := term.NewCanvas(width, 1)
	c.Put(sx, 0, "same", term.Muted, false)
	c.Put(wx, 0, words, term.Muted, false)
	return append([]string{bar}, c.Lines(sh.Style)...)
}

// rangeWords is the range in words: "likely 15% to 21% less", "likely 4% less to 9% more"; for success, "likely 3 to
// 12 points more pass".
func rangeWords(res experiment.MetricResult, lo, hi float64) string {
	if math.Round(lo) == 0 && math.Round(hi) == 0 { // a range too narrow to name, as when every run passed
		return "likely no change"
	}
	if res.Ratio {
		less, more := "less", "more"
		if res.Metric == experiment.MetricTime {
			less, more = "faster", "slower"
		}
		switch {
		case hi <= 0:
			return fmt.Sprintf("likely %.0f%% to %.0f%% %s", math.Abs(math.Round(hi)), math.Abs(math.Round(lo)), less)
		case lo >= 0:
			return fmt.Sprintf("likely %.0f%% to %.0f%% %s", math.Round(lo), math.Round(hi), more)
		}
		return fmt.Sprintf("likely %.0f%% %s to %.0f%% %s", math.Abs(math.Round(lo)), less, math.Round(hi), more)
	}
	pts := func(x float64) string { // "1 point", "12 points"
		if math.Abs(x) == 1 {
			return "point"
		}
		return "points"
	}
	switch {
	case hi <= 0:
		return fmt.Sprintf("likely %.0f to %.0f %s fewer pass", math.Abs(math.Round(hi)), math.Abs(math.Round(lo)), pts(math.Round(lo)))
	case lo >= 0:
		return fmt.Sprintf("likely %.0f to %.0f %s more pass", math.Round(lo), math.Round(hi), pts(math.Round(hi)))
	}
	return fmt.Sprintf("likely %.0f %s fewer to %.0f more pass", math.Abs(math.Round(lo)), pts(math.Round(lo)), math.Round(hi))
}

// armSide is one version's results in its panel.
type armSide struct {
	passed, counted int
	// judgeFixed and judgeCounted are the judge-graded tasks' runs: the judge's grades, apart from the tests'.
	judgeFixed, judgeCounted int
	cost, time               *float64
	notes                    []string
}

// sidesOf collects each version's passes, typical cost and time (the analysis' levels: geometric means per run) and
// what is notable: runs cut short at their cap or the time limit, runs left out by the sandbox.
func sidesOf(rep report.Report) [2]armSide {
	var s [2]armSide
	for i, a := range rep.Arms {
		if i > 1 {
			break
		}
		for _, t := range rep.Tasks {
			cell := t.Arms[a.Name]
			if t.Judged {
				s[i].judgeFixed += cell.Successes
				s[i].judgeCounted += cell.Counted
				continue
			}
			s[i].passed += cell.Successes
			s[i].counted += cell.Counted
		}
		for _, r := range rep.Analysis.Results {
			level := r.A
			if i == 1 {
				level = r.B
			}
			switch r.Metric {
			case experiment.MetricCost:
				s[i].cost = level
			case experiment.MetricTime:
				s[i].time = level
			}
		}
		if a.Capped > 0 {
			s[i].notes = append(s[i].notes, fmt.Sprintf("%d %s cut short at the cap", a.Capped, plural(a.Capped, "run", "runs")))
		}
		if a.TimedOut > 0 {
			s[i].notes = append(s[i].notes, fmt.Sprintf("%d %s out of time", a.TimedOut, plural(a.TimedOut, "run", "runs")))
		}
		left := 0
		for _, r := range rep.Runs {
			if r.Arm == a.Name && r.Outcome == run.OutcomeSandboxFlagged {
				left++
			}
		}
		if left > 0 {
			s[i].notes = append(s[i].notes, fmt.Sprintf("%d left out by the sandbox", left))
		}
		if g := rep.JudgeGrading; g != nil && i < len(g.Arms) && g.Arms[i].Ungraded > 0 {
			s[i].notes = append(s[i].notes, fmt.Sprintf("%d without the judge's grade", g.Arms[i].Ungraded))
		}
	}
	return s
}

// armPanels draws each version's results in a box of its color, side by side (one under the other when narrow).
func armPanels(rep report.Report, sh term.Shapes, m marks, f runFacts, w int) []string {
	sides := sidesOf(rep)
	perRun := rep.Lock.Design.Repeats > 1
	const gap = 4
	boxW := (w - 2 - gap) / 2
	stacked := boxW < 34
	if stacked {
		boxW = min(w-2, 60)
	}
	rows := 0
	content := make([][]reportLine, 2)
	for i := range 2 {
		content[i] = sideLines(sh, m, sides[i], perRun, armRole(i), boxW-4)
		rows = max(rows, len(content[i]))
	}
	height := rows + 3
	draw := func(c *term.Canvas, x, y, arm int) {
		box(c, m, x, y, boxW, height, armRole(arm))
		title := term.Truncate(f.labels[arm], boxW-4, sh.Ellipsis())
		c.Put(x+(boxW-term.Width(title))/2, y+1, title, armRole(arm), true)
		for j, l := range content[arm] {
			putReportLine(c, x+2, y+2+j, l)
		}
	}
	if stacked {
		c := term.NewCanvas(w, 2*height)
		draw(c, 2, 0, 0)
		draw(c, 2, height, 1)
		return c.Lines(sh.Style)
	}
	c := term.NewCanvas(w, height)
	x0 := max((w-2*boxW-gap)/2, 2)
	draw(c, x0, 0, 0)
	draw(c, x0+boxW+gap, 0, 1)
	return c.Lines(sh.Style)
}

// reportLine is a line of parts, each in its role.
type reportLine []part

// putReportLine puts a line's parts from x.
func putReportLine(c *term.Canvas, x, y int, l reportLine) {
	for _, p := range l {
		x = c.Put(x, y, p.text, p.role, p.bold)
	}
}

// sideLines are a version's lines in its panel, inner cells wide: passed as a bar across the panel and in words, the
// typical cost and time, and what is notable.
func sideLines(sh term.Shapes, m marks, s armSide, perRun bool, role term.Role, inner int) []reportLine {
	const label = 14
	frac := 0.0
	if s.counted > 0 {
		frac = float64(s.passed) / float64(s.counted)
	}
	unit, passed := " a task", fmt.Sprintf("%d of %d passed", s.passed, s.counted)
	if perRun {
		unit, passed = " a run", fmt.Sprintf("%d of %d runs passed", s.passed, s.counted)
	}
	var judged reportLine // beside judge-graded tasks: the judge's grades on a line of their own, and the tests' named
	if s.judgeCounted > 0 {
		passed += " the tests"
		fixed := fmt.Sprintf("%d of %d fixed", s.judgeFixed, s.judgeCounted)
		if perRun {
			fixed = fmt.Sprintf("%d of %d runs fixed", s.judgeFixed, s.judgeCounted)
		}
		judged = reportLine{{text: fixed}, {text: ", says the judge", role: term.Muted}}
		if s.counted == 0 { // judge-graded tasks only: the bar is the judge's
			frac = float64(s.judgeFixed) / float64(s.judgeCounted)
		}
	}
	// The bar is drawn with the shapes (partial blocks), then stripped of its styles for the canvas: its fill takes
	// the arm's color and its track the muted one.
	bar := term.Plain(sh.Bar(term.Bar{Fraction: frac}, inner))
	filled := strings.TrimRight(bar, "░.")
	lines := []reportLine{{{text: filled, role: role}, {text: bar[len(filled):], role: term.Muted}}}
	if s.counted > 0 || s.judgeCounted == 0 {
		lines = append(lines, reportLine{{text: passed}})
	}
	if judged != nil {
		lines = append(lines, judged)
	}
	if s.cost != nil {
		lines = append(lines, reportLine{{text: term.Pad("typical cost", label), role: term.Muted}, {text: fmt.Sprintf("$%.2f", *s.cost)}, {text: unit, role: term.Muted}})
	}
	if s.time != nil {
		lines = append(lines, reportLine{{text: term.Pad("typical time", label), role: term.Muted}, {text: term.Elapsed(time.Duration(*s.time * float64(time.Second)))}, {text: unit, role: term.Muted}})
	}
	for _, n := range s.notes {
		lines = append(lines, reportLine{{text: m.warn + " " + n, role: term.LevelCaution}})
	}
	return lines
}

// wrapped lays text on lines of at most width cells, the first after lead and the rest after as many spaces, each
// painted in role.
func wrapped(st term.Style, lead, text string, role term.Role, width int) []string {
	var out []string
	for i, l := range term.Wrap(text, max(width-term.Width(lead), 10)) {
		prefix := lead
		if i > 0 {
			prefix = strings.Repeat(" ", term.Width(lead))
		}
		out = append(out, prefix+st.Paint(role, strings.ReplaceAll(l, nbsp, " ")))
	}
	return out
}

// maxPassedRows is how many of the tasks both versions passed the task block lists before it counts the rest.
const maxPassedRows = 6

// taskRow is one counted task in the task block.
type taskRow struct {
	name   string
	ca, cb report.TaskCell
	judged bool
}

// taskGrid lists every counted task in groups: where the versions ended differently, where both failed (to check),
// where both ended the same with a mixed result (possible with repeats) and where both passed (a few, then a count).
// Each row has the versions' runs as ✓ and ✗, the mean cost as a bar on one scale for the whole block, and the cost. A
// last line counts the tasks each version was cheaper on. Tasks whose every run was left out stay a count.
func taskGrid(rep report.Report, sh term.Shapes, m marks, f runFacts, w int) []string {
	st := sh.Style
	if len(rep.Arms) < 2 {
		return nil
	}
	a, b := rep.Arms[0].Name, rep.Arms[1].Name
	var differ, failed, mixed, passed []taskRow
	uncounted := 0
	for _, t := range rep.Tasks {
		ca, cb := t.Arms[a], t.Arms[b]
		if ca.Counted == 0 && cb.Counted == 0 {
			if ca.Marks+cb.Marks != "" {
				uncounted++ // every run left out
			}
			continue // else not run: a seq-v1 experiment stopped before it
		}
		row := taskRow{term.Sanitize(t.Task), ca, cb, t.Judged}
		switch {
		case ca.Successes != cb.Successes || ca.Counted != cb.Counted:
			differ = append(differ, row)
		case ca.Counted > 0 && ca.Successes == ca.Counted:
			passed = append(passed, row)
		case ca.Successes == 0:
			failed = append(failed, row)
		default:
			mixed = append(mixed, row)
		}
	}
	shown := passed
	if len(shown) > maxPassedRows {
		shown = shown[:maxPassedRows]
	}
	all := slices.Concat(differ, failed, mixed, passed)
	var out []string
	if len(all) > 0 {
		out = append(out, taskBlock(sh, m, f, w, all, [][]taskRow{differ, failed, mixed, shown}, len(passed)-len(shown))...)
		if line := cheaperWords(st, f, all); line != "" && !f.aa {
			out = append(out, "  "+line)
		}
	}
	if uncounted > 0 {
		out = append(out, "  "+st.Paint(term.Muted, m.words(fmt.Sprintf("%s not counted: every run was left out", taskCount(uncounted)))))
	}
	return out
}

// taskBlock draws the groups' rows in columns shared by all of them. scale is every counted task, which sets the name
// column and the bars' scale.
func taskBlock(sh term.Shapes, m marks, f runFacts, w int, scale []taskRow, groups [][]taskRow, morePassed int) []string {
	st := sh.Style
	const judgeMark = " (judge)" // a judge-graded task's ✓ and ✗ are the judge's grades: its name keeps the mark when cut
	const nameMax, barMax, gap = 30, 12, 3
	markW, costW, maxCost := 1, 0, 0.0
	nameW := 8
	for _, r := range scale {
		for _, c := range []report.TaskCell{r.ca, r.cb} {
			markW = max(markW, len([]rune(c.Marks)))
			costW = max(costW, term.Width(taskCost(m, c)))
			if c.CostUSD != nil {
				maxCost = max(maxCost, *c.CostUSD)
			}
		}
	}
	for _, r := range scale {
		nameW = max(nameW, min(term.Width(r.name)+judgeW(r, judgeMark), nameMax+judgeW(r, judgeMark)))
	}
	nameW = min(nameW, nameMax+term.Width(judgeMark))
	// Bars need room: the name column gives way (to 16 cells) before they are left out, and under 70 columns they are.
	barRoom := func(name int) int {
		return (w - 1 - (2 + name + 2) - gap - 2*(markW+costW+2)) / 2
	}
	for nameW > 16 && barRoom(nameW) < 8 {
		nameW--
	}
	barW := min(barRoom(nameW), barMax)
	if w < 70 || barW < 4 {
		barW = 0
	}
	groupW := markW + 1 + costW
	if barW > 0 {
		groupW += 1 + barW
	}
	for _, l := range f.labels { // a version's name has room above its column, up to a limit
		groupW = max(groupW, min(term.Width(l), 14))
	}
	arm := func(c report.TaskCell, role term.Role) string {
		out := term.Pad(taskMarks(st, m, c.Marks), markW)
		if barW > 0 {
			out += " " + term.Pad(costBar(sh, c, maxCost, role, barW), barW)
		}
		return term.Pad(out+" "+term.Pad(st.Paint(term.Muted, taskCost(m, c)), costW), groupW)
	}
	label := func(i int) string {
		return st.Paint(armRole(i), term.Truncate(f.labels[i], groupW+gap-1, sh.Ellipsis()))
	}
	spacer := strings.Repeat(" ", gap)
	out := []string{"  " + term.Pad(st.Heading("every task"), nameW) + "  " + term.Pad(label(0), groupW+gap) + label(1)}
	group := func(head string, rows []taskRow) {
		if len(rows) == 0 {
			return
		}
		out = append(out, "  "+head)
		for _, r := range rows {
			name := sh.Fit(r.name, nameW)
			if r.judged {
				name = sh.Fit(r.name, nameW-term.Width(judgeMark)) + st.Paint(term.Muted, judgeMark)
			}
			out = append(out, strings.TrimRight("  "+term.Pad(name, nameW)+"  "+arm(r.ca, term.ArmA)+spacer+arm(r.cb, term.ArmB), " "))
		}
	}
	n := func(rows []taskRow) string { return fmt.Sprintf(" %s %d", m.sep, len(rows)) }
	group(st.Paint(term.Default, "they differ"+n(groups[0])), groups[0])
	failedHead := "both failed" + n(groups[1])
	check := " " + m.sep + " check these tasks: agentium task show NAME"
	if 2+term.Width(failedHead+check) > w-1 { // a narrow terminal: the shorter words, so the command stays whole
		check = " " + m.sep + " check: agentium task show NAME"
	}
	group(st.Paint(term.LevelCaution, failedHead)+st.Paint(term.Muted, m.words(check)), groups[1])
	group(st.Paint(term.Default, "ended the same, mixed"+n(groups[2])), groups[2])
	passed := len(groups[3]) + morePassed
	group(st.Paint(term.Default, fmt.Sprintf("both passed %s %d", m.sep, passed)), groups[3])
	if morePassed > 0 {
		out = append(out, "  "+st.Paint(term.Muted, fmt.Sprintf("+ %d more passed in both", morePassed)))
	}
	return out
}

// judgeW is the width a judge-graded task's mark adds to its name.
func judgeW(r taskRow, mark string) int {
	if r.judged {
		return term.Width(mark)
	}
	return 0
}

// cheaperWords counts the tasks each version cost less on, among those with a cost in both: "lean was cheaper on 8 of 8
// tasks"; "" when none has.
func cheaperWords(st term.Style, f runFacts, rows []taskRow) string {
	var cheaper [2]int
	both := 0
	for _, r := range rows {
		if r.ca.CostUSD == nil || r.cb.CostUSD == nil {
			continue
		}
		both++
		switch ca, cb := *r.ca.CostUSD, *r.cb.CostUSD; {
		case ca < cb:
			cheaper[0]++
		case cb < ca:
			cheaper[1]++
		}
	}
	switch {
	case both == 0:
		return ""
	case cheaper[0] == cheaper[1]:
		return st.Paint(term.Muted, fmt.Sprintf("neither version was cheaper on more tasks (%d of %d each)", cheaper[0], both))
	}
	win := 0
	if cheaper[1] > cheaper[0] {
		win = 1
	}
	return fmt.Sprintf("%s was cheaper on %d of %d tasks", st.Paint(armRole(win), f.labels[win]), cheaper[win], both)
}

// costBar is a task's mean cost as a bar in its version's color, on the scale where maxCost fills width. Empty without
// a counted cost.
func costBar(sh term.Shapes, c report.TaskCell, maxCost float64, role term.Role, width int) string {
	if c.CostUSD == nil || maxCost <= 0 {
		return ""
	}
	bar := term.Plain(sh.Bar(term.Bar{Fraction: *c.CostUSD / maxCost}, width))
	return sh.Style.Paint(role, strings.TrimRight(bar, "░."))
}

// taskMarks are a task's runs in one version: ✓ and ✗ in run order, – for one not counted, and for none run.
func taskMarks(st term.Style, m marks, marks string) string {
	if marks == "" {
		return st.Paint(term.Muted, m.none) // not run
	}
	var b strings.Builder
	for _, r := range marks {
		switch r {
		case '●':
			b.WriteString(st.Paint(term.OutcomeOK, m.ok))
		case '○':
			b.WriteString(st.Paint(term.OutcomeFailed, m.fail))
		default:
			b.WriteString(st.Paint(term.Muted, m.none))
		}
	}
	return b.String()
}

// taskCost is a task's mean cost in one version, ≥ when a run was cut short; "" without a counted run.
func taskCost(m marks, c report.TaskCell) string {
	if c.CostUSD == nil {
		return ""
	}
	cost := fmt.Sprintf("$%.2f", *c.CostUSD)
	if c.Capped+c.TimedOut > 0 {
		cost = m.atLeast + cost
	}
	return cost
}

// judgeLines are the judges' opinions in plain words, labelled as an opinion: per version, how many passing fixes the
// judge thinks right; with the pair judge, whose fix it preferred, by task, or that there are too few to say.
func judgeLines(rep report.Report, sh term.Shapes, m marks, f runFacts, w int) []string {
	st := sh.Style
	var lines []string
	if j := rep.Judge; j != nil {
		for i, a := range j.Arms {
			if i > 1 {
				break
			}
			p := a.Passing
			if p.Judged == 0 {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s: the judge thinks %d of %d passing fixes are right", st.Paint(armRole(i), f.labels[i]), p.Fixed.Count, p.Judged))
		}
		if j.Pending > 0 {
			lines = append(lines, st.Paint(term.Muted, fmt.Sprintf("%d %s not judged yet", j.Pending, plural(j.Pending, "run", "runs"))))
		}
	}
	if p := rep.PairJudge; p != nil {
		lines = append(lines, pairLine(st, f, p.Tasks))
	}
	if len(lines) == 0 {
		return nil
	}
	out := []string{"  " + st.Heading("the judge's opinion") + st.Paint(term.Muted, "  an AI reading the fixes, not a test")}
	for _, l := range lines {
		out = append(out, wrapped(st, "  ", m.words(l), term.Default, w-1)...)
	}
	return out
}

// pairLine is the pair judge's preference over tasks in words: "the judge preferred lean's fix in 7 of 10 tasks
// (unvalidated)", counting the tasks it compared (ties included), and "may be chance" when the binomial test cannot
// tell it from an even split; "too few to say" below the floor of judge.MinPreferences.
func pairLine(st term.Style, f runFacts, t judge.PreferenceSummary) string {
	compared := t.Complete
	if !t.Enough {
		return fmt.Sprintf("which fix is better: too few to say (%d %s with a preference; it takes %d)", t.A+t.B, plural(t.A+t.B, "task", "tasks"),
			judge.MinPreferences)
	}
	arm, n := 1, t.B
	if t.A > t.B {
		arm, n = 0, t.A
	}
	line := fmt.Sprintf("the judge preferred %s's fix in %d of %d tasks", st.Paint(armRole(arm), f.labels[arm]), n, compared)
	if t.A == t.B {
		line = fmt.Sprintf("the judge preferred neither fix overall (%d tasks compared)", compared)
	}
	if t.P >= 0.05 {
		return line + st.Paint(term.Muted, " (unvalidated, may be chance)")
	}
	return line + st.Paint(term.Muted, " (unvalidated)")
}

// nbsp joins words that wrapped keeps on one line.
const nbsp = "\u00a0"

// earlyStopNote says, after a seq-v1 experiment stopped at an early look with a verdict, that an early stop overstates
// the effect, as the Markdown's note does: "it stopped early: the true saving is likely smaller than 50%". "" otherwise.
func earlyStopNote(rep report.Report, f runFacts) string {
	s := rep.Analysis.Sequential
	if s == nil || s.Ended != experiment.LookStop {
		return ""
	}
	l := s.ReportedLook()
	if l == nil || l.Look >= len(s.Planned) {
		return ""
	}
	a, _ := reportAnswerState(rep, f)
	if !a.HasEstimate {
		return "it stopped early: the true difference is likely smaller than the estimate"
	}
	size := math.Round(100 * math.Abs(a.Estimate-1))
	switch a.Verdict {
	case stats.Improved, stats.ImprovedSmall:
		return fmt.Sprintf("it stopped early: the true saving is likely smaller than %.0f%%", size)
	case stats.Regressed:
		return fmt.Sprintf("it stopped early: the true extra cost is likely smaller than %.0f%%", size)
	}
	return "it stopped early: the true difference is likely smaller than the estimate"
}

// reportNotes are the dim one-liners that matter: what was left out, what the answer leans on, and where the numbers
// are.
func reportNotes(rep report.Report, sh term.Shapes, m marks, f runFacts, w int) []string {
	st := sh.Style
	var notes []string
	if rep.Status != experiment.StatusDone {
		notes = append(notes, fmt.Sprintf("not finished: %d of %d runs done", rep.Settled, rep.Slots))
	}
	if ex := rep.Analysis.Excluded; len(ex) > 0 {
		n := 0
		var why []string
		for _, k := range []struct{ outcome, words string }{{agent.OutcomeUnfair, "setup changed"}, {agent.OutcomeInfra, "infrastructure"},
			{run.OutcomeSandboxFlagged, "sandbox"}, {agent.OutcomeCancelled, "stopped"}, {experiment.OutcomeUngraded, "ungraded by the judge"},
			{experiment.OutcomeGradePending, "awaiting the judge's grade"}} {
			if c := ex[k.outcome]; c > 0 {
				n += c
				why = append(why, fmt.Sprintf("%d %s", c, k.words))
			}
		}
		if n > 0 {
			notes = append(notes, fmt.Sprintf("%d %s not counted (%s); their cost is in the total", n, plural(n, "run", "runs"), strings.Join(why, ", ")))
		}
	}
	for i, a := range rep.Arms {
		if i < 2 && a.Behavior.ConfigPasses > 0 {
			notes = append(notes, fmt.Sprintf("%d %s %s passed by changing the test setup: counted as failed", a.Behavior.ConfigPasses, f.labels[i],
				plural(a.Behavior.ConfigPasses, "run", "runs")))
		}
	}
	if s := rep.Analysis.Sandbox; s != nil && (s.Imbalanced || len(s.Disagrees) > 0) {
		notes = append(notes, "the sandbox left out more runs on one side, so the answer is not sure")
	}
	if u := rep.Analysis.Ungraded; u != nil && (u.Imbalanced || len(u.Disagrees) > 0) {
		notes = append(notes, "the judge left more runs without a grade on one side, so the answer is not sure")
	}
	switch {
	case rep.Lock.Grader == "" || f.sandboxed:
	case f.host:
		notes = append(notes, "hidden tests ran on your machine, outside the sandbox")
	default: // a mode this Agentium does not describe (container-v1 until the containers plan's step 4)
		notes = append(notes, "hidden tests ran in "+term.Sanitize(rep.Lock.Grader)+", a mode this Agentium does not describe")
	}
	if note := earlyStopNote(rep, f); note != "" {
		notes = append(notes, note)
	}
	// The command stays whole: its spaces do not break (wrapped turns them back into spaces).
	command := strings.Join([]string{"agentium", "experiment", "report", term.Sanitize(rep.Experiment), "--details"}, nbsp)
	notes = append(notes, "the numbers behind this: "+command)
	var out []string
	for _, n := range notes {
		out = append(out, wrapped(st, "  ", m.words(n), term.Muted, w-1)...)
	}
	return out
}
