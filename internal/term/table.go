package term

import (
	"fmt"
	"io"
	"strings"
)

// Column is a table column: its header and alignment.
type Column struct {
	Name  string
	Right bool // right-aligned, for numbers and money
}

// Left is a left-aligned column.
func Left(name string) Column { return Column{Name: name} }

// Right is a right-aligned column.
func Right(name string) Column { return Column{Name: name, Right: true} }

// Table lays out rows in columns sized to their widest cell (escape codes ignored), two spaces apart. The header is
// printed in the heading style, and left out when every column name is empty. Lines never end in spaces.
type Table struct {
	Indent string // before every line
	style  Style
	cols   []Column
	rows   []row
}

// row is a table row, or a line printed as it is between rows.
type row struct {
	cells  []string
	line   string
	isLine bool
}

// NewTable starts a table with these columns.
func NewTable(style Style, cols ...Column) *Table {
	return &Table{style: style, cols: cols}
}

// Row adds a row. Cells may carry styles; missing cells are empty and extra cells are an error when writing.
func (t *Table) Row(cells ...string) {
	t.rows = append(t.rows, row{cells: cells})
}

// Line adds a line between rows, printed as it is: a note on the row above. Its width does not size the columns.
func (t *Table) Line(text string) {
	t.rows = append(t.rows, row{line: text, isLine: true})
}

// Write prints the table.
func (t *Table) Write(w io.Writer) error {
	widths := make([]int, len(t.cols))
	header := false
	for i, c := range t.cols {
		widths[i] = Width(c.Name)
		header = header || c.Name != ""
	}
	for n, r := range t.rows {
		if len(r.cells) > len(t.cols) {
			return fmt.Errorf("table row %d has %d cells for %d columns", n+1, len(r.cells), len(t.cols))
		}
		for i, cell := range r.cells {
			widths[i] = max(widths[i], Width(cell))
		}
	}
	var b strings.Builder
	line := func(cells []string, heading bool) {
		var l strings.Builder
		l.WriteString(t.Indent)
		for i := range t.cols {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			if heading {
				cell = t.style.Heading(cell)
			}
			pad := strings.Repeat(" ", widths[i]-Width(cell))
			if i > 0 {
				l.WriteString("  ")
			}
			if t.cols[i].Right {
				l.WriteString(pad + cell)
			} else {
				l.WriteString(cell + pad)
			}
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteString("\n")
	}
	if header {
		names := make([]string, len(t.cols))
		for i, c := range t.cols {
			names[i] = c.Name
		}
		line(names, true)
	}
	for _, r := range t.rows {
		if r.isLine {
			b.WriteString(r.line + "\n")
			continue
		}
		line(r.cells, false)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
