package term

import "strings"

// Canvas is a grid of cells to draw on, for pictures that Row and Flow do not draw: boxes inside an outline, a line
// that a dot travels along. Text is put at a column and row, one character per cell (a wide one takes two), each cell
// with its role and weight; Lines paints the grid, each run of one style once. A canvas is not safe for concurrent
// use: a Frame makes its own.
type Canvas struct {
	width int
	rows  [][]canvasCell
}

// canvasCell is one cell: its character ("" for the second half of a wide one), its role and whether it is bold.
type canvasCell struct {
	glyph string
	role  Role
	bold  bool
	wide  bool // the first half of a wide character
}

// NewCanvas is an empty canvas width cells wide and height rows high.
func NewCanvas(width, height int) *Canvas {
	c := &Canvas{width: max(width, 0)}
	for range max(height, 0) {
		c.rows = append(c.rows, blankRow(c.width))
	}
	return c
}

func blankRow(width int) []canvasCell {
	row := make([]canvasCell, width)
	for i := range row {
		row[i] = canvasCell{glyph: " "}
	}
	return row
}

// Width is the canvas's width in cells.
func (c *Canvas) Width() int { return c.width }

// Height is the canvas's height in rows.
func (c *Canvas) Height() int { return len(c.rows) }

// Put writes text from column x of row y in role, bold or not, and returns the column after it. Text is plain: control
// characters and escape codes are dropped (Sanitize it first when it comes from outside Agentium; this only keeps the
// grid's cells true). What falls outside the canvas is cut; a wide character that would straddle its right edge is
// left out.
func (c *Canvas) Put(x, y int, text string, role Role, bold bool) int {
	if y < 0 || y >= len(c.rows) {
		return x + Width(text)
	}
	row := c.rows[y]
	for _, r := range Sanitize(text) {
		w := RuneWidth(r)
		if w == 0 || r == '\n' {
			continue
		}
		if x >= 0 && x+w <= c.width {
			c.clear(row, x)
			if w == 2 {
				c.clear(row, x+1)
				row[x] = canvasCell{glyph: string(r), role: role, bold: bold, wide: true}
				row[x+1] = canvasCell{role: role, bold: bold}
			} else {
				row[x] = canvasCell{glyph: string(r), role: role, bold: bold}
			}
		}
		x += w
	}
	return x
}

// clear empties cell x of row before it is written, and the other half of a wide character it is part of.
func (c *Canvas) clear(row []canvasCell, x int) {
	switch {
	case row[x].wide && x+1 < len(row):
		row[x+1] = canvasCell{glyph: " "}
	case row[x].glyph == "" && x > 0:
		row[x-1] = canvasCell{glyph: " "}
	}
	row[x] = canvasCell{glyph: " "}
}

// HLine draws n cells of glyph from column x of row y.
func (c *Canvas) HLine(x, y, n int, glyph string, role Role) {
	for i := range max(n, 0) {
		c.Put(x+i, y, glyph, role, false)
	}
}

// VLine draws n cells of glyph down from row y of column x.
func (c *Canvas) VLine(x, y, n int, glyph string, role Role) {
	for i := range max(n, 0) {
		c.Put(x, y+i, glyph, role, false)
	}
}

// Glyph is the character at column x of row y: " " outside the canvas or where nothing was put, "" for the second
// half of a wide character.
func (c *Canvas) Glyph(x, y int) string {
	if y < 0 || y >= len(c.rows) || x < 0 || x >= c.width {
		return " "
	}
	return c.rows[y][x].glyph
}

// Lines paints each row with st, trailing blanks trimmed: a bold run is a Heading around its color, so do not make
// grey roles bold (at 8 colors grey is dim, which shares bold's reset).
func (c *Canvas) Lines(st Style) []string {
	out := make([]string, len(c.rows))
	for y, row := range c.rows {
		end := len(row)
		for end > 0 && row[end-1].glyph == " " && row[end-1].role == Default && !row[end-1].bold {
			end--
		}
		var b strings.Builder
		for i := 0; i < end; {
			j := i
			var run strings.Builder
			for ; j < end && row[j].role == row[i].role && row[j].bold == row[i].bold; j++ {
				run.WriteString(row[j].glyph)
			}
			text := st.Paint(row[i].role, run.String())
			if row[i].bold {
				text = st.Heading(text)
			}
			b.WriteString(text)
			i = j
		}
		out[y] = b.String()
	}
	return out
}
