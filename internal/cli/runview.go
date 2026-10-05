package cli

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// The views of a running experiment (experiment run, start --yes).
const (
	viewDashboard = "dashboard" // the quiet live view redrawn in place: the default on a terminal
	viewFlow      = "flow"      // the step boxes with a moving dot, redrawn in place: the earlier default, kept as it was
	viewLog       = "log"       // a styled, append-only log: nothing is redrawn
	viewPlain     = ""          // the plain lines, with the status line on a terminal: off a terminal, --json, NO_COLOR
)

// viewEnv is the environment variable that sets the view once, as --view does for one command.
const viewEnv = "AGENTIUM_VIEW"

// viewFlag is --view: empty, "dashboard", "flow" or "log".
type viewFlag string

func (v *viewFlag) String() string { return string(*v) }

func (v *viewFlag) Set(s string) error {
	if !knownView(s) {
		return fmt.Errorf("--view is %s, %s or %s, not %q", viewDashboard, viewFlow, viewLog, s)
	}
	*v = viewFlag(s)
	return nil
}

// askedView is the view asked for: --view, else AGENTIUM_VIEW, else "" (the default). An unknown AGENTIUM_VIEW is a
// usage error, found before anything runs.
func askedView(flagValue viewFlag, getenv func(string) string) (string, error) {
	if flagValue != "" {
		return string(flagValue), nil
	}
	switch v := strings.TrimSpace(getenv(viewEnv)); v {
	case "", viewDashboard, viewFlow, viewLog:
		return v, nil
	default:
		return "", fmt.Errorf("%s is %s, %s or %s, not %q", viewEnv, viewDashboard, viewFlow, viewLog, v)
	}
}

// knownView reports whether s names a view that --view and AGENTIUM_VIEW take.
func knownView(s string) bool { return s == viewDashboard || s == viewFlow || s == viewLog }

// chooseView decides how a run is shown, and what the terminal can show. The plain view (today's lines, byte for
// byte) is for everything that is not a terminal at least term.MinWidth wide: a pipe, a file, TERM=dumb, a JSON
// document, and a command run inside another's JSON output. On such a terminal the dashboard (the quiet view) is the default, unless
// NO_COLOR is set; asking for a view (--view or AGENTIUM_VIEW) gets it even with NO_COLOR, without color.
func chooseView(env Env, asked string, asJSON bool) (string, term.Capabilities) {
	caps := term.DetectCapabilities(env.Terminal, env.Getenv, envSize(env))
	switch {
	case asJSON || env.JSON || env.Plain || !caps.Terminal || caps.Width < term.MinWidth:
		return viewPlain, caps
	case asked != "":
		return asked, caps
	case caps.Color == term.NoColor:
		return viewPlain, caps
	}
	return viewDashboard, caps
}

// envSize is the terminal's size now from env (zeros when unknown), for term.DetectCapabilities and the display.
func envSize(env Env) func() (int, int) {
	return func() (int, int) {
		cols, rows := 0, 0
		if env.Columns != nil {
			cols = env.Columns()
		}
		if env.Rows != nil {
			rows = env.Rows()
		}
		return cols, rows
	}
}

// marks are the screen's small symbols, in Unicode or, where the locale is not UTF-8, in ASCII.
type marks struct {
	ok, fail, none, sep, dot, warn string
	atLeast                        string // before a cost that is a lower bound: "≥", or ">=" in ASCII
	// box: the step boxes' corners and lines; outline: the sandbox's; line: the dotted connectors; rule: the header's.
	boxTL, boxTR, boxBL, boxBR, boxH, boxV string
	outTL, outTR, outBL, outBR, outH, outV string
	line, rule, ellipsis                   string
}

// words is text with its separators in the marks' characters: "·" is "-" in ASCII.
func (m marks) words(text string) string {
	if m.sep == "·" {
		return text
	}
	return strings.ReplaceAll(text, "·", m.sep)
}

