package term

import (
	"math"
	"strings"
)

// Shapes draws panels, bars, interval bars, legends and spinners: in color when its Style is on, with box-drawing and
// block characters unless ASCII is set (a locale that is not UTF-8). Every shape is measured in display cells and fitted
// to the width it is given; none ends in a newline. Use Capabilities.Shapes to get one.
type Shapes struct {
	Style Style
	ASCII bool
}

// glyphs is a character set for the shapes.
type glyphs struct {
	topLeft, topRight, bottomLeft, bottomRight, horizontal, vertical string
	ellipsis, chip                                                   string
	full, track                                                      string
	eighths                                                          []string // partial cells, 1 to 7 eighths full
	line, interval, zero, mark, estimate                             string
	spinner                                                          []string
}

var (
	unicodeGlyphs = glyphs{
		topLeft: "╭", topRight: "╮", bottomLeft: "╰", bottomRight: "╯", horizontal: "─", vertical: "│",
		ellipsis: "…", chip: "■",
		full: "█", track: "░", eighths: []string{"▏", "▎", "▍", "▌", "▋", "▊", "▉"},
		line: "─", interval: "━", zero: "│", mark: "┊", estimate: "●",
		spinner: strings.Split(string(spinnerFrames), ""),
	}
	asciiGlyphs = glyphs{
		topLeft: "+", topRight: "+", bottomLeft: "+", bottomRight: "+", horizontal: "-", vertical: "|",
		ellipsis: "...", chip: "#",
		full: "#", track: ".",
		line: "-", interval: "=", zero: "|", mark: ":", estimate: "o",
		spinner: []string{"|", "/", "-", `\`},
	}
)

func (d Shapes) glyphs() glyphs {
	if d.ASCII {
		return asciiGlyphs
	}
	return unicodeGlyphs
}

// Ellipsis is what marks cut text: "…", or "..." in ASCII.
func (d Shapes) Ellipsis() string { return d.glyphs().ellipsis }

// Fit cuts text to width cells with the shapes' ellipsis (see Truncate).
func (d Shapes) Fit(text string, width int) string { return Truncate(text, width, d.Ellipsis()) }

// Spinner is the spinner's frame for tick, muted: it shows motion, so it needs no color.
func (d Shapes) Spinner(tick int) string {
	frames := d.glyphs().spinner
	return d.Style.Paint(Muted, frames[((tick%len(frames))+len(frames))%len(frames)])
}

// Overflow is what a panel does with a line wider than its inside.
type Overflow int

const (
	Cut      Overflow = iota // cut it with an ellipsis
	WrapText                 // wrap it at spaces onto more lines
)

// MaxContentWidth is how wide panels and flows are drawn at most by default: wide terminals get more space, not more
// content.
const MaxContentWidth = 100

// Panel is a box with a title. Title and Right may be styled; the title is drawn bold.
type Panel struct {
	MaxWidth int      // the widest it is drawn: 0 means MaxContentWidth, negative means no limit
	Title    string   // in the top border, on the left; cut to fit
	Right    string   // in the top border, on the right; left out when there is no room for it
	Lines    []string // the content, one entry per line (an entry with newlines is several)
	Overflow Overflow
	Border   Role // the frame's color; Default means Muted
}

// PanelMinWidth is the narrowest panel drawn; a narrower width is raised to it.
const PanelMinWidth = 8

// Panel draws p width cells wide: a top border with the titles, each content line between two borders with one space
// of padding, and a bottom border. The content is fitted to width-4 cells.
func (d Shapes) Panel(p Panel, width int) []string {
	g := d.glyphs()
	width = max(capWidth(width, p.MaxWidth), PanelMinWidth)
	border := p.Border
	if border == Default {
		border = Muted
	}
	edge := func(s string) string { return d.Style.Paint(border, s) }
	inner := width - 2

	// Top: ╭─ Title ───── Right ─╮. Each title takes its width plus a space either side; one rule cell stays on each
	// end and at least one between them.
	title, right := p.Title, p.Right
	used := 1 // the rule cell before the title
	if title != "" {
		title = d.Fit(title, inner-4)
		used += Width(title) + 2
	}
	rightW := 0
	if right != "" {
		rightW = Width(right) + 3
		if used+1+rightW > inner {
			right, rightW = "", 0
		}
	}
	top := edge(g.topLeft + g.horizontal)
	if title != "" {
		top += " " + d.Style.Heading(title) + " "
	}
	top += edge(strings.Repeat(g.horizontal, max(inner-used-rightW, 0)))
	if right != "" {
		top += " " + right + " " + edge(g.horizontal)
	}
	top += edge(g.topRight)

	lines := []string{top}
	content := width - 4
	for _, entry := range p.Lines {
		var parts []string
		if p.Overflow == WrapText {
			parts = Wrap(entry, content)
		} else {
			for _, part := range strings.Split(entry, "\n") {
				parts = append(parts, d.Fit(part, content))
			}
		}
		for _, part := range parts {
			lines = append(lines, edge(g.vertical)+" "+Pad(part, content)+" "+edge(g.vertical))
		}
	}
	return append(lines, edge(g.bottomLeft+strings.Repeat(g.horizontal, inner)+g.bottomRight))
}

// cell is one cell of a bar: its character and color.
type cell struct {
	glyph string
	role  Role
}

// paintCells joins cells, painting each run of one color once.
func (d Shapes) paintCells(cells []cell) string {
	var b strings.Builder
	for i := 0; i < len(cells); {
		j, run := i, ""
		for ; j < len(cells) && cells[j].role == cells[i].role; j++ {
			run += cells[j].glyph
		}
		b.WriteString(d.Style.Paint(cells[i].role, run))
		i = j
	}
	return b.String()
}

// barMinWidth is the fewest cells a bar or interval bar gets: the label is cut before the bar shrinks below it.
const barMinWidth = 4

// row lays out a label, a bar n cells wide and a value in width cells, and returns n. The label is cut when the bar
// would be narrower than barMinWidth.
func (d Shapes) row(label string, labelWidth int, value string, valueWidth int, width int) (left, right string, n int) {
	lw, vw := max(Width(label), labelWidth), max(Width(value), valueWidth)
	gaps := 0
	if lw > 0 {
		gaps++
	}
	if vw > 0 {
		gaps++
	}
	n = width - lw - vw - gaps
	if n < barMinWidth && lw > 0 {
		lw = max(lw-(barMinWidth-n), 0)
		label = d.Fit(label, lw)
		n = width - lw - vw - gaps
	}
	n = max(n, 1)
	if lw > 0 {
		left = Pad(label, lw) + " "
	}
	if vw > 0 {
		right = " " + PadLeft(value, vw)
	}
	return left, right, n
}

// Bar is a horizontal bar: a label, a fill and a value.
type Bar struct {
	Label      string
	LabelWidth int     // pads the label to this width, so bars line up; 0 means its own width
	Fraction   float64 // how full, 0 to 1; outside is clamped and NaN is empty
	Value      string  // after the bar, right-aligned in ValueWidth
	ValueWidth int
	Role       Role // the fill's color, such as Level(used, limit) or an arm
}

// Bar draws b in width cells. The fill has eighth-cell steps (whole cells in ASCII); any amount above zero shows at
// least one step and any amount below full leaves at least one, so 0.1% and 99.9% never read as 0 and 100%.
func (d Shapes) Bar(b Bar, width int) string {
	g := d.glyphs()
	left, right, n := d.row(b.Label, b.LabelWidth, b.Value, b.ValueWidth, width)
	f := b.Fraction
	if math.IsNaN(f) {
		f = 0
	}
	f = min(max(f, 0), 1)
	steps := 8
	if d.ASCII {
		steps = 1
	}
	total := n * steps
	filled := int(math.Round(f * float64(total)))
	if f > 0 && filled == 0 {
		filled = 1
	}
	if f < 1 && filled == total && total > 1 {
		filled = total - 1
	}
	cells := make([]cell, 0, n)
	for i := 0; i < filled/steps; i++ {
		cells = append(cells, cell{g.full, b.Role})
	}
	if part := filled % steps; part > 0 {
		cells = append(cells, cell{g.eighths[part-1], b.Role})
	}
	for len(cells) < n {
		cells = append(cells, cell{g.track, Muted})
	}
	return Truncate(left+d.paintCells(cells)+right, width, "")
}

// Interval is an estimate and its interval on a scale with zero marked: how a verdict looks.
type Interval struct {
	Label               string
	LabelWidth          int
	Low, Estimate, High float64
	Min, Max            float64 // the scale's ends; equal or infinite means symmetric about zero, holding every value
	Mark                float64 // a second line, such as the no-loss margin; 0 draws none
	Value               string  // after the scale, right-aligned in ValueWidth: the numbers in words
	ValueWidth          int
	Role                Role // the interval's and estimate's color, such as VerdictRole of its verdict
}

// IntervalBar draws iv in width cells: a muted line for the scale, the interval over it in its role's color, a mark
// for Mark, a line at zero and a dot for the estimate, in that order of precedence. Values beyond the scale are drawn
// at its end; NaN values are left out.
func (d Shapes) IntervalBar(iv Interval, width int) string {
	g := d.glyphs()
	left, right, n := d.row(iv.Label, iv.LabelWidth, iv.Value, iv.ValueWidth, width)
	lo, hi := iv.Min, iv.Max
	if !(hi > lo) || math.IsInf(lo, 0) || math.IsInf(hi, 0) {
		m := 0.0
		for _, v := range []float64{iv.Low, iv.Estimate, iv.High, iv.Mark} {
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				m = max(m, math.Abs(v))
			}
		}
		if m == 0 {
			m = 1
		}
		lo, hi = -m*1.15, m*1.15
	}
	col := func(v float64) (int, bool) {
		if math.IsNaN(v) {
			return 0, false
		}
		c := math.Round((v - lo) / (hi - lo) * float64(n-1))
		if math.IsNaN(c) { // the builtin min and max keep a NaN, and int(NaN) is no index
			return 0, false
		}
		return int(min(max(c, 0), float64(n-1))), true
	}
	cells := make([]cell, n)
	for i := range cells {
		cells[i] = cell{g.line, Muted}
	}
	if a, ok := col(iv.Low); ok {
		if b, ok := col(iv.High); ok {
			for i := min(a, b); i <= max(a, b); i++ {
				cells[i] = cell{g.interval, iv.Role}
			}
		}
	}
	if iv.Mark != 0 {
		if c, ok := col(iv.Mark); ok {
			cells[c] = cell{g.mark, Muted}
		}
	}
	if c, ok := col(0); ok && lo <= 0 && hi >= 0 {
		cells[c] = cell{g.zero, Default}
	}
	if c, ok := col(iv.Estimate); ok {
		cells[c] = cell{g.estimate, iv.Role}
	}
	return Truncate(left+d.paintCells(cells)+right, width, "")
}

// Chip is one entry of a legend: a colored square and what it stands for.
type Chip struct {
	Role  Role
	Label string
}

// Legend lays chips out three spaces apart, starting a new line where the next would pass width.
func (d Shapes) Legend(width int, chips ...Chip) []string {
	g := d.glyphs()
	var lines []string
	line, lineW := "", 0
	for _, c := range chips {
		text := d.Style.Paint(c.Role, g.chip) + " " + c.Label
		w := Width(text)
		if lineW > 0 && lineW+3+w > width {
			lines = append(lines, d.Fit(line, width))
			line, lineW = "", 0
		}
		if lineW > 0 {
			line, lineW = line+"   ", lineW+3
		}
		line, lineW = line+text, lineW+w
	}
	if line != "" {
		lines = append(lines, d.Fit(line, width))
	}
	return lines
}

// capWidth is width limited by limit: 0 means MaxContentWidth, negative means none.
func capWidth(width, limit int) int {
	switch {
	case limit < 0:
		return width
	case limit == 0:
		limit = MaxContentWidth
	}
	return min(width, limit)
}
