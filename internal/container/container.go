package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// ErrGone: the container was removed (after a timeout, a cancel or a result that cannot be judged), or its removal
// failed and is left to Run, and it runs nothing more.
var ErrGone = errors.New("the grade's container is gone")

// ErrUnjudgeable: the grade's own input was used (its code ran, or its tree was read) and the result cannot be judged:
// the counters after a command cannot be read (a fork bomb left running holds every process slot), the container
// ended mid-command, the docker client failed or its exit status is not the daemon's record of the command, the tree
// could not be copied in (too large, too many entries, changed or unreadable), or the container could not be removed
// afterwards (ErrCleanup). The grade's code can cause most of these on purpose after a failing test, and none of them
// says how the grade did, so the caller settles it as left out (as a limit hit, Counters.Hit, and infra-sandbox runs
// are), counted in the per-arm check, and never retries it: a retry would be a re-roll (isolation decision 3). Errors
// before any of the grade's input is used (Open, Usable, Fits, Image, ErrMismatch, ErrProbe, the create and start) are
// infrastructure, and may be retried. Agentium's own cancel is never ErrUnjudgeable: the error is ctx's.
var ErrUnjudgeable = errors.New("the grade's result cannot be judged")

// ErrCleanup: the grade's container (and its volume) could not be removed, and is left for recovery to remove by its
// labels (RemoveRun). It is joined to whatever else happened and never replaces it. After the grade's input was used it
// is also ErrUnjudgeable, unless ctx was cancelled; it never undoes a settled Result (see Run).
var ErrCleanup = errors.New("the grade's container could not be removed")

// errCopyEnded: docker stopped reading the copy-in's stream (it ended, failed or timed out), so the walk stops. It is
// never the tree's doing on its own: the copy-in is judged by why docker stopped.
var errCopyEnded = errors.New("the copy-in ended")

const (
	// removeTimeout bounds a removal, which runs even after ctx is cancelled.
	removeTimeout = 30 * time.Second
	// countersTimeout bounds the read of the counters after a command.
	countersTimeout = 30 * time.Second
	// statusTimeout bounds the wait for the daemon's record of a command's end after its client returned. The daemon
	// logs it as it closes the command's streams, before the client can ask for the exit code, so only a client that
	// did not see the command end (it failed) waits this long.
	statusTimeout = 10 * time.Second
)

// Container is a grade's container, started, checked and proved isolated. It exists only inside Run's function and is
// not safe for concurrent use.
type Container struct {
	d        *Docker
	spec     Spec
	name     string
	digest   string
	imageEnv []string
	id       string // the daemon's full ID of the container, for its events
	created  string // when the daemon created it, in the daemon's own clock (RFC 3339)
	until    string // a minute past its deadline, in the same clock: no event of it can come later
	gone     bool
	// stopped is set when a removal failed: the container runs nothing more, and Run tries the removal again.
	stopped bool
	// used is set once the grade's input was used: a copy-in streamed or was refused on the tree, or a command was
	// sent. A failed removal after that is ErrUnjudgeable as well as ErrCleanup.
	used bool
	// mayExist is set from the moment create is sent until the container is known to be gone. A create the daemon
	// refused (its name already in use, say) leaves it unset: the container of that name is not this grade's, and
	// removing it could kill another grade.
	mayExist bool
}

// Name is the container's name.
func (c *Container) Name() string { return c.name }

// InspectDigest is the normalized digest of the daemon's record that the inspect check accepted.
func (c *Container) InspectDigest() string { return c.digest }

// ImageEnv is the environment the container's commands inherit from the image (PATH among it), as the daemon recorded
// it; a command's Env overrides it.
func (c *Container) ImageEnv() []string { return append([]string(nil), c.imageEnv...) }