var (
	unicodeMarks = marks{ok: "✓", fail: "✗", none: "–", sep: "·", dot: "●", warn: "!", atLeast: "≥",
		boxTL: "┌", boxTR: "┐", boxBL: "└", boxBR: "┘", boxH: "─", boxV: "│",
		outTL: "╭", outTR: "╮", outBL: "╰", outBR: "╯", outH: "┄", outV: "┆",
		line: "╌", rule: "─", ellipsis: "…"}
	asciiMarks = marks{ok: "+", fail: "x", none: "-", sep: "-", dot: "o", warn: "!", atLeast: ">=",
		boxTL: "+", boxTR: "+", boxBL: "+", boxBR: "+", boxH: "-", boxV: "|",
		outTL: ".", outTR: ".", outBL: "'", outBR: "'", outH: ".", outV: ":",
		line: ".", rule: "-", ellipsis: "..."}
)

func marksFor(sh term.Shapes) marks {
	if sh.ASCII {
		return asciiMarks
	}
	return unicodeMarks
}

// runFacts is what both designed views know of an experiment from its lock: the arms in words, the question, and
// what each step runs in.
type runFacts struct {
	labels    [2]string // the arms' names in words, A's first
	arms      map[string]int
	question  string
	aa        bool
	goal      string
	host      bool // the hidden tests run on the host, unsandboxed (an old lock's empty mode too): the screens warn
	sandboxed bool // the hidden tests run in the grading sandbox (the agent always runs in its own)
	perArm    [2]int
	slotArm   []int // each schedule position's arm: 0 or 1
	tasks     int
	looks     []int // seq-v1: the tasks each check counts; nil for a fixed design
	budget    float64
	limit     float64 // the usage limit, a share of the plan (0.85)
	taskWidth int     // the widest task name shown, in cells
	margin    float64 // the primary metric's margin
	terms     string  // how the runs are run, in words (termsOf)
	// concurrency is how many runs go at once: the quiet view keeps that many rows for them. report is the experiment's
	// name, for the command that opens its report at the end; "" draws none.
	concurrency int
	report      string
}

// maxNameWidth caps the arms' and tasks' names on the screen.
const maxNameWidth = 24

func factsOf(lock experiment.Lock, usageLimit float64) runFacts {
	d := lock.Design
	f := runFacts{arms: map[string]int{}, aa: d.Template == experiment.TemplateAA, goal: d.Goal,
		host: task.GraderOf(lock.Grader) == task.GraderHost, sandboxed: task.GraderOf(lock.Grader) == task.GraderSandbox, tasks: len(lock.Tasks), budget: d.BudgetUSD, limit: usageLimit / 100}
	f.labels = armLabels(d)
	for i, a := range d.Arms {
		if i < 2 {
			f.arms[a.Name] = i
		}
	}
	f.question = runQuestion(d, f.labels)
	f.terms = termsOf(d, f.labels)
	for _, s := range lock.Schedule {
		i := f.arms[s.Arm]
		f.slotArm = append(f.slotArm, i)
		f.perArm[i]++
	}
	if lock.Sequential != nil {
		f.looks = lock.Sequential.Looks
	}
	f.taskWidth = 8
	for _, t := range lock.Tasks {
		f.taskWidth = max(f.taskWidth, min(term.Width(term.Sanitize(t.Name)), maxNameWidth))
	}
	f.concurrency = max(d.Concurrency, 1)
	f.margin = d.CostMargin
	if d.Goal == experiment.GoalBetter {
		f.margin = d.SuccessMargin
	}
	return f
}

// armLabels are the arms' names in words: their contexts ("baseline" for the base's own), or their models in a model
// A/B. Two arms with the same name (an A/A) are numbered.
func armLabels(d experiment.Design) [2]string {
	var l [2]string
	for i, a := range d.Arms {
		if i > 1 {
			break
		}
		switch {
		case d.PerArmProfiles():
			l[i] = strings.TrimPrefix(experiment.Profile(d.ArmModel(a), d.ArmEffort(a)), "claude-")
		case a.Context == experiment.BaseContext:
			l[i] = "baseline"
		default:
			l[i] = a.Context
		}
		l[i] = term.Truncate(term.Sanitize(l[i]), maxNameWidth-2, "…")
	}
	if l[0] == l[1] {
		l[0], l[1] = l[0]+" 1", l[1]+" 2"
	}
	return l
}

