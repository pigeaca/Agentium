package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// chainBoxWidth is how wide the chain's boxes are drawn: room for two short lines, the same on every box so the
// dotted connectors run straight down the middle.
const chainBoxWidth = 64

// runWhere says which experiment a stored run belongs to: in the plain lines ("experiment   NAME, slot 3 (from 0), attempt 1")
// and in words for the picture ("part of experiment NAME"); both "" for a run of no experiment. The slot and attempt are
// left to --details.
func runWhere(ctx context.Context, w *workspace, stored store.Run) (plain, designed string) {
	if stored.ExperimentID == 0 {
		return "", ""
	}
	name := fmt.Sprintf("#%d", stored.ExperimentID)
	if all, err := w.db.Experiments(ctx, w.project.ID); err == nil {
		for _, e := range all {
			if e.ID == stored.ExperimentID {
				name = e.Name
			}
		}
	}
	if stored.Kind == "calibration" {
		return fmt.Sprintf("experiment   %s (a calibration before its first pair)", name), "a calibration for experiment " + name
	}
	return fmt.Sprintf("experiment   %s, slot %d (from 0), attempt %d", name, stored.Slot, stored.Attempt), "part of experiment " + name
}

// writeRun prints a run (run show): on a terminal that shows the designed console, the chain of boxes of runShowView
// unless details asks for every line; everywhere else (a pipe, NO_COLOR, TERM=dumb, a narrow terminal) the lines of
// printRun, the experiment's slot and the records' files, byte for byte as before.
func writeRun(env Env, rec run.Record, where, words string, details bool) error {
	if caps := term.DetectCapabilities(env.Terminal, env.Getenv, envSize(env)); !details && !env.Plain && caps.Designed() {
		_, err := io.WriteString(env.Stdout, strings.Join(runShowView(rec, words, caps.Shapes(), caps.Width), "\n")+"\n")
		return err
	}
	printRun(env, rec)
	if where != "" {
		fmt.Fprintf(env.Stdout, "  %s\n", where)
	}
	if entries, err := os.ReadDir(rec.RecordsDir); err == nil {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		fmt.Fprintf(env.Stdout, "  files        %s\n", strings.Join(names, ", "))
	}
	return nil
}

// runShowView draws one run for reading on a terminal: the run as a chain of boxes joined by dotted lines, in plain
// words (fresh copy, Claude works, hidden tests, result), each box two lines at most with its time on the right, then
// a few dim lines for the rest. where says which experiment the run is part of (in words), or "". Every name that comes from
// outside Agentium (tasks, models, notes, paths) is sanitized before it is drawn. The full figures are in --details,
// the records and the JSON.
func runShowView(rec run.Record, where string, sh term.Shapes, width int) []string {
	st := sh.Style
	m := marksFor(sh)
	w := min(width, term.MaxContentWidth)
	var out []string
	out = append(out, " "+st.Heading(term.Sanitize(rec.Task))+st.Paint(term.Muted, " "+m.sep+" arm ")+st.Paint(runArmRole(rec.Arm), term.Sanitize(rec.Arm)))
	out = append(out, " "+st.Paint(term.Muted, strings.Repeat(m.rule, max(w-1, 1))))
	flow := term.Flow{MaxWidth: w, Dotted: true}
	for _, n := range runChain(rec, sh, m) {
		flow.Rows = append(flow.Rows, []term.Node{n})
	}
	out = append(out, sh.Flow(flow, w)...)
	out = append(out, "")
	out = append(out, runDimLines(rec, where, sh, m, w)...)
	for i, line := range out {
		out[i] = term.Truncate(line, width, sh.Ellipsis())
	}
	return out
}

// runArmRole is an arm's color as the dashboard has it: A's blue, any other arm's orange.
func runArmRole(arm string) term.Role {
	if arm == "A" {
		return term.ArmA
	}
	return term.ArmB
}

