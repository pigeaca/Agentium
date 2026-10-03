package term

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func liveCaps() Capabilities {
	return Capabilities{Terminal: true, Color: TrueColor, UTF8: true, Width: 80, Height: 24}
}

func frameOf(lines ...string) Frame { return func(int, int) []string { return lines } }

// startRegion runs a region on v with a fixed size, the throttle at an hour (so only the first change and Flush
// draw) and the spinner driven by ticks.
func startRegion(t *testing.T, v io.Writer, size func() (int, int), ticks chan time.Time) (*region, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if size == nil {
		size = func() (int, int) { return 80, 24 }
	}
	r := newRegion(v, size, time.Hour, ticks)
	go r.run(ctx)
	t.Cleanup(func() {
		cancel()
		r.Close()
	})
	return r, cancel
}

// assertClean checks the fake terminal after Close: exactly want on screen, the cursor shown at the start of the line
// under it, every synchronized update ended and no escape code the terminal did not know.
func assertClean(t *testing.T, v *vt, want string) {
	t.Helper()
	if got := v.screen(); got != want {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want)
	}
	row, col := v.cursor()
	if wantRow := strings.Count(want, "\n") + 1; want == "" && (row != 0 || col != 0) || want != "" && (row != wantRow || col != 0) {
		t.Errorf("cursor at row %d col %d, want the start of the line under the output", row, col)
	}
	if !v.cursorShown {
		t.Error("the cursor is still hidden")
	}
	if v.syncDepth != 0 {
		t.Errorf("synchronized output left at depth %d", v.syncDepth)
	}
	if len(v.unknown) > 0 {
		t.Errorf("unknown escape codes: %v", v.unknown)
	}
}

func TestDisplayIsPlainWhereNotLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps Capabilities
		opts DisplayOptions
	}{
		{"a pipe, color forced", Capabilities{Color: TrueColor, UTF8: true, Width: 80, Height: 24}, DisplayOptions{}},
		{"a pipe, the dashboard forced", Capabilities{Width: 80, Height: 24}, DisplayOptions{Force: true}},
		{"TERM=dumb", DetectCapabilities(true, env(map[string]string{"TERM": "dumb"}), nil), DisplayOptions{}},
		{"narrow", Capabilities{Terminal: true, Color: Basic, Width: 59, Height: 24}, DisplayOptions{Force: true}},
		{"NO_COLOR", Capabilities{Terminal: true, Width: 80, Height: 24}, DisplayOptions{}},
	} {
		var out, errs bytes.Buffer
		d := NewDisplay(context.Background(), &out, tc.caps, tc.opts)
		if d.Live() {
			t.Errorf("%s: live", tc.name)
		}
		d.Update(frameOf("region"))
		d.Log("one")
		d.Writer().Write([]byte("two\npart"))
		d.Over(&errs).Write([]byte("err\n"))
		d.Flush()
		if err := d.Close(); err != nil {
			t.Errorf("%s: Close: %v", tc.name, err)
		}
		d.Log("after")
		if got := out.String(); got != "one\ntwo\npartafter\n" || errs.String() != "err\n" {
			t.Errorf("%s: out %q, errs %q", tc.name, got, errs.String())
		}
	}
	d := NewDisplay(context.Background(), &bytes.Buffer{}, Capabilities{Terminal: true, Width: 80, Height: 24},
		DisplayOptions{Force: true, Spin: -1})
	defer d.Close()
	if !d.Live() {
		t.Error("--view dashboard with NO_COLOR on a terminal draws the region")
	}
}

func TestRegionDrawsAboveAndClears(t *testing.T) {
	v := newVT(80)
	r, _ := startRegion(t, v, nil, nil)
	r.Log("before")
	r.Flush()
	if v.screen() != "before" {
		t.Fatalf("screen %q", v.screen())
	}
	r.Update(frameOf("status", "line 2"))
	r.Flush()
	if got := v.screen(); got != "before\nstatus\nline 2" || v.cursorShown {
		t.Fatalf("screen %q, cursor shown %v", got, v.cursorShown)
	}
	r.Log("event 1")
	r.Log("event 2")
	r.Update(frameOf("status 2"))
	r.Flush()
	if got := v.screen(); got != "before\nevent 1\nevent 2\nstatus 2" {
		t.Fatalf("screen %q", got)
	}
	r.Update(nil)
	r.Flush()
	if got := v.screen(); got != "before\nevent 1\nevent 2" {
		t.Fatalf("a nil frame hides the region: %q", got)
	}
	r.Update(frameOf("back"))
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	assertClean(t, v, "before\nevent 1\nevent 2")
	if err := r.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	r.Update(frameOf("ignored"))
	r.Log("after")
	r.Flush()
	assertClean(t, v, "before\nevent 1\nevent 2\nafter")
}