// termsOf is how the runs are run, in plain words, as the plain lines' "Running up to …" says it: "2 at a time · each
// run up to $3 and 20 min · Ctrl-C stops; run again to go on". The budget is in the status line.
func termsOf(d experiment.Design, labels [2]string) string {
	parts := []string{fmt.Sprintf("%d at a time", d.Concurrency)}
	if d.Concurrency == 1 {
		parts[0] = "one at a time"
	}
	capA := money(d.ArmRunBudgetUSD(d.Arms[0]))
	runCap := capA
	if capB := money(d.ArmRunBudgetUSD(d.Arms[len(d.Arms)-1])); d.PerArmProfiles() && capB != capA {
		runCap = fmt.Sprintf("%s (%s) or %s (%s)", capA, labels[0], capB, labels[1])
	}
	parts = append(parts, "each run up to "+runCap+" and "+minutes(d.Timeout))
	if d.Judge != nil {
		parts = append(parts, "its judge up to "+money(d.JudgeCapUSD()))
	}
	if d.JudgePairs != nil {
		parts = append(parts, "each pair's comparison up to "+money(d.PairJudgeCapUSD()))
	}
	if len(d.JudgeGraded) > 0 {
		parts = append(parts, "a judge-graded run's grading up to "+money(d.GradingCapUSD()))
	}
	return strings.Join(append(parts, "Ctrl-C stops; run again to go on"), " · ")
}

// packParts lays text's " · "-separated parts on as few lines of at most width cells as they fit (a part wider than
// a line has one to itself).
func packParts(text string, width int) []string {
	var lines []string
	for _, p := range strings.Split(text, " · ") {
		if n := len(lines); n > 0 && term.Width(lines[n-1])+3+term.Width(p) <= width {
			lines[n-1] += " · " + p
			continue
		}
		lines = append(lines, p)
	}
	return lines
}

