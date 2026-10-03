// Package term styles console output. Color and emphasis are used only on a terminal, so pipes, files and tests get
// plain text with the same words; tables are sized to their content either way.
//
// It also holds the designed console's primitives: Capabilities (what the output can show), a palette by Role, widths
// in display cells, Shapes (panels, bars, interval bars, legends and spinners, with an ASCII fallback) and a Display
// (a live region redrawn in place under a log, or plain lines where it cannot be drawn).
package term

import (
	"regexp"
	"strings"
)

// Style applies color and emphasis when they are on. When they are off (the zero Style), every method returns its
// text unchanged. The named styles below (Heading, Good, ...) always use the basic colors, so their output does not
// depend on the terminal; Paint uses the palette at the Style's color depth.
type Style struct {
	on    bool
	depth ColorDepth // when on: Basic unless raised by WithDepth
}

// Colored is a Style with color and emphasis on, whatever the output is, in the basic colors.
func Colored() Style { return Style{on: true, depth: Basic} }

// Detect decides whether output gets color and emphasis:
//   - a non-empty NO_COLOR turns them off (no-color.org);
//   - otherwise a non-empty FORCE_COLOR other than "0" turns them on, for pipes into a pager such as less -R;
//   - otherwise they are on when the output is a terminal and TERM is not "dumb".
//
// getenv may be nil, which reads as an empty environment.
func Detect(terminal bool, getenv func(string) string) Style {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if getenv("NO_COLOR") != "" {
		return Style{}
	}
	if force := getenv("FORCE_COLOR"); force != "" && force != "0" {
		return Colored()
	}
	if terminal && getenv("TERM") != "dumb" {
		return Colored()
	}
	return Style{}
}

// On reports whether the Style adds escape codes.
func (s Style) On() bool { return s.on }

// Each color ends with the code that resets only the color, so a colored word keeps the bold of a heading around it.
// Bold and dim share their reset code (22), so Heading and Note must not nest: the inner one would end both.
const (
	bold, boldOff = "\x1b[1m", "\x1b[22m"
	dim, dimOff   = "\x1b[2m", "\x1b[22m"
	red           = "\x1b[31m"
	green         = "\x1b[32m"
	yellow        = "\x1b[33m"
	cyan          = "\x1b[36m"
	colorOff      = "\x1b[39m"
)

// wrap styles each line of text on its own, so a pager showing part of the text, or a redrawn status line, never
// starts or ends inside a style.
func (s Style) wrap(start, end, text string) string {
	if !s.on || text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = start + line + end
		}
	}
	return strings.Join(lines, "\n")
}

// Heading is bold: section headings and table headers.
func (s Style) Heading(text string) string { return s.wrap(bold, boldOff, text) }

// Note is dim: notes, footnotes and other secondary lines.
func (s Style) Note(text string) string { return s.wrap(dim, dimOff, text) }

// Good is green: ok, valid, passed, improved.
func (s Style) Good(text string) string { return s.wrap(green, colorOff, text) }

// Warn is yellow: warnings, and results that are not counted or not settled.
func (s Style) Warn(text string) string { return s.wrap(yellow, colorOff, text) }

// Bad is red: missing requirements, failures and regressions.
func (s Style) Bad(text string) string { return s.wrap(red, colorOff, text) }

// Command is cyan: a command for the user to type next.
func (s Style) Command(text string) string { return s.wrap(cyan, colorOff, text) }

// statuses maps the words Agentium prints for outcomes, validations, checks and experiment states to their style.
// A word missing here prints unstyled.
var statuses = map[string]func(Style, string) string{
	// good
	"ok": Style.Good, "valid": Style.Good, "yes": Style.Good, "pass": Style.Good, "passed": Style.Good,
	"improved": Style.Good, "no loss": Style.Good, "done": Style.Good,
	// not counted, not settled, or worth a look
	"WARNING": Style.Warn, "warning": Style.Warn, "exploratory": Style.Warn, "inconclusive": Style.Warn,
	"cancelled": Style.Warn, "paused": Style.Warn, "retrying": Style.Warn, "stopped": Style.Warn, "usage": Style.Warn,
	"budget": Style.Warn, "infra": Style.Warn, "infra-sandbox": Style.Warn, "unfair": Style.Warn, "unchecked": Style.Warn,
	"unverified": Style.Warn, "not validated": Style.Warn,
	// failed
	"MISSING": Style.Bad, "NOT OK": Style.Bad, "invalid": Style.Bad, "flaky": Style.Bad, "no": Style.Bad, "fail": Style.Bad,
	"failed": Style.Bad, "FAILED": Style.Bad, "regressed": Style.Bad, "capped": Style.Bad, "timeout": Style.Bad, "error": Style.Bad,
}

// Status styles a status by its leading words: the part before ": " or " (" is looked up, then its first word, so
// "invalid: base/hidden-tests wanted fail", "NOT OK (timed out)" and "paused at the usage limit" are all styled.
// Trailing spaces from padding stay inside the style. Unknown statuses print unstyled.
func (s Style) Status(text string) string {
	key := strings.TrimRight(text, " ")
	if i := strings.Index(key, ": "); i >= 0 {
		key = key[:i]
	}
	if i := strings.Index(key, " ("); i >= 0 {
		key = key[:i]
	}
	style, ok := statuses[key]
	if !ok {
		first, _, _ := strings.Cut(key, " ")
		style, ok = statuses[first]
	}
	if ok {
		return style(s, text)
	}
	return text
}

// escape matches the SGR escape codes this package writes (and any other CSI sequence).
var escape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// Plain removes escape codes from text.
func Plain(text string) string { return escape.ReplaceAllString(text, "") }

// OrNone is value, or "none found" when it is empty: how reports show a detail that was not there.
func OrNone(value string) string {
	if value == "" {
		return "none found"
	}
	return value
}
