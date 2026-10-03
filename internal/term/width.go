package term

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Width is how many cells text takes on a terminal, without escape codes: wide (East Asian and emoji) characters take
// two, combining marks, zero-width formats and control characters none, everything else one.
func Width(text string) int {
	n := 0
	for _, r := range Plain(text) {
		n += RuneWidth(r)
	}
	return n
}

// RuneWidth is how many cells r takes on a terminal: 0, 1 or 2 (see Width).
func RuneWidth(r rune) int {
	switch {
	case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		return 0
	case r < 0x300:
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case wide(r):
		return 2
	}
	return 1
}

// wideRanges are the East Asian Wide and Fullwidth ranges (Unicode 15) that terminals draw two cells wide, merged where
// neighbours are close: CJK, Hangul, fullwidth forms and emoji with emoji presentation.
var wideRanges = [][2]rune{
	{0x1100, 0x115f}, {0x231a, 0x231b}, {0x2329, 0x232a}, {0x23e9, 0x23ec}, {0x23f0, 0x23f0}, {0x23f3, 0x23f3},
	{0x25fd, 0x25fe}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267f, 0x267f}, {0x2693, 0x2693}, {0x26a1, 0x26a1},
	{0x26aa, 0x26ab}, {0x26bd, 0x26be}, {0x26c4, 0x26c5}, {0x26ce, 0x26ce}, {0x26d4, 0x26d4}, {0x26ea, 0x26ea},
	{0x26f2, 0x26f3}, {0x26f5, 0x26f5}, {0x26fa, 0x26fa}, {0x26fd, 0x26fd}, {0x2705, 0x2705}, {0x270a, 0x270b},
	{0x2728, 0x2728}, {0x274c, 0x274c}, {0x274e, 0x274e}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27b0, 0x27b0}, {0x27bf, 0x27bf}, {0x2b1b, 0x2b1c}, {0x2b50, 0x2b50}, {0x2b55, 0x2b55}, {0x2e80, 0x303e},
	{0x3041, 0x33ff}, {0x3400, 0x4dbf}, {0x4e00, 0x9fff}, {0xa000, 0xa4cf}, {0xa960, 0xa97f}, {0xac00, 0xd7a3},
	{0xf900, 0xfaff}, {0xfe10, 0xfe19}, {0xfe30, 0xfe6f}, {0xff00, 0xff60}, {0xffe0, 0xffe6}, {0x16fe0, 0x16fe4},
	{0x17000, 0x18aff}, {0x1b000, 0x1b2ff}, {0x1f004, 0x1f004}, {0x1f0cf, 0x1f0cf}, {0x1f18e, 0x1f18e},
	{0x1f191, 0x1f19a}, {0x1f200, 0x1f202}, {0x1f210, 0x1f23b}, {0x1f240, 0x1f248}, {0x1f250, 0x1f251},
	{0x1f260, 0x1f265}, {0x1f300, 0x1f320}, {0x1f32d, 0x1f335}, {0x1f337, 0x1f37c}, {0x1f37e, 0x1f393},
	{0x1f3a0, 0x1f3ca}, {0x1f3cf, 0x1f3d3}, {0x1f3e0, 0x1f3f0}, {0x1f3f4, 0x1f3f4}, {0x1f3f8, 0x1f43e},
	{0x1f440, 0x1f440}, {0x1f442, 0x1f4fc}, {0x1f4ff, 0x1f53d}, {0x1f54b, 0x1f54e}, {0x1f550, 0x1f567},
	{0x1f57a, 0x1f57a}, {0x1f595, 0x1f596}, {0x1f5a4, 0x1f5a4}, {0x1f5fb, 0x1f64f}, {0x1f680, 0x1f6c5},
	{0x1f6cc, 0x1f6cc}, {0x1f6d0, 0x1f6d2}, {0x1f6d5, 0x1f6d7}, {0x1f6eb, 0x1f6ec}, {0x1f6f4, 0x1f6fc},
	{0x1f7e0, 0x1f7eb}, {0x1f90c, 0x1f93a}, {0x1f93c, 0x1f945}, {0x1f947, 0x1f9ff}, {0x1fa70, 0x1faff},
	{0x20000, 0x2fffd}, {0x30000, 0x3fffd},
}

func wide(r rune) bool {
	i := sort.Search(len(wideRanges), func(i int) bool { return wideRanges[i][1] >= r })
	return i < len(wideRanges) && wideRanges[i][0] <= r
}

// eachPart calls f for each escape code (esc true) and each printable rune of text, in order.
func eachPart(text string, f func(part string, esc bool)) {
	for text != "" {
		if loc := escape.FindStringIndex(text); loc != nil && loc[0] == 0 {
			f(text[:loc[1]], true)
			text = text[loc[1]:]
			continue
		}
		_, size := utf8.DecodeRuneInString(text)
		f(text[:size], false)
		text = text[size:]
	}
}

