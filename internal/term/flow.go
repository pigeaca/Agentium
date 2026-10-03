package term

import (
	"slices"
	"strings"
)

// Node is one box of a Row or a Flow.
type Node struct {
	Panel  Panel
	Weight int // its share of the row's width against the others'; 0 means 1
	Width  int // the most the box takes of its share (centred in it); 0 means the whole share
}

// RowMinWidth is the narrowest share a box of a row may get (or its Width, when that is less); below it the row is
// stacked.
const RowMinWidth = 20

// Widths splits width into n shares, gap cells apart, in proportion to weights (missing or 0 count as 1). The cells
// left over by rounding go to the first shares.
func Widths(width, gap, n int, weights ...int) []int {
	if n <= 0 {
		return nil
	}
	weight := func(i int) int {
		if i < len(weights) && weights[i] > 0 {
			return weights[i]
		}
		return 1
	}
	total := 0
	for i := 0; i < n; i++ {
		total += weight(i)
	}
	avail := max(width-gap*(n-1), 0)
	shares, used := make([]int, n), 0
	for i := range shares {
		shares[i] = avail * weight(i) / total
		used += shares[i]
	}
	for i := 0; used < avail; i = (i + 1) % n {
		shares[i]++
		used++
	}
	return shares
}

// placedRow is a row laid out: its lines, and the columns where connectors enter it (the tops of its boxes) and leave
// it (their bottoms, on its last line).
type placedRow struct {
	lines   []string
	in, out []int
}

// Row lays nodes out side by side in width cells, gap apart, each box centred in its share and every box as tall as
// the tallest (padded with empty lines). Where a share would be narrower than RowMinWidth (or the box's Width, when
// that is less) it stacks them instead, one under another, each centred; connectors then enter the first and leave
// the last.
func (d Shapes) Row(nodes []Node, width, gap int) []string {
	return d.placeRow(nodes, width, gap, -1).lines
}

// center is the column of a box's middle, where connectors meet it.
func center(offset, width int) int { return offset + (width-1)/2 }

// placeRow lays a row out. A row of one box is centred, or put with its middle at column at when at is not negative.
func (d Shapes) placeRow(nodes []Node, width, gap, at int) placedRow {
	if len(nodes) == 0 {
		return placedRow{}
	}
	weights := make([]int, len(nodes))
	for i, n := range nodes {
		weights[i] = n.Weight
	}
	shares := Widths(width, gap, len(nodes), weights...)
	boxWidth := func(n Node, share int) int {
		if n.Width > 0 {
			share = min(share, n.Width)
		}
		return max(share, PanelMinWidth)
	}
	tooNarrow := false
	for i, n := range nodes {
		need := RowMinWidth
		if n.Width > 0 {
			need = max(min(n.Width, need), PanelMinWidth)
		}
		tooNarrow = tooNarrow || shares[i] < need
	}
	if len(nodes) > 1 && tooNarrow {
		var p placedRow
		for i, n := range nodes {
			w := boxWidth(n, width)
			offset := (width - w) / 2
			for _, line := range d.Panel(n.Panel, w) {
				p.lines = append(p.lines, strings.Repeat(" ", offset)+line)
			}
			if i == 0 {
				p.in = []int{center(offset, w)}
			}
			p.out = []int{center(offset, w)}
		}
		return p
	}
	boxes := make([][]string, len(nodes))
	widths, offsets := make([]int, len(nodes)), make([]int, len(nodes))
	tallest, start := 0, 0
	for i, n := range nodes {
		widths[i] = boxWidth(n, shares[i])
		offsets[i] = start + (shares[i]-widths[i])/2
		if len(nodes) == 1 && at >= 0 {
			offsets[i] = min(max(at-(widths[i]-1)/2, 0), max(width-widths[i], 0))
		}
		start += shares[i] + gap
		boxes[i] = d.Panel(n.Panel, widths[i])
		tallest = max(tallest, len(boxes[i]))
	}
	for i, n := range nodes {
		if short := tallest - len(boxes[i]); short > 0 {
			n.Panel.Lines = append(slices.Clone(n.Panel.Lines), make([]string, short)...)
			boxes[i] = d.Panel(n.Panel, widths[i])
		}
	}
	p := placedRow{lines: make([]string, tallest)}
	for l := range p.lines {
		var b strings.Builder
		at := 0
		for i := range nodes {
			line := ""
			if l < len(boxes[i]) {
				line = boxes[i][l]
			}
			b.WriteString(strings.Repeat(" ", max(offsets[i]-at, 0)) + line)
			at = offsets[i] + widths[i]
		}
		p.lines[l] = strings.TrimRight(b.String(), " ")
	}
	for i := range nodes {
		p.in = append(p.in, center(offsets[i], widths[i]))
	}
	p.out = p.in
	return p
}

// Flow is a data-flow picture: rows of boxes, top to bottom, each row joined to the next at the boxes' middles. One
// box into several is a split, several into one a merge, one into one an arrow; two rows of boxes whose middles line
// up join pairwise; any other pair of rows joins through one bar (every box above into every box below). It draws
// these simple shapes only, not general graphs.
type Flow struct {
	Rows     [][]Node
	MaxWidth int  // the widest it is drawn, from the left edge: 0 means MaxContentWidth, negative means no limit
	Gap      int  // cells between boxes in a row; 0 means 3
	Line     Role // the connectors' color; Default means Muted, the borders' color
}

