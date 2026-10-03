package term

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// sequence matches a terminal escape sequence: a CSI sequence (styles, cursor movement, erasing), an OSC string (window
// titles, hyperlinks, clipboard), another string sequence (DCS, SOS, PM, APC), or an escape and one character.
var sequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b[\]PX^_][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[^\[\]PX^_]?`)

// style matches the escape codes that only style text (SGR).
var style = regexp.MustCompile(`^\x1b\[[0-9;]*m$`)

// Sanitize makes text from outside Agentium (an agent's output, a task's or a branch's name) safe to print: it removes
// every escape sequence, every control character but the newline (C1 included, whether encoded or as raw bytes), every
// byte that is not UTF-8 and the bidirectional overrides that could reorder what is shown, and turns tabs into spaces.
// Style it afterwards.
func Sanitize(text string) string { return clean(text, false) }

// keepStyles is Sanitize that keeps styles: what the live region applies to every line, so a line can move neither
// the cursor nor anything else, and the region's count of its rows stays true.
func keepStyles(text string) string { return clean(text, true) }

func clean(text string, styles bool) string {
	if utf8.ValidString(text) && !strings.ContainsFunc(text, unsafeRune) {
		return text
	}
	var b strings.Builder
	plain := func(part string) {
		for part != "" {
			r, size := utf8.DecodeRuneInString(part)
			part = part[size:]
			switch {
			case r == utf8.RuneError && size == 1: // a byte that is not UTF-8, such as a raw 8-bit CSI (0x9b)
			case r == '\t':
				b.WriteByte(' ')
			case !unsafeRune(r):
				b.WriteRune(r)
			}
		}
	}
	at := 0
	for _, loc := range sequence.FindAllStringIndex(text, -1) {
		plain(text[at:loc[0]])
		if seq := text[loc[0]:loc[1]]; styles && style.MatchString(seq) {
			b.WriteString(seq)
		}
		at = loc[1]
	}
	plain(text[at:])
	return b.String()
}

// unsafeRune is a control character other than the newline (C0, DEL and C1) or a bidirectional embedding, override or
// isolate.
func unsafeRune(r rune) bool {
	return r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)) ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}
