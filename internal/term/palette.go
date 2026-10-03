package term

import (
	"strconv"
	"strings"
)

// ColorDepth is how many colors a terminal shows.
type ColorDepth int

const (
	NoColor   ColorDepth = iota // no escape codes at all
	Basic                       // the 8 ANSI colors, plus bold and dim
	Color256                    // the xterm 256-color palette
	TrueColor                   // 24-bit color (COLORTERM=truecolor)
)

func (d ColorDepth) String() string {
	return [...]string{"none", "8 colors", "256 colors", "truecolor"}[max(min(int(d), 3), 0)]
}

// Depth is the Style's color depth: NoColor when it is off.
func (s Style) Depth() ColorDepth {
	if !s.on {
		return NoColor
	}
	return max(s.depth, Basic)
}

// WithDepth is s at color depth d. A Style that is off stays off, and NoColor turns it off: depth never turns color
// on, so NO_COLOR and pipes keep plain text.
func (s Style) WithDepth(d ColorDepth) Style {
	if !s.on || d <= NoColor {
		return Style{}
	}
	return Style{on: true, depth: min(d, TrueColor)}
}

// Role is what a color means. Screens paint by role, never by a color, so one palette serves every screen.
type Role int

const (
	Default Role = iota // the terminal's own color: Paint leaves the text as it is

	ArmA // the first arm of an experiment (A, the baseline)
	ArmB // the second arm (B, the change)

	OutcomeOK       // a run that passed
	OutcomeFailed   // a run that failed its tests
	OutcomeInfra    // a run that never had a fair attempt (infrastructure)
	OutcomeLeftOut  // a run that is not counted
	VerdictImproved // B is better
	VerdictRegressed
	VerdictNoLoss // no loss beyond the margin, or equivalent
	VerdictInconclusive

	LevelCalm    // money or the usage window, well within the limit
	LevelCaution // nearing the limit
	LevelAlarm   // at or nearly at the limit

	Accent // titles, spinners and commands
	Muted  // borders, bar tracks and secondary text
)

// shade is a role's color at each depth: a basic SGR code (or dim), a 256-palette index and an RGB value.
type shade struct {
	basic string // "" means dim
	x256  int
	rgb   [3]uint8
}

var (
	greenShade  = shade{"32", 78, [3]uint8{95, 215, 135}}
	redShade    = shade{"31", 203, [3]uint8{255, 95, 95}}
	yellowShade = shade{"33", 220, [3]uint8{255, 215, 0}}
	greyShade   = shade{"", 245, [3]uint8{138, 138, 138}}
	shades      = map[Role]shade{
		ArmA:                shade{"36", 117, [3]uint8{135, 215, 255}},
		ArmB:                shade{"35", 213, [3]uint8{255, 135, 255}},
		OutcomeOK:           greenShade,
		OutcomeFailed:       redShade,
		OutcomeInfra:        yellowShade,
		OutcomeLeftOut:      greyShade,
		VerdictImproved:     greenShade,
		VerdictRegressed:    redShade,
		VerdictNoLoss:       shade{"34", 69, [3]uint8{95, 135, 255}},
		VerdictInconclusive: greyShade,
		LevelCalm:           greenShade,
		LevelCaution:        yellowShade,
		LevelAlarm:          redShade,
		Accent:              shade{"36", 38, [3]uint8{0, 175, 215}},
		Muted:               shade{"", 243, [3]uint8{118, 118, 118}},
	}
)

// Paint colors text by role at the Style's depth, each line on its own like the named styles. At the basic depth the
// grey roles (OutcomeLeftOut, VerdictInconclusive, Muted) are dim, which shares its reset with bold: do not paint them
// inside a Heading. Off, or for Default, text is returned as it is.
func (s Style) Paint(r Role, text string) string {
	sh, ok := shades[r]
	if !ok || !s.on {
		return text
	}
	switch s.Depth() {
	case TrueColor:
		return s.wrap("\x1b[38;2;"+strconv.Itoa(int(sh.rgb[0]))+";"+strconv.Itoa(int(sh.rgb[1]))+";"+
			strconv.Itoa(int(sh.rgb[2]))+"m", colorOff, text)
	case Color256:
		return s.wrap("\x1b[38;5;"+strconv.Itoa(sh.x256)+"m", colorOff, text)
	}
	if sh.basic == "" {
		return s.wrap(dim, dimOff, text)
	}
	return s.wrap("\x1b["+sh.basic+"m", colorOff, text)
}

// Level is the warning role of used against limit: calm below 70%, caution from 70%, alarm from 90%. A limit of zero
// or less means no limit: calm.
func Level(used, limit float64) Role {
	if limit <= 0 || used < 0.7*limit {
		return LevelCalm
	}
	if used < 0.9*limit {
		return LevelCaution
	}
	return LevelAlarm
}

// VerdictRole is the role of a verdict as Agentium words it (stats: "improved", "improved, but small", "regressed",
// "no loss beyond the margin", "equivalent", "inconclusive", "exploratory"); anything else is inconclusive.
func VerdictRole(verdict string) Role {
	switch {
	case strings.HasPrefix(verdict, "improved"):
		return VerdictImproved
	case strings.HasPrefix(verdict, "regressed"):
		return VerdictRegressed
	case strings.HasPrefix(verdict, "no loss"), strings.HasPrefix(verdict, "equivalent"):
		return VerdictNoLoss
	}
	return VerdictInconclusive
}
