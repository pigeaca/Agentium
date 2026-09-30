package term

import (
	"os"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		vars     map[string]string
		want     bool
	}{
		{"terminal", true, nil, true},
		{"pipe", false, nil, false},
		{"NO_COLOR on a terminal", true, map[string]string{"NO_COLOR": "1"}, false},
		{"empty NO_COLOR is unset", true, map[string]string{"NO_COLOR": ""}, true},
		{"dumb terminal", true, map[string]string{"TERM": "dumb"}, false},
		{"FORCE_COLOR into a pipe", false, map[string]string{"FORCE_COLOR": "1"}, true},
		{"FORCE_COLOR=0 forces nothing", false, map[string]string{"FORCE_COLOR": "0"}, false},
		{"FORCE_COLOR on a dumb terminal", true, map[string]string{"FORCE_COLOR": "1", "TERM": "dumb"}, true},
		{"NO_COLOR beats FORCE_COLOR", true, map[string]string{"NO_COLOR": "1", "FORCE_COLOR": "1"}, false},
	} {
		if got := Detect(tc.terminal, env(tc.vars)).On(); got != tc.want {
			t.Errorf("%s: color on = %v, want %v", tc.name, got, tc.want)
		}
	}
	if Detect(true, nil).On() != true {
		t.Error("a nil getenv should read as an empty environment")
	}
}

func TestPlainStyleChangesNothing(t *testing.T) {
	var s Style
	for _, f := range []func(string) string{s.Heading, s.Note, s.Good, s.Warn, s.Bad, s.Command, s.Status} {
		if got := f("ok"); got != "ok" {
			t.Errorf("plain style changed %q to %q", "ok", got)
		}
	}
}

func TestStatus(t *testing.T) {
	s := Colored()
	for _, tc := range []struct{ text, want string }{
		{"ok", s.Good("ok")},
		{"ok      ", s.Good("ok      ")},
		{"MISSING", s.Bad("MISSING")},
		{"WARNING", s.Warn("WARNING")},
		{"NOT OK (timed out)", s.Bad("NOT OK (timed out)")},
		{"invalid: base/hidden-tests wanted fail", s.Bad("invalid: base/hidden-tests wanted fail")},
		{"cancelled", s.Warn("cancelled")},
		{"yes", s.Good("yes")},
		{"no", s.Bad("no")},
		{"paused at the usage limit", s.Warn("paused at the usage limit")},
		{"stopped (its Agentium process ended; run it again to resume)", s.Warn("stopped (its Agentium process ended; run it again to resume)")},
		{"no loss", s.Good("no loss")},
		{"draft", "draft"},
		{"running", "running"},
		{"-", "-"},
	} {
		if got := s.Status(tc.text); got != tc.want {
			t.Errorf("Status(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestNestedStylesKeepTheOuterOne(t *testing.T) {
	s := Colored()
	got := s.Heading("Experiment ab: " + s.Warn("stopped") + " early")
	// The yellow ends with the color reset only, so the bold continues to " early".
	if !strings.HasSuffix(got, colorOff+" early"+boldOff) {
		t.Errorf("nested styles: %q", got)
	}
}

func TestWidth(t *testing.T) {
	s := Colored()
	for _, tc := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{s.Good("ok"), 2},
		{"6 × 3", 5},
		{"19–29%", 6},
		{s.Heading("A") + s.Bad("β"), 2},
	} {
		if got := Width(tc.text); got != tc.want {
			t.Errorf("Width(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
	if got := Plain(s.Heading("x") + s.Good("y")); got != "xy" {
		t.Errorf("Plain = %q", got)
	}
}

func TestIsTerminal(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if IsTerminal(devnull) || Columns(devnull) != 0 {
		t.Error("/dev/null is a character device, not a terminal")
	}
	file, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if IsTerminal(file) {
		t.Error("a regular file is not a terminal")
	}
}
