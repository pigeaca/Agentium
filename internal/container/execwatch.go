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

// execWatch reads the daemon's own record of marked execs from `docker events`: each exec whose argv ends with one of
// its nonces (only Agentium's argv can: the grade has no docker client), and the exit code the daemon logged when the
// exec ended. The stream runs beside the execs and replays from the container's creation, in the daemon's own clock, so
// no clock is compared. Its first exec is a marker (markReady): once the daemon's record of the marker's end arrives,
// the subscription is live, so the exec after it cannot be missed however busy the daemon is. The daemon can deliver
// one event twice (from the replay and live, when the subscription lands between its buffering and its publishing), so
// a repeat with the same exec ID and exit code is ignored, and only a conflicting one is an error. The stream is a
// child of the caller, stopped (its process group killed) by stop; should Agentium die first, it still ends by itself
// shortly after the container's deadline (--until, in the daemon's clock), as the container does.
type execWatch struct {
	container string // the container's full ID
	cancel    context.CancelFunc
	ended     chan struct{} // closed when the events client has exited
	changed   chan struct{} // signalled on every change of the state below

	mu     sync.Mutex
	line   []byte // the partial line so far
	long   bool   // the current line is past maxEventLine, and is dropped
	execs  map[string]*execRecord
	died   bool  // the container died
	bad    error // an event about a marked exec that could not be read, or that conflicts with an earlier one
	runErr error // why the events client ended
	stderr *capped
}

// execRecord is the daemon's record of one marked exec so far.
type execRecord struct {
	id   string // the exec's ID, once its exec_start was seen
	exit *int   // its exit code, once its exec_die was seen
}

func newExecWatch(container string, nonces ...string) *execWatch {
	w := &execWatch{container: container, ended: make(chan struct{}), changed: make(chan struct{}, 1), stderr: &capped{max: 4 << 10},
		execs: map[string]*execRecord{}}
	for _, n := range nonces {
		w.execs[n] = &execRecord{}
	}
	return w
}

// newNonce is a random marker for one exec's argv.
func newNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("exec marker: %w", err)
	}
	return "agentium-exec-" + hex.EncodeToString(b), nil
}

// watchExec starts the events stream for the execs marked by nonces. The caller stops it.
func (c *Container) watchExec(ctx context.Context, nonces ...string) *execWatch {
	w := newExecWatch(c.id, nonces...)
	wctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
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

// event reads one event. Events of other execs (the probes, the counters, the copy-in) are ignored.
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
	id := ev.Actor.Attributes["execID"]
	switch {
	case strings.HasPrefix(ev.Action, "exec_start: "):
		for nonce, rec := range w.execs {
			if !strings.HasSuffix(ev.Action, " "+nonce) {
				continue
			}
			switch {
			case id == "":
				w.fail(fmt.Errorf("the daemon's exec_start has no exec ID"))
			case rec.id == "":
				rec.id = id
			case rec.id != id:
				w.fail(fmt.Errorf("two execs carry one marker"))
			}
		}
	case ev.Action == "exec_die" && id != "":
		for _, rec := range w.execs {
			if rec.id != id {
				continue
			}
			code, err := strconv.Atoi(ev.Actor.Attributes["exitCode"])
			switch {
			case err != nil:
				w.fail(fmt.Errorf("the daemon's exec_die has the exit code %q", ev.Actor.Attributes["exitCode"]))
			case rec.exit == nil:
				rec.exit = &code
			case *rec.exit != code:
				w.fail(fmt.Errorf("the daemon recorded two exit codes for one exec, %d and %d", *rec.exit, code))
			}
		}
	case ev.Action == "die":
		w.died = true
	}
}

// fail keeps the first unreadable or conflicting record.
func (w *execWatch) fail(err error) {
	if w.bad == nil {
		w.bad = err
	}
}

// failureLocked is why the watch can no longer be trusted, if it cannot: a record it could not read or that
// conflicts, the container's end, or the stream's end.
func (w *execWatch) failureLocked() error {
	switch {
	case w.bad != nil:
		return w.bad
	case w.died:
		return errors.New("the container ended")
	case w.runErr != nil:
		return w.runErr
	}
	return nil
}

// running waits at most limit for the daemon's record of the exec marked by nonce starting, and reports whether it is
// still running: started, not ended, and the watch healthy. A watch that failed is an error even after the start was
// seen, since the record can no longer show the exec ending.
func (w *execWatch) running(ctx context.Context, nonce string, limit time.Duration) error {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		w.mu.Lock()
		rec, failure := *w.execs[nonce], w.failureLocked()
		w.mu.Unlock()
		switch {
		case failure != nil:
			return failure
		case rec.exit != nil:
			return fmt.Errorf("the daemon recorded the command's end (exit %d)", *rec.exit)
		case rec.id != "":
			return nil
		}
		select {
		case <-w.changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("no record of the command starting within %s", limit)
		}
	}
}

// wait returns the exit code the daemon recorded for the exec marked by nonce, waiting at most limit for it. No record
// of its end (the exec never started, it is still running though its client returned, the container died first, or
// the stream ended) is an error, and so is a record that could not be read or conflicts.
func (w *execWatch) wait(ctx context.Context, nonce string, limit time.Duration) (int, error) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		w.mu.Lock()
		rec, bad, failure := *w.execs[nonce], w.bad, w.failureLocked()
		w.mu.Unlock()
		switch {
		case bad != nil:
			return 0, bad
		case rec.exit != nil:
			return *rec.exit, nil
		case failure != nil:
			return 0, failure
		}
		select {
		case <-w.changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-timer.C:
			if rec.id == "" {
				return 0, fmt.Errorf("no record of the exec starting within %s", limit)
			}
			return 0, fmt.Errorf("no record of the exec ending within %s", limit)
		}
	}
}

// markReady runs a no-op exec marked by marker, as MainUser (which the grade cannot signal), and waits for the
// daemon's record of its end on w. After that, w's subscription is live and a record of the next exec cannot be
// missed; and a daemon whose events cannot judge a command (an old engine without exec IDs or exit codes, a socket
// proxy that blocks events, a client that writes warnings) fails here, before the command runs.
func (c *Container) markReady(ctx context.Context, w *execWatch, marker string) error {
	_, stderr, res, err := c.d.call(ctx, c.execArgs(MainUser, false, nil, "/", markerArgv(marker)...), nil, controlTimeout)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 || strings.TrimSpace(stderr) != "" {
		return fmt.Errorf("the marker exited %d: %s", res.ExitCode, firstLine(stderr))
	}
	exit, err := w.wait(ctx, marker, c.d.statusWait)
	if err != nil {
		return fmt.Errorf("the daemon's record of the marker: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("the daemon recorded the marker's exit as %d", exit)
	}
	return nil
}

// markerArgv is the marker's argv: a no-op shell with the marker as its last argument.
func markerArgv(marker string) []string { return []string{"sh", "-c", ":", marker} }
