package term

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// experimentFlow is a running experiment as a flow: the experiment splits into its arms, which merge into grading
// and then the current look. Each box holds two lines at most, and no number shows twice. Bars are drawn to the
// inside of the boxes at width.
func experimentFlow(d Shapes, width, done int, spent float64, okA, failA, okB, failB, tick int, elapsed string) Flow {
	s := d.Style
	tally := func(ok, failed int) string {
		return s.Paint(OutcomeOK, fmt.Sprintf("%d ok", ok)) + "  " + s.Paint(OutcomeFailed, fmt.Sprintf("%d failed", failed))
	}
	running := func(task string) string { return d.Spinner(tick) + " " + s.Paint(Muted, task) }
	const total, budget = 24, 12.0
	bar := func(label string, f float64, value string, role Role, width int) string {
		return d.Bar(Bar{Label: label, LabelWidth: 5, Fraction: f, Value: value, ValueWidth: 12, Role: role}, width)
	}
	const head = 46
	inner := min(head, width) - 4
	return Flow{Rows: [][]Node{
		{{Width: head, Panel: Panel{Title: "Experiment lean-ab", Right: s.Paint(Muted, elapsed), Lines: []string{
			bar("runs", float64(done)/total, fmt.Sprintf("%d of %d", done, total), Default, inner),
			bar("spend", spent/budget, fmt.Sprintf("$%.2f of $%.0f", spent, budget), Level(spent, budget), inner),
		}}}},
		{
			{Width: 36, Panel: Panel{Title: s.Paint(ArmA, "A") + " lean context", Lines: []string{tally(okA, failA), running("parse-iso-weeks")}}},
			{Width: 36, Panel: Panel{Title: s.Paint(ArmB, "B") + " full context", Lines: []string{tally(okB, failB), running("csv-quoting")}}},
		},
		{{Width: head, Panel: Panel{Title: "Grading", Right: s.Paint(Muted, "sandbox-v1"), Lines: []string{
			s.Paint(OutcomeOK, "canary ok") + s.Paint(Muted, ", no denials"),
		}}}},
		{{Width: head, Panel: Panel{Title: "Look 2 of 3", Lines: []string{
			d.IntervalBar(Interval{Label: "success", Low: -4, Estimate: 9, High: 22, Value: "+9 pts", ValueWidth: 6,
				Role: VerdictInconclusive}, inner),
			s.Paint(Muted, "inconclusive so far; next look at 18 runs"),
		}}}},
	}}
}

// runFlow is one run as a chain of its stages, each with its time.
func runFlow(d Shapes) Flow {
	s := d.Style
	stage := func(title, took, line string) []Node {
		return []Node{{Width: 46, Panel: Panel{Title: title, Right: s.Paint(Muted, took), Lines: []string{line}}}}
	}
	return Flow{Rows: [][]Node{
		stage("Checkout", "3s", "base 7d888f7, a fresh copy"),
		stage("Agent", "2m41s", "23 turns, $0.41"),
		stage("Grading", "18s", s.Paint(OutcomeOK, "12 of 12 tests passed")),
		stage("Record", "0s", "run 41"),
	}}
}

