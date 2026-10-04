package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
// A task the judge cannot judge (HasReferenceCode) never needs it, nor does a judge-graded task (spec.JudgeGraded): its
// run is graded by the judge within the run, and a run it could not grade is infrastructure, tried again whole.
func NeedsJudging(rec Record, spec task.Spec) bool {
	return rec.Passed != nil && !spec.JudgeGraded() && HasReferenceCode(spec) && (rec.Judge == nil || rec.Judge.Stopped != "")
}

// Judge asks the judge s about a graded run (rec.Passed set) and stores its verdict in rec.Judge. It reads the task's
// instruction, the reference solution's code diff and the run's agent.diff from its records. The judge never decides
// anything: whatever happens here leaves the run's outcome, Passed and Metrics as they were.
//   - A task without a reference in code (HasReferenceCode) gets a note and no verdict: NeedsJudging is false for it.
//   - A run that can never be judged (its diff is missing, the reference diff fails or changes no code) gets a final,
//     cost-free verdict with the reason in Errors, so resumes do not try it again.
//   - What may pass (an interrupt, a judge folder that cannot be made, an instruction file above it) gets a verdict
//     stopped with judge.StoppedCall, which a resume judges again; an interrupt keeps what was spent.
//
// A run judged before (a verdict that stopped early, judged again on resume) keeps its earlier spend: the new verdict's
// CostUSD adds it, since it was spent all the same. A stored verdict replaces the run's earlier "not judged:" notes.
//
// The calls start in an empty folder in the run's records, which agents may not read, and which is removed afterwards;
// with an API key or a token, Claude Code gets a fresh config folder there too.
func (env Env) Judge(ctx context.Context, spec Spec, s judge.Settings, rec *Record) {
	if rec.Passed == nil {
		return
	}
	env.judge(ctx, spec, s, rec)
}