// runResult is the run's result for the result box and the heading, with its color. A run with no grade says why.
func runResult(rec run.Record, m marks) (string, term.Role) {
	switch {
	case rec.Passed != nil && *rec.Passed:
		if rec.GradedBy == task.GradingJudge {
			return m.ok + " passed, the judge says fixed", term.OutcomeOK
		}
		return m.ok + " passed", term.OutcomeOK
	case rec.Passed != nil:
		return m.fail + " failed", term.OutcomeFailed
	case rec.Outcome == run.OutcomeSandboxFlagged:
		return m.warn + " left out (sandbox)", term.OutcomeInfra
	case rec.Outcome == agent.OutcomeInfra || rec.Outcome == agent.OutcomeUnfair:
		return m.warn + " no fair attempt", term.OutcomeInfra
	}
	return m.none + " not graded", term.OutcomeLeftOut
}

// runChain is the run's boxes, in order: the fresh copy, the agent, the grading and the result.
func runChain(rec run.Record, sh term.Shapes, m marks) []term.Node {
	st := sh.Style
	dim := func(s string) string { return st.Paint(term.Muted, s) }
	node := func(p term.Panel) term.Node { return term.Node{Width: chainBoxWidth, Panel: p} }
	took := func(d time.Duration) string {
		if d <= 0 {
			return ""
		}
		return dim(term.Elapsed(d))
	}
	seconds := func(cmds []task.Command) time.Duration {
		var total float64
		for _, c := range cmds {
			total += c.Seconds
		}
		return time.Duration(total * float64(time.Second))
	}

	// 1. A fresh copy: the arm's context, and what had to be set up in it.
	ctxWords := "arm " + term.Sanitize(rec.Arm) + "'s context"
	if rec.ContextHead != "" {
		ctxWords += " (" + term.Sanitize(rec.ContextHead[:min(len(rec.ContextHead), 7)]) + ")"
	}
	setup := "no setup"
	if n := len(rec.Setup); n > 0 {
		setup = fmt.Sprintf("setup: %d %s", n, plural(n, "command", "commands"))
	}
	copyBox := node(term.Panel{Title: "fresh copy", Right: took(seconds(rec.Setup)),
		Lines: []string{"a clean copy with " + ctxWords, dim(setup)}})

	// 2. The agent works in its own sandbox.
	mt := rec.Metrics
	agentLine := fmt.Sprintf("%s %s %d %s %s %s", "in a sandbox", m.sep, mt.Turns, plural(mt.Turns, "turn", "turns"), m.sep, cents(rec.Spend().AgentUSD))
	if words, role := agentOutcome(rec.Outcome); words != "" {
		agentLine = st.Paint(role, words) + dim(fmt.Sprintf(" %s %d %s %s %s", m.sep, mt.Turns, plural(mt.Turns, "turn", "turns"), m.sep, cents(rec.Spend().AgentUSD)))
	}
	agentBox := node(term.Panel{Title: "Claude works", Right: took(time.Duration(mt.DurationMS) * time.Millisecond), Border: term.Sandbox, Dashed: true,
		Lines: []string{agentLine, dim(toolsUsed(mt, m))}})

	// 3. Grading: hidden tests (in the sandbox or on the host), or the judge's votes.
	grading := gradingBox(rec, sh, m, dim, node, took(seconds(rec.Verify)))

	// 4. The result and what it took in all.
	word, role := runResult(rec, m)
	b := rec.Behavior
	change := fmt.Sprintf("%d %s %s +%d -%d", b.FilesChanged, plural(b.FilesChanged, "file", "files"), m.sep, b.LinesAdded, b.LinesRemoved)
	if rec.Passed == nil {
		change = "nothing was graded"
	}
	var total time.Duration
	if !rec.Started.IsZero() && rec.Finished.After(rec.Started) {
		total = rec.Finished.Sub(rec.Started)
	}
	resultBox := node(term.Panel{Title: "result", Right: took(total), Border: role,
		Lines: []string{st.Heading(st.Paint(role, word)), dim(change)}})
	return []term.Node{copyBox, agentBox, grading, resultBox}
}

