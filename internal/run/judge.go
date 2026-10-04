package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/agent"
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
// run is graded by the judge, and a grade still pending is NeedsGrading's.
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

// judge is Judge without its check that the run was graded.
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
		env.progress("  judge: %s, $%.2f", Describe(v), v.CostUSD)
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
	call = env.countSpend(call, priorCost)
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

// countSpend has call report each call's cost as it lands to env.judgeSpent, with priorUSD before it, so a crash loses
// none of it: an interrupted call's too, which the reply reports with the error (judge.Cost); call itself without
// judgeSpent.
func (env Env) countSpend(call judge.Caller, priorUSD float64) judge.Caller {
	if env.judgeSpent == nil {
		return call
	}
	spent := 0.0
	return func(ctx context.Context, prompt string) (judge.Reply, error) {
		reply, err := call(ctx, prompt)
		if cost := judge.Cost(reply); cost > 0 && (err == nil || ctx.Err() != nil) { // as judge.Judge counts it
			spent += cost
			env.judgeSpent(priorUSD + spent)
		}
		return reply, err
	}
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

// MaxGradeErrors is how many grading attempts of one judge-graded run may end in an error that leaves its grade open
// (a call that could not be made, timeouts, error results, the call cap) before the grade is final: the run is left
// ungraded (Record.Ungraded), so an outage cannot loop. A usage limit or an interrupt does not count: a resume lifts it.
const MaxGradeErrors = 3

// NeedsGrading reports whether a judge-graded run's grade is pending: a fair run (the agent's own attempt) the judge has
// neither graded nor left ungraded for good. An experiment grades it again from its stored records (Env.Regrade), never
// by running the agent again; until then no analysis counts it, and a seq-v1 look waits for it.
func NeedsGrading(rec Record) bool {
	if rec.GradedBy != task.GradingJudge || rec.Passed != nil || rec.Ungraded != "" {
		return false
	}
	switch rec.Outcome {
	case agent.OutcomeOK, agent.OutcomeCapped, agent.OutcomeTimeout:
		return true
	}
	return false
}

// The notes a grading attempt leaves; each attempt replaces the last's.
const (
	notePending  = "not graded yet: "
	noteUngraded = "not graded: "
)

// gradingSettings is the judge that grades spec's run: the experiment's (spec.JudgeGrading) or judge.GradingSettings.
func gradingSettings(spec Spec) judge.Settings {
	if spec.JudgeGrading != nil {
		return spec.JudgeGrading.WithDefaults()
	}
	return judge.GradingSettings()
}

// pendingRecord is rec as stored while the judge grades it: pending (fair, not graded), its verdict so far with the
// spend usd and stopped, so that if Agentium dies, recovery stores a run whose grade a resume continues.
func pendingRecord(rec Record, s judge.Settings, usd float64) Record {
	p := rec
	p.Passed = nil
	v := judge.ContinueFrom(rec.Judge, s)
	v.CostUSD, v.Stopped = usd, judge.StoppedCall
	v.Errors = append(v.Errors, "Agentium stopped while the judge graded the run")
	p.Judge = &v
	p.Notes = append(slices.DeleteFunc(slices.Clone(rec.Notes), isGradeNote), notePending+"Agentium stopped while the judge graded it (graded again from its change, without running the agent again)")
	return p
}

func isGradeNote(n string) bool {
	return strings.HasPrefix(n, notePending) || strings.HasPrefix(n, noteUngraded)
}

// gradeByJudge grades a judge-graded task's run (spec.Task.JudgeGraded) once grade has measured its change: the judge
// compares the run's agent.diff with the reference solution's code diff (never tests: the task has none) and its
// majority of spec.JudgeGrading's repeats (judge.GradingSettings by default) decides (judge.Grade). The run's outcome is
// the agent's and stays so: what the judge does decides only Passed (gradeAttempt).
//
// persist stores a record as the run's finished start file. Grading can take repeats × judge.CallTimeout, so the
// records are redacted and the run persisted first as pending, and again after each call that reports a cost: if
// Agentium dies while grading, recovery stores the run with its grade pending and what the judge spent so far, which an
// experiment's resume grades again from its records. The error is persist's or the redaction's, before any call.
func (env Env) gradeByJudge(ctx context.Context, spec Spec, rec *Record, persist func(Record) error) error {
	s := gradingSettings(spec)
	if err := env.redactRecords(rec.RecordsDir); err != nil {
		return err
	}
	rec.Finished = env.Now().UTC() // the deferred write sets it again once graded
	if err := persist(pendingRecord(*rec, s, rec.Spend().JudgeUSD)); err != nil {
		return err
	}
	grading := env
	grading.judgeSpent = func(usd float64) {
		p := pendingRecord(*rec, s, usd)
		p.Finished = env.Now().UTC()
		_ = persist(p) // best effort: the final write follows, and a failure only risks this spend if Agentium also dies
	}
	env.step(StepJudgeGrading)
	grading.gradeAttempt(ctx, spec, s, rec)
	return nil
}

// Regrade grades again a judge-graded run whose grade is pending (NeedsGrading) from its stored records: the task's
// reference and the run's agent.diff. The agent never runs again. The attempt continues the run's verdict
// (judge.GradeRun: answers given stand, only unsettled repeats are asked) and keeps its spend. persist, when set, stores
// the record, still pending, as each call's cost lands, so a crash loses none of it; the caller stores the result. A run
// that is not pending is left as it is.
func (env Env) Regrade(ctx context.Context, spec Spec, rec *Record, persist func(Record) error) {
	if !NeedsGrading(*rec) {
		return
	}
	s := gradingSettings(spec)
	if persist != nil {
		env.judgeSpent = func(usd float64) {
			_ = persist(pendingRecord(*rec, s, usd)) // best effort, as gradeByJudge's
		}
	}
	env.gradeAttempt(ctx, spec, s, rec)
}

// gradeAttempt makes one grading attempt on rec, continuing its verdict (judge.ContinueFrom), and settles what it
// leaves (settleGrade). The calls start in an empty folder in the run's records, removed afterwards, as Judge's do.
func (env Env) gradeAttempt(ctx context.Context, spec Spec, s judge.Settings, rec *Record) {
	rec.Passed = nil
	rec.Notes = slices.DeleteFunc(rec.Notes, isGradeNote)
	start := judge.ContinueFrom(rec.Judge, s)
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
		rec.Judge = &v
		env.progress("  judge (grading): %s, $%.2f", Describe(v), v.CostUSD)
	}
	stopped := func(err error) { // may pass: the grade stays open
		v := start
		v.Stopped, v.Errors = judge.StoppedCall, append(slices.Clone(v.Errors), err.Error())
		if ctx.Err() != nil {
			v.Errors[len(v.Errors)-1] = "interrupted: " + err.Error()
		}
		keep(v)
	}
	final := func(err error) { // never passes: the run is left ungraded
		v := start
		v.Errors = append(slices.Clone(v.Errors), err.Error())
		keep(v)
		rec.Ungraded = err.Error()
	}
	defer env.settleGrade(ctx, rec)
	if !HasReferenceCode(spec.Task) {
		rec.Ungraded = "the task's reference solution changes no code, so the judge has nothing to compare the run with"
		return
	}
	reference, err := judge.ReferenceDiff(ctx, env.Bare, spec.Task.Base, spec.Task.Solution, spec.Task.Reference)
	if err != nil {
		if ctx.Err() != nil {
			stopped(err)
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
		stopped(fmt.Errorf("judge folder: %w", err))
		return
	}
	defer os.RemoveAll(dir)
	j := claude.Judgement{CLI: env.CLI, Dir: dir, SignIn: env.SignIn, Secret: env.Secret, Home: env.Home}
	if env.SignIn != claude.SignInLogin {
		j.ConfigDir = filepath.Join(dir, "config")
		if err := os.MkdirAll(j.ConfigDir, 0o700); err != nil {
			stopped(fmt.Errorf("judge folder: %w", err))
			return
		}
	}
	call, err := judge.GradingCaller(s, j, env.Environ, 0)
	if err != nil {
		stopped(err)
		return
	}
	call = env.countSpend(call, start.CostUSD)
	v, err := judge.GradeRun(ctx, judge.Input{Instruction: spec.Instruction, Reference: reference, Candidate: string(candidate)}, s, call, &start)
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

// settleGrade reads what a grading attempt left in rec:
//   - a majority grades the run (Passed);
//   - a verdict no longer open (a tie, refusals or malformed replies that leave no majority within reach: judge.Open),
//     or an attempt that can never pass (gradeAttempt's final), leaves it ungraded for good: Ungraded says why, and
//     the analysis leaves it out, never as a fail or a pass, and nothing tries it again;
//   - anything else leaves the grade pending for an experiment to grade again (Regrade): a usage limit or an
//     interrupt as it is, and an error counted in GradeErrors, up to MaxGradeErrors, after which it is ungraded too.
func (env Env) settleGrade(ctx context.Context, rec *Record) {
	ungraded := func(why string) {
		rec.Ungraded = why
		note := noteUngraded + why + " (left out of the analysis, not tried again)"
		rec.Notes = append(rec.Notes, note)
		env.progress("  %s", env.Style.Warn("warning: "+note))
	}
	if rec.Ungraded != "" {
		ungraded(rec.Ungraded)
		return
	}
	if rec.Judge == nil {
		ungraded("the judge gave no verdict")
		return
	}
	passed, ok, why := judge.Grade(*rec.Judge)
	switch {
	case ok:
		rec.Passed = &passed
		env.progress("  graded by the judge (unvalidated): %s", env.Style.Status(map[bool]string{true: "passed", false: "failed"}[passed]))
		return
	case !judge.Open(*rec.Judge):
		ungraded(why)
		return
	case ctx.Err() == nil && rec.Judge.Stopped != judge.StoppedLimit:
		rec.GradeErrors++
		if rec.GradeErrors >= MaxGradeErrors {
			ungraded(fmt.Sprintf("%s; %d grading attempts ended in errors", why, rec.GradeErrors))
			return
		}
	}
	note := notePending + why + " (pending: an experiment grades it again from the run's change, and the agent does not run again; a run outside an experiment stays ungraded)"
	rec.Notes = append(rec.Notes, note)
	env.progress("  %s", env.Style.Warn("warning: "+note))
}

// GradeWords is a judge-graded run's grade in a few words, for the run's own lines: "fixed (4 of 5)", "not fixed (3 of
// 5 said partly or no)", "grade pending (…)" while an experiment may still grade it, or "not graded (why)" for a run
// left ungraded; "" for a run graded by tests. Callers say it is the judge's.
func GradeWords(rec Record) string {
	if rec.GradedBy != task.GradingJudge {
		return ""
	}
	switch {
	case rec.Ungraded != "":
		return "not graded (" + rec.Ungraded + ")"
	case NeedsGrading(rec):
		if rec.Judge != nil {
			if _, ok, why := judge.Grade(*rec.Judge); !ok {
				return "grade pending (" + why + ")"
			}
		}
		return "grade pending"
	case rec.Judge == nil:
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