// judge is Judge without its check that the run was graded: gradeByJudge asks it for the verdict that grades the run.
func (env Env) judge(ctx context.Context, spec Spec, s judge.Settings, rec *Record) {
	if !HasReferenceCode(spec.Task) {
		rec.Notes = append(rec.Notes, "not judged: the task has no reference solution in code to judge against")
		return
	}
	s = s.WithDefaults()
	priorCost := rec.Spend().JudgeUSD
	blank := func() judge.Verdict {
		return judge.Verdict{Version: judge.Version, Answers: []string{}, Reasons: []string{}, Requested: s.Repeats, Model: s.Model,
			Effort: s.Effort, CostUSD: priorCost}
	}
	keep := func(v judge.Verdict) {
		// Claude Code's texts may quote what it was given or its environment: redacted like every record.
		redact := func(text string) string { return string(Redact([]byte(text), env.Secret)) }
		v.Reason = redact(v.Reason)
		for i := range v.Reasons {
			v.Reasons[i] = redact(v.Reasons[i])
		}
		for i := range v.Errors {
			v.Errors[i] = redact(v.Errors[i])
		}
		rec.Notes = slices.DeleteFunc(rec.Notes, func(n string) bool { return strings.HasPrefix(n, "not judged: ") })
		rec.Judge = &v
		label := "judge"
		if rec.GradedBy == task.GradingJudge {
			label = "judge (grading)"
		}
		env.progress("  %s: %s, $%.2f", label, Describe(v), v.CostUSD)
	}
	final := func(err error) { // never judged again
		v := blank()
		v.Errors = []string{err.Error()}
		keep(v)
	}
	again := func(err error) { // judged again on resume
		v := blank()
		v.Stopped, v.Errors = judge.StoppedCall, []string{err.Error()}
		if ctx.Err() != nil {
			v.Errors = []string{"interrupted: " + err.Error()}
		}
		keep(v)
	}
	reference, err := judge.ReferenceDiff(ctx, env.Bare, spec.Task.Base, spec.Task.Solution, spec.Task.Reference)
	if err != nil {
		if ctx.Err() != nil {
			again(err)
		} else {
			final(err)
		}
		return
	}
	candidate, err := os.ReadFile(filepath.Join(rec.RecordsDir, "agent.diff"))
	if err != nil {
		final(fmt.Errorf("the run's diff: %w", err))
		return
	}
	dir := filepath.Join(rec.RecordsDir, "judge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		again(fmt.Errorf("judge folder: %w", err))
		return
	}
	defer os.RemoveAll(dir)
	j := claude.Judgement{CLI: env.CLI, Dir: dir, SignIn: env.SignIn, Secret: env.Secret, Home: env.Home}
	if env.SignIn != claude.SignInLogin {
		j.ConfigDir = filepath.Join(dir, "config")
		if err := os.MkdirAll(j.ConfigDir, 0o700); err != nil {
			again(fmt.Errorf("judge folder: %w", err))
			return
		}
	}
	// The same settings to both: Judge counts the repeats, the caller sets the model and effort of each call.
	call, err := judge.ClaudeCaller(s, j, env.Environ, 0)
	if err != nil {
		again(err)
		return
	}
	if env.judgeSpent != nil { // each call's reported cost as it lands, so a crash loses none of it
		inner, spent := call, 0.0
		call = func(ctx context.Context, prompt string) (judge.Reply, error) {
			reply, err := inner(ctx, prompt)
			var out struct {
				CostUSD float64 `json:"total_cost_usd"`
			}
			if err == nil && json.Unmarshal(reply.Stdout, &out) == nil && out.CostUSD > 0 {
				spent += out.CostUSD
				env.judgeSpent(priorCost + spent)
			}
			return reply, err
		}
	}
	v, err := judge.Judge(ctx, judge.Input{Instruction: spec.Instruction, Reference: reference, Candidate: string(candidate)}, s, call)
	v.CostUSD += priorCost
	if err != nil {
		if ctx.Err() == nil { // the reference changes no code: nothing was spent
			final(err)
			return
		}
		v.Stopped = judge.StoppedCall
		v.Errors = append(v.Errors, "interrupted: "+err.Error())
	}
	keep(v)
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
	case v.Fixed == "" && v.Stopped == "" && len(v.Errors) > 0:
		text = "no answer: " + lastOf(v.Errors)
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

// gradeByJudge grades a judge-graded task's run (spec.Task.JudgeGraded) once grade has measured its change: the judge
// compares the run's agent.diff with the reference solution's code diff (never tests: the task has none) and its
// majority of spec.JudgeGrading's repeats (judge.GradingSettings by default) decides (judge.Grade): a pass, a fail,
// or, when errors, a refusal, a usage limit or an interrupt leave too few answers, no grade. A run without a grade is
// infrastructure (claude.OutcomeInfra: not counted, tried again by an experiment), never a failure; an interrupted one
// is cancelled (unfinished). The verdict is kept in rec.Judge, its spend counted as the judge's (Spend), its texts
// redacted, and Passed follows it.
//
// persist stores a record as the run's finished start file. Judging can take repeats × judge.CallTimeout, so the
// records are redacted and the run persisted first as not graded, and again after each call that reports a cost: if
// Agentium dies while judging, recovery stores an infrastructure run with what the judge spent so far, which an
// experiment tries again (its spend counted).
func (env Env) gradeByJudge(ctx context.Context, spec Spec, rec *Record, persist func(Record) error, unfinished func(error) (Record, error)) (Record, error) {
	s := judge.GradingSettings()
	if spec.JudgeGrading != nil {
		s = spec.JudgeGrading.WithDefaults()
	}
	const stoppedNote = "not graded: Agentium stopped while the judge graded it (infrastructure)"
	partial := func(costUSD float64) Record {
		p := *rec
		p.Outcome, p.Passed = claude.OutcomeInfra, nil
		p.Notes = append(slices.Clone(rec.Notes), stoppedNote)
		p.Judge = &judge.Verdict{Version: judge.Version, Answers: []string{}, Reasons: []string{}, Requested: s.Repeats, Model: s.Model,
			Effort: s.Effort, CostUSD: costUSD, Stopped: judge.StoppedCall, Errors: []string{"Agentium stopped while judging"}}
		return p
	}
	if err := env.redactRecords(rec.RecordsDir); err != nil {
		return unfinished(err)
	}
	rec.Finished = env.Now().UTC() // the deferred write sets it again once graded
	if err := persist(partial(0)); err != nil {
		return unfinished(err)
	}
	grading := env
	grading.judgeSpent = func(usd float64) {
		p := partial(usd)
		p.Finished = env.Now().UTC()
		_ = persist(p) // best effort: the final write follows, and a failure only risks this spend if Agentium also dies
	}
	env.step(StepJudgeGrading)
	grading.judge(ctx, spec, s, rec)
	if rec.Judge == nil { // the task has no reference in code: judge left a note, and nothing was spent
		rec.Outcome, rec.Passed = claude.OutcomeInfra, nil
		rec.Notes = append(rec.Notes, "not graded: the judge has no reference solution in code to compare the run with (infrastructure)")
		env.progress("  %s", env.Style.Warn("warning: not graded: the task's reference solution changes no code"))
		return *rec, nil
	}
	passed, ok, why := judge.Grade(*rec.Judge)
	switch {
	case ok:
		rec.Passed = &passed
		env.progress("  graded by the judge (unvalidated): %s", env.Style.Status(map[bool]string{true: "passed", false: "failed"}[passed]))
	case ctx.Err() != nil:
		return unfinished(ctx.Err())
	default:
		rec.Outcome, rec.Passed = claude.OutcomeInfra, nil
		note := "not graded: " + why + " (infrastructure: not counted, and an experiment tries the run again)"
		rec.Notes = append(rec.Notes, note)
		env.progress("  %s", env.Style.Warn("warning: "+note))
	}
	return *rec, nil
}

// GradeWords is a judge-graded run's grade in a few words, for the run's own lines: "fixed (4 of 5)", "not fixed (3 of
// 5 said partly or no)", or why it has none; "" for a run graded by tests. Callers say it is the judge's.
func GradeWords(rec Record) string {
	if rec.GradedBy != task.GradingJudge {
		return ""
	}
	if rec.Judge == nil {
		return "not graded"
	}
	v := *rec.Judge
	passed, ok, why := judge.Grade(v)
	switch {
	case !ok:
		return "not graded (" + why + ")"
	case v.Empty:
		return "not fixed (the run changed no code)"
	case passed:
		return "fixed (" + GradeVotes(rec) + ")"
	}
	return "not fixed (" + GradeVotes(rec) + " said partly or no)"
}

// GradeVotes is how many of the judge's requested calls carried a judge-graded run's grade: "4 of 5" (said fixed, for a
// pass; said otherwise, for a fail); "" without a grade or for a candidate that changed no code.
func GradeVotes(rec Record) string {
	if rec.GradedBy != task.GradingJudge || rec.Judge == nil || rec.Judge.Empty {
		return ""
	}
	v := *rec.Judge
	passed, ok, _ := judge.Grade(v)
	if !ok {
		return ""
	}
	yes := 0
	for _, a := range v.Answers {
		if a == judge.Yes {
			yes++
		}
	}
	n := yes
	if !passed {
		n = len(v.Answers) - yes
	}
	return fmt.Sprintf("%d of %d", n, max(v.Requested, len(v.Answers)))
}
