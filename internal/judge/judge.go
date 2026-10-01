// Package judge asks a reference-guided LLM judge whether a run's change does what its task asks: fixed ("yes"),
// "partly" or "no", with a one-line reason. The judge sees the task's instruction, the reference solution's code diff and
// the agent's code diff, never the tests or their result. Its verdict is a second opinion shown next to the tests; it
// decides nothing (.agents/decisions/2026-10-01-llm-judge-alongside-tests.md).
//
// The prompts, the schema, the code-only filter, the cut and the majority rule are the judge pilot's, unchanged
// (docs/research/judge-pilot/protocol.md), so the pilot's figures on noise and cost apply.
package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/task"
)

// Verdicts.
const (
	Yes    = "yes"
	Partly = "partly"
	No     = "no"
)

// Defaults, the judge pilot's settings.
const (
	DefaultModel   = "claude-opus-5-5"
	DefaultEffort  = "high"
	DefaultRepeats = 3
	// MaxDiffChars cuts each diff (in characters) so one huge change cannot make a call fail or cost a lot.
	MaxDiffChars = 40000
	// CallCapUSD is each call's --max-budget-usd. The pilot's calls cost $0.03–0.33 (the dearest wrote the prompt cache).
	CallCapUSD = 1.0
	// EstimateUSD is the pilot's mean cost of one call (176 calls on claude-opus-5-5 at high effort): previews state it as
	// that, not as a measure of this project.
	EstimateUSD = 0.065
	// CallTimeout bounds one call; CallGrace is how long it may finish after an interrupt.
	CallTimeout = 10 * time.Minute
	CallGrace   = 10 * time.Second
)

// SystemPrompt replaces Claude Code's own system prompt for the judge.
const SystemPrompt = "You review code changes for a coding task against a reference change a developer made. You see diffs only: you " +
	"cannot run code, and you do not know whether any change passes tests. Judge behavior, not style: a different " +
	"approach is fine when it achieves the same result. Reply with the JSON the schema asks for, and nothing else."

// promptTemplate takes the instruction, the reference diff and the candidate diff, in that order.
const promptTemplate = `Task instruction:
<instruction>
%s
</instruction>

Reference change (the developer's fix, test files left out):
<reference>
%s
</reference>

Candidate change (test files left out):
<candidate>
%s
</candidate>

Does the candidate change do what the task asks, as the reference does? Answer "yes" if it fully does, "partly" if it
does only some of it or only for some inputs, and "no" if it does not, or only works around what checks it. Give the
reason in one sentence.`

// Schema is the answer's JSON schema, byte for byte the pilot's.
const Schema = `{"type": "object", "properties": {"fixed": {"type": "string", "enum": ["yes", "partly", "no"]}, "reason": {"type": "string"}}, "required": ["fixed", "reason"], "additionalProperties": false}`

// Settings are an experiment's judge settings.
type Settings struct {
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
	Repeats int    `json:"repeats"`
}

// Input is what one run's judgement reads.
type Input struct {
	Instruction string
	Reference   string // the reference solution's diff (ReferenceDiff)
	Candidate   string // the run's agent.diff
}

// Verdict is a run's judgement: the majority of its repeats' answers.
type Verdict struct {
	// Fixed is the majority answer, Partly when the answers have no majority, and empty when no repeat answered.
	Fixed   string   `json:"fixed,omitempty"`
	Answers []string `json:"answers"`
	// Reason is the reason given with the first answer that matches Fixed (or the first answer, with no majority).
	Reason  string  `json:"reason,omitempty"`
	Model   string  `json:"model"`
	Effort  string  `json:"effort,omitempty"`
	CostUSD float64 `json:"cost_usd"` // every call's, answered or not
	// Errors are the calls that brought no valid answer, in order.
	Errors []string `json:"errors,omitempty"`
	// Truncated: a diff was cut at MaxDiffChars, so the judge did not see all of it.
	Truncated bool `json:"truncated,omitempty"`
	// Empty: the candidate changed no code (only tests or documents, or nothing); the judge was not asked.
	Empty bool `json:"empty,omitempty"`
}

// Reply is how one call ended.
type Reply struct {
	Stdout   []byte // Claude Code's JSON result
	Stderr   string
	ExitCode int
	TimedOut bool
}

// Caller makes one judge call. An error means the call could not be made at all, or ctx was cancelled.
type Caller func(ctx context.Context, prompt string) (Reply, error)