// Run gives fn a grade's container: created in spec's shape (never pulling), checked against the daemon's record
// before it starts (ErrMismatch), given its work/ and cache/ folders, started, and proved isolated from inside
// (ErrProbe). Whatever happens, on success, error, cancel or panic, the container and its volume are removed before
// Run returns (docker rm -f -v by name, bounded and outside ctx's cancellation). If Agentium dies instead, the deadline
// and --rm remove a started container, and recovery removes a created one by its labels (RemoveRun).
//
// The caller classifies what Run returns in this order (step 4 of the container plan):
//  1. A settled result stands: a Result that Exec returned with a nil error is judged as it is, whatever Run returns
//     afterwards. ErrCleanup from Run then only means that the container is left for recovery, which removes it by
//     its labels.
//  2. Agentium's own cancel: the error is ctx's (context.Canceled or DeadlineExceeded), never ErrUnjudgeable.
//  3. ErrUnjudgeable (from CopyIn, Exec, or a removal that failed after the grade's input was used): the run is left
//     out and never retried.
//  4. Anything else is infrastructure before the grade's input was used, or Agentium's own (a refused Command), and
//     may be retried. With ErrCleanup among it, the leftover still holds the grade's name, so recovery must remove it
//     first.
func (d *Docker) Run(ctx context.Context, spec Spec, fn func(ctx context.Context, c *Container) error) (err error) {
	if err := spec.validate(); err != nil {
		return err
	}
	if err := d.Fits(spec.Limits); err != nil {
		return err
	}
	if spec.Deps != "" {
		if err := d.checkDepsVolume(ctx, spec.Deps); err != nil {
			return err
		}
	}
	c := &Container{d: d, spec: spec, name: Name(spec)}
	defer func() {
		r := recover()
		if rmErr := c.kill(ctx); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		if r != nil {
			panic(r)
		}
	}()
	if err := c.start(ctx); err != nil {
		return err
	}
	return fn(ctx, c)
}

// start creates, checks, prepares, starts and probes the container. Any failure leaves the removal to Run.
func (c *Container) start(ctx context.Context) error {
	c.mayExist = true
	_, stderr, res, err := c.d.call(ctx, createArgs(c.spec), nil, controlTimeout)
	if err == nil && res.ExitCode != 0 {
		// The client exited with an error: almost always the daemon refused the create, and nothing was made. That is
		// not certain (the client can fail after the daemon made it), but removing by name here could kill another
		// grade's container of that name, so a container left that way is for recovery to remove by its labels.
		c.mayExist = false
		return fmt.Errorf("create %s: exit %d: %s", c.name, res.ExitCode, firstLine(stderr))
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", c.name, err) // cancelled or timed out: it may exist, so Run removes it
	}
	raw, err := c.d.output(ctx, "container", "inspect", c.name)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", c.name, err)
	}
	if c.digest, c.imageEnv, err = checkInspect(raw, c.spec, c.d.engine.DefaultRuntime); err != nil {
		return fmt.Errorf("%s: %w", c.name, err)
	}
	var created time.Time
	if c.id, created, err = inspectIdentity(raw); err != nil {
		return fmt.Errorf("%s: %w", c.name, err)
	}
	c.created = created.Format(time.RFC3339Nano)
	c.until = created.Add(c.spec.Deadline + time.Minute).Format(time.RFC3339Nano)
	var skeleton bytes.Buffer
	if err := skeletonTar(&skeleton); err != nil {
		return fmt.Errorf("skeleton: %w", err)
	}
	if err := c.check(c.d.call(ctx, []string{"cp", "-", c.name + ":" + GradeDir}, &skeleton, controlTimeout)); err != nil {
		return fmt.Errorf("skeleton into %s: %w", c.name, err)
	}
	if _, err := c.d.output(ctx, "start", c.name); err != nil {
		return fmt.Errorf("start %s: %w", c.name, err)
	}
	// The probes run as the grade's user, since they prove the grade's own view (its user, what it can write). No code
	// of the grade has run yet, so nothing can interfere with them.
	probes, probeErr, probeRes, err := c.d.call(ctx, c.execArgs(User, false, nil, "/", "sh", "-c", probeScript), nil, controlTimeout)
	if err != nil {
		return fmt.Errorf("probe %s: %w", c.name, err)
	}
	if probeRes.ExitCode != 0 {
		return fmt.Errorf("%w: the probe script exited %d: %s", ErrProbe, probeRes.ExitCode, firstLine(probeErr))
	}
	return checkProbes(string(probes), c.spec.Deps != "")
}

func (c *Container) check(_ []byte, stderr string, res runner.Result, err error) error {
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(stderr))
	}
	return nil
}

