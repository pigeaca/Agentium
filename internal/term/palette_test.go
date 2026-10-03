package term

import (
	"strings"
	"testing"
)

func TestPaintByDepth(t *testing.T) {
	for _, tc := range []struct {
		depth ColorDepth
		role  Role
		want  string
	}{
		{Basic, ArmA, "\x1b[36mA\x1b[39m"},
		{Basic, Muted, "\x1b[2mA\x1b[22m"}, // grey is dim in 8 colors
		{Color256, ArmB, "\x1b[38;5;215mA\x1b[39m"},
		{Color256, Sandbox, "\x1b[38;5;141mA\x1b[39m"},
		{Color256, ArmA, "\x1b[38;5;75mA\x1b[39m"},
		{TrueColor, OutcomeOK, "\x1b[38;2;135;215;135mA\x1b[39m"},
		{Basic, ArmB, "\x1b[33mA\x1b[39m"},
		{Basic, OutcomeInfra, "\x1b[35mA\x1b[39m"}, // not arm B's yellow
		{Basic, LevelCaution, "\x1b[35mA\x1b[39m"},
		{Basic, Sandbox, "\x1b[34mA\x1b[39m"},
		{TrueColor, Default, "A"},
		{NoColor, ArmA, "A"},
	} {
		if got := Colored().WithDepth(tc.depth).Paint(tc.role, "A"); got != tc.want {
			t.Errorf("Paint(%v, %v) = %q, want %q", tc.depth, tc.role, got, tc.want)
		}
	}
	if got := (Style{}).WithDepth(TrueColor).Paint(ArmA, "A"); got != "A" {
		t.Errorf("depth must not turn color on: %q", got)
	}
}

// At 8 colors, the arms, the sandbox and the warnings each keep a color of their own.
func TestBasicColorsStayApart(t *testing.T) {
	st := Colored().WithDepth(Basic)
	seen := map[string]Role{}
	for _, r := range []Role{ArmA, ArmB, Sandbox, OutcomeInfra, OutcomeOK, OutcomeFailed} {
		code := st.Paint(r, "x")
		if other, ok := seen[code]; ok {
			t.Errorf("roles %d and %d share %q", other, r, code)
		}
		seen[code] = r
	}
}

func TestEveryRoleHasAShade(t *testing.T) {
	for r := ArmA; r <= Muted; r++ {
		for _, d := range []ColorDepth{Basic, Color256, TrueColor} {
			if got := Colored().WithDepth(d).Paint(r, "x"); !strings.HasPrefix(got, "\x1b[") {
				t.Errorf("role %d at %v is not painted: %q", r, d, got)
			}
		}
	}
}

func TestNamedStylesIgnoreDepth(t *testing.T) {
	// Existing screens use Good, Bad and the rest; a deeper palette must not change their bytes.
	for _, d := range []ColorDepth{Basic, Color256, TrueColor} {
		s := Colored().WithDepth(d)
		if s.Good("ok") != Colored().Good("ok") || s.Status("failed") != Colored().Status("failed") {
			t.Errorf("named styles changed at %v", d)
		}
	}
	// Detect gives the basic depth whatever FORCE_COLOR asks for: the named styles never go deeper.
	for _, force := range []string{"1", "2", "3"} {
		if got := Detect(false, env(map[string]string{"FORCE_COLOR": force})).Depth(); got != Basic {
			t.Errorf("Detect with FORCE_COLOR=%s: depth %v, want 8 colors", force, got)
		}
	}
	if Detect(true, nil).Depth() != Basic || Detect(false, nil).Depth() != NoColor {
		t.Error("Detect gives the basic depth on a terminal and none on a pipe")
	}
}

func TestLevel(t *testing.T) {
	for _, tc := range []struct {
		used, limit float64
		want        Role
	}{
		{0, 10, LevelCalm}, {6.9, 10, LevelCalm}, {7, 10, LevelCaution}, {8.9, 10, LevelCaution},
		{9, 10, LevelAlarm}, {12, 10, LevelAlarm}, {5, 0, LevelCalm},
	} {
		if got := Level(tc.used, tc.limit); got != tc.want {
			t.Errorf("Level(%v, %v) = %v, want %v", tc.used, tc.limit, got, tc.want)
		}
	}
}

func TestVerdictRole(t *testing.T) {
	for verdict, want := range map[string]Role{
		"improved": VerdictImproved, "improved, but small": VerdictImproved, "regressed": VerdictRegressed,
		"no loss beyond the margin": VerdictNoLoss, "equivalent": VerdictNoLoss, "inconclusive": VerdictInconclusive,
		"exploratory": VerdictInconclusive, "": VerdictInconclusive,
	} {
		if got := VerdictRole(verdict); got != want {
			t.Errorf("VerdictRole(%q) = %v, want %v", verdict, got, want)
		}
	}
}
