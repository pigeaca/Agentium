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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// Why a judgement stopped early (Verdict.Stopped). StoppedLimit should pause what is judging: the next calls would fail.
const (
	StoppedLimit = "limit" // a usage limit, sign-in, billing or a refused model: every next call would hit it too
	StoppedCall  = "call"  // a call could not be made at all (the CLI did not start, a folder could not be created)
)

// Version numbers the judge's protocol (prompts, schema, filters, rules) in stored verdicts. Change it with them.
const Version = 1

// Settings are an experiment's judge settings.
type Settings struct {
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
	Repeats int    `json:"repeats"`
}

// WithDefaults fills what s leaves out with the pilot's settings.
func (s Settings) WithDefaults() Settings {
	if s.Model == "" {
		s.Model = DefaultModel
	}
	if s.Effort == "" {
		s.Effort = DefaultEffort
	}
	if s.Repeats < 1 {
		s.Repeats = DefaultRepeats
	}
	return s
}

// Input is what one run's judgement reads.
type Input struct {
	Instruction string
	Reference   string // the reference solution's diff (ReferenceDiff)
	Candidate   string // the run's agent.diff
}

// Verdict is a run's judgement: the majority of its repeats' answers. Its texts (reasons, errors) are Claude Code's
// and may name paths: scrub them before they are shared.
type Verdict struct {
	Version int `json:"version"`
	// Fixed is the majority answer, Partly when the answers have no majority, and empty when no repeat answered.
	Fixed string `json:"fixed,omitempty"`
	// Answers and Reasons are the repeats that answered, in order; Requested is how many repeats were asked for.
	Answers   []string `json:"answers"`
	Reasons   []string `json:"reasons"`
	Requested int      `json:"requested"`
	// Reason is the reason given with the first answer that matches Fixed; empty when none does (the answers
	// disagreed, so Fixed is Partly by rule).
	Reason  string  `json:"reason,omitempty"`
	Model   string  `json:"model"`
	Effort  string  `json:"effort,omitempty"`
	CostUSD float64 `json:"cost_usd"` // every call's that reported one, answered or not; a timed-out call may not have
	// Errors are the calls that brought no valid answer, in order.
	Errors []string `json:"errors,omitempty"`
	// Stopped says why the judgement ended before its repeats did: a usage limit or a sign-in failure, which the next
	// calls would hit too, or a call that could not be made.
	Stopped string `json:"stopped,omitempty"` // StoppedLimit or StoppedCall
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

// filePath reads the path a "diff --git" header names, whatever the user's diff settings: "a/X b/X", no prefixes
// (diff.noprefix), other one-letter prefixes (diff.mnemonicPrefix), and C-quoted names (core.quotePath). Both sides
// must name the same file, which is what tells where an unquoted name with spaces splits. A rename ("a/old b/new", which
// a user's diff.renames can put in agent.diff) cannot be read: ok is false, and CodeOnly keeps the file.
func filePath(header string) (string, bool) {
	rest := strings.TrimPrefix(strings.TrimRight(header, "\r\n"), "diff --git ")
	same := func(src, dst string) (string, bool) {
		if src == dst {
			return dst, true
		}
		if len(src) > 2 && len(dst) > 2 && src[1] == '/' && dst[1] == '/' && src[2:] == dst[2:] {
			return dst[2:], true
		}
		return "", false
	}
	if strings.HasPrefix(rest, `"`) {
		end := closingQuote(rest)
		if end < 0 || end+2 >= len(rest) || rest[end+1] != ' ' {
			return "", false
		}
		src, err1 := strconv.Unquote(rest[:end+1])
		dst, err2 := strconv.Unquote(rest[end+2:])
		if err1 != nil || err2 != nil {
			return "", false
		}
		return same(src, dst)
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == ' ' {
			if p, ok := same(rest[:i], rest[i+1:]); ok {
				return p, true
			}
		}
	}
	return "", false
}

// closingQuote is the index of the quote that closes the C-quoted string s starts with, or -1.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// CodeOnly is diff without test files (task.IsTestFile) and documents (claudectx.IsDocument): what the task's behavior
// depends on. Neither a missing nor an extra plan or README update decides a verdict, and the hidden tests stay hidden.
// Every "diff --git" line starts a file; text before the first is dropped. A file whose header cannot be read is kept:
// the reference is already filtered by path, and a candidate's own tests are the agent's work, so keeping is the safe
// side. A diff with no header at all is kept whole.
func CodeOnly(diff string) string {
	if !strings.HasPrefix(diff, "diff --git ") && !strings.Contains(diff, "\ndiff --git ") {
		return diff
	}
	var out strings.Builder
	keep := false
	for _, line := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			p, ok := filePath(line)
			keep = !ok || (!task.IsTestFile(p) && !claudectx.IsDocument(p))
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
	kindInfra     = "infra"     // no answer came: not JSON, an error result, a failed exit, a timeout
	kindMalformed = "malformed" // an answer came, without a valid verdict
)

type answer struct {
	fixed, reason string
	cost          float64
	err, kind     string
}

var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

// stopText matches errors that every next call would hit as well: usage limits, sign-in and billing, a refused model.
// Other errors (an overload, a timeout, a transport failure) leave one repeat out and the judgement goes on.
var stopText = regexp.MustCompile(`(?i)usage limit|session limit|weekly limit|limit reached|hit your .{0,20}limit|rate limit|authenticat|not logged in|/login|oauth|credit balance|api key|does not support the model|model.{0,40}not (found|available)`)

// parse reads Claude Code's JSON result of a single judgement: the structured verdict, or one written as JSON in the text.
func parse(r Reply) answer { return parseField(r, "fixed", []string{Yes, Partly, No}) }

// parseField reads the answer in the schema's field, which must be one of allowed (answer.fixed holds it).
func parseField(r Reply, field string, allowed []string) answer {
	var out struct {
		IsError          bool            `json:"is_error"`
		Result           json.RawMessage `json:"result"`
		TotalCostUSD     float64         `json:"total_cost_usd"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	notJSON := json.Unmarshal(r.Stdout, &out) != nil
	if r.TimedOut { // interrupted, Claude Code may still have reported its cost
		return answer{cost: out.TotalCostUSD, err: "timed out", kind: kindInfra}
	}
	if notJSON {
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
	var v map[string]any
	if json.Unmarshal(out.StructuredOutput, &v) != nil || v[field] == nil {
		v = nil
		if m := jsonObject.FindString(result); m != "" {
			_ = json.Unmarshal([]byte(m), &v)
		}
	}
	fixed, _ := v[field].(string)
	if !slices.Contains(allowed, fixed) {
		return answer{cost: out.TotalCostUSD, err: "no valid verdict in: " + short(result), kind: kindMalformed}
	}
	if r.ExitCode != 0 {
		return answer{cost: out.TotalCostUSD, err: fmt.Sprintf("exit %d: %s", r.ExitCode, short(r.Stderr)), kind: kindInfra}
	}
	reason, _ := v["reason"].(string)
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

// Judge asks the judge s.Repeats times (s.WithDefaults) and takes the majority.
//   - A reply without a valid verdict is asked once more; if that fails too, the repeat is left out.
//   - A call that brings no answer leaves its repeat out. When its error is one every next call would hit (stopText: a
//     usage limit, sign-in, billing, a refused model) or the call could not be made at all, the judgement stops there
//     (Verdict.Stopped), keeping the answers so far.
//
// A candidate with no code is not judged (Verdict.Empty). An input without a reference diff is an error, as is a
// cancelled ctx; then the returned Verdict still holds what was spent, which callers must count.
func Judge(ctx context.Context, in Input, s Settings, call Caller) (Verdict, error) {
	s = s.WithDefaults()
	v := Verdict{Version: Version, Answers: []string{}, Reasons: []string{}, Requested: s.Repeats, Model: s.Model, Effort: s.Effort}
	if strings.TrimSpace(CodeOnly(in.Reference)) == "" {
		return v, errors.New("judge: the reference changes no code, so there is nothing to judge against")
	}
	if strings.TrimSpace(CodeOnly(in.Candidate)) == "" {
		v.Empty = true
		return v, nil
	}
	text, truncated := Prompt(in)
	v.Truncated = truncated
	for r := 0; r < s.Repeats && v.Stopped == ""; r++ {
		for attempt := 0; attempt < 2; attempt++ {
			reply, err := call(ctx, text)
			if ctx.Err() != nil {
				return v, ctx.Err()
			}
			if err != nil {
				v.Errors = append(v.Errors, err.Error())
				v.Stopped = StoppedCall
				break
			}
			a := parse(reply)
			v.CostUSD += a.cost
			if a.kind == "" {
				v.Answers, v.Reasons = append(v.Answers, a.fixed), append(v.Reasons, a.reason)
				break
			}
			v.Errors = append(v.Errors, a.err)
			if a.kind == kindInfra {
				if stopText.MatchString(a.err) {
					v.Stopped = StoppedLimit
				}
				break
			}
		}
	}
	v.Fixed = Majority(v.Answers)
	for i, a := range v.Answers {
		if a == v.Fixed {
			v.Reason = v.Reasons[i]
			break
		}
	}
	return v, nil
}

// ClaudeCaller makes each call through Claude Code (claude.RunJudgement) with s's model and effort and the judge's
// system prompt and schema, in a fresh empty folder under j.Dir that is removed afterwards. j.Dir must be a folder of
// Agentium's own (in its data folder) with no instruction file above it, which Claude Code would load into the judge's
// context. timeout bounds each call (CallTimeout when zero).
func ClaudeCaller(s Settings, j claude.Judgement, environ []string, timeout time.Duration) (Caller, error) {
	return newCaller(s, j, environ, timeout, Schema)
}

// PairCaller is ClaudeCaller with the pair schema (PairSchema), for JudgePair.
func PairCaller(s Settings, j claude.Judgement, environ []string, timeout time.Duration) (Caller, error) {
	return newCaller(s, j, environ, timeout, PairSchema)
}

func newCaller(s Settings, j claude.Judgement, environ []string, timeout time.Duration, schema string) (Caller, error) {
	if above := instructionFilesAbove(filepath.Join(j.Dir, "call")); len(above) > 0 { // the calls start one level below
		return nil, fmt.Errorf("judge: Claude Code would load %s above the judge's folder", strings.Join(above, ", "))
	}
	s = s.WithDefaults()
	j.Model, j.Effort, j.SystemPrompt, j.Schema = s.Model, s.Effort, SystemPrompt, schema
	if j.BudgetUSD == 0 {
		j.BudgetUSD = CallCapUSD
	}
	if timeout <= 0 {
		timeout = CallTimeout
	}
	base := j.Dir
	return func(ctx context.Context, prompt string) (Reply, error) {
		dir, err := os.MkdirTemp(base, "call-")
		if err != nil {
			return Reply{}, fmt.Errorf("judge folder: %w", err)
		}
		defer os.RemoveAll(dir)
		files := make([]*os.File, 2) // stdout, stderr: files, not pipes (claude.RunJudgement)
		for i, pattern := range []string{"out-*.json", "err-*.txt"} {
			if files[i], err = os.CreateTemp(base, pattern); err != nil {
				return Reply{}, fmt.Errorf("judge output: %w", err)
			}
			defer os.Remove(files[i].Name())
			defer files[i].Close()
		}
		call := j
		call.Dir = dir
		result, err := claude.RunJudgement(ctx, call, prompt, environ, files[0], files[1], timeout, CallGrace)
		if err != nil {
			return Reply{}, err
		}
		stdout, err1 := os.ReadFile(files[0].Name())
		stderr, err2 := os.ReadFile(files[1].Name())
		if err := errors.Join(err1, err2); err != nil {
			return Reply{}, fmt.Errorf("judge output: %w", err)
		}
		return Reply{Stdout: stdout, Stderr: strings.TrimSpace(string(stderr)), ExitCode: result.ExitCode, TimedOut: result.TimedOut}, nil
	}, nil
}

// instructionFilesAbove lists the instruction files Claude Code would load from the folders above dir (as
// internal/run's guard for runs does).
func instructionFilesAbove(dir string) []string {
	var found []string
	for d := filepath.Dir(dir); d != filepath.Dir(d); d = filepath.Dir(d) {
		for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"} {
			if info, err := os.Stat(filepath.Join(d, name)); err == nil && !info.IsDir() {
				found = append(found, filepath.Join(d, name))
			}
		}
	}
	return found
}

// ReferenceDiff is the reference solution's change to its code: git diff from base to solution in the bare repository,
// over the task's reference files that are neither tests nor documents. The user's git settings are ignored, as in the
// pilot (GIT_CONFIG_GLOBAL, which git 2.32 and newer read; gitx already drops the system's), and prefixes are pinned, so the diff the judge reads does
// not depend on whose machine made it.
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
	out, err := gitx.OutputEnv(ctx, []string{"GIT_CONFIG_GLOBAL=" + os.DevNull}, nil, args...)
	if err != nil {
		return "", fmt.Errorf("reference diff: %w", err)
	}
	return string(out), nil
}