// execArgs is a docker exec as user (User for the grade's own, MainUser for Agentium's reads), in dir (under
// /grade/work for the grade's; / for Agentium's reads, which then need nothing of the image's working folder), with
// env, of argv; interactive passes stdin.
func (c *Container) execArgs(user string, interactive bool, env []string, dir string, argv ...string) []string {
	args := []string{"exec"}
	if interactive {
		args = append(args, "--interactive")
	}
	args = append(args, "--user", user)
	if dir != "" {
		args = append(args, "--workdir", dir)
	}
	for _, kv := range env {
		args = append(args, "--env", kv)
	}
	return append(append(args, c.name), argv...)
}

// CopyIn sends the tree at root into /grade/work as a tar stream (WriteTar: links never followed, nothing opened in a
// way that can block, the tree only read), unpacked by the image's tar as the grade's user, so the daemon never
// resolves a path in a tree the agent wrote. A tree the stream refuses (ErrTooLarge, or changed or unreadable) or the
// image's tar fails on is ErrUnjudgeable: the agent controls the tree. So is a client failure or a timeout
// (limits.Timeout) once the stream has started, since a tree can be made slow enough to reach it; before that, such an
// error is plain. The walk stops as soon as docker stops reading, so CopyIn always returns. A failed or cancelled
// copy-in leaves a partial tree, so the container is removed.
func (c *Container) CopyIn(ctx context.Context, root string, limits CopyLimits) (TarStats, error) {
	if c.gone || c.stopped {
		return TarStats{}, ErrGone
	}
	// An OS pipe, not an io.Pipe: docker reads it directly, so the call returns the moment docker exits, whatever the
	// walk is doing (an io.Pipe's copier would hold the call until the walk next writes).
	pr, pw, err := os.Pipe()
	if err != nil {
		return TarStats{}, fmt.Errorf("copy into %s: %w", c.name, err)
	}
	defer pr.Close()
	walkCtx, stopWalk := context.WithCancelCause(ctx)
	defer stopWalk(nil)
	var stats TarStats
	var writeErr error
	var started atomic.Bool // set once docker has taken the first bytes of the tree
	done := make(chan struct{})
	go func() {
		defer close(done)
		stats, writeErr = writeTar(walkCtx, startWriter{walkCtx, pw, &started}, root, limits, c.d.hooks)
		pw.Close() // tar sees the stream's end; after an error, the writer's error decides
	}()
	timeout := limits.Timeout
	if timeout <= 0 {
		timeout = DefaultCopyLimits().Timeout
	}
	_, stderr, res, err := c.d.call(ctx, c.execArgs(User, true, nil, WorkDir, "tar", "-x", "-f", "-"), pr, timeout)
	// Docker has stopped reading: the walk stops at its next entry, and its next write fails (no reader is left).
	stopWalk(errCopyEnded)
	pr.Close()
	<-done
	cut := errors.Is(writeErr, errCopyEnded)
	if started.Load() || writeErr != nil && !cut && !errors.Is(writeErr, errNoRoot) {
		c.used = true
	}
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case writeErr != nil && errors.Is(writeErr, errNoRoot):
		err = writeErr
	case writeErr != nil && !cut:
		// The writer's error decides (tar may accept a stream cut at an entry's boundary), and it comes from the tree.
		err = fmt.Errorf("%w: %w", ErrUnjudgeable, writeErr)
	case err != nil && started.Load():
		err = fmt.Errorf("%w: %w", ErrUnjudgeable, err)
	case err != nil:
	case res.ExitCode != 0:
		err = fmt.Errorf("%w: tar exited %d: %s", ErrUnjudgeable, res.ExitCode, firstLine(stderr))
	case cut:
		err = fmt.Errorf("%w: tar ended before the whole tree was sent", ErrUnjudgeable)
	default:
		return stats, nil
	}
	return stats, errors.Join(fmt.Errorf("copy into %s: %w", c.name, err), c.kill(ctx))
}

// startWriter records that a write was taken, and reports a write that failed because the walk was stopped (docker
// stopped reading) as the walk's cause, errCopyEnded, not as the tree's doing.
type startWriter struct {
	ctx     context.Context
	w       io.Writer
	started *atomic.Bool
}

