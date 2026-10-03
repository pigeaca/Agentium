package term

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// variants are the three ways shapes are drawn: color with Unicode at 24-bit depth, ASCII for a locale that is not
// UTF-8 (still in color, 8 of them), and plain for no color.
var variants = []struct {
	name   string
	shapes Shapes
}{
	{"color", Shapes{Style: Colored().WithDepth(TrueColor)}},
	{"ascii", Shapes{Style: Colored(), ASCII: true}},
	{"plain", Shapes{}},
}

// showcase draws every shape at width: the goldens and the demo. wide adds a line of wide characters, which terminals
// draw in two cells but an SVG of the output cannot line up.
func showcase(d Shapes, width int, wide bool) string {
	var lines []string
	add := func(l ...string) { lines = append(lines, l...) }
	s := d.Style
	add(d.Legend(width,
		Chip{ArmA, "A lean context"}, Chip{ArmB, "B full context"}, Chip{OutcomeOK, "ok"}, Chip{OutcomeFailed, "failed"},
		Chip{OutcomeInfra, "infrastructure"}, Chip{OutcomeLeftOut, "left out"})...)
	p := Panel{
		Title: "Experiment lean-ab", Right: d.Spinner(0) + " look 2 of 3",
		Lines: []string{
			s.Paint(ArmA, "A") + " lean   " + s.Paint(OutcomeOK, "7 ok") + "  " + s.Paint(OutcomeFailed, "2 failed") + "  " +
				s.Paint(OutcomeInfra, "1 infra"),
			s.Paint(ArmB, "B") + " full   " + s.Paint(OutcomeOK, "9 ok") + "  " + s.Paint(OutcomeFailed, "1 failed"),
			s.Paint(Muted, "a note far too long to fit on one line of a narrow panel, so it is cut with an ellipsis"),
		},
	}
	if wide {
		p.Lines = append(p.Lines, "wide: 日本語のタスク名 and an emoji 🙂 measured in cells")
	}
	add(d.Panel(p, width)...)
	add(d.Panel(Panel{
		Title: "A title much too long for a narrow panel to hold in full", Right: "dropped when there is no room",
		Overflow: WrapText,
		Lines: []string{"Wrapped: " + s.Paint(VerdictNoLoss, "no loss beyond the margin") +
			" on success; cost is inconclusive with these tasks and needs another look."},
	}, width)...)
	add(d.Panel(Panel{}, width)...)
	bars := []Bar{
		{Label: "tasks", Fraction: 12.0 / 20, Value: "12 of 20", Role: Accent},
		{Label: "budget", Fraction: 0.3333, Value: "$4.00 of $12", Role: Level(4, 12)},
		{Label: "window", Fraction: 0.78, Value: "78% resets 14:05", Role: Level(78, 100)},
		{Label: "spent", Fraction: 0.95, Value: "$11.40 of $12", Role: Level(11.4, 12)},
		{Label: "none", Fraction: 0, Value: "0"},
		{Label: "a sliver", Fraction: 0.001, Value: "0.1%"},
		{Label: "almost", Fraction: 0.999, Value: "99.9%"},
		{Label: "full", Fraction: 1, Value: "100%", Role: OutcomeOK},
		{Label: "beyond", Fraction: 3, Value: "clamped"},
		{Label: "NaN", Fraction: math.NaN(), Value: "empty"},
	}
	for _, b := range bars {
		b.LabelWidth, b.ValueWidth = 8, 16
		add(d.Bar(b, width))
	}
	ivs := []Interval{
		{Label: "success", Low: 3, Estimate: 12, High: 21, Value: "+12 pts [+3, +21]", Role: VerdictRole("improved")},
		{Label: "cost", Low: -0.42, Estimate: -0.1, High: 0.18, Mark: 0.25, Value: "-$0.10 [-0.42, +0.18]",
			Role: VerdictRole("no loss beyond the margin")},
		{Label: "turns", Low: 1, Estimate: 4, High: 6, Min: -10, Max: 5, Value: "beyond the scale",
			Role: VerdictRole("regressed")},
		{Label: "time", Low: -30, Estimate: 2, High: 40, Value: "inconclusive", Role: VerdictRole("inconclusive")},
		{Label: "nothing", Low: math.NaN(), Estimate: math.NaN(), High: math.NaN(), Value: "no data"},
	}
	for _, iv := range ivs {
		iv.LabelWidth, iv.ValueWidth = 8, 21
		add(d.IntervalBar(iv, width))
	}
	var spin []string
	for i := 0; i < 4; i++ {
		spin = append(spin, d.Spinner(i))
	}
	add("spinner " + strings.Join(spin, " ") + " " + d.Fit("and a cut line that does not fit at all at forty", width-16))
	return strings.Join(lines, "\n") + "\n"
}

