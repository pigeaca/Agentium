package term

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Frame draws the live region: the lines to show in width cells and at most height lines (lines past it are cut, so a
// frame that may be taller should compact itself), and the spinner's tick. It runs on the display's writer goroutine,
// so it must only read what the caller will not change: build it from a snapshot (a copy) of the state it shows. It
// must not call the display's methods: Flush and a Log that waits for room would deadlock. It may run again for a
// resize or a spinner tick.
type Frame func(width, height, tick int) []string

// Display is where a long command writes: a log of lines, and on a terminal a live region under them that is redrawn
// in place, as docker build does. Lines logged print above the region and stay in the scrollback. All its methods are
// safe for concurrent use. On a terminal, lines and frames lose every control character and escape code but styles
// and newlines, so nothing in them can move the cursor; still Sanitize text from outside Agentium before styling it,
// for the plain display passes it through as it is. Write nothing to the same terminal except through it while it is open.
type Display interface {
	// Live reports whether the region is drawn; when false, Update does nothing and lines print as they come.
	Live() bool
	// Update replaces the region with f (nil hides it). It returns at once; the region is redrawn soon after.
	Update(f Frame)
	// Log prints text and a newline above the region. It waits while more than MaxPending lines are queued (a terminal
	// that cannot keep up), so memory stays bounded.
	Log(text string)
	// Writer is a writer for the display's output that prints each whole line written to it above the region, and
	// what is left of a last, unfinished line at Close. Each call makes a new writer that the display keeps until
	// Close: get one and reuse it.
	Writer() io.Writer
	// Over is like Writer for another stream on the same terminal (standard error): its lines go to w, in order with
	// the display's own.
	Over(w io.Writer) io.Writer
	// Flush draws what is pending now, ignoring the throttle, and returns when it is on screen.
	Flush()
	// Close prints what is pending, clears the region, resets every style and shows the cursor. It is safe to call more
	// than once, and from a deferred call during a panic. It returns the first error writing or drawing (to an Over
	// stream included). Lines logged after it print plainly.
	Close() error
}

// DisplayOptions configures NewDisplay. The zero value of each field has a default.
type DisplayOptions struct {
	// Force draws the live region on a terminal even when color is off (--view dashboard with NO_COLOR). Nothing ever
	// draws it off a terminal, on TERM=dumb or on a terminal narrower than MinWidth.
	Force bool
	// Size is the terminal's columns and rows now, read before each frame so a resize applies; nil, or zeros, means the
	// capabilities' Width and Height.
	Size func() (cols, rows int)
	// MinInterval is the least time between two redraws: 0 means 100ms, at most 10 a second.
	MinInterval time.Duration
	// Spin is the spinner's step, which redraws the region when its lines change: 0 means 125ms; negative means none.
	Spin time.Duration
}

// NewDisplay returns a live display on out when c is a terminal at least MinWidth wide with color on (or Force), and
// otherwise a plain one that writes each line as it comes and never an escape code of its own. Cancelling ctx clears
// the live region at once, so an interrupt message prints on a clean screen; lines still print until Close. Always
// call Close (defer it): it stops the writer goroutine.
func NewDisplay(ctx context.Context, out io.Writer, c Capabilities, o DisplayOptions) Display {
	if !c.Terminal || c.Width < MinWidth || (c.Color == NoColor && !o.Force) {
		return &printer{out: out}
	}
	size := func() (int, int) {
		cols, rows := 0, 0
		if o.Size != nil {
			cols, rows = o.Size()
		}
		if cols <= 0 {
			cols = c.Width
		}
		if rows <= 0 {
			rows = c.Height
		}
		return cols, rows
	}
	interval := o.MinInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	var ticks <-chan time.Time
	var stopTicker func()
	if o.Spin >= 0 {
		spin := o.Spin
		if spin == 0 {
			spin = 125 * time.Millisecond
		}
		t := time.NewTicker(spin)
		ticks, stopTicker = t.C, t.Stop
	}
	r := newRegion(out, size, interval, ticks)
	go func() {
		if stopTicker != nil {
			defer stopTicker()
		}
		r.run(ctx)
	}()
	return r
}

// printer is the plain display: lines as they come.
type printer struct {
	mu  sync.Mutex
	out io.Writer
}

func (p *printer) Live() bool   { return false }
func (p *printer) Update(Frame) {}
func (p *printer) Flush()       {}
func (p *printer) Close() error { return nil }

func (p *printer) Log(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	io.WriteString(p.out, text+"\n")
}

func (p *printer) Writer() io.Writer { return p.Over(p.out) }

// Over passes writes through untouched, one at a time.
func (p *printer) Over(w io.Writer) io.Writer { return lockedWriter{&p.mu, w} }

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// Escape codes of the live region. Synchronized output (DEC mode 2026) makes a terminal show a redraw at once instead
// of line by line; terminals without it ignore it.
const (
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
	syncStart  = "\x1b[?2026h"
	syncEnd    = "\x1b[?2026l"
	eraseDown  = "\r\x1b[J" // to the start of the line, then erase it and everything below
)

// MaxPending is how many lines a live display queues before Log waits for the terminal to take them.
const MaxPending = 1000

// logEntry is a line to print above the region, on another stream's writer (nil: the display's output).
type logEntry struct {
	to   *lineWriter
	text string
}

// region is the live display. Every write to out happens on the writer goroutine (run), or after it ended (Close),
// so frames and lines never interleave. The cursor rests at the start of the line under the region.
type region struct {
	out   io.Writer
	size  func() (cols, rows int)
	every time.Duration
	ticks <-chan time.Time

	mu       sync.Mutex
	space    *sync.Cond // on mu: the queue has room again, or the display closed
	frame    Frame
	frameGen int // counts Updates, so a frame that panicked is dropped only if it is still the current one
	pending  []logEntry
	writers  []*lineWriter
	closed   bool
	err      error
	passMu   sync.Mutex // orders lines logged after the writer goroutine ended

	wake    chan struct{} // capacity 1: something changed
	flushes chan chan struct{}
	quit    chan struct{}
	done    chan struct{}

	// The writer goroutine's own state.
	tick     int
	drawn    []string // the region's lines on screen now
	drawnAt  int      // the width they were drawn at
	hidden   bool     // the cursor is hidden
	stopped  bool     // ctx is done or Close was called: the region is cleared and not drawn again
	finished bool     // the styles were reset and the cursor shown after stopping
	broken   bool     // out failed: nothing more is written to it
}

func newRegion(out io.Writer, size func() (int, int), every time.Duration, ticks <-chan time.Time) *region {
	r := &region{out: out, size: size, every: every, ticks: ticks, wake: make(chan struct{}, 1),
		flushes: make(chan chan struct{}), quit: make(chan struct{}), done: make(chan struct{})}
	r.space = sync.NewCond(&r.mu)
	return r
}

func (r *region) Live() bool { return true }

func (r *region) Update(f Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.frame = f
	r.frameGen++
	r.signal()
}

func (r *region) Log(text string) { r.logTo(nil, text) }

// logTo queues text for to (nil: the display's output), waiting while the queue is full.
func (r *region) logTo(to *lineWriter, text string) {
	if to != nil && to.w == nil {
		to = nil
	}
	r.mu.Lock()
	for !r.closed && len(r.pending) >= MaxPending {
		r.signal() // the writer goroutine draws at once when the queue is full
		r.space.Wait()
	}
	if !r.closed {
		r.pending = append(r.pending, logEntry{to, text})
		r.signal()
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	// Closed: once the writer goroutine has ended, print plainly.
	<-r.done
	r.passMu.Lock()
	defer r.passMu.Unlock()
	w := r.out
	if to != nil {
		if to.err != nil {
			return
		}
		w = to.w
	}
	io.WriteString(w, endStyles(keepStyles(text))+"\n")
}

// endStyles ends each line of text that has a style with a reset, so no style (a background, hidden text) outlives it.
func endStyles(text string) string {
	if !strings.Contains(text, "\x1b[") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.Contains(line, "\x1b[") {
			lines[i] = line + reset
		}
	}
	return strings.Join(lines, "\n")
}

// signal wakes the writer goroutine; r.mu is held.
func (r *region) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *region) Writer() io.Writer { return r.newLineWriter(nil) }