func TestRegionRedrawsOnlyOnChange(t *testing.T) {
	v := newVT(80)
	ticks := make(chan time.Time)
	r, _ := startRegion(t, v, nil, ticks)
	r.Update(frameOf("still"))
	r.Flush()
	writes := v.writes
	r.Update(frameOf("still"))
	ticks <- time.Now()
	r.Flush()
	if v.writes != writes {
		t.Errorf("an unchanged frame was redrawn: %d writes, want %d", v.writes, writes)
	}
	r.Update(func(_, tick int) []string { return []string{fmt.Sprint("spin ", tick)} })
	r.Flush()
	ticks <- time.Now()
	r.Flush()
	if got := v.screen(); got != "spin 2" {
		t.Errorf("after a tick: %q", got)
	}
}

func TestRegionThrottles(t *testing.T) {
	v := newVT(80)
	r, _ := startRegion(t, v, nil, nil)
	for i := 0; i < 100; i++ {
		r.Update(frameOf(fmt.Sprint("frame ", i)))
	}
	r.Flush()
	if n := strings.Count(v.rawString(), syncStart); n > 2 {
		t.Errorf("%d draws for 100 updates inside the throttle, want the first and the flush", n)
	}
	if got := v.screen(); got != "frame 99" {
		t.Errorf("screen %q", got)
	}
}

func TestRegionFitsTheTerminal(t *testing.T) {
	v := newVT(20)
	r, _ := startRegion(t, v, func() (int, int) { return 20, 5 }, nil)
	var widths []int
	r.Update(func(width, _ int) []string {
		widths = append(widths, width)
		lines := []string{strings.Repeat("x", 50), "日本語日本語日本語日本語"}
		for i := 0; i < 10; i++ {
			lines = append(lines, fmt.Sprint("line ", i))
		}
		return lines
	})
	r.Flush()
	want := strings.Repeat("x", 19) + "\n日本語日本語日本語\nline 0\nline 1"
	if got := v.screen(); got != want {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want)
	}
	if widths[0] != 19 {
		t.Errorf("the frame was offered %d cells, want 19", widths[0])
	}
	r.Close()
	assertClean(t, v, "")
}

func TestRegionFollowsAResize(t *testing.T) {
	v := newVT(80)
	var cols atomic.Int64
	cols.Store(80)
	r, _ := startRegion(t, v, func() (int, int) { return int(cols.Load()), 24 }, nil)
	r.Log("log a")
	r.Update(func(width, _ int) []string {
		return []string{strings.Repeat("=", width), strings.Repeat("-", width), "short"}
	})
	r.Flush()
	if got := v.screen(); got != "log a\n"+strings.Repeat("=", 79)+"\n"+strings.Repeat("-", 79)+"\nshort" {
		t.Fatalf("screen %q", got)
	}
	v.resize(40)
	cols.Store(40)
	r.Log("log b")
	r.Flush()
	want := "log a\nlog b\n" + strings.Repeat("=", 39) + "\n" + strings.Repeat("-", 39) + "\nshort"
	if got := v.screen(); got != want {
		t.Fatalf("after narrowing:\n%s\nwant:\n%s", got, want)
	}
	v.resize(100)
	cols.Store(100)
	r.Flush()
	want = "log a\nlog b\n" + strings.Repeat("=", 99) + "\n" + strings.Repeat("-", 99) + "\nshort"
	if got := v.screen(); got != want {
		t.Fatalf("after widening:\n%s\nwant:\n%s", got, want)
	}
	r.Close()
	assertClean(t, v, "log a\nlog b")
}

func TestRegionClearsOnCancel(t *testing.T) {
	v := newVT(80)
	r, cancel := startRegion(t, v, nil, nil)
	r.Log("running")
	r.Update(frameOf("busy", "busier"))
	r.Flush()
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for v.screen() != "running" || !v.cursorShown {
		if time.Now().After(deadline) {
			t.Fatalf("the region was not cleared after a cancel: %q", v.screen())
		}
		time.Sleep(time.Millisecond)
	}
	r.Update(frameOf("not drawn"))
	r.Log("interrupted")
	r.Flush()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	assertClean(t, v, "running\ninterrupted")
}

