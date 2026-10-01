package judge

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
)

func pairResult(prefer, reason string, cost float64) Reply {
	out, _ := json.Marshal(map[string]any{"structured_output": map[string]string{"prefer": prefer, "reason": reason}, "total_cost_usd": cost, "result": ""})
	return Reply{Stdout: out}
}

// pairFake returns replies in order and records the prompts.
func pairFake(replies ...Reply) (Caller, *[]string) {
	var prompts []string
	return func(ctx context.Context, prompt string) (Reply, error) {
		prompts = append(prompts, prompt)
		if len(prompts) > len(replies) {
			return Reply{}, errors.New("too many calls")
		}
		return replies[len(prompts)-1], nil
	}, &prompts
}

const (
	diffA = "diff --git a/a.go b/a.go\n+change made by arm A\n"
	diffB = "diff --git a/b.go b/b.go\n+change made by arm B\n"
)

var pairIn = PairInput{Instruction: "do it", Reference: codeDiff, A: diffA, B: diffB}

func TestJudgePairOrdersAndMapsBack(t *testing.T) {
	cases := []struct {
		name         string
		ab, ba       string
		prefer       string
		flip         bool
		wantComplete bool
	}{
		{"B both times", "second", "first", PreferB, false, true},
		{"A both times", "first", "second", PreferA, false, true},
		{"position bias is a flip and a tie", "first", "first", PreferTie, true, true},
		{"the other position bias", "second", "second", PreferTie, true, true},
		{"tie both times is no flip", "tie", "tie", PreferTie, false, true},
		{"tie against a preference is a flip", "tie", "first", PreferTie, true, true},
	}
	for _, c := range cases {
		call, prompts := pairFake(pairResult(c.ab, "r1", 0.1), pairResult(c.ba, "r2", 0.2))
		v, err := JudgePair(context.Background(), pairIn, Settings{Repeats: 5}, call)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(*prompts) != 2 { // repeats do not apply to pairs
			t.Fatalf("%s: %d calls", c.name, len(*prompts))
		}
		firstOf := func(p string) string { return p[strings.Index(p, "<first>"):strings.Index(p, "</first>")] }
		if !strings.Contains(firstOf((*prompts)[0]), "arm A") || !strings.Contains(firstOf((*prompts)[1]), "arm B") {
			t.Errorf("%s: orders wrong:\n%s\n%s", c.name, (*prompts)[0], (*prompts)[1])
		}
		if v.Prefer != c.prefer || v.Flip != c.flip || v.Complete() != c.wantComplete || v.Version != Version ||
			v.AB.Answer != c.ab || v.BA.Answer != c.ba || v.AB.Reason != "r1" || v.BA.Reason != "r2" ||
			math.Abs(v.CostUSD-0.3) > 1e-9 || v.Model != DefaultModel || v.Effort != DefaultEffort {
			t.Errorf("%s: %+v", c.name, v)
		}
	}
}

