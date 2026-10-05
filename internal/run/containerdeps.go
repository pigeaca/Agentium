package run

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/checkout"
	"github.com/pigeaca/agentium/internal/container"
	"github.com/pigeaca/agentium/internal/home"
)

// ContainerDocker is what a container deps warm-up needs of the Docker driver (*container.Docker).
type ContainerDocker interface {
	EnsureDepsVolume(ctx context.Context, v container.DepsVolume) (created string, err error)
	SeedDeps(ctx context.Context, v container.DepsVolume, img container.Image, seeds []container.Seed, entries []container.SeedEntry, limits container.CopyLimits) ([]string, error)
	WarmRun(ctx context.Context, w container.WarmSpec, tree string, cmds []container.WarmCommand, out io.Writer) (passed int, failed string, err error)
}

// ContainerDepsInput is one base's deps warm-up for container grading (container plan, step 3). Nothing calls it yet:
// grading in containers is wired in step 4.
type ContainerDepsInput struct {
	Docker ContainerDocker
	Data   string          // container.DataID of the data folder
	Image  container.Image // the grading image; its ID keys the volume
	// Profiles are the run's build tools; Base the task's base commit, the only tree a warm-up runs (a trusted commit:
	// never an agent's work).
	Profiles []buildtool.Profile
	Base     string
	// HostWarmFailed: the host's warm-up of this base failed, so Gradle warms in the container (with network).
	HostWarmFailed bool
	Limits         container.Limits
	LogPath        string // the warm-up's output is appended here
}

