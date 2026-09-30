package term

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func newLine(out *bytes.Buffer, o StatusOptions) *StatusLine {
	o.Interval = -1
	if o.Getenv == nil {
		o.Getenv = env(map[string]string{"TERM": "xterm"})
	}
	return NewStatusLine(out, o)
}

func TestStatusLinePassThroughWhenNotLive(t *testing.T) {
	for name, o := range map[string]StatusOptions{
		"not a terminal": {Terminal: false},
		"dumb terminal":  {Terminal: true, Getenv: env(map[string]string{"TERM": "dumb"})},
	} {
		var out bytes.Buffer
		s := newLine(&out, o)
		s.Show(func() string { return "working" })
		s.Step("step")
		s.Tick()
		s.Write([]byte("one\n"))
		s.Write([]byte("partial"))
		s.Stop()
		s.Stop()
		if got := out.String(); got != "one\npartial" || s.Live() {
			t.Errorf("%s: output %q, live %v", name, got, s.Live())
		}
	}
}

func TestStatusLineNoColorStillLive(t *testing.T) {
	var out bytes.Buffer
	s := newLine(&out, StatusOptions{Terminal: true, Getenv: env(map[string]string{"TERM": "xterm", "NO_COLOR": "1"})})
	s.Step("x")
	if !s.Live() || !strings.Contains(out.String(), "x") {
		t.Errorf("NO_COLOR must not turn the line off: %q", out.String())
	}
}

func TestStatusLineOrdering(t *testing.T) {
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	s := newLine(&out, StatusOptions{Terminal: true, Now: func() time.Time { return now }})
	s.Show(func() string { return "2 of 4 settled" })
	if want := clearLine + "⠋ 2 of 4 settled  0s"; out.String() != want {
		t.Fatalf("first draw %q, want %q", out.String(), want)
	}
	out.Reset()
	s.Write([]byte("event\n"))
	want := clearLine + "event\n" + clearLine + "⠋ 2 of 4 settled  0s"
	if out.String() != want {
		t.Errorf("write %q, want %q", out.String(), want)
	}
	out.Reset()
	now = now.Add(65 * time.Second)
	s.Tick()
	if want := clearLine + "⠙ 2 of 4 settled  1m05s"; out.String() != want {
		t.Errorf("tick %q, want %q", out.String(), want)
	}
	out.Reset()
	s.Step("next")
	if !strings.HasSuffix(out.String(), "⠙ next  0s") {
		t.Errorf("Step restarts the time: %q", out.String())
	}
}

func TestStatusLineWaitsForPartialLine(t *testing.T) {
	var out bytes.Buffer
	s := newLine(&out, StatusOptions{Terminal: true})
	s.Show(func() string { return "s" })
	out.Reset()
	s.Write([]byte("partial"))
	if out.String() != clearLine+"partial" {
		t.Errorf("status drawn after a partial line: %q", out.String())
	}
	out.Reset()
	s.Write([]byte(" done\n"))
	if !strings.HasSuffix(out.String(), "⠋ s  0s") {
		t.Errorf("status not back after the line ended: %q", out.String())
	}
}

func TestStatusLineTruncates(t *testing.T) {
	var out bytes.Buffer
	cols := 20
	s := newLine(&out, StatusOptions{Terminal: true, Style: Colored(), Columns: func() int { return cols }})
	s.Show(func() string { return "\x1b[31m" + strings.Repeat("abcdefghij", 10) + "\x1b[0m" })
	if w := Width(strings.TrimPrefix(out.String(), clearLine)); w != 19 {
		t.Errorf("width %d, want 19 (columns minus 1): %q", w, out.String())
	}
	if !strings.Contains(Plain(out.String()), "…") {
		t.Errorf("cut text ends with an ellipsis: %q", Plain(out.String()))
	}
	cols = 10 // a resize applies at the next draw
	out.Reset()
	s.Tick()
	if w := Width(strings.TrimPrefix(out.String(), clearLine)); w != 9 {
		t.Errorf("after resize width %d, want 9: %q", w, out.String())
	}
}

func TestStatusLineWidthFallbacks(t *testing.T) {
	s := NewStatusLine(&bytes.Buffer{}, StatusOptions{Interval: -1})
	if s.width() != 80 {
		t.Errorf("default width %d", s.width())
	}
	s = NewStatusLine(&bytes.Buffer{}, StatusOptions{Interval: -1, Getenv: env(map[string]string{"COLUMNS": "50"}), Columns: func() int { return 0 }})
	if s.width() != 50 {
		t.Errorf("COLUMNS width %d", s.width())
	}
}

func TestStatusLineStopLeavesNothing(t *testing.T) {
	var out bytes.Buffer
	s := newLine(&out, StatusOptions{Terminal: true, Style: Colored()})
	s.Show(func() string { return "busy" })
	s.Write([]byte("line\n"))
	s.Tick()
	s.Stop()
	s.Stop() // twice is fine
	if !strings.HasSuffix(out.String(), clearLine) {
		t.Errorf("Stop must end with the line cleared: %q", out.String())
	}
	n := out.Len()
	s.Tick()
	s.Show(func() string { return "again" })
	s.Write([]byte("after\n"))
	if got := out.String()[n:]; got != "after\n" {
		t.Errorf("after Stop writes pass through, got %q", got)
	}
}

func TestStatusLineStopWithRealTicker(t *testing.T) {
	var out bytes.Buffer
	s := NewStatusLine(&out, StatusOptions{Terminal: true, Interval: time.Millisecond, Getenv: env(nil)})
	s.Show(func() string { return "x" })
	time.Sleep(10 * time.Millisecond)
	s.Stop()
	n := out.Len() // Stop returned: no goroutine writes now
	time.Sleep(10 * time.Millisecond)
	if out.Len() != n {
		t.Error("the ticker kept drawing after Stop")
	}
}

func TestStatusLineOverSharesTheLine(t *testing.T) {
	var out, errs bytes.Buffer
	s := newLine(&out, StatusOptions{Terminal: true})
	s.Show(func() string { return "s" })
	out.Reset()
	s.Over(&errs).Write([]byte("err\n"))
	if errs.String() != "err\n" || !strings.HasPrefix(out.String(), clearLine) || !strings.HasSuffix(out.String(), "s  0s") {
		t.Errorf("stderr %q, stdout %q", errs.String(), out.String())
	}
}

func TestStatusLineConcurrentWrites(t *testing.T) {
	var out bytes.Buffer
	s := newLine(&out, StatusOptions{Terminal: true})
	s.Show(func() string { return "busy" })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Write([]byte("line\n"))
				s.Tick()
				s.Step("step")
			}
		}()
	}
	wg.Wait()
	s.Stop()
	if got := strings.Count(out.String(), "line\n"); got != 400 {
		t.Errorf("%d lines, want 400", got)
	}
}

func TestElapsed(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Second: "0s", 8 * time.Second: "8s", 185 * time.Second: "3m05s", 3720 * time.Second: "1h02m"} {
		if got := Elapsed(d); got != want {
			t.Errorf("Elapsed(%s) = %q, want %q", d, got, want)
		}
	}
}