func (s startWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if n > 0 {
		s.started.Store(true)
	}
	if err != nil && s.ctx.Err() != nil {
		err = context.Cause(s.ctx)
	}
	return n, err
}

// Command is one command to run in the container.
type Command struct {
	Command string        // run with sh -c
	Dir     string        // relative to /grade/work, slash-separated; "" is the copy's root
	Env     []string      // NAME=value, over the image's environment; a bare NAME (docker's pass-through) is refused
	Timeout time.Duration // 0: none beyond ctx and the container's deadline
	Output  io.Writer     // stdout and stderr; nil discards them
}

// Result is how a command ended, with the container's counters after it.
type Result struct {
	ExitCode int // -1 when it timed out
	TimedOut bool
	Duration time.Duration
	Counters Counters
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Exec runs one command as the grade's user and reads the cgroup counters after it (as MainUser, so the grade's code
// cannot kill the read). Its Result is settled exactly when its error is nil: then the command ended with ExitCode
// (a non-zero exit is a Result) or timed out, and the counters were read.
//
// An exit status counts only when the daemon's own record of this exec agrees with it: the docker client exits 1 both
// when the command did and when its API calls failed, so the exit code alone cannot tell a failed test from a failed
// client. A watch on the daemon's events (execWatch) finds the exec by a nonce in its argv and reads the exit code the
// daemon logged for it. The command's stderr goes to its stdout inside the container, so the client's own stderr holds
// only the client's messages. Any of these is ErrUnjudgeable, and the container is removed: the client reported an
// error, no record of the exec's end within statusTimeout, or a record that disagrees with the client.
//
// On a timeout (Agentium's clock) the daemon's record must show the command still running; the counters are read,
// then the container is removed: killing the docker client would leave the command running inside. If that removal
// fails, the result still stands and Run retries the removal (and reports it); later calls are ErrGone. On ctx's
// cancel the container is removed at once and the error is ctx's. Any other error (the counters unreadable, the
// container ended mid-command, the client failed) is ErrUnjudgeable, and the container is removed. Refused input (Env,
// Dir) is an error before anything runs.
func (c *Container) Exec(ctx context.Context, cmd Command) (Result, error) {
	if c.gone || c.stopped {
		return Result{ExitCode: -1}, ErrGone
	}
	dir, err := workDir(cmd.Dir)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	for _, kv := range cmd.Env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !envName.MatchString(name) || strings.ContainsRune(kv, 0) {
			return Result{ExitCode: -1}, fmt.Errorf("command environment: %q is not NAME=value", name)
		}
	}
	out := cmd.Output
	if out == nil {
		out = io.Discard
	}
	nonce, err := newNonce()
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("exec in %s: %w", c.name, err)
	}
	w := c.watchExec(ctx, nonce)
	defer w.stop()
	clientErr := &capped{max: 64 << 10}
	c.used = true
	res, err := runner.Run(ctx, runner.Spec{Args: append([]string{c.d.bin}, c.d.args(c.execArgs(User, false, cmd.Env, dir, commandArgv(cmd.Command, nonce)...))...),
		Environ: c.d.environ, Timeout: cmd.Timeout, Output: out, Stderr: clientErr})
	result := Result{ExitCode: res.ExitCode, TimedOut: res.TimedOut, Duration: res.Duration}
	unjudgeable := func(format string, args ...any) (Result, error) {
		return result, errors.Join(fmt.Errorf("%w: exec in %s: %s", ErrUnjudgeable, c.name, fmt.Sprintf(format, args...)), c.kill(ctx))
	}
	if ctx.Err() != nil {
		return result, errors.Join(fmt.Errorf("exec in %s: %w", c.name, ctx.Err()), c.kill(ctx))
	}
	if err != nil {
		return result, errors.Join(fmt.Errorf("%w: exec in %s: %w", ErrUnjudgeable, c.name, err), c.kill(ctx))
	}
	if res.TimedOut {
		result.ExitCode = -1
		if started, ended := w.running(); !started || ended {
			return unjudgeable("timed out, and the daemon's record has the command started %v, ended %v", started, ended)
		}
	} else {
		if msg := strings.TrimSpace(clientErr.String()); msg != "" {
			return unjudgeable("the docker client exited %d and reported: %s", res.ExitCode, firstLine(c.d.redact(msg)))
		}
		exit, err := w.wait(ctx, c.d.statusWait)
		if ctx.Err() != nil {
			return result, errors.Join(fmt.Errorf("exec in %s: %w", c.name, ctx.Err()), c.kill(ctx))
		}
		if err != nil {
			return unjudgeable("the docker client exited %d, and the daemon's record of the command: %v", res.ExitCode, err)
		}
		if exit != res.ExitCode {
			return unjudgeable("the docker client exited %d, and the daemon recorded the command's exit as %d", res.ExitCode, exit)
		}
	}
	counters, err := c.Counters(ctx)
	result.Counters = counters
	if err != nil {
		return result, errors.Join(fmt.Errorf("%w: after the command: %w", ErrUnjudgeable, err), c.kill(ctx))
	}
	if res.TimedOut {
		_ = c.kill(ctx) // settled; a failed removal leaves the container stopped, and Run removes it or reports it
	}
	return result, nil
}