// ContainerDepsState is what a base's container warm-up left, in its stamp: the volume grades mount read-only (and the
// daemon's creation time of it, which tells it from an earlier volume of that name), the Python venv in it, and notes.
type ContainerDepsState struct {
	Volume  string   `json:"volume"`
	Created string   `json:"volume_created"`
	Venv    string   `json:"venv,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// containerWarmVersion names the container warm-up's recipe in its stamps: a change makes every base warm again.
const containerWarmVersion = "container-deps-1"

// containerWarmStepTimeout bounds one warm-up command in a container.
const containerWarmStepTimeout = 30 * time.Minute

// containerDepsState is where a deps volume's state lives on the host, in the data folder's cache (agents may not read
// it): its lock, the files seeded into it, its stamps per base, and its last use. One folder per volume, apart from the
// host's warm-up state, whose cleanup never touches it.
func containerDepsState(layout home.Layout, volume string) string {
	return filepath.Join(layout.Cache, "container-deps", volume)
}

// ContainerDepsUsed is a deps volume's last use as the data folder recorded it; zero when unknown.
func ContainerDepsUsed(layout home.Layout, volume string) time.Time {
	if layout.Cache == "" {
		return time.Time{}
	}
	info, err := os.Lstat(filepath.Join(containerDepsState(layout, volume), "used"))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// ForgetContainerDeps removes a deps volume's state (its stamps, the record of what was seeded, its last use), but not
// its lock, which a warm-up may be waiting on.
func ForgetContainerDeps(layout home.Layout, volume string) error {
	if layout.Cache == "" || volume == "" || strings.ContainsAny(volume, `/\`) || volume == "." || volume == ".." {
		return nil
	}
	state := containerDepsState(layout, volume)
	entries, err := os.ReadDir(state)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("container deps state: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if e.Name() != "lock" {
			errs = append(errs, os.RemoveAll(filepath.Join(state, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// RemoveContainerDeps removes a deps volume for clean, holding its warm-up lock (a warm-up never finds its volume gone
// between its steps): a volume used since lastUsed stays (ErrCleanUsed), one whose lock a warm-up holds too
// (ErrCleanBusy). remove is the Docker removal (container.RemoveIdle), which refuses a volume a container mounts; its
// state goes only after it, so a volume that stays keeps its record of what it holds (which spares its seeds sending
// it again). A crash in between leaves stamps of a volume that is gone, which the next warm-up ignores: they name
// the old volume's creation time.
func RemoveContainerDeps(ctx context.Context, layout home.Layout, volume string, lastUsed time.Time, remove func() error) error {
	state := containerDepsState(layout, volume)
	if err := os.MkdirAll(state, 0o700); err != nil {
		return fmt.Errorf("container deps state: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, cleanLockWait)
	unlock, err := lockFile(waitCtx, filepath.Join(state, "lock"), nil)
	cancel()
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return ErrCleanBusy
	case err != nil:
		return fmt.Errorf("lock: %w", err)
	}
	defer unlock()
	if used := ContainerDepsUsed(layout, volume); used.After(lastUsed) {
		return ErrCleanUsed
	}
	if err := remove(); err != nil {
		return err
	}
	return ForgetContainerDeps(layout, volume)
}

// WarmContainerDeps warms a project's deps volume for one base and one grading image, once (a stamp per base, under a
// lock per volume): the volume is made when missing; seeded with what the host's platform-independent caches hold that
// it lacks (the volume's own tar never replaces a file it holds, which a grade may be reading or a warm-up wrote:
// container.SeedDeps); then the base's checkout, a trusted commit, runs the
// container warm-up's steps in the grading image, with the network and the volume writable (buildtool.ContainerWarmSteps:
// Python's venv always, Gradle when the host's warm-up failed). A step that fails is a note, and the base is not
// stamped, so the next use tries again. Grades mount the volume read-only.
func (env Env) WarmContainerDeps(ctx context.Context, in ContainerDepsInput) (ContainerDepsState, error) {
	deps := env.depsFolder()
	if deps == "" {
		return ContainerDepsState{}, errors.New("container deps: the data folder has no deps folder")
	}
	if in.Docker == nil || !fullCommit.MatchString(in.Base) {
		return ContainerDepsState{}, fmt.Errorf("container deps: base %q", in.Base)
	}
	v := container.DepsVolume{Data: in.Data, Project: strings.ToLower(filepath.Base(deps)), Image: in.Image.ID}
	state := containerDepsState(env.Layout, v.Name())
	if err := os.MkdirAll(state, 0o700); err != nil {
		return ContainerDepsState{}, fmt.Errorf("container deps state: %w", err)
	}
	wait := cmp.Or(env.WarmWait, DefaultWarmWait)
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	unlock, err := lockFile(waitCtx, filepath.Join(state, "lock"), func() {
		env.progress("  waiting for another warm-up of the container's dependencies (up to %s)", wait)
	})
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return ContainerDepsState{}, fmt.Errorf("%w: another warm-up of the container's dependencies held the lock for %s", errWarmWait, wait)
		}
		return ContainerDepsState{}, fmt.Errorf("container deps lock: %w", err)
	}
	defer unlock()
	created, err := in.Docker.EnsureDepsVolume(ctx, v)
	if err != nil {
		return ContainerDepsState{}, err
	}
	defer markUsed(filepath.Join(state, "used"))
	if err := touch(filepath.Join(state, "used")); err != nil {
		return ContainerDepsState{}, err
	}
	module := ""
	if key := buildtool.ModuleKey(env.Module); key != "" {
		module = "-" + key
	}
	stamp := filepath.Join(state, "stamp-"+containerWarmVersion+module+"-"+in.Base)
	if s, ok := readContainerStamp(stamp, v.Name(), created); ok {
		markUsed(stamp)
		return s, nil
	}
	// The base's checkout, in the data folder's cache (agents may not read it), removed after.
	parent := filepath.Join(env.Layout.Cache, "warm")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return ContainerDepsState{}, fmt.Errorf("container warm-up checkout: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "c-")
	if err != nil {
		return ContainerDepsState{}, fmt.Errorf("container warm-up checkout: %w", err)
	}
	defer os.RemoveAll(dir)
	repo := filepath.Join(dir, "repo")
	if err := checkout.New(ctx, env.Bare, in.Base, repo); err != nil {
		return ContainerDepsState{}, fmt.Errorf("container warm-up checkout: %w", err)
	}
	moduleDir := filepath.Join(repo, filepath.FromSlash(env.Module))
	// Seeds: what the record says the volume holds is not sent again (what it holds anyway is never replaced). The
	// record is the daemon's volume's: one made since (the old one removed) is seeded whole.
	seededPath := filepath.Join(state, "seeded")
	seeded := readSeeded(seededPath, created)
	seeds, entries := buildtool.ContainerSeeds(in.Profiles, deps, moduleDir, env.Environ, env.Home)
	for i := range seeds {
		own := seeds[i].Skip
		seeds[i].Skip = func(name string) bool { return seeded[name] || own != nil && own(name) }
	}
	entries = slices.DeleteFunc(entries, func(e container.SeedEntry) bool { return !e.Dir && seeded[e.Name] })
	written, err := in.Docker.SeedDeps(ctx, v, in.Image, seeds, entries, container.SeedLimits())
	if err != nil {
		return ContainerDepsState{}, err
	}
	if len(written) > 0 || len(seeded) == 0 {
		for _, name := range written {
			seeded[name] = true
		}
		if err := writeSeeded(seededPath, created, seeded); err != nil {
			return ContainerDepsState{}, err
		}
	}
	warm := buildtool.ContainerWarmSteps(in.Profiles, moduleDir, in.HostWarmFailed)
	result := ContainerDepsState{Volume: v.Name(), Created: created, Venv: warm.Venv, Notes: warm.Notes}
	if len(warm.Steps) > 0 {
		cmds := make([]container.WarmCommand, len(warm.Steps))
		for i, s := range warm.Steps {
			cmds[i] = container.WarmCommand{Command: s.Command, Dir: env.Module, Env: s.Env, Timeout: containerWarmStepTimeout}
		}
		log, err := os.OpenFile(cmp.Or(in.LogPath, os.DevNull), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return ContainerDepsState{}, fmt.Errorf("container warm-up log: %w", err)
		}
		fmt.Fprintf(log, "$ container deps warm-up of %s in %s (%s)\n", in.Base, v.Name(), in.Image.ID)
		_, failed, err := in.Docker.WarmRun(ctx, container.WarmSpec{Volume: v, Image: in.Image, Limits: in.Limits,
			Deadline: time.Duration(len(cmds))*containerWarmStepTimeout + 5*time.Minute}, repo, cmds, log)
		log.Close()
		if err != nil {
			return ContainerDepsState{}, err
		}
		if failed != "" {
			result.Notes = append(result.Notes, "the container's dependency warm-up failed ("+failed+"): grades may not build offline; see the log (not stamped: the next use tries again)")
			return result, nil
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return ContainerDepsState{}, err
	}
	if err := buildtool.WriteFileSynced(stamp, data, 0o600); err != nil {
		return ContainerDepsState{}, fmt.Errorf("container deps stamp: %w", err)
	}
	return result, nil
}

// readContainerStamp reads a base's stamp: whether it was warmed into this very volume (its name and creation time).
func readContainerStamp(path, volume, created string) (ContainerDepsState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ContainerDepsState{}, false
	}
	var s ContainerDepsState
	if json.Unmarshal(data, &s) != nil || s.Volume != volume || s.Created == "" || s.Created != created {
		return ContainerDepsState{}, false
	}
	return s, true
}

// readSeeded reads the record of what was seeded into the volume made at created: its first line names that volume's
// creation, and every other line is a file or link under /deps. A record of another volume, or none, is empty.
func readSeeded(path, created string) map[string]bool {
	seeded := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return seeded
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || sc.Text() != "volume "+created || created == "" {
		return seeded
	}
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			seeded[line] = true
		}
	}
	if sc.Err() != nil {
		return map[string]bool{}
	}
	return seeded
}

// writeSeeded writes the record whole: a crash leaves the old one, which only lacks names, so those are seeded again.
func writeSeeded(path, created string, seeded map[string]bool) error {
	names := make([]string, 0, len(seeded))
	for name := range seeded {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteString("volume " + created + "\n")
	for _, n := range names {
		b.WriteString(n + "\n")
	}
	if err := buildtool.WriteFileSynced(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("container deps record: %w", err)
	}
	return nil
}

// touch makes an empty file when it is missing.
func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}