// connection is how a connector cell joins its neighbours.
type connection struct{ up, down, left, right bool }

var unicodeJoins = map[connection]string{
	{up: true, down: true}: "│", {left: true, right: true}: "─",
	{down: true, right: true}: "┌", {down: true, left: true}: "┐", {up: true, right: true}: "└", {up: true, left: true}: "┘",
	{up: true, left: true, right: true}: "┴", {down: true, left: true, right: true}: "┬",
	{up: true, down: true, right: true}: "├", {up: true, down: true, left: true}: "┤",
	{up: true, down: true, left: true, right: true}: "┼",
}

// join is the character for a connector cell.
func (d Shapes) join(c connection) string {
	if d.ASCII {
		switch {
		case c.left || c.right:
			if !c.up && !c.down {
				return "-"
			}
			return "+"
		}
		return "|"
	}
	return unicodeJoins[c]
}

// Flow draws f in width cells, or MaxWidth when that is less.
func (d Shapes) Flow(f Flow, width int) []string {
	width = capWidth(width, f.MaxWidth)
	gap := f.Gap
	if gap <= 0 {
		gap = 3
	}
	role := f.Line
	if role == Default {
		role = Muted
	}
	rows := slices.DeleteFunc(slices.Clone(f.Rows), func(r []Node) bool { return len(r) == 0 })
	placed := make([]placedRow, len(rows))
	for i, nodes := range rows {
		placed[i] = d.placeRow(nodes, width, gap, -1)
	}
	// A row of one box lines its middle up with the middle box of the row above, or else of the row below, when that
	// row has an odd number of boxes: an arrow then runs straight, never a step of one cell.
	middle := func(cols []int) int {
		if len(cols)%2 == 1 {
			return cols[len(cols)/2]
		}
		return -1
	}
	for i, nodes := range rows {
		if len(nodes) != 1 {
			continue
		}
		at := -1
		if i > 0 {
			at = middle(placed[i-1].out)
		}
		if at < 0 && i+1 < len(rows) {
			at = middle(placed[i+1].in)
		}
		if at >= 0 {
			placed[i] = d.placeRow(nodes, width, gap, at)
		}
	}
	var out []string
	var above []int
	for i, p := range placed {
		if i > 0 {
			out = append(out, d.connect(above, p.in, role)...)
		}
		if i < len(rows)-1 {
			last := len(p.lines) - 1
			for _, c := range p.out {
				p.lines[last] = overlay(p.lines[last], c, d.Style.Paint(role, d.join(connection{left: true, right: true, down: true})))
			}
		}
		out = append(out, p.lines...)
		above = p.out
	}
	return out
}

// connect draws the lines between boxes leaving at columns up and boxes entering at columns down: a bar joining them
// (left out when each goes straight down), then an arrow into each box below.
func (d Shapes) connect(up, down []int, role Role) []string {
	arrow := "▼"
	if d.ASCII {
		arrow = "v"
	}
	all := append(slices.Clone(up), down...)
	lo, hi := slices.Min(all), slices.Max(all)
	var lines []string
	if !slices.Equal(sortedSet(up), sortedSet(down)) {
		cells := make([]string, hi+1)
		for c := range cells {
			cells[c] = " "
			if c >= lo {
				cells[c] = d.join(connection{up: slices.Contains(up, c), down: slices.Contains(down, c), left: c > lo, right: c < hi})
			}
		}
		lines = append(lines, strings.Repeat(" ", lo)+d.Style.Paint(role, strings.Join(cells[lo:], "")))
	}
	cells := []rune(strings.Repeat(" ", hi+1))
	for _, c := range down {
		cells[c] = []rune(arrow)[0]
	}
	var b strings.Builder
	for c := 0; c <= hi; c++ {
		if cells[c] == ' ' {
			b.WriteByte(' ')
			continue
		}
		b.WriteString(d.Style.Paint(role, string(cells[c])))
	}
	return append(lines, strings.TrimRight(b.String(), " "))
}

func sortedSet(cols []int) []int {
	s := slices.Clone(cols)
	slices.Sort(s)
	return slices.Compact(s)
}

// overlay puts glyph (styled as it is) over the character at cell col of a styled line, padding a shorter line with
// spaces. The style in force there is closed before the glyph and reopened after it, so neither leaks into the other.
func overlay(line string, col int, glyph string) string {
	var b strings.Builder
	var st sgrState
	used, placed := 0, false
	put := func() {
		if codes := st.codes(); codes != "" {
			b.WriteString(reset + glyph + codes)
		} else {
			b.WriteString(glyph)
		}
		placed = true
	}
	eachPart(line, func(part string, esc bool) {
		if esc {
			st.apply(part)
			b.WriteString(part)
			return
		}
		r := []rune(part)[0]
		w := RuneWidth(r)
		switch {
		case placed || used+w <= col:
			b.WriteString(part)
		case used == col:
			put()
			if w == 2 {
				b.WriteByte(' ')
			}
		default: // a wide character straddles col: replace it
			b.WriteByte(' ')
			put()
		}
		used += w
	})
	if !placed {
		b.WriteString(strings.Repeat(" ", max(col-used, 0)))
		put()
	}
	return b.String()
}
