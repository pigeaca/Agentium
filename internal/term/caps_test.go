package term

import (
	"testing"
)

func TestDetectCapabilities(t *testing.T) {
	size := func(c, r int) func() (int, int) { return func() (int, int) { return c, r } }
	utf := map[string]string{"LANG": "en_US.UTF-8"}
	with := func(base map[string]string, kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, tc := range []struct {
		name     string
		terminal bool
		vars     map[string]string
		size     func() (int, int)
		want     Capabilities
		designed bool
	}{
		{"8-color terminal", true, with(utf, "TERM", "xterm"), size(100, 30),
			Capabilities{Terminal: true, Color: Basic, UTF8: true, Width: 100, Height: 30}, true},
		{"256 colors", true, with(utf, "TERM", "xterm-256color"), size(80, 24),
			Capabilities{Terminal: true, Color: Color256, UTF8: true, Width: 80, Height: 24}, true},
		{"truecolor", true, with(utf, "TERM", "xterm-256color", "COLORTERM", "truecolor"), size(80, 24),
			Capabilities{Terminal: true, Color: TrueColor, UTF8: true, Width: 80, Height: 24}, true},
		{"24bit", true, with(utf, "COLORTERM", "24bit"), size(80, 24),
			Capabilities{Terminal: true, Color: TrueColor, UTF8: true, Width: 80, Height: 24}, true},
		{"pipe", false, with(utf, "COLORTERM", "truecolor"), nil,
			Capabilities{UTF8: true, Width: 80, Height: 24}, false},
		{"pipe forced to 256", false, with(utf, "FORCE_COLOR", "2"), nil,
			Capabilities{Color: Color256, UTF8: true, Width: 80, Height: 24}, false},
		{"FORCE_COLOR=3", true, with(utf, "FORCE_COLOR", "3"), size(80, 24),
			Capabilities{Terminal: true, Color: TrueColor, UTF8: true, Width: 80, Height: 24}, true},
		{"NO_COLOR", true, with(utf, "NO_COLOR", "1", "COLORTERM", "truecolor"), size(80, 24),
			Capabilities{Terminal: true, UTF8: true, Width: 80, Height: 24}, false},
		{"dumb", true, with(utf, "TERM", "dumb"), size(80, 24),
			Capabilities{UTF8: true, Width: 80, Height: 24}, false},
		{"narrow", true, utf, size(59, 24),
			Capabilities{Terminal: true, Color: Basic, UTF8: true, Width: 59, Height: 24}, false},
		{"exactly MinWidth", true, utf, size(60, 24),
			Capabilities{Terminal: true, Color: Basic, UTF8: true, Width: 60, Height: 24}, true},
		{"C locale", true, map[string]string{"LANG": "C"}, size(80, 24),
			Capabilities{Terminal: true, Color: Basic, Width: 80, Height: 24}, true},
		{"no locale at all", true, nil, size(80, 24),
			Capabilities{Terminal: true, Color: Basic, Width: 80, Height: 24}, true},
		{"LC_ALL wins over LANG", true, with(utf, "LC_ALL", "POSIX"), size(80, 24),
			Capabilities{Terminal: true, Color: Basic, Width: 80, Height: 24}, true},
		{"LC_CTYPE utf8", true, map[string]string{"LC_CTYPE": "en_GB.utf8", "LANG": "C"}, size(80, 24),
			Capabilities{Terminal: true, Color: Basic, UTF8: true, Width: 80, Height: 24}, true},
		{"size unknown: COLUMNS and LINES", true, with(utf, "COLUMNS", "132", "LINES", "50"), size(0, 0),
			Capabilities{Terminal: true, Color: Basic, UTF8: true, Width: 132, Height: 50}, true},
	} {
		got := DetectCapabilities(tc.terminal, env(tc.vars), tc.size)
		if got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
		if got.Designed() != tc.designed {
			t.Errorf("%s: designed %v, want %v", tc.name, got.Designed(), tc.designed)
		}
	}
	if got := DetectCapabilities(true, nil, nil); got.Width != 80 || got.Height != 24 || got.Color != Basic {
		t.Errorf("nil getenv and size: %+v", got)
	}
}

func TestCapabilitiesStyleAndShapes(t *testing.T) {
	c := Capabilities{Terminal: true, Color: TrueColor, UTF8: false, Width: 80}
	if c.Style().Depth() != TrueColor || !c.Shapes().ASCII {
		t.Errorf("style %v, shapes %+v", c.Style().Depth(), c.Shapes())
	}
	c.Color = NoColor
	if c.Style().On() {
		t.Error("no color must give a plain style")
	}
}
