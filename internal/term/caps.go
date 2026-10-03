package term

import (
	"strconv"
	"strings"
)

// MinWidth is the narrowest terminal that gets the designed console; a narrower one gets plain lines.
const MinWidth = 60

// Capabilities is what the output can show. Screens choose between their designed form and plain text with Designed,
// and draw with Style and Shapes.
type Capabilities struct {
	Terminal bool       // output is a terminal that can move the cursor: a terminal whose TERM is not "dumb"
	Color    ColorDepth // NoColor when color is off (see Detect)
	UTF8     bool       // the locale is UTF-8, so box-drawing and block characters show; else shapes use ASCII
	Width    int        // columns: measured, else $COLUMNS, else 80
	Height   int        // rows: measured, else $LINES, else 24
}

// DetectCapabilities reads the capabilities of an output from whether it is a terminal, the environment and its size
// now (size may be nil, or report zeros, when unknown):
//   - color as Detect decides, at 24-bit depth for COLORTERM=truecolor or 24bit, 256 colors for a TERM with
//     "256color" or any other COLORTERM, else 8; FORCE_COLOR=2 and 3 raise it to 256 colors and 24-bit;
//   - UTF-8 when the first set of LC_ALL, LC_CTYPE and LANG names a UTF-8 locale (none set is the C locale: ASCII).
//
// getenv may be nil, which reads as an empty environment.
func DetectCapabilities(terminal bool, getenv func(string) string, size func() (cols, rows int)) Capabilities {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	c := Capabilities{Terminal: terminal && getenv("TERM") != "dumb", UTF8: utf8Locale(getenv)}
	if Detect(terminal, getenv).On() {
		c.Color = colorDepth(getenv)
	}
	if size != nil {
		c.Width, c.Height = size()
	}
	if c.Width <= 0 {
		c.Width = envInt(getenv, "COLUMNS", 80)
	}
	if c.Height <= 0 {
		c.Height = envInt(getenv, "LINES", 24)
	}
	return c
}

func colorDepth(getenv func(string) string) ColorDepth {
	depth := Basic
	switch ct := strings.ToLower(getenv("COLORTERM")); {
	case ct == "truecolor" || ct == "24bit":
		depth = TrueColor
	case ct != "" || strings.Contains(getenv("TERM"), "256color"):
		depth = Color256
	}
	switch getenv("FORCE_COLOR") {
	case "2":
		depth = max(depth, Color256)
	case "3":
		depth = TrueColor
	}
	return depth
}

func utf8Locale(getenv func(string) string) bool {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := getenv(name); v != "" {
			v = strings.ToLower(v)
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}

func envInt(getenv func(string) string, name string, fallback int) int {
	if n, err := strconv.Atoi(getenv(name)); err == nil && n > 0 {
		return n
	}
	return fallback
}

// Designed reports whether screens show their designed form (panels, bars, a live region): on a terminal at least
// MinWidth wide, with color on. Otherwise they print today's plain text.
func (c Capabilities) Designed() bool {
	return c.Terminal && c.Color != NoColor && c.Width >= MinWidth
}

// Style is the Style for these capabilities: off without color, else at their depth.
func (c Capabilities) Style() Style { return Colored().WithDepth(c.Color) }

// Shapes draws with these capabilities' style and characters.
func (c Capabilities) Shapes() Shapes { return Shapes{Style: c.Style(), ASCII: !c.UTF8} }
