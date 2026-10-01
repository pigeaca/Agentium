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
// A task the judge cannot judge (HasReferenceCode) never needs it.
func NeedsJudging(rec Record, spec task.Spec) bool {
	return rec.Passed != nil && HasReferenceCode(spec) && (rec.Judge == nil || rec.Judge.Stopped != "")
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
