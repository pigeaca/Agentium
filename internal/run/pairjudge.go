package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/judge"
)

// PairJudgement is the pair judge's comparison of a pair's two passing runs (judge.JudgePair, both orders), kept on the
// pair's arm-B run (Record.PairJudge). It is unvalidated and exploratory: it decides nothing.
type PairJudgement struct {
	// RunA is the arm-A run the holding run was compared with.
	RunA    string            `json:"run_a"`
	Verdict judge.PairVerdict `json:"verdict"`
}

// NeedsPairJudging reports whether the comparison held by b, a pair's arm-B run, is still to be made, as a resume
// decides it: there is none yet (Agentium stopped before it, say), or it stopped early (Verdict.Stopped: a usage limit,
// a call that could not be made, an interrupt), which is judged again. One that ran its course is final, answered or
// not, and so is an Empty one. Which pairs may be judged at all (both runs passing, a reference in code) is the
// experiment's to decide.
func NeedsPairJudging(b Record) bool { return b.PairJudge == nil || b.PairJudge.Verdict.Stopped != "" }

// JudgePair asks the pair judge s which of a pair's two runs, a (arm A) and b (arm B), fixed the task better, and
// returns the comparison for b.PairJudge. It reads the task's instruction, the reference solution's code diff and both
// runs' agent.diff from their records. It never changes either run's outcome, Passed or Metrics.
//   - A pair that can never be judged (a diff is missing, the reference diff fails or changes no code) gets a final,
//     cost-free comparison with the reason in Errors, so resumes do not try it again.
//   - What may pass (an interrupt, a judge folder that cannot be made, an instruction file above it) gets a comparison
//     stopped with judge.StoppedCall, which a resume judges again; an interrupt keeps what was spent.
//
// b's earlier comparison (one that stopped early, judged again) keeps its spend: the new comparison's CostUSD adds it.
// spent, when set, gets the comparison as it stands after each call that reported a cost (stopped, with everything
// spent so far), so the caller can store it before the next call: if Agentium dies while judging, none of the spend is
// lost, and the pair is judged again on resume.
//
// The calls start in an empty folder in b's records, which agents may not read, and which is removed afterwards; with an
// API key or a token, Claude Code gets a fresh config folder there too.
func (env Env) JudgePair(ctx context.Context, spec Spec, s judge.Settings, a, b Record, spent func(PairJudgement)) PairJudgement {
	s = s.WithDefaults()
	priorCost := b.Spend().PairJudgeUSD
	blank := func() PairJudgement {
		return PairJudgement{RunA: a.ID, Verdict: judge.PairVerdict{Version: judge.PairVersion, Model: s.Model, Effort: s.Effort, CostUSD: priorCost}}
	}
	final := func(err error) PairJudgement { // never judged again
		p := blank()
		p.Verdict.Errors = []string{err.Error()}
		return env.redactPair(p)
	}
	again := func(err error) PairJudgement { // judged again on resume
		p := blank()
		p.Verdict.Stopped, p.Verdict.Errors = judge.StoppedCall, []string{err.Error()}
		if ctx.Err() != nil {
			p.Verdict.Errors = []string{"interrupted: " + err.Error()}
		}
		return env.redactPair(p)
	}
	reference, err := judge.ReferenceDiff(ctx, env.Bare, spec.Task.Base, spec.Task.Solution, spec.Task.Reference)
	if err != nil {
		if ctx.Err() != nil {
			return again(err)
		}
		return final(err)
	}
	diffs := make([]string, 2)
	for i, rec := range []Record{a, b} {
		data, err := os.ReadFile(filepath.Join(rec.RecordsDir, "agent.diff"))
		if err != nil {
			return final(fmt.Errorf("run %s's diff: %w", rec.ID, err))
		}
		diffs[i] = string(data)
	}
	dir := PairJudgeDir(b)
	if err := os.RemoveAll(dir); err != nil { // one left by a comparison Agentium died in
		return again(fmt.Errorf("judge folder: %w", err))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return again(fmt.Errorf("judge folder: %w", err))
	}
	defer os.RemoveAll(dir)
	j := claude.Judgement{CLI: env.CLI, Dir: dir, SignIn: env.SignIn, Secret: env.Secret, Home: env.Home}
	if env.SignIn != claude.SignInLogin {
		j.ConfigDir = filepath.Join(dir, "config")
		if err := os.MkdirAll(j.ConfigDir, 0o700); err != nil {
			return again(fmt.Errorf("judge folder: %w", err))
		}
	}
	call, err := judge.PairCaller(s, j, env.Environ, 0)
	if err != nil {
		return again(err)
	}
	if spent != nil { // each call's reported cost as it lands, so a crash loses none of it
		inner, so := call, 0.0
		call = func(ctx context.Context, prompt string) (judge.Reply, error) {
			reply, err := inner(ctx, prompt)
			var out struct {
				CostUSD float64 `json:"total_cost_usd"`
			}
			if err == nil && json.Unmarshal(reply.Stdout, &out) == nil && out.CostUSD > 0 {
				so += out.CostUSD
				p := blank()
				p.Verdict.CostUSD += so
				p.Verdict.Stopped, p.Verdict.Errors = judge.StoppedCall, []string{"Agentium stopped while judging"}
				spent(p)
			}
			return reply, err
		}
	}
	v, err := judge.JudgePair(ctx, judge.PairInput{Instruction: spec.Instruction, Reference: reference, A: diffs[0], B: diffs[1]}, s, call)
	v.CostUSD += priorCost
	if err != nil {
		if ctx.Err() == nil { // the reference changes no code: nothing was spent
			return final(err)
		}
		v.Stopped = judge.StoppedCall
		v.Errors = append(v.Errors, "interrupted: "+err.Error())
	}
	return env.redactPair(PairJudgement{RunA: a.ID, Verdict: v})
}