func (r *region) Over(w io.Writer) io.Writer { return r.newLineWriter(w) }

func (r *region) newLineWriter(w io.Writer) *lineWriter {
	lw := &lineWriter{r: r, w: w}
	r.mu.Lock()
	r.writers = append(r.writers, lw)
	r.mu.Unlock()
	return lw
}

func (r *region) Flush() {
	ack := make(chan struct{})
	select {
	case r.flushes <- ack:
		<-ack
	case <-r.done:
	}
}

func (r *region) Close() error {
	r.mu.Lock()
	writers := r.writers
	r.mu.Unlock()
	for _, lw := range writers {
		lw.finish()
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.quit)
		r.space.Broadcast()
	}
	r.mu.Unlock()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *region) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = err
	}
}

// run is the writer goroutine: it draws on a change at most once per r.every, at once for a flush, and for the last
// time when ctx is done or Close is called.
func (r *region) run(ctx context.Context) {
	defer close(r.done)
	var last time.Time
	var later *time.Timer
	var laterC <-chan time.Time
	draw := func() {
		if later != nil {
			later.Stop()
			later, laterC = nil, nil
		}
		r.draw()
		last = time.Now()
	}
	request := func() {
		r.mu.Lock()
		full := len(r.pending) >= MaxPending
		r.mu.Unlock()
		if full {
			draw() // Log is waiting for room
			return
		}
		if laterC != nil {
			return // a redraw is already due
		}
		if wait := r.every - time.Since(last); wait > 0 {
			later = time.NewTimer(wait)
			laterC = later.C
			return
		}
		draw()
	}
	ctxDone := ctx.Done()
	for {
		select {
		case <-r.wake:
			request()
		case <-r.ticks:
			r.tick++
			request()
		case <-laterC:
			later, laterC = nil, nil
			draw()
		case ack := <-r.flushes:
			draw()
			close(ack)
		case <-ctxDone:
			ctxDone = nil
			r.stopped = true
			draw()
		case <-r.quit:
			r.stopped = true
			draw()
			return
		}
	}
}