// agentOutcome says in words when the agent did not simply finish, with its color; "" when it did.
func agentOutcome(outcome string) (string, term.Role) {
	switch outcome {
	case agent.OutcomeOK, run.OutcomeSandboxFlagged: // the latter is the grading's, said in its box
		return "", term.Default
	case agent.OutcomeCapped:
		return "hit its cap", term.OutcomeFailed
	case agent.OutcomeTimeout:
		return "ran out of time", term.OutcomeFailed
	case agent.OutcomeInfra:
		return "infrastructure failed", term.OutcomeInfra
	case agent.OutcomeUnfair:
		return "setup drifted", term.OutcomeInfra
	case agent.OutcomeCancelled:
		return "stopped", term.OutcomeLeftOut
	}
	return term.Sanitize(outcome), term.OutcomeLeftOut
}

// toolsUsed names the tools the agent used most, with their counts: "used Bash ×5, Edit ×3, Read ×2 and 1 more".
func toolsUsed(mt agent.Metrics, m marks) string {
	type use struct {
		name string
		n    int
	}
	var uses []use
	for name, n := range mt.ToolUses {
		uses = append(uses, use{term.Sanitize(name), n})
	}
	if len(uses) == 0 {
		return "used no tools"
	}
	slices.SortFunc(uses, func(a, b use) int {
		if a.n != b.n {
			return b.n - a.n
		}
		return strings.Compare(a.name, b.name)
	})
	cross := "×"
	if m.sep != "·" {
		cross = "x"
	}
	var parts []string
	for _, u := range uses[:min(len(uses), 3)] {
		parts = append(parts, fmt.Sprintf("%s %s%d", u.name, cross, u.n))
	}
	text := "used " + strings.Join(parts, ", ")
	if more := len(uses) - 3; more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}

// gradingBox is the third box. Tests run in the grading sandbox or on the host (a warning); a judge-graded run is
// graded by the judge's votes instead, and no tests run.
func gradingBox(rec run.Record, sh term.Shapes, m marks, dim func(string) string, node func(term.Panel) term.Node, took string) term.Node {
	st := sh.Style
	if rec.GradedBy == task.GradingJudge {
		lines := []string{term.Sanitize(run.GradeWords(rec))}
		role := term.Muted
		if rec.Judge != nil {
			v := rec.Judge
			lines = append(lines, dim(fmt.Sprintf("%s %s %s %s unvalidated", term.Sanitize(v.Model), m.sep, cents(v.CostUSD), m.sep)))
			role = term.OutcomeOK
			if rec.Passed != nil && !*rec.Passed {
				role = term.OutcomeFailed
			}
		}
		if rec.Passed == nil {
			role = term.Muted
		}
		return node(term.Panel{Title: "the judge", Border: role, Lines: lines})
	}
	if g := rec.Sandbox; rec.Outcome == run.OutcomeSandboxFlagged && g != nil && g.FlaggedCount > 0 {
		return node(term.Panel{Title: "hidden tests", Right: took, Border: term.Sandbox, Dashed: true, Lines: []string{"in a sandbox",
			st.Paint(term.OutcomeInfra, fmt.Sprintf("%s tests failed: %d blocked %s the agent's sandbox allows", m.warn, g.FlaggedCount,
				plural(g.FlaggedCount, "action", "actions")))}})
	}
	if g := rec.Sandbox; rec.Passed == nil && g != nil && g.Canary != task.CanaryPassed {
		return node(term.Panel{Title: "hidden tests", Border: term.Sandbox, Dashed: true,
			Lines: []string{"in a sandbox", st.Paint(term.OutcomeFailed, "the sandbox check failed: nothing was graded")}})
	}
	if rec.Passed == nil {
		return node(term.Panel{Title: "hidden tests", Lines: []string{dim("the tests did not run"), dim(gradingWhere(rec))}})
	}
	p := term.Panel{Title: "hidden tests", Right: took}
	switch task.GraderOf(rec.Grader) {
	case task.GraderSandbox:
	case task.GraderHost:
		p.Lines = []string{st.Paint(term.OutcomeInfra, m.warn+" run on your machine, outside the sandbox"), dim(testsWords(rec))}
		return node(p)
	default: // container-v1 included, until the containers plan's step 4: not a sandbox panel, and not "your machine"
		p.Lines = []string{dim(task.DescribeGrader(rec.Grader)), dim(testsWords(rec))}
		return node(p)
	}
	p.Border, p.Dashed = term.Sandbox, true
	line1, line2 := "in a sandbox", testsWords(rec)
	if g := rec.Sandbox; g != nil {
		switch {
		case g.Canary != task.CanaryPassed:
			line2 = st.Paint(term.OutcomeFailed, "the sandbox check failed: nothing was graded")
		case g.Unread != "":
			line2 = dim("sandbox check passed " + m.sep + " blocked actions not read")
		case g.FlaggedCount > 0:
			line2 = st.Paint(term.OutcomeInfra, fmt.Sprintf("%s %d blocked %s the agent's sandbox allows", m.warn, g.FlaggedCount, plural(g.FlaggedCount, "action", "actions")))
		default:
			line2 = dim(fmt.Sprintf("sandbox check passed %s %d blocked %s", m.sep, g.DenialCount, plural(g.DenialCount, "action", "actions")))
		}
	}
	p.Lines = []string{line1, line2}
	return node(p)
}