// commandArgv is a command's argv in the container: sh runs the command with its stderr joined to its stdout (so the
// docker client's own stderr holds only the client's messages), and nonce is the outer shell's last argument, which
// marks this exec in the daemon's events. The command itself is the inner shell's whole script ($0 is sh, and it gets
// no arguments), as it would be run on the host.
func commandArgv(command, nonce string) []string {
	return []string{"sh", "-c", `exec sh -c "$1" 2>&1`, "sh", command, nonce}
}

// workDir is a command's folder in the container: under /grade/work, never above it.
func workDir(dir string) (string, error) {
	if dir == "" || dir == "." {
		return WorkDir, nil
	}
	clean := path.Clean(dir)
	if path.IsAbs(dir) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(dir, 0) {
		return "", fmt.Errorf("command folder %q: not under the copy", dir)
	}
	return WorkDir + "/" + clean, nil
}

// Counters reads the container's cgroup counters (cumulative since it started), as MainUser: the grade's code, which
// runs as User, cannot kill the read.
func (c *Container) Counters(ctx context.Context) (Counters, error) {
	if c.gone || c.stopped {
		return Counters{}, ErrGone
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), countersTimeout)
	defer cancel()
	out, stderr, res, err := c.d.call(cctx, c.execArgs(MainUser, false, nil, "/", "sh", "-c", countersScript), nil, countersTimeout)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(stderr))
	}
	if err != nil {
		return Counters{}, fmt.Errorf("cgroup counters of %s: %w", c.name, err)
	}
	return parseCounters(string(out))
}

// kill removes the container (once, and only if this grade may have created it) and marks it gone. A failed removal
// marks it stopped (it runs nothing more; Run tries again) and is ErrCleanup, and ErrUnjudgeable once the grade's input
// was used, unless ctx was cancelled (Agentium's own cancel stays its error).
func (c *Container) kill(ctx context.Context) error {
	if c.gone {
		return nil
	}
	if !c.mayExist {
		c.gone = true
		return nil
	}
	err := c.d.removeName(ctx, c.name)
	if err == nil {
		c.gone, c.mayExist = true, false
		return nil
	}
	c.stopped = true
	err = fmt.Errorf("%w (recovery removes it by its labels): %w", ErrCleanup, err)
	if c.used && ctx.Err() == nil {
		err = fmt.Errorf("%w: %w", ErrUnjudgeable, err)
	}
	return err
}