func TestDeferredCloseAfterAPanic(t *testing.T) {
	v := newVT(80)
	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Errorf("recovered %v", p)
			}
		}()
		d := NewDisplay(context.Background(), v, liveCaps(), DisplayOptions{Spin: -1})
		defer d.Close()
		d.Log("started")
		d.Update(frameOf("busy"))
		d.Flush()
		panic("boom")
	}()
	assertClean(t, v, "started")
}

func TestRegionSurvivesAPanickingFrame(t *testing.T) {
	v := newVT(80)
	r, _ := startRegion(t, v, nil, nil)
	r.Update(func(int, int) []string { panic("bad frame") })
	r.Flush()
	r.Log("still logging")
	r.Update(frameOf("a good frame"))
	r.Flush()
	if got := v.screen(); got != "still logging\na good frame" {
		t.Errorf("screen %q", got)
	}
	err := r.Close()
	if err == nil || !strings.Contains(err.Error(), "bad frame") {
		t.Errorf("Close: %v", err)
	}
	assertClean(t, v, "still logging")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestRegionStopsOnAWriteError(t *testing.T) {
	r, _ := startRegion(t, failingWriter{}, nil, nil)
	r.Update(frameOf("x"))
	r.Log("y")
	r.Flush()
	if err := r.Close(); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("Close: %v", err)
	}
}

func TestRegionWritersKeepLinesWhole(t *testing.T) {
	v := newVT(80)
	r, _ := startRegion(t, v, nil, nil)
	r.Update(frameOf("region"))
	w := r.Writer()
	w.Write([]byte("abc"))
	r.Flush()
	if got := v.screen(); got != "region" {
		t.Errorf("a partial line printed early: %q", got)
	}
	w.Write([]byte("def\nghi"))
	r.Over(v).Write([]byte("on stderr\n"))
	r.Log("logged")
	r.Flush()
	if got := v.screen(); got != "abcdef\non stderr\nlogged\nregion" {
		t.Errorf("screen %q", got)
	}
	r.Close()
	assertClean(t, v, "abcdef\non stderr\nlogged\nghi")
}

func TestRegionConcurrentUse(t *testing.T) {
	v := newVT(120)
	d := NewDisplay(context.Background(), v, liveCaps(), DisplayOptions{MinInterval: time.Millisecond, Spin: time.Millisecond,
		Size: func() (int, int) { return 120, 40 }})
	var wg sync.WaitGroup
	var want []string
	for g := 0; g < 8; g++ {
		for i := 0; i < 50; i++ {
			want = append(want, fmt.Sprintf("g%d line %d", g, i))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := d.Writer()
			for i := 0; i < 50; i++ {
				snapshot := fmt.Sprintf("g%d at %d", g, i)
				d.Update(func(_, tick int) []string { return []string{snapshot, fmt.Sprint("tick ", tick)} })
				if i%2 == 0 {
					d.Log(fmt.Sprintf("g%d line %d", g, i))
				} else {
					fmt.Fprintf(w, "g%d line %d\n", g, i)
				}
				if i%10 == 0 {
					d.Flush()
				}
			}
		}()
	}
	wg.Wait()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(v.screen(), "\n")
	if len(lines) != len(want) {
		t.Fatalf("%d lines on screen, want %d:\n%s", len(lines), len(want), v.screen())
	}
	next := map[string]int{}
	for _, line := range lines {
		var g, i int
		if _, err := fmt.Sscanf(line, "g%d line %d", &g, &i); err != nil {
			t.Fatalf("a stray line: %q", line)
		}
		key := fmt.Sprint(g)
		if i != next[key] {
			t.Fatalf("goroutine %d: line %d came when %d was due", g, i, next[key])
		}
		next[key]++
	}
	assertClean(t, v, strings.Join(lines, "\n"))
}

func TestRegionDropsCursorMovesInLines(t *testing.T) {
	v := newVT(80)
	r, _ := startRegion(t, v, nil, nil)
	r.Update(frameOf("status\x1b[5A\x1b[2Jstill one line", Colored().Good("ok")))
	r.Log("agent said \x1b[1A\x1b[Kup\rand\tover")
	r.Flush()
	if got := v.screen(); got != "agent said upand over\nstatusstill one line\nok" {
		t.Errorf("screen %q", got)
	}
	if !strings.Contains(v.rawString(), Colored().Good("ok")) {
		t.Error("styles are kept")
	}
	r.Close()
	assertClean(t, v, "agent said upand over")
}
