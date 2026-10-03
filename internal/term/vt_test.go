package term

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// vt is a fake terminal for the live region's tests: it applies what is written to a grid of cells and keeps the raw
// bytes. It knows what the region writes (text, newline as a terminal driver sends it: CR LF, cursor up, erase below
// and to the end of the line, cursor shown and hidden, synchronized output and colors) and records anything else as
// unknown. The screen grows downward without limit, so scrolled-off lines stay in it like scrollback. A resize
// reflows rows wider than the new width, as most terminals do.
type vt struct {
	mu          sync.Mutex
	width       int
	rows        [][]rune // 0 marks the second cell of a wide character
	row, col    int
	wrapPending bool // the last column was written: the next character goes on the next row
	cursorShown bool
	syncDepth   int
	unknown     []string
	raw         bytes.Buffer
	writes      int
}

func newVT(width int) *vt { return &vt{width: width, rows: [][]rune{nil}, cursorShown: true} }

func (v *vt) Write(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.writes++
	v.raw.Write(p)
	s := string(p)
	for s != "" {
		if loc := escape.FindStringIndex(s); loc != nil && loc[0] == 0 {
			v.csi(s[:loc[1]])
			s = s[loc[1]:]
			continue
		}
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch r {
		case '\n':
			v.col, v.wrapPending = 0, false
			v.down()
		case '\r':
			v.col, v.wrapPending = 0, false
		case '\x1b':
			v.unknown = append(v.unknown, "a lone escape")
		default:
			v.put(r)
		}
	}
	return len(p), nil
}

func (v *vt) down() {
	v.row++
	for len(v.rows) <= v.row {
		v.rows = append(v.rows, nil)
	}
}

func (v *vt) put(r rune) {
	w := RuneWidth(r)
	if w == 0 {
		return
	}
	if v.wrapPending || v.col+w > v.width {
		v.col, v.wrapPending = 0, false
		v.down()
	}
	line := v.rows[v.row]
	for len(line) < v.col+w {
		line = append(line, ' ')
	}
	line[v.col] = r
	if w == 2 {
		line[v.col+1] = 0
	}
	v.rows[v.row] = line
	v.col += w
	if v.col >= v.width {
		v.col, v.wrapPending = v.width-1, true
	}
}

func (v *vt) csi(code string) {
	params, final := code[2:len(code)-1], code[len(code)-1]
	n := 1
	if p, err := strconv.Atoi(params); err == nil {
		n = p
	}
	switch {
	case final == 'm':
	case final == 'A':
		v.row, v.wrapPending = max(v.row-n, 0), false
	case final == 'J' && params == "":
		v.rows = v.rows[:v.row+1]
		v.eraseLine()
	case final == 'K' && params == "":
		v.eraseLine()
	case code == hideCursor:
		v.cursorShown = false
	case code == showCursor:
		v.cursorShown = true
	case code == syncStart:
		v.syncDepth++
	case code == syncEnd:
		v.syncDepth--
	default:
		v.unknown = append(v.unknown, fmt.Sprintf("%q", code))
	}
}

func (v *vt) eraseLine() {
	if line := v.rows[v.row]; len(line) > v.col {
		v.rows[v.row] = line[:v.col]
	}
}

// resize changes the width, splitting rows that no longer fit.
func (v *vt) resize(width int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var rows [][]rune
	cursor := 0
	for i, line := range v.rows {
		line = []rune(strings.TrimRight(string(line), " "))
		if i == v.row {
			cursor = len(rows)
		}
		for len(line) > width {
			rows = append(rows, line[:width])
			line = line[width:]
		}
		rows = append(rows, line)
	}
	v.rows, v.width, v.row = rows, width, cursor+v.col/width
	v.col %= width
}

// screen is every row with its trailing spaces removed, without the empty rows at the end.
func (v *vt) screen() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var lines []string
	for _, line := range v.rows {
		lines = append(lines, strings.TrimRight(strings.ReplaceAll(string(line), "\x00", ""), " "))
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// cursor is where the next character goes.
func (v *vt) cursor() (row, col int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.row, v.col
}

func (v *vt) rawString() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.raw.String()
}