// removeName removes a container and its anonymous volumes by name, and waits until the daemon no longer knows it. It
// runs outside ctx's cancellation, bounded by removeTimeout, so a cancelled grade still cleans up. A container already
// gone (--rm after the deadline) is not an error.
func (d *Docker) removeName(ctx context.Context, name string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	_, stderr, res, err := d.call(rctx, []string{"rm", "--force", "--volumes", name}, nil, removeTimeout)
	if err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	if res.ExitCode != 0 && !noSuchContainer(stderr) && !strings.Contains(stderr, "already in progress") {
		return fmt.Errorf("remove %s: exit %d: %s", name, res.ExitCode, firstLine(stderr))
	}
	for {
		_, stderr, res, err := d.call(rctx, []string{"container", "inspect", "--format", "{{.State.Status}}", name}, nil, removeTimeout)
		if err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
		if res.ExitCode != 0 && noSuchContainer(stderr) {
			return nil
		}
		select {
		case <-rctx.Done():
			return fmt.Errorf("remove %s: still there after %s", name, removeTimeout)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func noSuchContainer(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "no such container")
}

// checkDepsVolume refuses a deps volume that does not exist (a mount would create an empty, unlabelled one) or that is
// not a plain local volume: the local driver's options can bind a host folder.
func (d *Docker) checkDepsVolume(ctx context.Context, name string) error {
	out, stderr, res, err := d.call(ctx, []string{"volume", "inspect", "--format", "{{json .}}", name}, nil, controlTimeout)
	if err != nil {
		return fmt.Errorf("deps volume %s: %w", name, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("deps volume %s: %s", name, firstLine(stderr))
	}
	var v struct {
		Driver  string
		Options map[string]string
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &v); err != nil {
		return fmt.Errorf("deps volume %s: %w", name, err)
	}
	if v.Driver != "local" || len(v.Options) > 0 {
		return fmt.Errorf("deps volume %s: driver %q with %d options, want a plain local volume", name, v.Driver, len(v.Options))
	}
	return nil
}

// Leftover is a container or anonymous volume of this mode in a data folder.
type Leftover struct {
	Kind  string // "container" or "volume"
	Name  string
	Run   string
	State string // a container's: created, running, exited, removing, dead
}

// Leftovers lists the containers (any state, including created and never started) and labelled volumes of a data
// folder, for recovery and clean to match against dead runs. Deps volumes carry no run label and are not listed.
func (d *Docker) Leftovers(ctx context.Context, data string) ([]Leftover, error) {
	if !dataPattern.MatchString(data) {
		return nil, fmt.Errorf("leftovers: data ID %q", data)
	}
	filters := []string{"--filter", "label=" + LabelData + "=" + data, "--filter", "label=" + LabelMode + "=" + Mode, "--filter", "label=" + LabelRun}
	out, err := d.output(ctx, append([]string{"ps", "--all", "--no-trunc", "--format", `{{.Names}}	{{.State}}	{{.Label "agentium.run"}}`}, filters...)...)
	if err != nil {
		return nil, fmt.Errorf("leftovers: %w", err)
	}
	var list []Leftover
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 {
			list = append(list, Leftover{Kind: "container", Name: f[0], State: f[1], Run: f[2]})
		}
	}
	out, err = d.output(ctx, append([]string{"volume", "ls", "--format", `{{.Name}}	{{.Label "agentium.run"}}`}, filters...)...)
	if err != nil {
		return nil, fmt.Errorf("leftovers: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 2 {
			list = append(list, Leftover{Kind: "volume", Name: f[0], Run: f[1]})
		}
	}
	return list, nil
}

// RemoveRun removes every container (whatever its state) and labelled volume of one run in a data folder, found by
// their labels: recovery and clean call it for runs that are dead, never for a live one. It runs outside ctx's
// cancellation, bounded, like every removal.
func (d *Docker) RemoveRun(ctx context.Context, data, run string) error {
	if !dataPattern.MatchString(data) || !runPattern.MatchString(run) {
		return fmt.Errorf("remove run: data %q, run %q", data, run)
	}
	filters := []string{"--filter", "label=" + LabelData + "=" + data, "--filter", "label=" + LabelRun + "=" + run, "--filter", "label=" + LabelMode + "=" + Mode}
	out, err := d.output(ctx, append([]string{"ps", "--all", "--quiet", "--no-trunc"}, filters...)...)
	if err != nil {
		return fmt.Errorf("remove run %s: %w", run, err)
	}
	var errs []error
	for _, id := range strings.Fields(string(out)) {
		errs = append(errs, d.removeName(ctx, id))
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	out, err = d.output(rctx, append([]string{"volume", "ls", "--quiet"}, filters...)...)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("remove run %s: %w", run, err))...)
	}
	for _, vol := range strings.Fields(string(out)) {
		_, stderr, res, err := d.call(rctx, []string{"volume", "rm", vol}, nil, removeTimeout)
		if err == nil && res.ExitCode != 0 && !strings.Contains(strings.ToLower(stderr), "no such volume") {
			err = fmt.Errorf("remove volume %s: exit %d: %s", vol, res.ExitCode, firstLine(stderr))
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