// draw brings the screen up to date in one write: it moves to the region's first line, erases it and everything
// below, prints the pending lines and draws the region again. It writes nothing when nothing changed.
func (r *region) draw() {
	r.mu.Lock()
	logs := r.pending
	r.pending = nil
	r.space.Broadcast()
	frame, gen := r.frame, r.frameGen
	r.mu.Unlock()
	if r.broken {
		return
	}
	cols, rows := r.size()
	var lines []string
	if frame != nil && !r.stopped {
		lines = r.render(frame, gen, cols, rows)
	}
	final := r.stopped && !r.finished
	if len(logs) == 0 && cols == r.drawnAt && slices.Equal(lines, r.drawn) && !final {
		return
	}
	var b bytes.Buffer
	redraw := len(r.drawn) > 0 || len(lines) > 0
	if redraw {
		b.WriteString(syncStart)
	}
	if up := r.drawnRows(cols); up > 0 {
		b.WriteString("\x1b[" + strconv.Itoa(up) + "A" + eraseDown)
	}
	for _, l := range logs {
		text := endStyles(keepStyles(l.text))
		if l.to == nil {
			b.WriteString(text + "\n")
			continue
		}
		if l.to.err != nil {
			continue // that stream failed before: its lines are dropped, the rest goes on
		}
		// A line for another stream: what is before it goes out first.
		if !r.write(b.Bytes()) {
			return
		}
		b.Reset()
		if _, err := l.to.w.Write([]byte(text + "\n")); err != nil {
			l.to.err = err
			r.fail(fmt.Errorf("print above the live region: %w", err))
		}
	}
	if len(lines) > 0 && !r.hidden {
		b.WriteString(hideCursor)
		r.hidden = true
	}
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	if final {
		// Whatever a line left open, and whether or not the cursor was hidden: the terminal is the user's again.
		b.WriteString(reset + showCursor)
		r.hidden, r.finished = false, true
	}
	if redraw {
		b.WriteString(syncEnd)
	}
	r.drawn, r.drawnAt = lines, cols
	r.write(b.Bytes())
}

// write writes p to the display's output, and on an error records it and stops all drawing.
func (r *region) write(p []byte) bool {
	if len(p) == 0 {
		return true
	}
	if _, err := r.out.Write(p); err != nil {
		r.broken = true
		r.fail(fmt.Errorf("draw the live region: %w", err))
		return false
	}
	return true
}

// drawnRows is how many terminal rows the region's lines take now at cols columns. Each was drawn narrower than the
// terminal; when it has narrowed since, a terminal that reflows its lines (as most do) wraps each onto more rows.
func (r *region) drawnRows(cols int) int {
	n := 0
	for _, line := range r.drawn {
		w := Width(line)
		if cols >= r.drawnAt || w < cols || cols <= 0 {
			n++
			continue
		}
		n += (w + cols - 1) / cols
	}
	return n
}

// render calls the frame and fits its lines: control characters and escape codes other than styles are removed, each
// line is cut to cols-1 cells, so the cursor never waits at a line's end, and ends with a reset when it has a style,
// and at most rows-1 lines are kept, so the region never scrolls its own first line away. A frame that panics is
// dropped and the panic becomes Close's error.
func (r *region) render(f Frame, gen, cols, rows int) (lines []string) {
	defer func() {
		if p := recover(); p != nil {
			r.fail(fmt.Errorf("draw the live region: the frame panicked: %v", p))
			r.mu.Lock()
			if r.frameGen == gen {
				r.frame = nil
			}
			r.mu.Unlock()
			lines = nil
		}
	}()
	width, height := max(cols-1, 1), max(rows-1, 1)
	for _, entry := range f(width, height, r.tick) {
		for _, line := range strings.Split(entry, "\n") {
			lines = append(lines, endStyles(Truncate(keepStyles(line), width, "")))
		}
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return lines
}

// lineWriter turns writes into lines logged above the region.
type lineWriter struct {
	r   *region
	w   io.Writer // nil: the region's output
	err error     // the first error writing to w: the writer goroutine's, then (after it ended) logTo's
	mu  sync.Mutex
	buf []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.r.logTo(l, string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

// finish logs an unfinished last line.
func (l *lineWriter) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.r.logTo(l, string(l.buf))
		l.buf = nil
	}
}