// PairJudgeDir is the folder in b's records that b's comparison runs its calls in, and removes afterwards. Agentium
// dying while comparing leaves it, with a config folder that may hold the sign-in (an API key or a token): an
// experiment removes the folders of its runs before it compares again (RemovePairJudgeDir).
func PairJudgeDir(b Record) string { return filepath.Join(b.RecordsDir, "pair-judge") }

// RemovePairJudgeDir removes the folder a comparison left in b's records, if any. No comparison may be running on b.
func RemovePairJudgeDir(b Record) error {
	if b.RecordsDir == "" {
		return nil
	}
	if err := os.RemoveAll(PairJudgeDir(b)); err != nil {
		return fmt.Errorf("remove the pair judge folder of run %s: %w", b.ID, err)
	}
	return nil
}

// redactPair redacts the comparison's texts: Claude Code's may quote what it was given or its environment.
func (env Env) redactPair(p PairJudgement) PairJudgement {
	redact := func(texts []string) []string {
		out := make([]string, len(texts))
		for i, t := range texts {
			out[i] = string(Redact([]byte(t), env.Secret))
		}
		if texts == nil {
			return nil
		}
		return out
	}
	v := &p.Verdict
	v.Errors = redact(v.Errors)
	for _, o := range []*judge.PairOrder{&v.AB, &v.BA} {
		o.Reason = string(Redact([]byte(o.Reason), env.Secret))
		o.Errors = redact(o.Errors)
	}
	return p
}

// DescribePair is a comparison in a few words: which arm the judge preferred in both orders, or why there is no
// preference.
func DescribePair(v judge.PairVerdict) string {
	var text string
	switch {
	case v.Empty:
		text = "not asked (a run changed no code)"
	case v.Flip:
		text = "tie (the two orders disagreed)"
	case v.Prefer == judge.PreferTie:
		text = "tie"
	case v.Prefer != "":
		text = "prefers " + v.Prefer
	case v.Stopped == "" && len(v.Errors) > 0:
		text = "no answer: " + lastOf(v.Errors)
	default:
		text = "no answer"
	}
	switch v.Stopped {
	case judge.StoppedLimit:
		text += "; stopped at a usage limit or sign-in failure"
	case judge.StoppedCall:
		text += "; stopped: " + strings.TrimSpace(lastOf(v.Errors))
	}
	return text
}