// gradingWhere says where the tests would have run, for a run that never reached them.
func gradingWhere(rec run.Record) string {
	if rec.Grader == "" {
		return "no grading mode recorded"
	}
	return "they would run " + task.DescribeGrader(rec.Grader)
}

// testsWords says how many verification commands ran.
func testsWords(rec run.Record) string {
	n := len(rec.Verify)
	if n == 0 {
		return "no commands recorded"
	}
	return fmt.Sprintf("%d %s", n, plural(n, "command", "commands"))
}

// runDimLines are the lines under the chain: which experiment, Claude Code's version and model, what drifted, the
// notes, where the records are and how to see all of it.
func runDimLines(rec run.Record, where string, sh term.Shapes, m marks, width int) []string {
	st := sh.Style
	var out []string
	dim := func(text string) { out = append(out, wrapped(st, " ", text, term.Muted, width)...) }
	if where != "" {
		dim(term.Sanitize(where))
	}
	mt := rec.Metrics
	dim(strings.Join([]string{"Claude Code " + term.Sanitize(term.OrNone(mt.CLIVersion)), term.Sanitize(term.OrNone(mt.Model))}, " "+m.sep+" "))
	for _, d := range rec.Drift {
		out = append(out, " "+st.Warn(term.Truncate(term.Sanitize("unfair: "+d), width-2, m.ellipsis)))
	}
	for _, n := range rec.Notes {
		dim(term.Sanitize(n))
	}
	out = append(out, " "+st.Paint(term.Muted, "records "+tailFit(term.Sanitize(rec.RecordsDir), width-1-len("records "), m.ellipsis)))
	// The command stays whole: its spaces do not break.
	dim("everything: " + strings.Join([]string{"agentium", "run", "show", term.Sanitize(rec.ID), "--details"}, nbsp))
	return out
}

// tailFit cuts path from the left to width cells with an ellipsis, so the end that tells folders apart (the run's own)
// stays.
func tailFit(path string, width int, ellipsis string) string {
	if term.Width(path) <= width {
		return path
	}
	runes := []rune(path)
	for i := range runes {
		if tail := ellipsis + string(runes[i:]); term.Width(tail) <= width {
			return tail
		}
	}
	return term.Truncate(path, width, ellipsis)
}

// cents is a cost in dollars and cents; "<$0.01" for a cost under a cent, which "$0.00" would call free.
func cents(usd float64) string {
	if usd > 0 && usd < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}
