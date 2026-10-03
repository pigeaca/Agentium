package term

import (
	"strings"
	"testing"
)

func TestCanvasPutsTextInCells(t *testing.T) {
	c := NewCanvas(10, 2)
	if end := c.Put(1, 0, "ab", ArmA, true); end != 3 {
		t.Errorf("Put returned column %d, want 3", end)
	}
	c.Put(8, 0, "xyz", Default, false) // cut at the right edge
	c.Put(0, 1, "日本", Default, false)  // wide: two cells each
	c.Put(1, 1, "q", Default, false)   // over the second half of a wide character: the whole of it goes
	c.Put(-1, 5, "never", Default, false)
	got := c.Lines(Style{})
	if want := []string{" ab     xy", " q本"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines %q, want %q", got, want)
	}
	if c.Glyph(3, 1) != "" || c.Glyph(2, 1) != "本" || c.Glyph(20, 0) != " " {
		t.Errorf("glyphs %q %q %q", c.Glyph(3, 1), c.Glyph(2, 1), c.Glyph(20, 0))
	}
}

func TestCanvasPaintsRunsOnce(t *testing.T) {
	c := NewCanvas(8, 1)
	c.HLine(0, 0, 3, "╌", Muted)
	c.Put(3, 0, "●", ArmB, false)
	c.Put(4, 0, "ok", OutcomeOK, true)
	got := c.Lines(Colored().WithDepth(Color256))[0]
	want := "\x1b[38;5;243m╌╌╌\x1b[39m\x1b[38;5;215m●\x1b[39m\x1b[1m\x1b[38;5;114mok\x1b[39m\x1b[22m"
	if got != want {
		t.Errorf("painted %q, want %q", got, want)
	}
	if Width(got) != 6 {
		t.Errorf("width %d, want 6", Width(got))
	}
}

func TestCanvasDropsControlCharacters(t *testing.T) {
	c := NewCanvas(12, 1)
	c.Put(0, 0, "a\x1b[2Jb\tc\x07d", Default, false)
	if got := c.Lines(Style{})[0]; got != "ab cd" {
		t.Errorf("line %q, want the text without its controls", got)
	}
}