var diffHeader = regexp.MustCompile(`^diff --git a/(.*) b/(.*)$`)

// CodeOnly is diff without test files (task.IsTestFile) and documents (claudectx.IsDocument): what the task's behavior
// depends on. Neither a missing nor an extra plan or README update decides a verdict, and the hidden tests stay hidden.
// A file's path is the "b/" side of its "diff --git a/X b/Y" line; text before the first such line is dropped.
func CodeOnly(diff string) string {
	var out strings.Builder
	keep := false
	for _, line := range strings.SplitAfter(diff, "\n") {
		if m := diffHeader.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
			keep = !task.IsTestFile(m[2]) && !claudectx.IsDocument(m[2])
		}
		if keep {
			out.WriteString(line)
		}
	}
	return out.String()
}

// clip cuts diff at MaxDiffChars characters, saying so in the text.
func clip(diff string) (string, bool) {
	if utf8.RuneCountInString(diff) <= MaxDiffChars {
		return diff, false
	}
	n := 0
	for i := range diff {
		if n == MaxDiffChars {
			return diff[:i] + fmt.Sprintf("\n[... diff cut at %d characters ...]\n", MaxDiffChars), true
		}
		n++
	}
	return diff, false
}

// Prompt is the judge's prompt for in, and whether a diff was cut.
func Prompt(in Input) (string, bool) {
	ref, cutRef := clip(CodeOnly(in.Reference))
	cand, cutCand := clip(CodeOnly(in.Candidate))
	return fmt.Sprintf(promptTemplate, in.Instruction, ref, cand), cutRef || cutCand
}

// Error kinds of a reply without a valid answer.
const (
	kindInfra     = "infra"     // no answer came: not JSON, an error result (a refused model, a usage limit), a timeout
	kindMalformed = "malformed" // an answer came, without a valid verdict
)

type answer struct {
	fixed, reason string
	cost          float64
	err, kind     string
}

var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

