package judge

import (
	"math"
	"strings"
	"testing"
)

// A judge-graded run passes on a majority of the requested calls saying "yes", fails on a majority saying otherwise,
// and has no grade when errors, a refusal or a limit leave too few answers for either: never a failure for want of them.
func TestGrade(t *testing.T) {
	five := func(answers ...string) Verdict { return Verdict{Requested: 5, Answers: answers} }
	for name, c := range map[string]struct {
		v              Verdict
		passed, graded bool
		why            string
	}{
		"all yes":                 {five(Yes, Yes, Yes, Yes, Yes), true, true, ""},
		"3 of 5 yes":              {five(Yes, No, Yes, Partly, Yes), true, true, ""},
		"partly is not fixed":     {five(Partly, Partly, Yes, Partly, Yes), false, true, ""},
		"3 of 5 no":               {five(No, No, No, Yes, Yes), false, true, ""},
		"no majority among three": {five(Yes, Yes, Partly, No, No), false, true, ""}, // 2 fixed, 3 not: not fixed
		"decided before errors":   {Verdict{Requested: 5, Answers: []string{Yes, Yes, Yes}, Errors: []string{"timed out", "timed out"}}, true, true, ""},
		"decided before a limit":  {Verdict{Requested: 5, Answers: []string{No, Partly, No}, Stopped: StoppedLimit}, false, true, ""},
		"errors leave a tie":      {Verdict{Requested: 5, Answers: []string{Yes, Yes, No, No}, Errors: []string{"exit 1"}}, false, false, "answered 4 of 5 times (2 fixed, 2 not)"},
		"a usage limit":           {Verdict{Requested: 5, Answers: []string{Yes, Yes}, Stopped: StoppedLimit}, false, false, "it stopped at a usage limit"},
		"a call that failed":      {Verdict{Requested: 5, Answers: []string{}, Stopped: StoppedCall, Errors: []string{"fork failed"}}, false, false, "it stopped early"},
		"every call failed":       {Verdict{Requested: 5, Answers: []string{}, Errors: []string{"x", "x", "x", "x", "x"}}, false, false, "answered 0 of 5 times"},
		"an even tie":             {Verdict{Requested: 4, Answers: []string{Yes, No, Yes, Partly}}, false, false, "tie (2 fixed, 2 not)"},
		"no code changed":         {Verdict{Requested: 5, Empty: true}, false, true, ""},
		"never asked":             {Verdict{}, false, false, "not asked"},
	} {
		passed, graded, why := Grade(c.v)
		if passed != c.passed || graded != c.graded || !strings.Contains(why, c.why) || (c.graded && why != "") {
			t.Errorf("%s: Grade = %v, %v, %q; want %v, %v, %q", name, passed, graded, why, c.passed, c.graded, c.why)
		}
	}
}

// Grading's cap is each call's cap, twice (a malformed reply is asked again), for every repeat: 5 × 2 × $0.50 for the
// default judge, whose calls get no overshoot allowance; any other judge's calls hold one.
func TestGradingCap(t *testing.T) {
	g := GradingSettings()
	if g.Repeats != GradeRepeats || g.Model != DefaultModel || g.Effort != DefaultEffort || GradeRepeats != 5 {
		t.Fatalf("grading settings %+v", g)
	}
	if got := CapUSD(g); math.Abs(got-5) > 1e-9 {
		t.Errorf("CapUSD(grading) = %v, want 5", got)
	}
	if CallCapFor(Settings{Model: "claude-sonnet-5-5", Effort: DefaultEffort}) <= CallCapUSD {
		t.Error("another judge's calls hold no overshoot allowance")
	}
}
