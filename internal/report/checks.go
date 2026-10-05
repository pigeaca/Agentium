package report

import (
	"fmt"
	"strings"

	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

// CheckRow is one of the project's rule checks (agentium check add) counted per arm: how many of an arm's counted runs
// met it. The counts are read again from each run's stored transcript and change when the report is made, so runs from
// before the check existed are counted too. They are counts, never a verdict.
type CheckRow struct {
	Name    string               `json:"name"`
	Kind    string               `json:"kind"` // run.CheckRan, run.CheckChanged or run.CheckNotChanged
	Pattern string               `json:"pattern"`
	Arms    map[string]CheckCell `json:"arms"`
}

// CheckCell is a check's counts in one arm. Unread counts the counted runs whose transcript (a ran check) or change (the
// others) was missing or unreadable: they are neither met nor unmet, so Met + Unread <= Counted.
type CheckCell struct {
	Met     int `json:"met"`
	Counted int `json:"counted"`
	Unread  int `json:"unread"`
}

// Rule is the check's rule in words.
func (c CheckRow) Rule() string {
	pattern := term.Sanitize(c.Pattern) // the owner's text, drawn on a screen
	switch c.Kind {
	case run.CheckRan:
		return fmt.Sprintf("ran a command containing %q", pattern)
	case run.CheckChanged:
		return "changed a file matching " + pattern
	default:
		return "changed no file matching " + pattern
	}
}

// Counts is the cell in words: "7 of 8", with "(2 unread)" when some runs could not be read.
func (c CheckCell) Counts() string {
	s := fmt.Sprintf("%d of %d", c.Met, c.Counted)
	if c.Unread > 0 {
		s += fmt.Sprintf(" (%d unread)", c.Unread)
	}
	return s
}

// checksIntro says what the check rows are.
const checksIntro = "Counts of the runs that followed your rule checks (agentium check list), read from each run's transcript and change: not a verdict."

// checkRows counts the checks over each arm's counted runs. Results holds, by run ID, each counted run's result for every
// check, in the order of checks (Input.CheckResults); a run without an entry is unread.
func checkRows(in Input, arms []Arm) []CheckRow {
	if len(in.Checks) == 0 {
		return nil
	}
	rows := make([]CheckRow, len(in.Checks))
	for i, c := range in.Checks {
		rows[i] = CheckRow{Name: in.scrub(c.Name), Kind: c.Kind, Pattern: in.scrub(c.Pattern), Arms: map[string]CheckCell{}}
		for _, a := range arms {
			cell := CheckCell{}
			for _, r := range in.Runs {
				if r.Record.Arm != a.Name || !counted(r.Record) {
					continue
				}
				cell.Counted++
				results := in.CheckResults[r.ID]
				switch {
				case i >= len(results) || results[i] == run.CheckUnread:
					cell.Unread++
				case results[i] == run.CheckMet:
					cell.Met++
				}
			}
			rows[i].Arms[a.Name] = cell
		}
	}
	return rows
}

// markdownChecks writes the checks table after the behavior table; nothing without checks.
func (r Report) markdownChecks(b *strings.Builder) {
	if len(r.Checks) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## Rule checks\n\n%s\n\n| Check | Rule |", checksIntro)
	for _, a := range r.Arms {
		fmt.Fprintf(b, " %s |", a.label())
	}
	b.WriteString("\n|---|---" + strings.Repeat("|---", len(r.Arms)) + "|\n")
	for _, c := range r.Checks {
		fmt.Fprintf(b, "| `%s` | %s |", c.Name, markdownCell(c.Rule()))
		for _, a := range r.Arms {
			fmt.Fprintf(b, " %s |", c.Arms[a.Name].Counts())
		}
		b.WriteString("\n")
	}
}

// markdownCell keeps a free text inside one table cell.
func markdownCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ", "\r", " ").Replace(s)
}

// terminalChecks writes the checks table of the plain terminal report; nothing without checks.
func (r Report) terminalChecks(b *strings.Builder, st term.Style, section func(name, intro string), table func(...term.Column) *term.Table) error {
	if len(r.Checks) == 0 {
		return nil
	}
	section("Rule checks", checksIntro)
	cols := []term.Column{term.Left("Check"), term.Left("Rule")}
	for _, a := range r.Arms {
		cols = append(cols, term.Right(a.label()))
	}
	t := table(cols...)
	for _, c := range r.Checks {
		row := []string{term.Sanitize(c.Name), c.Rule()}
		for _, a := range r.Arms {
			row = append(row, c.Arms[a.Name].Counts())
		}
		t.Row(row...)
	}
	return t.Write(b)
}
