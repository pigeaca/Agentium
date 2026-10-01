package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/task"
)

// HasReferenceCode reports whether a task can be judged: it has a reference solution whose reference files hold code
// (not only tests or documents), which the judge compares the run's change with.
func HasReferenceCode(spec task.Spec) bool {
	if spec.Solution == "" {
		return false
	}
	for _, p := range spec.Reference {
		if !task.IsTestFile(p) && !claudectx.IsDocument(p) {
			return true
		}
	}
	return false
}

// NeedsJudging reports whether a graded run still needs the judge, as a resume decides it:
//   - no verdict yet: it was never judged (its process stopped first, say);
//   - a verdict that stopped early (Verdict.Stopped: a usage limit, sign-in, a call that could not be made, an
//     interrupt): judged again;
//   - a verdict that ran its course is final, even with no answer (every call failed: Fixed and Stopped empty), and so
//     is an unjudged candidate with no code (Empty).
//
// A task the judge cannot judge (HasReferenceCode) never needs it.
func NeedsJudging(rec Record, spec task.Spec) bool {
	return rec.Passed != nil && HasReferenceCode(spec) && (rec.Judge == nil || rec.Judge.Stopped != "")
}

// JudgeCostUSD is what the judge spent on a run: none without a verdict. It counts against an experiment's budget,
// never toward the run's cost (Metrics.CostUSD).
func (r Record) JudgeCostUSD() float64 {
	if r.Judge == nil {
		return 0
	}
	return r.Judge.CostUSD
}

// Judge asks the judge s about a graded run (rec.Passed set) and stores its verdict in rec.Judge. It reads the task's
// instruction, the reference solution's code diff and the run's agent.diff from its records. The judge never decides
// anything: whatever happens here leaves the run's outcome, Passed and Metrics as they were, and a judge that cannot
// judge leaves a note instead of a verdict (a task without a reference in code, a missing diff, a judge that could not
// start).
//
// A run judged before (a verdict that stopped early, judged again on resume) keeps its earlier spend: the new verdict's
// CostUSD adds it, since it was spent all the same. An interrupt keeps what was spent too, as a verdict stopped with
// judge.StoppedCall, so a resume judges the run again.
//
// The calls start in an empty folder in the run's records, which agents may not read, and which is removed afterwards;
// with an API key or a token, Claude Code gets a fresh config folder there too.
func (env Env) Judge(ctx context.Context, spec Spec, s judge.Settings, rec *Record) {
	if rec.Passed == nil {
		return
	}
	note := func(format string, a ...any) {
		rec.Notes = append(rec.Notes, "not judged: "+fmt.Sprintf(format, a...))
	}
	if !HasReferenceCode(spec.Task) {
		note("the task has no reference solution in code to judge against")
		return
	}
	reference, err := judge.ReferenceDiff(ctx, env.Bare, spec.Task.Base, spec.Task.Solution, spec.Task.Reference)
	if err != nil {
		note("%v", err)
		return
	}
	candidate, err := os.ReadFile(filepath.Join(rec.RecordsDir, "agent.diff"))
	if err != nil {
		note("the run's diff: %v", err)
		return
	}
	dir := filepath.Join(rec.RecordsDir, "judge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		note("judge folder: %v", err)
		return
	}
	defer os.RemoveAll(dir)
	j := claude.Judgement{CLI: env.CLI, Dir: dir, SignIn: env.SignIn, Secret: env.Secret, Home: env.Home}
	if env.SignIn != claude.SignInLogin {
		j.ConfigDir = filepath.Join(dir, "config")
		if err := os.MkdirAll(j.ConfigDir, 0o700); err != nil {
			note("judge folder: %v", err)
			return
		}
	}
	// The same settings to both: Judge counts the repeats, the caller sets the model and effort of each call.
	call, err := judge.ClaudeCaller(s, j, env.Environ, 0)
	if err != nil {
		note("%v", err)
		return
	}
	prior := rec.Judge
	v, err := judge.Judge(ctx, judge.Input{Instruction: spec.Instruction, Reference: reference, Candidate: string(candidate)}, s, call)
	if err != nil {
		if ctx.Err() == nil { // the reference changes no code: nothing was spent
			note("%v", err)
			return
		}
		v.Stopped = judge.StoppedCall
		v.Errors = append(v.Errors, "interrupted: "+err.Error())
	}
	if prior != nil {
		v.CostUSD += prior.CostUSD
	}
	// Claude Code's texts may quote what it was given or its environment: redacted like every record.
	redact := func(text string) string { return string(Redact([]byte(text), env.Secret)) }
	v.Reason = redact(v.Reason)
	for i := range v.Reasons {
		v.Reasons[i] = redact(v.Reasons[i])
	}
	for i := range v.Errors {
		v.Errors[i] = redact(v.Errors[i])
	}
	rec.Judge = &v
	env.progress("  judge: %s, $%.2f", Describe(v), v.CostUSD)
}

// Describe is a verdict in a few words: the answer and how many repeats agreed, or why there is none.
func Describe(v judge.Verdict) string {
	agreed := 0
	for _, a := range v.Answers {
		if a == v.Fixed {
			agreed++
		}
	}
	var text string
	switch {
	case v.Empty:
		text = "not asked (the run changed no code)"
	case v.Fixed == "":
		text = "no answer"
	case v.Fixed == judge.Yes:
		text = fmt.Sprintf("fixed (%d of %d)", agreed, v.Requested)
	case v.Fixed == judge.Partly && agreed*2 <= len(v.Answers):
		text = fmt.Sprintf("partly (no majority of %d)", len(v.Answers))
	default:
		text = fmt.Sprintf("%s (%d of %d)", v.Fixed, agreed, v.Requested)
	}
	switch v.Stopped {
	case judge.StoppedLimit:
		text += "; stopped at a usage limit or sign-in failure"
	case judge.StoppedCall:
		text += "; stopped: " + strings.TrimSpace(lastOf(v.Errors))
	}
	return text
}

func lastOf(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[len(list)-1]
}
