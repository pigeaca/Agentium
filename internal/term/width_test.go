package term

import (
	"strings"
	"testing"
)

func TestWidthCountsCells(t *testing.T) {
	s := Colored()
	for _, tc := range []struct {
		text string
		want int
	}{
		{"表格", 4},  // CJK: two cells each
		{"a🙂b", 4}, // emoji presentation: two
		{"é", 1},   // e and a combining acute: one
		{"a‍b", 2}, // zero-width joiner: none
		{"╭─╮│█▏░━●┊■⠋…", 13},          // the shapes' characters: one each
		{s.Paint(ArmA, "日本") + "x", 5}, // escape codes: none
		{"\t", 0},
	} {
		if got := Width(tc.text); got != tc.want {
			t.Errorf("Width(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestTruncateKeepsStylesAndCells(t *testing.T) {
	s := Colored()
	for _, tc := range []struct {
		text     string
		width    int
		ellipsis string
		want     string
	}{
		{"abcdef", 6, "…", "abcdef"},
		{"abcdef", 4, "…", "abc…"},
		{"abcdef", 4, "...", "a..."},
		{"abcdef", 2, "...", "ab"}, // no room for the ellipsis: a plain cut
		{"日本語", 4, "…", "日…"},      // a wide character never straddles the cut
		{"日本語", 5, "", "日本"},
		{s.Good("abcdef"), 4, "…", s.Good("abc…")}, // the color still ends
		{"", 0, "…", ""},
	} {
		got := Truncate(tc.text, tc.width, tc.ellipsis)
		if got != tc.want {
			t.Errorf("Truncate(%q, %d, %q) = %q, want %q", tc.text, tc.width, tc.ellipsis, got, tc.want)
		}
		if Width(got) > tc.width {
			t.Errorf("Truncate(%q, %d) is %d cells", tc.text, tc.width, Width(got))
		}
	}
}

func TestPad(t *testing.T) {
	if got := Pad(Colored().Bad("ab"), 4); Plain(got) != "ab  " {
		t.Errorf("Pad = %q", got)
	}
	if got := PadLeft("日", 4); got != "  日" {
		t.Errorf("PadLeft = %q", got)
	}
	if got := Pad("abcdef", 3); got != "abcdef" {
		t.Errorf("Pad of wider text = %q", got)
	}
}

func TestWrap(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  []string
	}{
		{"the quick brown fox", 10, []string{"the quick", "brown fox"}},
		{"one\ntwo three", 5, []string{"one", "two", "three"}},
		{"abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
		{"日本語の文", 5, []string{"日本", "語の", "文"}},
		{"", 5, []string{""}},
		{"a b", 0, []string{"a", "b"}},
	} {
		got := Wrap(tc.text, tc.width)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("Wrap(%q, %d) = %q, want %q", tc.text, tc.width, got, tc.want)
		}
	}
}

func TestWrapCarriesStylesAcrossLines(t *testing.T) {
	s := Colored()
	got := Wrap("ok "+s.Bad("failed twice")+" then", 10)
	want := []string{
		"ok " + red + "failed" + reset,
		red + "twice" + colorOff + " then",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Wrap = %q, want %q", got, want)
	}
	for _, line := range Wrap("plain words only here", 6) {
		if strings.Contains(line, "\x1b") {
			t.Errorf("plain text got an escape code: %q", line)
		}
	}
}

func TestSanitize(t *testing.T) {
	s := Colored()
	for _, tc := range []struct{ text, want, styled string }{
		{"plain text", "plain text", "plain text"},
		{s.Good("ok") + " done", "ok done", s.Good("ok") + " done"},
		{"up\x1b[2Aand\x1b[Jgone", "upandgone", "upandgone"},
		{"title\x1b]0;pwned\x07 here", "title here", "title here"},
		{"link \x1b]8;;http://x\x1b\\x\x1b]8;;\x1b\\", "link x", "link x"},
		{"bell\a back\b cr\r tab\tdel\x7f c1\u009b", "bell back cr tab del c1", "bell back cr tab del c1"},
		{"two\nlines", "two\nlines", "two\nlines"},
		{"lone \x1b", "lone ", "lone "},
		{"\x1bc reset", " reset", " reset"},
	} {
		if got := Sanitize(tc.text); got != tc.want {
			t.Errorf("Sanitize(%q) = %q, want %q", tc.text, got, tc.want)
		}
		if got := keepStyles(tc.text); got != tc.styled {
			t.Errorf("keepStyles(%q) = %q, want %q", tc.text, got, tc.styled)
		}
	}
}