func TestJudgePairFailures(t *testing.T) {
	cases := []struct {
		name     string
		replies  []Reply
		calls    int
		complete bool
		stopped  string
		ab, ba   bool // answered
		errs     int
	}{
		{"malformed is asked once more", []Reply{malformed, pairResult("first", "", 0.1), pairResult("second", "", 0.1)}, 3, true, "", true, true, 1},
		{"malformed twice leaves the order out", []Reply{malformed, malformed, pairResult("second", "", 0.1)}, 2, false, "", false, false, 2},
		{"a lost AB means BA is not asked", []Reply{overload, pairResult("second", "", 0.1)}, 1, false, "", false, false, 1},
		{"a usage limit stops before the next order", []Reply{limit}, 1, false, StoppedLimit, false, false, 1},
		{"a usage limit in the second order", []Reply{pairResult("first", "", 0.1), limit}, 2, false, StoppedLimit, true, false, 1},
		{"a single-schema reply is malformed", []Reply{result("yes", "", 0), result("yes", "", 0)}, 2, false, "", false, false, 2},
	}
	for _, c := range cases {
		call, prompts := pairFake(c.replies...)
		v, err := JudgePair(context.Background(), pairIn, Settings{}, call)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(*prompts) != c.calls || v.Complete() != c.complete || v.Stopped != c.stopped || v.AB.Answered != c.ab || v.BA.Answered != c.ba ||
			len(v.Errors) != c.errs || (!c.complete && (v.Prefer != "" || v.Flip)) {
			t.Errorf("%s: %d calls, %+v", c.name, len(*prompts), v)
		}
	}
	// A call that cannot be made stops, keeping nothing.
	v, err := JudgePair(context.Background(), pairIn, Settings{}, func(context.Context, string) (Reply, error) {
		return Reply{}, errors.New("exec: claude: not found")
	})
	if err != nil || v.Stopped != StoppedCall || v.Complete() || len(v.Errors) != 1 {
		t.Errorf("%+v, %v", v, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	v, err = JudgePair(ctx, pairIn, Settings{}, func(context.Context, string) (Reply, error) {
		cancel()
		return Reply{}, context.Canceled
	})
	if !errors.Is(err, context.Canceled) || v.Complete() {
		t.Errorf("cancelled: %+v, %v", v, err)
	}
}

func TestJudgePairEmptyInputs(t *testing.T) {
	doc := "diff --git a/README.md b/README.md\n+x\n"
	for _, in := range []PairInput{{Reference: codeDiff, A: doc, B: diffB}, {Reference: codeDiff, A: diffA, B: ""}} {
		call, prompts := pairFake()
		v, err := JudgePair(context.Background(), in, Settings{}, call)
		if err != nil || !v.Empty || v.Complete() || len(*prompts) != 0 {
			t.Errorf("%+v, %d calls, %v", v, len(*prompts), err)
		}
	}
	call, prompts := pairFake()
	if _, err := JudgePair(context.Background(), PairInput{Reference: doc, A: diffA, B: diffB}, Settings{}, call); err == nil || len(*prompts) != 0 {
		t.Errorf("an empty reference: %v, %d calls", err, len(*prompts))
	}
}

func TestPairPromptCutsAndFilters(t *testing.T) {
	long := "diff --git a/a.go b/a.go\n+" + strings.Repeat("é", MaxDiffChars+5) + "\n"
	p, cut := PairPrompt("i", codeDiff, long, mixedDiff)
	if !cut || !strings.Contains(p, "[... diff cut at") || strings.Contains(p, "fixture") {
		t.Errorf("cut %v, tests kept or no marker", cut)
	}
}

func TestPreference(t *testing.T) {
	mk := func(prefer string, flip bool) PairVerdict { return PairVerdict{Prefer: prefer, Flip: flip} }
	var vs []PairVerdict
	for i := 0; i < 4; i++ {
		vs = append(vs, mk(PreferB, false))
	}
	vs = append(vs, mk(PreferA, false), mk(PreferA, false), mk(PreferTie, true), mk(PreferTie, false), PairVerdict{}, PairVerdict{Empty: true})
	got := Preference(vs)
	// The pilot's figures: 4 of 6 gives p = 0.6875 and Wilson 30%-90%.
	if got.Complete != 8 || got.Incomplete != 1 || got.Empty != 1 || got.Ties != 2 || got.Flips != 1 || got.A != 2 || got.B != 4 || !got.Enough ||
		math.Abs(got.P-0.6875) > 1e-9 || math.Abs(got.BShare-2.0/3) > 1e-9 || math.Abs(got.Low-0.3) > 0.005 || math.Abs(got.High-0.9) > 0.005 {
		t.Errorf("%+v", got)
	}
	few := Preference(vs[2:6]) // 2 B, 2 A... too few preferences
	if few.Enough || few.A+few.B != 4 {
		t.Errorf("floor: %+v", few)
	}
	none := Preference(nil)
	if none.Enough || none.P != 1 || none.Low != 0 || none.High != 1 || none.BShare != 0 {
		t.Errorf("none: %+v", none)
	}
	if p := binomialTwoSided(0, 8); math.Abs(p-2.0/256) > 1e-12 {
		t.Errorf("p(0 of 8) = %v", p)
	}
	if p := binomialTwoSided(4, 8); p != 1 {
		t.Errorf("p(4 of 8) = %v", p)
	}
	if lo, hi := wilson(17, 17); hi != 1 || lo <= 0 {
		t.Errorf("wilson 17/17 = %v %v", lo, hi)
	}
	if lo, hi := wilson(0, 17); lo != 0 || hi >= 1 {
		t.Errorf("wilson 0/17 = %v %v", lo, hi)
	}
}

// TestPairCaller: the pair schema, not the single one, reaches a fake Claude Code, and its answer is read.
func TestPairCaller(t *testing.T) {
	root := t.TempDir()
	home, base := filepath.Join(root, "home"), filepath.Join(root, "judge")
	for _, d := range []string{home, base} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(root, "claude")
	script := `#!/bin/sh
cat > "$HOME/stdin"
for a in "$@"; do printf '%s\n' "[$a]"; done > "$HOME/args"
echo '{"structured_output": {"prefer": "second", "reason": "B is narrower"}, "total_cost_usd": 0.05, "result": ""}'
`
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	environ := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	call, err := PairCaller(Settings{}, claude.Judgement{CLI: cli, Dir: base, SignIn: claude.SignInLogin, Home: home}, environ, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err := JudgePair(context.Background(), pairIn, Settings{}, call)
	if err != nil || v.Prefer != PreferTie || !v.Flip || !v.AB.Answered || v.AB.Reason != "B is narrower" || v.CostUSD != 0.1 {
		t.Fatalf("%+v, %v", v, err)
	}
	args, _ := os.ReadFile(filepath.Join(home, "args"))
	if !strings.Contains(string(args), "[--json-schema]\n["+PairSchema+"]\n") || strings.Contains(string(args), Schema) ||
		!strings.Contains(string(args), "[--system-prompt]\n["+SystemPrompt+"]\n") {
		t.Errorf("args:\n%s", args)
	}
	stdin, _ := os.ReadFile(filepath.Join(home, "stdin"))
	if !strings.Contains(string(stdin), "<first>") {
		t.Errorf("stdin = %q", stdin)
	}
}

func TestJudgePairCancelledInBAKeepsAB(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	v, err := JudgePair(ctx, pairIn, Settings{}, func(context.Context, string) (Reply, error) {
		n++
		if n == 1 {
			return pairResult("first", "r", 0.1), nil
		}
		cancel()
		return Reply{}, context.Canceled
	})
	if !errors.Is(err, context.Canceled) || !v.AB.Answered || v.CostUSD != 0.1 || v.Complete() {
		t.Errorf("%+v, %v", v, err)
	}
}

func TestJudgePairTruncated(t *testing.T) {
	long := "diff --git a/a.go b/a.go\n+" + strings.Repeat("x", MaxDiffChars+5) + "\n"
	call, _ := pairFake(pairResult("first", "", 0), pairResult("second", "", 0))
	v, err := JudgePair(context.Background(), PairInput{Reference: codeDiff, A: long, B: diffB}, Settings{}, call)
	if err != nil || !v.Truncated || v.Version != PairVersion {
		t.Errorf("%+v, %v", v, err)
	}
}

// Keys match exactly, as the pilot's verdict.get does: "Fixed" is not "fixed".
func TestParseFieldMatchesKeysExactly(t *testing.T) {
	r := Reply{Stdout: []byte(`{"result": "{\"Fixed\": \"yes\"}", "structured_output": {"Fixed": "yes"}}`)}
	if a := parse(r); a.kind != kindMalformed {
		t.Errorf("%+v", a)
	}
}