// minutes is a duration in words: "20 min", or "1m30s".
func minutes(d time.Duration) string {
	if d > 0 && d%time.Minute == 0 {
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return term.Elapsed(d)
}

// runQuestion is what the experiment asks, in plain words.
func runQuestion(d experiment.Design, labels [2]string) string {
	switch {
	case d.Template == experiment.TemplateAA:
		return "checking how much results vary"
	case d.Goal == experiment.GoalBetter:
		return "does " + labels[1] + " pass more tasks?"
	}
	return "does " + labels[1] + " save money?"
}

// answerState is what the answer box says, before it is put in words: the latest verdict on the primary metric, from a
// check of a seq-v1 experiment (a look) or the analysis of a fixed design once every task is done, and how the
// execution ended.
type answerState struct {
	Metric      string   // experiment.MetricCost or MetricSuccess
	Verdict     string   // a stats verdict; "" while there is none (no check yet, or too few tasks for one)
	Estimate    float64  // cost: B's over A's (1.04 is 4% more); success: B's rate minus A's
	HasEstimate bool     // Estimate is set
	A, B        *float64 // success: each arm's rate
	Margin      float64  // the metric's margin: relative for cost (0.10), absolute for success (0.15)
	Decision    string   // seq-v1: the last check's (experiment.LookContinue, LookStop, LookFutility, LookFinal); "" before one
	Seq         bool     // the experiment checks as it goes (seq-v1)
	Next        int      // seq-v1: the tasks the next check counts; 0 when no check is left
	All         int      // the experiment's tasks
	Ended       string   // how the execution ended: "" while it runs, else its status (done, budget, usage or stopped)
	// Settle is about how many tasks would settle an answer that is not sure (MetricResult.TasksToResolve); 0 when
	// unknown.
	Settle int
}

// Final reports whether the box holds the answer rather than the answer so far.
func (a answerState) Final() bool {
	switch a.Decision {
	case experiment.LookStop, experiment.LookFutility, experiment.LookFinal:
		return true
	}
	return !a.Seq && a.Ended == experiment.StatusDone && a.Verdict != ""
}

// Title is the answer box's title.
func (a answerState) Title() string {
	if a.Final() {
		return "the answer"
	}
	return "the answer so far"
}

// decisive reports whether the verdict settles the question.
func (a answerState) decisive() bool {
	switch a.Verdict {
	case stats.Improved, stats.ImprovedSmall, stats.Regressed, stats.Equivalent, stats.NoLoss:
		return true
	}
	return false
}

// Role is the headline's color: green for a better B (or no loss), red for a worse one, else the terminal's own.
func (a answerState) Role() term.Role {
	if !a.decisive() {
		return term.Default
	}
	return term.VerdictRole(a.Verdict)
}

// answerOfLook is a seq-v1 check in the answer box's terms.
func answerOfLook(l experiment.Look, looks []int, tasks int) answerState {
	a := answerState{Metric: experiment.MetricCost, Decision: l.Decision, Seq: true, All: tasks}
	if l.Look < len(looks) {
		a.Next = looks[l.Look]
	}
	if l.Analysed {
		a.Verdict = l.Verdict
		if iv := l.IntervalOfVerdict(); iv != nil {
			a.Estimate, a.HasEstimate = iv.Estimate, true
		}
	}
	return a
}

// answerOfAnalysis is a fixed design's analysis, made once every task is done, in the answer box's terms.
func answerOfAnalysis(an experiment.Analysis, tasks int) (answerState, bool) {
	for _, r := range an.Results {
		if r.Role != experiment.RolePrimary {
			continue
		}
		a := answerState{Metric: r.Metric, Verdict: r.Verdict, Estimate: r.Boot95.Estimate, HasEstimate: r.Tasks >= 2, A: r.A, B: r.B,
			All: tasks, Ended: experiment.StatusDone, Settle: r.TasksToResolve}
		if math.IsNaN(a.Estimate) || math.IsInf(a.Estimate, 0) {
			a.HasEstimate = false
		}
		return a, true
	}
	return answerState{}, false
}

// answerWords puts the answer in plain words: a headline (what the runs show) and a status (how sure, and what comes
// next or why it stopped). It never says "look", "futility", "interval" or "window": those stay in the report.
func answerWords(a answerState, labels [2]string, aa bool) (headline, status string) {
	b := labels[1]
	if a.Metric == experiment.MetricSuccess {
		headline = successWords(a, labels, aa)
	} else {
		headline = costWords(a, b, aa)
	}
	var parts []string
	switch {
	case a.Decision == experiment.LookStop:
		parts = append(parts, "stopped early: sure enough")
	case a.Decision == experiment.LookFutility:
		parts = append(parts, "stopped early", "more tasks are unlikely to settle it")
	case a.Decision == experiment.LookFinal || a.Final():
		parts = append(parts, afterAll(a.All))
		switch {
		case a.decisive():
			parts = append(parts, "sure enough")
		case a.Verdict != "" && a.Settle > 0 && !aa: // an A/A has nothing to settle
			parts = append(parts, "not sure yet", fmt.Sprintf("about %d tasks in all could settle it", a.Settle))
		case a.Verdict != "":
			parts = append(parts, "not sure")
		}
	case a.Seq && a.Decision == "":
		parts = append(parts, nextCheck(a, "first check after %s"))
	case a.Seq && a.Verdict == "":
		parts = append(parts, "too few tasks finished in both", nextCheck(a, "next check after %s"))
	case a.Seq:
		parts = append(parts, "not sure yet", nextCheck(a, "next check after %s"))
	default:
		parts = append(parts, "the answer comes once "+allTasks(a.All)+" done")
	}
	if !a.Final() {
		if stop := stoppedWords(a.Ended); stop != "" { // what comes next will not come in this run
			parts = append(slicesWithout(parts, "next check after", "first check after", "the answer comes"), stop)
		}
	}
	return headline, strings.Join(parts, " · ")
}

// afterAll is "after all 16 tasks", or "after its one task".
func afterAll(n int) string {
	if n == 1 {
		return "after its one task"
	}
	return fmt.Sprintf("after all %d tasks", n)
}

// allTasks is "all 12 tasks are", or "its one task is".
func allTasks(n int) string {
	if n == 1 {
		return "its one task is"
	}
	return fmt.Sprintf("all %d tasks are", n)
}

// taskCount is "1 task" or "16 tasks".
func taskCount(n int) string {
	if n == 1 {
		return "1 task"
	}
	return fmt.Sprintf("%d tasks", n)
}

func nextCheck(a answerState, format string) string {
	if a.Next <= 0 {
		return ""
	}
	return fmt.Sprintf(format, taskCount(a.Next))
}

// slicesWithout drops the empty parts and those starting with any of prefixes.
func slicesWithout(parts []string, prefixes ...string) []string {
	var out []string
	for _, p := range parts {
		keep := p != ""
		for _, prefix := range prefixes {
			keep = keep && !strings.HasPrefix(p, prefix)
		}
		if keep {
			out = append(out, p)
		}
	}
	return out
}

// stoppedWords says why an execution ended before its answer.
func stoppedWords(status string) string {
	switch status {
	case experiment.StatusBudget:
		return "stopped at the budget"
	case experiment.StatusUsage:
		return "paused at your Claude plan's usage limit"
	case experiment.StatusStopped:
		return "stopped · run it again to go on"
	case experiment.StatusDone:
		return "every run is done: the report has the answer"
	}
	return ""
}

// signedPercent is a ratio's change: 1.04 is "+4%", 0.82 "-18%".
func signedPercent(ratio float64) string {
	p := math.Round(100 * (ratio - 1))
	if p == 0 {
		return "+0%"
	}
	return fmt.Sprintf("%+.0f%%", p)
}

// lessMore is a ratio's change in words: 0.82 is "18% less", 1.12 "12% more".
func lessMore(ratio float64) string {
	if ratio < 1 {
		return fmt.Sprintf("%.0f%% less", math.Round(100*(1-ratio)))
	}
	return fmt.Sprintf("%.0f%% more", math.Round(100*(ratio-1)))
}

func costWords(a answerState, b string, aa bool) string {
	est := ""
	if a.HasEstimate {
		est = " (" + signedPercent(a.Estimate) + ")"
	}
	margin := fmt.Sprintf("%.0f%%", 100*a.Margin)
	if a.Margin <= 0 {
		margin = "the margin"
	}
	switch {
	case a.Verdict == "" && a.Final():
		return "no answer: too few tasks finished in both"
	case a.Verdict == "":
		return "too early to tell"
	case a.Verdict == stats.Exploratory:
		return "too few tasks to tell" + est
	case !a.HasEstimate:
		return "no clear difference in cost"
	case aa && (a.Verdict == stats.Improved || a.Verdict == stats.ImprovedSmall || a.Verdict == stats.Regressed):
		return fmt.Sprintf("the two differ by %.0f%% with nothing changed: a false alarm, or something besides the context differs",
			math.Abs(math.Round(100*(a.Estimate-1))))
	case aa && a.Verdict == stats.Equivalent:
		return "no difference in cost, as expected" + est
	case a.Verdict == stats.Improved:
		return b + " is cheaper: " + lessMore(a.Estimate)
	case a.Verdict == stats.ImprovedSmall:
		return b + " is a little cheaper: " + lessMore(a.Estimate)
	case a.Verdict == stats.Regressed:
		return b + " costs more: " + lessMore(a.Estimate)
	case a.Verdict == stats.Equivalent:
		return "the same cost, within " + margin + est
	case a.Verdict == stats.NoLoss:
		return b + " costs no more, within " + margin + est
	case a.Final():
		return "no clear difference in cost"
	case math.Abs(a.Estimate-1) <= a.Margin:
		return "about the same cost" + est
	case a.Estimate < 1:
		return b + " may be cheaper (" + lessMore(a.Estimate) + ")"
	}
	return b + " may cost more (" + lessMore(a.Estimate) + ")"
}

func successWords(a answerState, labels [2]string, aa bool) string {
	b := labels[1]
	rates := ""
	if a.A != nil && a.B != nil {
		rates = fmt.Sprintf(" (%s %.0f%%, %s %.0f%%)", labels[1], 100**a.B, labels[0], 100**a.A)
	}
	points := fmt.Sprintf("%.0f points", 100*a.Margin)
	switch {
	case a.Verdict == "" && a.Final():
		return "no answer: too few tasks finished in both"
	case a.Verdict == "":
		return "too early to tell"
	case a.Verdict == stats.Exploratory:
		return "too few tasks to tell" + rates
	case !a.HasEstimate:
		return "no clear difference in passed tasks"
	case aa && (a.Verdict == stats.Improved || a.Verdict == stats.ImprovedSmall || a.Verdict == stats.Regressed):
		return "the two differ with nothing changed: a false alarm, or something besides the context differs" + rates
	case aa && (a.Verdict == stats.Equivalent || a.Verdict == stats.NoLoss):
		return "no difference in passed tasks, as expected" + rates
	case a.Verdict == stats.Improved:
		return b + " passes more tasks" + rates
	case a.Verdict == stats.ImprovedSmall:
		return b + " passes a few more tasks" + rates
	case a.Verdict == stats.Regressed:
		return b + " passes fewer tasks" + rates
	case a.Verdict == stats.Equivalent:
		return "they pass about as many tasks, within " + points + rates
	case a.Verdict == stats.NoLoss:
		return b + " passes no fewer tasks, within " + points + rates
	case a.Final():
		return "no clear difference in passed tasks" + rates
	case math.Abs(a.Estimate) <= a.Margin:
		return "about as many tasks pass" + rates
	case a.Estimate > 0:
		return b + " may pass more tasks" + rates
	}
	return b + " may pass fewer tasks" + rates
}

// outcomeWords is a finished run's result in words: the result box's (short) and the log's, and their color.
// sandboxDown says the grading sandbox could not start (run.StepSandboxDown): the run was not graded.
func outcomeWords(r experiment.Result, requeued, sandboxDown bool, m marks) (box, log string, role term.Role) {
	switch {
	case sandboxDown && !experiment.Settles(r.Outcome):
		return "not graded", "sandbox unavailable (not counted)", term.OutcomeLeftOut
	case r.Outcome == "" && requeued:
		return "stopped", "stopped before Claude began · runs again next time", term.OutcomeLeftOut
	case experiment.Fair(r.Outcome) && r.Passed == nil && r.GradePending:
		return "judge: pending", "judge: grade pending · graded again from its change", term.OutcomeLeftOut
	case experiment.Fair(r.Outcome) && r.Passed == nil && r.JudgeGraded:
		return "not graded", "judge: not graded (not counted, not tried again)", term.OutcomeLeftOut
	case experiment.Fair(r.Outcome) && r.Passed == nil:
		return "not graded", "not graded", term.OutcomeLeftOut
	case experiment.Fair(r.Outcome) && r.JudgeGraded && *r.Passed: // the judge's grade, labelled wherever a pass shows
		return m.ok + " judge: fixed", m.ok + " judge: fixed", term.OutcomeOK
	case experiment.Fair(r.Outcome) && r.JudgeGraded:
		return m.fail + " judge: not fixed", m.fail + " judge: not fixed", term.OutcomeFailed
	case experiment.Fair(r.Outcome) && *r.Passed:
		return m.ok + " passed", m.ok + " passed", term.OutcomeOK
	case experiment.Fair(r.Outcome):
		return m.fail + " failed", m.fail + " failed", term.OutcomeFailed
	case r.Outcome == "unfair":
		return "left out", "left out: setup changed", term.OutcomeLeftOut
	case r.Outcome == run.OutcomeSandboxFlagged:
		return "left out", "left out (sandbox)", term.OutcomeLeftOut
	case r.Outcome == agent.OutcomeCancelled:
		return "stopped", "stopped (not counted)", term.OutcomeLeftOut
	case r.Outcome == agent.OutcomeInfra:
		return "infra error", "infra error (not counted)", term.OutcomeInfra
	}
	words := term.Sanitize(r.Outcome)
	return words, words, term.OutcomeInfra
}

// money is a dollar amount: "$5" when whole, else "$4.80".
func money(usd float64) string {
	if usd == math.Trunc(usd) && usd < 1e6 {
		return fmt.Sprintf("$%.0f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// logEntry is one line of a running experiment's log: a finished run, or a sentence.
type logEntry struct {
	at       time.Time
	arm      int // -1: a sentence
	task     string
	result   experiment.Result // a run's
	requeued bool
	// sandboxDown: the run's grading sandbox could not start
	sandboxDown bool
	took        time.Duration
	judge       string    // the judge's verdict in words, when judged
	votes       string    // a judge-graded run's: how many of the judge's calls carried its grade ("4 of 5"), or why none
	words       string    // a sentence
	role        term.Role // a sentence's color
	sentence    bool
	check       bool // the sentence tells a check of the answer (the log view draws its box instead)
}

// format draws the entry in width cells (cut with an ellipsis; noWidth: whole, for lines that stay): the time, the arm
// in its color, the task, the outcome, the cost and the duration, or the time and a sentence. Names and words from outside Agentium are sanitized when the entry is made.
func (e logEntry) format(sh term.Shapes, m marks, f runFacts, width int) string {
	st := sh.Style
	at := st.Paint(term.Muted, e.at.Format("15:04:05"))
	if e.sentence {
		return term.Truncate(" "+at+"  "+st.Paint(e.role, m.words(e.words)), width, sh.Ellipsis())
	}
	_, words, role := outcomeWords(e.result, e.requeued, e.sandboxDown, m)
	words = m.words(words)
	labelWidth := max(term.Width(f.labels[0]), term.Width(f.labels[1]))
	arm := term.Pad(st.Paint(armRole(e.arm), f.labels[e.arm]), labelWidth)
	taskName := term.Pad(sh.Fit(e.task, f.taskWidth), f.taskWidth)
	line := " " + at + "  " + arm + "  " + taskName + "  " + term.Pad(st.Paint(role, words), 8)
	if cost := e.result.AgentUSD(); cost > 0 || experiment.Fair(e.result.Outcome) {
		line += "  " + fmt.Sprintf("$%.2f", cost)
	}
	if e.took > 0 {
		line += "  " + st.Paint(term.Muted, term.Elapsed(e.took))
	}
	if e.judge != "" {
		line += "  " + st.Paint(term.Muted, "judge: "+e.judge)
	}
	if e.votes != "" {
		line += "  " + st.Paint(term.Muted, "("+e.votes+")")
	}
	return term.Truncate(line, width, sh.Ellipsis())
}

// noWidth is the width of a log line that is printed whole: in the log view and the scrollback, a long line wraps.
const noWidth = 1 << 20

// armRole is an arm's color: blue for the first, orange for the second.
func armRole(arm int) term.Role {
	if arm == 1 {
		return term.ArmB
	}
	return term.ArmA
}

// questionLine is the question, colored by arm: "BASELINE  vs  TRIMMED   ·   does trimmed save money?".
func questionLine(sh term.Shapes, m marks, f runFacts) string {
	st := sh.Style
	return st.Heading(st.Paint(term.ArmA, strings.ToUpper(f.labels[0]))) + st.Paint(term.Muted, "  vs  ") +
		st.Heading(st.Paint(term.ArmB, strings.ToUpper(f.labels[1]))) + st.Paint(term.Muted, "   "+m.sep+"   ") + f.question
}

// legendLine explains the sandbox outline once.
func legendLine(sh term.Shapes, m marks) string {
	return sh.Style.Paint(term.Sandbox, m.outV+" sandbox") + sh.Style.Paint(term.Muted, ": no internet, your secrets hidden")
}

// hostWarning is the line that says the hidden tests run outside the sandbox (--grader host).
func hostWarning(sh term.Shapes, m marks) string {
	return sh.Style.Paint(term.OutcomeInfra, m.warn+" hidden tests run on your machine, outside the sandbox")
}

// answerBoxWidth is the width of the answer's box when width allows it: its words, with room either side.
func answerBoxWidth(a answerState, f runFacts) int {
	headline, status := answerWords(a, f.labels, f.aa)
	return max(term.Width(a.Title()), term.Width(headline), term.Width(status), 34) + 8
}

// answerBox draws the answer in a green box centred in width cells: its title, the headline and the status.
func answerBox(sh term.Shapes, m marks, a answerState, f runFacts, width int) []string {
	headline, status := answerWords(a, f.labels, f.aa)
	headline, status = m.words(headline), m.words(status)
	inner := min(answerBoxWidth(a, f), width) - 2
	inner = max(inner, 8)
	c := term.NewCanvas(width, 5)
	left := max((width-inner-2)/2, 0)
	box(c, m, left, 0, inner+2, 5, term.OutcomeOK)
	center := func(y int, text string, role term.Role, bold bool) {
		text = term.Truncate(text, inner-2, sh.Ellipsis())
		c.Put(left+1+(inner-term.Width(text))/2, y, text, role, bold)
	}
	center(1, a.Title(), term.Default, true)
	center(2, headline, a.Role(), false)
	center(3, fitParts(status, inner-2, m), term.Muted, false)
	return c.Lines(sh.Style)
}

// fitParts fits a status of " · "-separated parts (in the marks' separator) in width cells without changing its
// height: while it is too wide, it drops its least important part ("after all 16 tasks" before what settles it, and
// that before how sure it is), the earliest of equals first; a lone part still too wide is cut.
func fitParts(text string, width int, m marks) string {
	sep := " " + m.sep + " "
	parts := strings.Split(text, sep)
	rank := func(p string) int {
		switch {
		case strings.Contains(p, "sure"):
			return 3
		case strings.Contains(p, "settle"), strings.HasPrefix(p, "stopped"), strings.HasPrefix(p, "paused"), strings.HasPrefix(p, "next check"),
			strings.HasPrefix(p, "first check"):
			return 2
		}
		return 1
	}
	for len(parts) > 1 && term.Width(strings.Join(parts, sep)) > width {
		drop := 0
		for i, p := range parts {
			if rank(p) < rank(parts[drop]) {
				drop = i
			}
		}
		parts = append(parts[:drop:drop], parts[drop+1:]...)
	}
	return term.Truncate(strings.Join(parts, sep), width, m.ellipsis)
}

// answerLines is the answer on two lines, for a short terminal: the title and the headline, then the status, fitted
// to width cells (fitParts).
func answerLines(sh term.Shapes, m marks, a answerState, f runFacts, width int) []string {
	headline, status := answerWords(a, f.labels, f.aa)
	headline, status = m.words(headline), m.words(status)
	st := sh.Style
	return []string{" " + st.Paint(term.OutcomeOK, a.Title()+":") + " " + st.Paint(a.Role(), headline), "   " + st.Paint(term.Muted, fitParts(status, width-3, m))}
}

// answerLine is the answer on one line, for a very short terminal.
func answerLine(sh term.Shapes, m marks, a answerState, f runFacts) string {
	headline, status := answerWords(a, f.labels, f.aa)
	headline, status = m.words(headline), m.words(status)
	st := sh.Style
	return " " + st.Paint(term.OutcomeOK, a.Title()+":") + " " + st.Paint(a.Role(), headline) + st.Paint(term.Muted, " "+m.sep+" "+status)
}

// box draws a square-cornered box w cells wide and h rows high at x, y, its border in role.
func box(c *term.Canvas, m marks, x, y, w, h int, role term.Role) {
	c.Put(x, y, m.boxTL, role, false)
	c.HLine(x+1, y, w-2, m.boxH, role)
	c.Put(x+w-1, y, m.boxTR, role, false)
	for r := y + 1; r < y+h-1; r++ {
		c.Put(x, r, m.boxV, role, false)
		c.Put(x+w-1, r, m.boxV, role, false)
	}
	c.Put(x, y+h-1, m.boxBL, role, false)
	c.HLine(x+1, y+h-1, w-2, m.boxH, role)
	c.Put(x+w-1, y+h-1, m.boxBR, role, false)
}

// operationWords are the classes of sandbox operations, in plain words, by the start of their names (the sandbox's
// own: mach-lookup, file-read-data, network-outbound, …). Never a path or a name: those are the grade's choice.
var operationWords = []struct{ prefix, words string }{
	{"mach-", "a system service lookup"},
	{"file-read", "a file read"},
	{"file-write", "a file write"},
	{"network", "a network connection"},
	{"ipc", "shared memory"},
	{"process", "starting a program"},
	{"sysctl", "a system setting"},
	{"iokit", "a device"},
	{"signal", "a signal to a process"},
}

// blockedWords is a left-out run's flagged denials in plain words, each class once: "a system service lookup, a file
// read". ops is the record's FlaggedOperations ("mach-lookup, file-read-data").
func blockedWords(ops string) string {
	var words []string
	for _, op := range strings.Split(ops, ",") {
		op = strings.TrimSpace(term.Sanitize(op))
		if op == "" {
			continue
		}
		w := "an operation the agent's own sandbox allows"
		for _, c := range operationWords {
			if strings.HasPrefix(op, c.prefix) {
				w = c.words
				break
			}
		}
		if !slices.Contains(words, w) {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return "an operation the agent's own sandbox allows"
	}
	return strings.Join(words, ", ")
}
