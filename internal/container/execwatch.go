package container

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// maxEventLine caps one line of the events stream; an exec's line holds its whole command.
const maxEventLine = 1 << 20

// execWatch reads the daemon's own record of one command's exec from `docker events`: the exec whose argv ends with
// its nonce (only Agentium's argv can: the grade has no docker client), and the exit code the daemon logged when the
// exec ended. The stream runs beside the exec and replays from the container's creation, in the daemon's own clock,
// so neither a late subscription nor a long command loses the exec's events, and no clock is compared. It is a child
// of the exec's call, stopped (its process group killed) when Exec returns; should Agentium die first, the stream
// still ends by itself shortly after the container's deadline (--until, in the daemon's clock), as the container does.
type execWatch struct {
	nonce     string
	container string // the container's full ID
	cancel    context.CancelFunc
	ended     chan struct{} // closed when the events client has exited
	changed   chan struct{} // signalled on every change of the state below

	mu     sync.Mutex
	line   []byte // the partial line so far
	long   bool   // the current line is past maxEventLine, and is dropped
	execID string // the exec's ID, once its exec_start was seen
	exit   *int   // its exit code, once its exec_die was seen
	died   bool   // the container died
	bad    error  // an event about the exec that could not be read
	runErr error  // why the events client ended
	stderr *capped
}

// newNonce is a random marker for one exec's argv.
func newNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("exec marker: %w", err)
	}
	return "agentium-exec-" + hex.EncodeToString(b), nil
}

// watchExec starts the events stream for the exec marked by nonce. The caller stops it.
func (c *Container) watchExec(ctx context.Context, nonce string) *execWatch {
	wctx, cancel := context.WithCancel(ctx)
	w := &execWatch{nonce: nonce, container: c.id, cancel: cancel, ended: make(chan struct{}), changed: make(chan struct{}, 1), stderr: &capped{max: 4 << 10}}
	args := []string{"events", "--since", c.created, "--until", c.until, "--filter", "type=container", "--filter", "container=" + c.id,
		"--filter", "event=exec_start", "--filter", "event=exec_die", "--filter", "event=die", "--format", "{{json .}}"}
	go func() {
		defer close(w.ended)
		_, err := runner.Run(wctx, runner.Spec{Args: append([]string{c.d.bin}, c.d.args(args)...), Environ: c.d.environ, Output: w, Stderr: w.stderr})
		w.mu.Lock()
		if err == nil {
			err = fmt.Errorf("the events stream ended: %s", firstLine(c.d.redact(w.stderr.String())))
		}
		w.runErr = err
		w.mu.Unlock()
		w.signal()
	}()
	return w
}

// stop ends the events client and waits for it.
func (w *execWatch) stop() {
	w.cancel()
	<-w.ended
}

func (w *execWatch) signal() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// Write takes the events stream, one JSON event per line.
func (w *execWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !w.long && len(w.line)+len(chunk) <= maxEventLine {
			w.line = append(w.line, chunk...)
		} else {
			w.long, w.line = true, w.line[:0]
		}
		if i < 0 {
			break
		}
		if !w.long {
			w.event(w.line)
		}
		w.line, w.long = w.line[:0], false
		p = p[i+1:]
	}
	w.signal()
	return n, nil
}

// event reads one event. Events of other execs (the probes, the counters, earlier commands) are ignored.
func (w *execWatch) event(line []byte) {
	var ev struct {
		Type   string
		Action string
		Actor  struct {
			ID         string
			Attributes map[string]string
		}
	}
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "container" || ev.Actor.ID != w.container {
		return
	}
	switch {
	case strings.HasPrefix(ev.Action, "exec_start: ") && strings.HasSuffix(ev.Action, " "+w.nonce):
		if id := ev.Actor.Attributes["execID"]; id != "" && w.execID == "" {
			w.execID = id
		} else {
			w.bad = fmt.Errorf("exec_start without an exec ID, or seen twice")
		}
	case ev.Action == "exec_die" && w.execID != "" && ev.Actor.Attributes["execID"] == w.execID:
		code, err := strconv.Atoi(ev.Actor.Attributes["exitCode"])
		if err != nil {
			w.bad = fmt.Errorf("exec_die with exit code %q", ev.Actor.Attributes["exitCode"])
			return
		}
		w.exit = &code
	case ev.Action == "die":
		w.died = true
	}
}

// running reports whether the daemon's record has the exec started, and ended, so far.
func (w *execWatch) running() (started, ended bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.execID != "", w.exit != nil
}

// wait returns the exit code the daemon recorded for the exec, waiting at most limit for it. No record of its end (the
// exec never started, it is still running though its client returned, the container died first, or the stream
// ended) is an error.
func (w *execWatch) wait(ctx context.Context, limit time.Duration) (int, error) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		w.mu.Lock()
		exit, died, bad, runErr := w.exit, w.died, w.bad, w.runErr
		w.mu.Unlock()
		switch {
		case bad != nil:
			return 0, bad
		case exit != nil:
			return *exit, nil
		case died:
			return 0, errors.New("the container ended before the command's end was recorded")
		case runErr != nil:
			return 0, runErr
		}
		select {
		case <-w.changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-timer.C:
			if started, _ := w.running(); !started {
				return 0, fmt.Errorf("no record of the command starting within %s", limit)
			}
			return 0, fmt.Errorf("no record of the command ending within %s", limit)
		}
	}
}
