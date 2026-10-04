package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// ErrGone: the container was removed (after a timeout or a cancel) and runs nothing more.
var ErrGone = errors.New("the grade's container is gone")

const (
	// removeTimeout bounds a removal, which runs even after ctx is cancelled.
	removeTimeout = 30 * time.Second
	// copyTimeout bounds a copy-in (step 0: 0.53 s for 177 MB).
	copyTimeout = 10 * time.Minute
	// countersTimeout bounds the read of the counters after a command.
	countersTimeout = 30 * time.Second
)

// Container is a grade's container, started, checked and proved isolated. It exists only inside Run's function and is
// not safe for concurrent use.
type Container struct {
	d        *Docker
	spec     Spec
	name     string
	digest   string
	imageEnv []string
	gone     bool
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
// Run returns (docker rm -f -v by name, bounded and outside ctx's cancellation), and a failed removal is part of the
// error. If Agentium dies instead, the deadline and --rm remove a started container, and recovery removes a created
// one by its labels (RemoveRun).
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
		// The daemon answered and refused: nothing was created (the create call is atomic).
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
	if c.digest, c.imageEnv, err = checkInspect(raw, c.spec); err != nil {
		return fmt.Errorf("%s: %w", c.name, err)
	}
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
	probes, probeErr, probeRes, err := c.d.call(ctx, c.execArgs(false, nil, "", "sh", "-c", probeScript), nil, controlTimeout)
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

// execArgs is a docker exec as the grade's user, in dir (under /grade/work; "" for the container's own folder), with
// env, of argv; interactive passes stdin.
func (c *Container) execArgs(interactive bool, env []string, dir string, argv ...string) []string {
	args := []string{"exec"}
	if interactive {
		args = append(args, "--interactive")
	}
	args = append(args, "--user", User)
	if dir != "" {
		args = append(args, "--workdir", dir)
	}
	for _, kv := range env {
		args = append(args, "--env", kv)
	}
	return append(append(args, c.name), argv...)
}

// CopyIn sends the tree at root into /grade/work as a tar stream (WriteTar: links never followed, the tree only read),
// unpacked by the image's tar as the grade's user, so the daemon never resolves a path in a tree the agent wrote. A
// tree over limit bytes is ErrTooLarge. A failed or cancelled copy-in leaves a partial tree, so the container is
// removed.
func (c *Container) CopyIn(ctx context.Context, root string, limit int64) (TarStats, error) {
	if c.gone {
		return TarStats{}, ErrGone
	}
	pr, pw := io.Pipe()
	var stats TarStats
	var writeErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		stats, writeErr = WriteTar(pw, root, limit)
		pw.CloseWithError(writeErr) // nil closes the stream normally
	}()
	_, stderr, res, err := c.d.call(ctx, c.execArgs(true, nil, WorkDir, "tar", "-x", "-f", "-"), pr, copyTimeout)
	pr.CloseWithError(errors.New("copy-in ended")) // unblocks the writer if docker stopped reading
	<-done
	switch {
	case err != nil:
	case writeErr != nil:
		// The writer's error decides: tar may accept a stream cut at an entry's boundary.
		err = writeErr
	case res.ExitCode != 0:
		err = fmt.Errorf("tar exited %d: %s", res.ExitCode, firstLine(stderr))
	default:
		return stats, nil
	}
	return stats, errors.Join(fmt.Errorf("copy into %s: %w", c.name, err), c.kill(ctx))
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

// Exec runs one command as the grade's user and reads the cgroup counters after it. A non-zero exit is a Result; an
// error means the result cannot be trusted (the container is gone, or its counters cannot be read) and is
// infrastructure. On a timeout the counters are read, then the container is removed: killing the docker client would
// leave the command running inside. On a cancel the container is removed at once.
func (c *Container) Exec(ctx context.Context, cmd Command) (Result, error) {
	if c.gone {
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
	res, err := runner.Run(ctx, runner.Spec{Args: append([]string{c.d.bin}, c.d.args(c.execArgs(false, cmd.Env, dir, "sh", "-c", cmd.Command))...),
		Environ: c.d.environ, Timeout: cmd.Timeout, Output: out})
	result := Result{ExitCode: res.ExitCode, TimedOut: res.TimedOut, Duration: res.Duration}
	if err != nil {
		return result, errors.Join(fmt.Errorf("exec in %s: %w", c.name, err), c.kill(ctx))
	}
	if res.TimedOut {
		result.ExitCode = -1
		counters, cErr := c.Counters(ctx)
		result.Counters = counters
		return result, errors.Join(cErr, c.kill(ctx))
	}
	counters, err := c.Counters(ctx)
	result.Counters = counters
	return result, err
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

// Counters reads the container's cgroup counters (cumulative since it started).
func (c *Container) Counters(ctx context.Context) (Counters, error) {
	if c.gone {
		return Counters{}, ErrGone
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), countersTimeout)
	defer cancel()
	out, stderr, res, err := c.d.call(cctx, c.execArgs(false, nil, "", "sh", "-c", countersScript), nil, countersTimeout)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(stderr))
	}
	if err != nil {
		return Counters{}, fmt.Errorf("cgroup counters of %s: %w", c.name, err)
	}
	return parseCounters(string(out))
}

// kill removes the container (once, and only if this grade may have created it) and marks it gone.
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