// Truncate cuts text to at most width cells, ending with ellipsis when it cut (left out when even it does not fit).
// Escape codes are kept, including those after the cut, so a style that was opened is still closed.
func Truncate(text string, width int, ellipsis string) string {
	if Width(text) <= width {
		return text
	}
	if Width(ellipsis) > width {
		ellipsis = ""
	}
	room := max(width-Width(ellipsis), 0)
	var b strings.Builder
	used, cut := 0, false
	eachPart(text, func(part string, esc bool) {
		if esc {
			b.WriteString(part)
			return
		}
		r, _ := utf8.DecodeRuneInString(part)
		if w := RuneWidth(r); !cut && used+w <= room {
			b.WriteString(part)
			used += w
			return
		}
		if !cut {
			cut = true
			b.WriteString(ellipsis)
		}
	})
	return b.String()
}

// Pad fills text with spaces on the right to width cells; wider text is returned as it is.
func Pad(text string, width int) string {
	return text + strings.Repeat(" ", max(width-Width(text), 0))
}

// PadLeft fills text with spaces on the left to width cells, for right alignment.
func PadLeft(text string, width int) string {
	return strings.Repeat(" ", max(width-Width(text), 0)) + text
}

// split cuts text after width cells: head holds the escape codes up to the cut, tail the rest. A wide character that
// would straddle the cut goes to tail; at least one character goes to head, so splitting always progresses.
func split(text string, width int) (head, tail string) {
	used, at := 0, len(text)
	pos := 0
	eachPart(text, func(part string, esc bool) {
		defer func() { pos += len(part) }()
		if esc || at < len(text) {
			return
		}
		r, _ := utf8.DecodeRuneInString(part)
		w := RuneWidth(r)
		if used+w > width && used > 0 {
			at = pos
			return
		}
		used += w
	})
	return text[:at], text[at:]
}

// Wrap breaks text into lines of at most width cells at spaces, splitting words longer than a line, and at every
// newline. A style that a line leaves open is reset at its end and reopened on the next line, so each line can be
// drawn on its own (inside a box, or redrawn); text without escape codes gets none.
func Wrap(text string, width int) []string {
	width = max(width, 1)
	var out []string
	for _, para := range strings.Split(text, "\n") {
		var lines []string
		line, lineW := "", 0
		for _, word := range strings.Split(para, " ") {
			ww := Width(word)
			switch {
			case lineW > 0 && lineW+1+ww <= width:
				line, lineW = line+" "+word, lineW+1+ww
				continue
			case lineW > 0:
				lines = append(lines, line)
				line, lineW = "", 0
			}
			for ww > width {
				head, tail := split(word, width)
				lines = append(lines, head)
				word, ww = tail, Width(tail)
			}
			line, lineW = word, ww
		}
		lines = append(lines, line)
		out = append(out, carryStyles(lines)...)
	}
	return out
}

// reset ends every style: it closes a line that Wrap broke inside a style.
const reset = "\x1b[0m"

// carryStyles makes each of lines, one paragraph broken apart, stand alone: a line that ends inside a style ends with a
// reset, and the next line starts with the codes that reopen that style.
func carryStyles(lines []string) []string {
	var open sgrState
	for i, line := range lines {
		if carried := open.codes(); carried != "" {
			line = carried + line
		}
		for _, code := range escape.FindAllString(line, -1) {
			open.apply(code)
		}
		if i < len(lines)-1 && open.codes() != "" {
			line += reset
		}
		lines[i] = line
	}
	return lines
}

// sgrState is the style in force after a run of escape codes: the color and the intensity (bold or dim) this package
// writes, and any other code seen, kept as it is.
type sgrState struct {
	color, intensity string
	other            []string
}

func (s *sgrState) apply(code string) {
	params := strings.TrimSuffix(strings.TrimPrefix(code, "\x1b["), "m")
	switch {
	case !strings.HasSuffix(code, "m"):
		// Not a style (cursor movement and the like): nothing stays open.
	case params == "" || params == "0":
		*s = sgrState{}
	case params == "39":
		s.color = ""
	case params == "22":
		s.intensity = ""
	case params == "1" || params == "2":
		s.intensity = code
	case len(params) == 2 && (params[0] == '3' || params[0] == '9') && params[1] >= '0' && params[1] <= '7',
		strings.HasPrefix(params, "38;"):
		s.color = code
	default:
		s.other = append(s.other, code)
	}
}

// codes reopens the style: empty when none is in force.
func (s *sgrState) codes() string { return s.intensity + s.color + strings.Join(s.other, "") }
