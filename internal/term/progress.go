package term

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// clearLine returns the cursor to the start of the line and erases it.
const clearLine = "\r\x1b[K"

var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// StatusOptions configures a StatusLine. The zero value of each field has a default.
type StatusOptions struct {
	Terminal bool                // whether out is a terminal
	Getenv   func(string) string // for TERM and COLUMNS; nil reads nothing
	Style    Style               // styles the spinner and the text
	Columns  func() int          // the terminal's width now, so a resize applies; nil or 0 means unknown
	Now      func() time.Time    // nil means time.Now
	// Interval is the spinner's step: 0 means 125ms; negative starts no ticker, so tests call Tick themselves.
	Interval time.Duration
}

// StatusLine keeps one live line (a spinner, a text and the time elapsed) at the bottom of a terminal. Lines written
// through it print above that line: it is cleared, the text is written and the line is drawn again. It is safe for
// concurrent use. Where it is not live (not a terminal, or TERM=dumb) it passes writes through untouched and setting
// the status does nothing, so redirected output is exactly what the command prints. NO_COLOR only removes color, not
// the line.
type StatusLine struct {
	out   io.Writer
	live  bool
	style Style
	cols  func() int
	env   func(string) string
	now   func() time.Time

	mu      sync.Mutex
	text    func() string // nil: no status is shown
	since   time.Time
	frame   int
	drawn   bool // the status line is on screen now
	midLine bool // out holds a partial line: the status waits for its end, or it would be glued to it
	stopped bool
	quit    chan struct{}
	done    chan struct{}
}

// NewStatusLine wraps out. Call Stop when done (defer it): it ends the ticker and leaves the line cleared.
func NewStatusLine(out io.Writer, o StatusOptions) *StatusLine {
	s := &StatusLine{out: out, style: o.Style, cols: o.Columns, env: o.Getenv, now: o.Now}
	if s.now == nil {
		s.now = time.Now
	}
	s.live = o.Terminal && (o.Getenv == nil || o.Getenv("TERM") != "dumb")
	s.since = s.now()
	if s.live && o.Interval >= 0 {
		interval := o.Interval
		if interval == 0 {
			interval = 125 * time.Millisecond
		}
		s.quit, s.done = make(chan struct{}), make(chan struct{})
		go s.run(interval)
	}
	return s
}

func (s *StatusLine) run(interval time.Duration) {
	defer close(s.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Tick()
		case <-s.quit:
			return
		}
	}
}

// Live is whether the line is drawn at all.
func (s *StatusLine) Live() bool { return s.live }

// Write prints p above the status line.
func (s *StatusLine) Write(p []byte) (int, error) { return s.through(s.out, p, true) }

// Over returns a writer for another stream that shares the terminal (standard error): what it writes also prints
// above the status line.
func (s *StatusLine) Over(w io.Writer) io.Writer { return overWriter{s, w} }

type overWriter struct {
	s *StatusLine
	w io.Writer
}

func (o overWriter) Write(p []byte) (int, error) { return o.s.through(o.w, p, false) }

func (s *StatusLine) through(w io.Writer, p []byte, main bool) (int, error) {
	if !s.live {
		return w.Write(p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLocked()
	n, err := w.Write(p)
	if main && len(p) > 0 {
		s.midLine = p[len(p)-1] != '\n'
	}
	s.drawLocked()
	return n, err
}

// Show sets what the line says: text is called (under the line's lock, so it must not write to the line) each time it
// is drawn, so a countdown stays current. It returns plain text; the line adds the spinner, the elapsed time and the
// style. The elapsed time counts from the line's creation or the last Step.
func (s *StatusLine) Show(text func() string) {
	if !s.live {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text = text
	s.redrawLocked()
}

// Step shows text and restarts the elapsed time: for one step of a sequence.
func (s *StatusLine) Step(text string) {
	if !s.live {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text = func() string { return text }
	s.since = s.now()
	s.redrawLocked()
}

// Tick advances the spinner and redraws. The ticker calls it; tests call it directly.
func (s *StatusLine) Tick() {
	if !s.live {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frame++
	s.redrawLocked()
}

// Stop ends the ticker and clears the line, leaving nothing behind. It is safe to call more than once; writes after
// it pass through.
func (s *StatusLine) Stop() {
	if !s.live {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.clearLocked()
	s.text = nil
	s.mu.Unlock()
	if s.quit != nil {
		close(s.quit)
		<-s.done
	}
}

// redrawLocked replaces the line: a draw starts by clearing it, so the old line is cleared only when nothing is drawn.
func (s *StatusLine) redrawLocked() {
	if !s.drawLocked() {
		s.clearLocked()
	}
}

func (s *StatusLine) clearLocked() {
	if s.drawn {
		io.WriteString(s.out, clearLine)
		s.drawn = false
	}
}

// drawLocked draws the line without a newline, cut to fit the terminal: a line that wrapped could not be cleared. It
// reports whether it drew.
func (s *StatusLine) drawLocked() bool {
	if s.text == nil || s.stopped || s.midLine {
		return false
	}
	frame := string(spinnerFrames[s.frame%len(spinnerFrames)])
	plain := frame + " " + Plain(s.text()) + "  " + Elapsed(s.now().Sub(s.since))
	plain = truncate(plain, s.width()-1)
	if plain == "" {
		return false
	}
	_, size := utf8.DecodeRuneInString(plain)
	var b bytes.Buffer
	b.WriteString(clearLine)
	b.WriteString(s.style.Command(plain[:size]))
	if rest := plain[size:]; rest != "" {
		b.WriteString(s.style.Note(rest))
	}
	if _, err := s.out.Write(b.Bytes()); err != nil {
		return false
	}
	s.drawn = true
	return true
}

// width is the terminal's columns: measured, else $COLUMNS, else 80.
func (s *StatusLine) width() int {
	if s.cols != nil {
		if c := s.cols(); c > 0 {
			return c
		}
	}
	if s.env != nil {
		if c, err := strconv.Atoi(s.env("COLUMNS")); err == nil && c > 0 {
			return c
		}
	}
	return 80
}

// truncate cuts plain text to at most max cells, ending with an ellipsis when it cut (a single cell gets no ellipsis).
func truncate(text string, max int) string {
	if max <= 0 {
		return ""
	}
	if max == 1 {
		return Truncate(text, 1, "")
	}
	return Truncate(text, max, "…")
}

// Elapsed formats a duration for a status line: 8s, 3m05s, 1h02m.
func Elapsed(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 0:
		s = 0
		fallthrough
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s/60%60)
}