// flowShowcase draws the flows at width: the experiment, a run, and a three-way split that stacks below 66 columns.
func flowShowcase(d Shapes, width int) string {
	box := func(title string) Node { return Node{Panel: Panel{Title: title, Lines: []string{"one line"}}} }
	split := Flow{Rows: [][]Node{
		{{Width: 30, Panel: Panel{Title: "Pool", Lines: []string{"22 tasks"}}}},
		{box("valid"), box("weak"), box("flaky")},
		{{Width: 30, Panel: Panel{Title: "Report", Lines: []string{"3 groups"}}}},
	}}
	var parts []string
	for _, f := range []Flow{experimentFlow(d, width, 14, 5.9, 5, 1, 5, 1, 3, "4m02s"), runFlow(d), split} {
		parts = append(parts, strings.Join(d.Flow(f, width), "\n"))
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func TestFlowGolden(t *testing.T) {
	for _, v := range variants {
		for _, width := range []int{40, 60, 80, 120} {
			got := flowShowcase(v.shapes, width)
			for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
				if w := Width(line); w > width {
					t.Errorf("%s at %d: line %d is %d cells: %q", v.name, width, i+1, w, Plain(line))
				}
			}
			if v.name == "plain" && strings.Contains(got, "\x1b") {
				t.Errorf("plain flow wrote an escape code")
			}
			if v.name == "ascii" {
				for _, r := range Plain(got) {
					if r > 0x7e {
						t.Errorf("ASCII flow drew %q", r)
						break
					}
				}
			}
			path := filepath.Join("testdata", fmt.Sprintf("flow-%s-%d.golden", v.name, width))
			if *update {
				if err := os.WriteFile(path, []byte(readable(got)), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (go test ./internal/term -run TestFlowGolden -update writes it)", err)
			}
			if readable(got) != string(want) {
				t.Errorf("%s differs from the render:\n%s", path, readable(got))
			}
		}
	}
}

func TestWidths(t *testing.T) {
	for _, tc := range []struct {
		width, gap, n int
		weights       []int
		want          []int
	}{
		{80, 3, 2, nil, []int{39, 38}},
		{80, 3, 3, []int{2, 1, 1}, []int{38, 18, 18}},
		{10, 3, 1, nil, []int{10}},
		{4, 3, 3, nil, []int{0, 0, 0}},
		{80, 3, 0, nil, nil},
	} {
		got := Widths(tc.width, tc.gap, tc.n, tc.weights...)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("Widths(%d, %d, %d, %v) = %v, want %v", tc.width, tc.gap, tc.n, tc.weights, got, tc.want)
		}
	}
}

func TestRowPadsAndStacks(t *testing.T) {
	d := Shapes{}
	nodes := []Node{
		{Panel: Panel{Title: "a", Lines: []string{"1"}}},
		{Panel: Panel{Title: "b", Lines: []string{"1", "2", "3"}}},
	}
	got := strings.Join(d.Row(nodes, 43, 3), "\n") // two shares of 20
	want := strings.Join([]string{
		"╭─ a ──────────────╮   ╭─ b ──────────────╮",
		"│ 1                │   │ 1                │",
		"│                  │   │ 2                │",
		"│                  │   │ 3                │",
		"╰──────────────────╯   ╰──────────────────╯",
	}, "\n")
	if got != want {
		t.Errorf("row:\n%s\nwant:\n%s", got, want)
	}
	if got := d.Row(nodes, 42, 3); len(got) != 8 || !strings.HasPrefix(got[3], "╭─ b") {
		t.Errorf("a share below RowMinWidth stacks the boxes:\n%s", strings.Join(got, "\n"))
	}
}

func TestFlowConnectors(t *testing.T) {
	d := Shapes{}
	box := func(title string, width int) Node { return Node{Width: width, Panel: Panel{Title: title}} }
	got := strings.Join(d.Flow(Flow{Rows: [][]Node{
		{box("in", 12)},
		{box("a", 12), box("b", 12)},
		{box("out", 12)},
		{box("end", 12)},
	}}, 40), "\n")
	want := strings.Join([]string{
		"              ╭─ in ─────╮",
		"              ╰────┬─────╯",
		"        ┌──────────┴──────────┐",
		"        ▼                     ▼",
		"   ╭─ a ──────╮          ╭─ b ──────╮",
		"   ╰────┬─────╯          ╰────┬─────╯",
		"        └──────────┬──────────┘",
		"                   ▼",
		"              ╭─ out ────╮",
		"              ╰────┬─────╯",
		"                   ▼",
		"              ╭─ end ────╮",
		"              ╰──────────╯",
	}, "\n")
	if got != want {
		t.Errorf("flow:\n%s\nwant:\n%s", got, want)
	}
	d.ASCII = true
	got = strings.Join(d.Flow(Flow{Rows: [][]Node{{box("in", 12)}, {box("a", 12), box("b", 12)}}}, 40), "\n")
	if !strings.Contains(got, "+----+-----+") || !strings.Contains(got, "+----------+----------+") ||
		!strings.Contains(got, "v                     v") {
		t.Errorf("ASCII flow:\n%s", got)
	}
}

// A dotted chain joins its boxes with a dotted cell and no arrowhead or tee; a dashed panel has the sandbox's outline.
func TestFlowDottedAndDashedPanels(t *testing.T) {
	d := Shapes{}
	box := func(title string, dashed bool) Node {
		return Node{Width: 12, Panel: Panel{Title: title, Dashed: dashed, Lines: []string{"x"}}}
	}
	rows := [][]Node{{box("a", false)}, {box("b", true)}, {box("c", false)}}
	got := strings.Join(d.Flow(Flow{Rows: rows, Dotted: true}, 30), "\n")
	want := strings.Join([]string{
		"         ╭─ a ──────╮",
		"         │ x        │",
		"         ╰──────────╯",
		"              ┊",
		"         ╭┄ b ┄┄┄┄┄┄╮",
		"         ┆ x        ┆",
		"         ╰┄┄┄┄┄┄┄┄┄┄╯",
		"              ┊",
		"         ╭─ c ──────╮",
		"         │ x        │",
		"         ╰──────────╯",
	}, "\n")
	if got != want {
		t.Errorf("dotted flow:\n%s\nwant:\n%s", got, want)
	}
	for _, bad := range []string{"▼", "┬", "┴"} {
		if strings.Contains(got, bad) {
			t.Errorf("a dotted flow has %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "╭┄ b ┄") || !strings.Contains(got, "┆ x") || !strings.Contains(got, "╰┄┄┄") {
		t.Errorf("dashed panel:\n%s", got)
	}
	d.ASCII = true
	got = strings.Join(d.Flow(Flow{Rows: rows, Dotted: true}, 30), "\n")
	if strings.Contains(got, "v") || !strings.Contains(got, ":") || !strings.Contains(got, ".- b ") && !strings.Contains(got, ". b ") {
		t.Errorf("ASCII dotted flow:\n%s", got)
	}
}

func TestFlowPairsBoxesThatLineUp(t *testing.T) {
	d := Shapes{}
	box := func(title string) Node { return Node{Panel: Panel{Title: title}} }
	got := d.Flow(Flow{Rows: [][]Node{{box("a"), box("b")}, {box("c"), box("d")}}}, 60)
	if connector := got[2]; strings.ContainsAny(connector, "─┼├┤") || strings.Count(connector, "▼") != 2 {
		t.Errorf("rows that line up join pairwise, without a bar: %q", connector)
	}
}

func TestOverlay(t *testing.T) {
	s := Colored()
	for _, tc := range []struct {
		line  string
		col   int
		glyph string
		want  string
	}{
		{"abcde", 2, "X", "abXde"},
		{"ab", 4, "X", "ab  X"},
		{"日本", 2, "X", "日X "},
		{"日本", 1, "X", " X本"},
		{s.Note("abc"), 1, "X", dim + "a" + reset + "X" + dim + "c" + dimOff},
	} {
		if got := overlay(tc.line, tc.col, tc.glyph); got != tc.want {
			t.Errorf("overlay(%q, %d) = %q, want %q", tc.line, tc.col, got, tc.want)
		}
	}
}
