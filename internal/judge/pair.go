package judge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/pigeaca/agentium/internal/stats"
)

// The pair judge asks which of two changes is the better fix, in both orders, so the judge's liking for whichever comes
// first cancels out. Its prompt and schema are the pilot's, unchanged (docs/research/judge-pilot/judge_pilot.py).

// pairTemplate takes the instruction, the reference diff, then the first and the second change, in that order.
const pairTemplate = `Task instruction:
<instruction>
%s
</instruction>

Reference change (the developer's fix, test files left out):
<reference>
%s
</reference>

Change 1 (test files left out):
<first>
%s
</first>

Change 2 (test files left out):
<second>
%s
</second>

Which change is the better fix for the task: closer to doing exactly what was asked, more likely correct in cases a
test might not cover, and no broader than needed? Answer "first", "second" or "tie", and give the reason in one
sentence.`

// PairSchema is the pair answer's JSON schema, byte for byte the pilot's json.dumps(PAIR_SCHEMA).
const PairSchema = `{"type": "object", "properties": {"prefer": {"type": "string", "enum": ["first", "second", "tie"]}, "reason": {"type": "string"}}, "required": ["prefer", "reason"], "additionalProperties": false}`

// Answers in a pair reply, relative to the positions in the prompt.
const (
	first  = "first"
	second = "second"
	tie    = "tie"
)

// Preferences of a PairVerdict, relative to the arms.
const (
	PreferA   = "A"
	PreferB   = "B"
	PreferTie = "tie"
)

// MinPreferences is the plan's floor: with fewer pairs that prefer an arm, a summary says too little (Enough).
const MinPreferences = 5

// PairVersion numbers the pair judge's protocol (prompt, schema, rules) in stored pair verdicts, apart from Version.
const PairVersion = 1

// PairInput is what one pair's judgement reads: the changes of arm A and arm B on the same task.
type PairInput struct {
	Instruction string
	Reference   string
	A, B        string // the arms' agent.diff
}