// parse reads Claude Code's JSON result: the structured verdict, or one written as JSON in the text.
func parse(r Reply) answer {
	if r.TimedOut {
		return answer{err: fmt.Sprintf("timed out after %s (its cost is unknown)", CallTimeout), kind: kindInfra}
	}
	var out struct {
		IsError          bool            `json:"is_error"`
		Result           json.RawMessage `json:"result"`
		TotalCostUSD     float64         `json:"total_cost_usd"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(r.Stdout, &out); err != nil {
		text := strings.TrimSpace(string(r.Stdout))
		if text == "" {
			text = r.Stderr
		}
		return answer{err: fmt.Sprintf("exit %d, not JSON: %s", r.ExitCode, short(text)), kind: kindInfra}
	}
	var result string
	if json.Unmarshal(out.Result, &result) != nil {
		result = string(out.Result)
	}
	if out.IsError {
		return answer{cost: out.TotalCostUSD, err: "error result: " + short(result), kind: kindInfra}
	}
	var v struct {
		Fixed  any `json:"fixed"`
		Reason any `json:"reason"`
	}
	if json.Unmarshal(out.StructuredOutput, &v) != nil || v.Fixed == nil {
		v.Fixed, v.Reason = nil, nil
		if m := jsonObject.FindString(result); m != "" {
			_ = json.Unmarshal([]byte(m), &v)
		}
	}
	fixed, _ := v.Fixed.(string)
	if fixed != Yes && fixed != Partly && fixed != No {
		return answer{cost: out.TotalCostUSD, err: "no valid verdict in: " + short(result), kind: kindMalformed}
	}
	if r.ExitCode != 0 {
		return answer{cost: out.TotalCostUSD, err: fmt.Sprintf("exit %d: %s", r.ExitCode, short(r.Stderr)), kind: kindInfra}
	}
	reason, _ := v.Reason.(string)
	return answer{fixed: fixed, reason: reason, cost: out.TotalCostUSD}
}

func short(s string) string {
	if utf8.RuneCountInString(s) <= 200 {
		return s
	}
	return string([]rune(s)[:200]) + "…"
}

// Majority is the most common of answers when it is more than half of them, Partly when none is, and empty for none.
func Majority(answers []string) string {
	counts := map[string]int{}
	for _, a := range answers {
		counts[a]++
	}
	for _, a := range answers {
		if counts[a]*2 > len(answers) {
			return a
		}
	}
	if len(answers) == 0 {
		return ""
	}
	return Partly
}

// Judge asks the judge s.Repeats times and takes the majority. A reply without a valid verdict is asked once more; a
// call that brings no answer (an error result such as a usage limit, a timeout, a failed start) ends the judgement,
// since the next calls would most likely fail alike, and the verdict is the majority of the answers so far. Failures
// are in Verdict.Errors; the only error returned is ctx's.
func Judge(ctx context.Context, in Input, s Settings, call Caller) (Verdict, error) {
	repeats := s.Repeats
	if repeats < 1 {
		repeats = DefaultRepeats
	}
	v := Verdict{Answers: []string{}, Model: s.Model, Effort: s.Effort}
	if strings.TrimSpace(CodeOnly(in.Candidate)) == "" {
		v.Empty = true
		return v, nil
	}
	text, truncated := Prompt(in)
	v.Truncated = truncated
	var reasons []string
	stop := false
	for r := 0; r < repeats && !stop; r++ {
		for attempt := 0; attempt < 2; attempt++ {
			reply, err := call(ctx, text)
			if ctx.Err() != nil {
				return v, ctx.Err()
			}
			if err != nil {
				v.Errors, stop = append(v.Errors, err.Error()), true
				break
			}
			a := parse(reply)
			v.CostUSD += a.cost
			if a.kind == "" {
				v.Answers, reasons = append(v.Answers, a.fixed), append(reasons, a.reason)
				break
			}
			v.Errors = append(v.Errors, a.err)
			if a.kind == kindInfra {
				stop = true
				break
			}
		}
	}
	v.Fixed = Majority(v.Answers)
	for i, a := range v.Answers {
		if a == v.Fixed {
			v.Reason = reasons[i]
			break
		}
	}
	if v.Reason == "" && len(reasons) > 0 { // no majority: the first answer's reason
		v.Reason = reasons[0]
	}
	return v, nil
}

// ClaudeCaller makes each call through Claude Code (claude.RunJudgement) with the judge's system prompt and schema, in
// a fresh empty folder under j.Dir, removed afterwards. j.Dir must be a folder of Agentium's own (in its data folder).
func ClaudeCaller(j claude.Judgement, environ []string) Caller {
	j.SystemPrompt, j.Schema = SystemPrompt, Schema
	if j.BudgetUSD == 0 {
		j.BudgetUSD = CallCapUSD
	}
	base := j.Dir
	return func(ctx context.Context, prompt string) (Reply, error) {
		dir, err := os.MkdirTemp(base, "call-")
		if err != nil {
			return Reply{}, fmt.Errorf("judge folder: %w", err)
		}
		defer os.RemoveAll(dir)
		out, err := os.CreateTemp(base, "out-*.json")
		if err != nil {
			return Reply{}, fmt.Errorf("judge output: %w", err)
		}
		defer os.Remove(out.Name())
		defer out.Close()
		call := j
		call.Dir = dir
		stderr, result, err := claude.RunJudgement(ctx, call, prompt, environ, out, CallTimeout, CallGrace)
		if err != nil {
			return Reply{}, err
		}
		stdout, err := os.ReadFile(out.Name())
		if err != nil {
			return Reply{}, fmt.Errorf("judge output: %w", err)
		}
		return Reply{Stdout: stdout, Stderr: stderr, ExitCode: result.ExitCode, TimedOut: result.TimedOut}, nil
	}
}

// ReferenceDiff is the reference solution's change to its code: git diff from base to solution in the bare repository,
// over the task's reference files that are neither tests nor documents. Prefixes are pinned ("a/", "b/") whatever the
// user's diff settings, so CodeOnly can split it.
func ReferenceDiff(ctx context.Context, bare, base, solution string, reference []string) (string, error) {
	var specs []string
	for _, p := range reference {
		if !task.IsTestFile(p) && !claudectx.IsDocument(p) {
			specs = append(specs, ":(literal)"+p)
		}
	}
	if len(specs) == 0 {
		return "", errors.New("the task's reference changes no code: nothing to judge against")
	}
	args := append([]string{"--git-dir", bare, "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false", "diff", "--no-ext-diff",
		"--no-textconv", "--no-color", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", base, solution, "--"}, specs...)
	out, err := gitx.Output(ctx, nil, args...)
	if err != nil {
		return "", fmt.Errorf("reference diff: %w", err)
	}
	return string(out), nil
}
