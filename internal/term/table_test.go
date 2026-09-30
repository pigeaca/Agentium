package term

import (
	"strings"
	"testing"
)

func render(t *testing.T, table *Table) string {
	t.Helper()
	var b strings.Builder
	if err := table.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestTableSizesColumnsToContent(t *testing.T) {
	table := NewTable(Style{}, Left("NAME"), Left("SOURCE"), Right("TESTS"), Left("STATUS"))
	table.Row("deny-login-file", "commit 7d888f7ed874", "1", "valid")
	table.Row("a", "claude/chore/ab-minimal-context", "12", "invalid: base/hidden-tests wanted fail")
	want := "" +
		"NAME             SOURCE                           TESTS  STATUS\n" +
		"deny-login-file  commit 7d888f7ed874                  1  valid\n" +
		"a                claude/chore/ab-minimal-context     12  invalid: base/hidden-tests wanted fail\n"
	if got := render(t, table); got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableIgnoresEscapeCodes(t *testing.T) {
	s := Colored()
	table := NewTable(s, Left("OUTCOME"), Right("COST"))
	table.Row(s.Status("ok"), "$0.51")
	table.Row(s.Status("cancelled"), "$12.30")
	lines := strings.Split(strings.TrimSuffix(render(t, table), "\n"), "\n")
	var widths []int
	for _, line := range lines {
		widths = append(widths, Width(line))
	}
	if widths[0] != widths[1] || widths[1] != widths[2] {
		t.Errorf("styled rows are not aligned: widths %v\n%s", widths, strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[0], s.Heading("OUTCOME")) {
		t.Errorf("the header is not in the heading style: %q", lines[0])
	}
	for _, line := range lines {
		if Plain(line) != strings.TrimRight(Plain(line), " ") {
			t.Errorf("line ends in spaces: %q", line)
		}
	}
}

func TestTableWithoutHeaderAndWithIndent(t *testing.T) {
	table := NewTable(Style{}, Left(""), Left(""), Right(""), Left(""))
	table.Indent = "  "
	table.Row("CLAUDE.md", "instructions", "1.2 KB")
	table.Row(".claude/skills/review/SKILL.md", "skill", "210 B", " via CLAUDE.md")
	want := "" +
		"  CLAUDE.md                       instructions  1.2 KB\n" +
		"  .claude/skills/review/SKILL.md  skill          210 B   via CLAUDE.md\n"
	if got := render(t, table); got != want {
		t.Errorf("table:\n%q\nwant:\n%q", got, want)
	}
}

func TestTableLinesBetweenRows(t *testing.T) {
	table := NewTable(Style{}, Left("ARM"), Right("COST"))
	table.Row("A", "$1.00")
	table.Line("  arm A: a note much wider than the table, which does not widen its columns")
	table.Row("B", "$12.00")
	want := "" +
		"ARM    COST\n" +
		"A     $1.00\n" +
		"  arm A: a note much wider than the table, which does not widen its columns\n" +
		"B    $12.00\n"
	if got := render(t, table); got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableRejectsExtraCells(t *testing.T) {
	table := NewTable(Style{}, Left("A"))
	table.Row("1", "2")
	if err := table.Write(&strings.Builder{}); err == nil {
		t.Error("a row with more cells than columns should be an error")
	}
}