// readable shows escape codes as \e, so goldens diff as text.
func readable(s string) string { return strings.ReplaceAll(s, "\x1b", `\e`) }

func TestShapesGolden(t *testing.T) {
	for _, v := range variants {
		for _, width := range []int{40, 80, 120} {
			got := showcase(v.shapes, width, true)
			for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
				if w := Width(line); w > width {
					t.Errorf("%s at %d: line %d is %d cells: %q", v.name, width, i+1, w, Plain(line))
				}
			}
			if v.name == "plain" && strings.Contains(got, "\x1b") {
				t.Errorf("plain shapes wrote an escape code")
			}
			if v.name == "ascii" {
				for _, r := range Plain(got) {
					if r > 0x7e && !strings.ContainsRune("日本語のタスク名🙂", r) {
						t.Errorf("ASCII shapes drew %q", r)
						break
					}
				}
			}
			path := filepath.Join("testdata", fmt.Sprintf("shapes-%s-%d.golden", v.name, width))
			if *update {
				if err := os.WriteFile(path, []byte(readable(got)), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (go test ./internal/term -run TestShapesGolden -update writes it)", err)
			}
			if readable(got) != string(want) {
				t.Errorf("%s differs from the render:\n%s", path, readable(got))
			}
		}
	}
}

func TestPanelLayout(t *testing.T) {
	d := Shapes{}
	got := d.Panel(Panel{Title: "Run", Right: "3s", Lines: []string{"ok"}}, 16)
	want := []string{
		"╭─ Run ─── 3s ─╮",
		"│ ok           │",
		"╰──────────────╯",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("panel:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := d.Panel(Panel{Title: "x"}, 2); Width(got[0]) != PanelMinWidth {
		t.Errorf("a tiny panel is raised to PanelMinWidth: %q", got[0])
	}
}

func TestBarSteps(t *testing.T) {
	d := Shapes{}
	for _, tc := range []struct {
		f    float64
		want string
	}{
		{0, "░░░░"}, {0.001, "▏░░░"}, {0.5, "██░░"}, {0.53, "██▏░"}, {0.999, "███▉"}, {1, "████"},
	} {
		if got := d.Bar(Bar{Fraction: tc.f}, 4); got != tc.want {
			t.Errorf("Bar(%v) = %q, want %q", tc.f, got, tc.want)
		}
	}
	d.ASCII = true
	if got := d.Bar(Bar{Fraction: 0.999}, 4); got != "###." {
		t.Errorf("ASCII bar = %q", got)
	}
}

func TestIntervalBarMarksZero(t *testing.T) {
	d := Shapes{}
	// The scale -1.15..1.15 over 11 cells: zero is the middle cell.
	got := d.IntervalBar(Interval{Low: 0.2, Estimate: 0.6, High: 1}, 11)
	if want := "─────│━━●━─"; got != want {
		t.Errorf("IntervalBar = %q, want %q", got, want)
	}
	got = d.IntervalBar(Interval{Low: -0.5, Estimate: 0, High: 0.5}, 11)
	if want := "─━━━━●━━━━─"; got != want {
		t.Errorf("an estimate at zero hides the zero line: %q, want %q", got, want)
	}
}