// PairOrder is one of the two asks. Order "AB" shows A first; "BA" shows B first. An order never asked (judging stopped
// or AB was lost first) is {Answered: false} with no errors; an order asked and failed always has errors.
type PairOrder struct {
	// Answered is false when no valid answer came; Answer is then empty.
	Answered bool `json:"answered"`
	// Answer is "first", "second" or "tie", as the judge said it (positions, not arms).
	Answer  string   `json:"answer,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	CostUSD float64  `json:"cost_usd"`
	Errors  []string `json:"errors,omitempty"`
}

// arm maps the answer back to the arms: "A", "B" or "tie". aFirst tells whether A was shown first.
func (o PairOrder) arm(aFirst bool) string {
	switch {
	case o.Answer == tie:
		return PreferTie
	case (o.Answer == first) == aFirst:
		return PreferA
	default:
		return PreferB
	}
}

// PairVerdict is a pair's judgement. Its texts are Claude Code's and may name paths: scrub them before sharing.
type PairVerdict struct {
	Version int `json:"version"`
	// Prefer is "A", "B" or "tie" (arms, not positions); empty when the pair is incomplete: an order is unanswered, or
	// the pair is Empty.
	Prefer string `json:"prefer,omitempty"`
	// Flip: both orders answered and disagreed once mapped back to arms; Prefer is then "tie", as in the pilot.
	Flip bool `json:"flip,omitempty"`
	// AB and BA are the asks with A first and with B first.
	AB      PairOrder `json:"ab"`
	BA      PairOrder `json:"ba"`
	Model   string    `json:"model"`
	Effort  string    `json:"effort,omitempty"`
	CostUSD float64   `json:"cost_usd"` // both orders'
	Errors  []string  `json:"errors,omitempty"`
	// Stopped: StoppedLimit or StoppedCall, as in Verdict. It happened in the last order asked; when that was AB, BA was not
	// asked.
	Stopped string `json:"stopped,omitempty"`
	// Truncated: a diff was cut at MaxDiffChars. Empty: a change had no code, so the judge was not asked.
	Truncated bool `json:"truncated,omitempty"`
	Empty     bool `json:"empty,omitempty"`
}

// Complete tells whether both orders answered, which makes Prefer meaningful.
func (v PairVerdict) Complete() bool { return v.Prefer != "" }

// PairPrompt is the pair prompt with change first shown as Change 1 and second as Change 2, and whether a diff was cut.
func PairPrompt(instruction, reference, firstChange, secondChange string) (string, bool) {
	ref, cutRef := clip(CodeOnly(reference))
	f, cutF := clip(CodeOnly(firstChange))
	s, cutS := clip(CodeOnly(secondChange))
	return fmt.Sprintf(pairTemplate, instruction, ref, f, s), cutRef || cutF || cutS
}

// JudgePair asks which of A and B is the better fix, once with A first and once with B first (s.WithDefaults; repeats
// do not apply to pairs). Each order's answer is mapped back to the arms; when the orders disagree the pair is a Flip and
// counts as a tie.
//   - A reply without a valid answer is asked once more; if that fails too, that order is unanswered.
//   - A call with no answer leaves its order unanswered. When its error is one every next call would hit (stopText) or the
//     call could not be made at all, judging stops there (PairVerdict.Stopped) and the other order is not asked.
//   - With an order unanswered there is no Prefer: the pair is incomplete. When AB ends unanswered, BA is not asked, as the
//     pair could not be completed.
//
// A change with no code is not judged (Empty). A reference without code is an error, as is a cancelled ctx; then the
// returned verdict still holds what was spent, which callers must count.
func JudgePair(ctx context.Context, in PairInput, s Settings, call Caller) (PairVerdict, error) {
	s = s.WithDefaults()
	v := PairVerdict{Version: PairVersion, Model: s.Model, Effort: s.Effort}
	if strings.TrimSpace(CodeOnly(in.Reference)) == "" {
		return v, errors.New("judge: the reference changes no code, so there is nothing to judge against")
	}
	if strings.TrimSpace(CodeOnly(in.A)) == "" || strings.TrimSpace(CodeOnly(in.B)) == "" {
		v.Empty = true
		return v, nil
	}
	ab, cutAB := PairPrompt(in.Instruction, in.Reference, in.A, in.B)
	ba, cutBA := PairPrompt(in.Instruction, in.Reference, in.B, in.A)
	v.Truncated = cutAB || cutBA
	var err error
	if v.AB, err = v.ask(ctx, ab, call); err != nil {
		return v, err
	}
	if v.Stopped == "" && v.AB.Answered { // a pair needs both orders: asking BA after a lost AB only spends money
		if v.BA, err = v.ask(ctx, ba, call); err != nil {
			return v, err
		}
	}
	if v.AB.Answered && v.BA.Answered {
		x, y := v.AB.arm(true), v.BA.arm(false)
		switch {
		case x == y:
			v.Prefer = x
		default:
			v.Prefer, v.Flip = PreferTie, true
		}
	}
	return v, nil
}

// ask makes one order's call, with one more try for a malformed reply, and adds its cost and errors to v.
func (v *PairVerdict) ask(ctx context.Context, prompt string, call Caller) (PairOrder, error) {
	var o PairOrder
	for attempt := 0; attempt < 2; attempt++ {
		reply, err := call(ctx, prompt)
		if ctx.Err() != nil {
			return o, ctx.Err()
		}
		if err != nil {
			o.Errors = append(o.Errors, err.Error())
			v.Errors = append(v.Errors, err.Error())
			v.Stopped = StoppedCall
			return o, nil
		}
		a := parseField(reply, "prefer", []string{first, second, tie})
		o.CostUSD += a.cost
		v.CostUSD += a.cost
		if a.kind == "" {
			o.Answered, o.Answer, o.Reason = true, a.fixed, a.reason
			return o, nil
		}
		o.Errors = append(o.Errors, a.err)
		v.Errors = append(v.Errors, a.err)
		if a.kind == kindInfra {
			if stopText.MatchString(a.err) {
				v.Stopped = StoppedLimit
			}
			return o, nil
		}
	}
	return o, nil
}

// PreferenceSummary counts an experiment's pair verdicts.
type PreferenceSummary struct {
	Complete   int `json:"complete"`   // pairs with both orders answered
	Incomplete int `json:"incomplete"` // pairs not Empty with an order unanswered
	Empty      int `json:"empty"`      // pairs not asked because a change had no code
	Ties       int `json:"ties"`       // complete pairs that came out a tie, flips included
	Flips      int `json:"flips"`      // complete pairs whose orders disagreed
	A          int `json:"a"`          // pairs that preferred arm A
	B          int `json:"b"`          // pairs that preferred arm B
	// BShare is B's share of the pairs with a preference (A + B), with its 95% Wilson interval; P is the exact two-sided
	// binomial p-value against an even split. With no preference, BShare and P are 0 and 1, and the interval is 0 to 1.
	BShare float64 `json:"b_share"`
	Low    float64 `json:"low"`
	High   float64 `json:"high"`
	P      float64 `json:"p"`
	// Enough: at least MinPreferences pairs prefer an arm. With fewer, the figures are too thin to read as a result.
	Enough bool `json:"enough"`
}

// Preference counts verdicts; incomplete ones (an order unanswered, or Empty) are left out.
func Preference(verdicts []PairVerdict) PreferenceSummary {
	var p PreferenceSummary
	for _, v := range verdicts {
		if v.Empty {
			p.Empty++
			continue
		}
		if !v.Complete() {
			p.Incomplete++
			continue
		}
		p.Complete++
		if v.Flip {
			p.Flips++
		}
		switch v.Prefer {
		case PreferA:
			p.A++
		case PreferB:
			p.B++
		default:
			p.Ties++
		}
	}
	n := p.A + p.B
	if n > 0 {
		p.BShare = float64(p.B) / float64(n)
	}
	p.Low, p.High = wilson(p.B, n)
	p.P = binomialTwoSided(p.B, n)
	p.Enough = n >= MinPreferences
	return p
}

// wilson is the 95% Wilson interval of k in n, clamped to 0..1, as the pilot's.
func wilson(k, n int) (float64, float64) { return stats.Wilson(k, n) }

// binomialTwoSided is the exact two-sided p-value of k successes in n at p = 0.5: the total probability of outcomes no
// likelier than k's (within the pilot's 1e-12), as the pilot's.
func binomialTwoSided(k, n int) float64 {
	if n == 0 {
		return 1
	}
	prob := func(i int) float64 {
		a, _ := math.Lgamma(float64(n + 1))
		b, _ := math.Lgamma(float64(i + 1))
		c, _ := math.Lgamma(float64(n - i + 1))
		return math.Exp(a - b - c - float64(n)*math.Ln2)
	}
	pk, sum := prob(k), 0.0
	for i := 0; i <= n; i++ {
		if p := prob(i); p <= pk+1e-12 {
			sum += p
		}
	}
	return math.Min(1, sum)
}
