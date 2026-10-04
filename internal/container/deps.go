package container

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The labels of a deps volume (besides LabelData and LabelMode). It has no LabelRun: it belongs to a project and an
// image, not to a run, so Leftovers never lists it.
const (
	LabelDeps  = "agentium.deps"  // the project's key: its deps folder's name in the data folder
	LabelImage = "agentium.image" // the first 12 digits of the grading image's ID
)

// DepsVolume is a project's dependencies for one grading image, in a Docker volume: seeded from the host's
// platform-independent caches (SeedDeps) and warmed in a container of that image (Warm). Grades mount it read-only.
type DepsVolume struct {
	Data    string // DataID
	Project string // the project's key: lowercase letters, digits and '-', at most 32
	Image   string // the grading image's ID, sha256:<64 hex>
}

var projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Name is agentium-deps-<data>-<project>-<the image ID's first 12 digits>.
func (v DepsVolume) Name() string {
	return "agentium-deps-" + v.Data + "-" + v.Project + "-" + v.digest12()
}

// digest12 is the image ID's first 12 digits ("" for an unreadable ID, which validate refuses).
func (v DepsVolume) digest12() string {
	if !imageID.MatchString(v.Image) {
		return ""
	}
	return strings.TrimPrefix(v.Image, "sha256:")[:12]
}

func (v DepsVolume) validate() error {
	switch {
	case !dataPattern.MatchString(v.Data):
		return fmt.Errorf("deps volume: data ID %q", v.Data)
	case !projectPattern.MatchString(v.Project):
		return fmt.Errorf("deps volume: project %q", v.Project)
	case !imageID.MatchString(v.Image):
		return fmt.Errorf("deps volume: image %q is not an image ID", v.Image)
	}
	return nil
}

func (v DepsVolume) labels() map[string]string {
	return map[string]string{LabelData: v.Data, LabelMode: Mode, LabelDeps: v.Project, LabelImage: v.digest12()}
}

// EnsureDepsVolume makes the volume when it is missing: a plain local volume with its labels. An existing one must be
// exactly that, labels included: a volume of that name that someone else made (or one whose local options bind a
// host folder) is refused, never used. It returns the volume's creation time as the daemon records it, which tells
// this volume from an earlier one of the same name: what a caller recorded of an earlier one (stamps, what was
// seeded) does not hold for it.
func (d *Docker) EnsureDepsVolume(ctx context.Context, v DepsVolume) (created string, err error) {
	if err := v.validate(); err != nil {
		return "", err
	}
	name := v.Name()
	created, ok, err := d.depsVolumeIs(ctx, v)
	if err != nil || ok {
		return created, err
	}
	args := []string{"volume", "create", "--driver", "local"}
	for _, k := range slices.Sorted(mapKeys(v.labels())) {
		args = append(args, "--label", k+"="+v.labels()[k])
	}
	if _, err := d.output(ctx, append(args, name)...); err != nil {
		return "", fmt.Errorf("deps volume %s: %w", name, err)
	}
	if created, ok, err = d.depsVolumeIs(ctx, v); err != nil || !ok {
		return "", errors.Join(fmt.Errorf("deps volume %s: not found after it was made", name), err)
	}
	return created, nil
}

// depsVolumeIs reports whether the volume exists (false, nil when it does not), and when it was made, refusing one
// that is not exactly Agentium's: a plain local volume, without options, with the deps volume's labels.
func (d *Docker) depsVolumeIs(ctx context.Context, v DepsVolume) (string, bool, error) {
	name := v.Name()
	out, stderr, res, err := d.call(ctx, []string{"volume", "inspect", "--format", "{{json .}}", name}, nil, controlTimeout)
	if err != nil {
		return "", false, fmt.Errorf("deps volume %s: %w", name, err)
	}
	if res.ExitCode != 0 {
		if strings.Contains(strings.ToLower(stderr), "no such volume") {
			return "", false, nil
		}
		return "", false, fmt.Errorf("deps volume %s: %s", name, firstLine(stderr))
	}
	var got struct {
		Name      string
		Driver    string
		Options   map[string]string
		Labels    map[string]string
		CreatedAt string
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		return "", false, fmt.Errorf("deps volume %s: %w", name, err)
	}
	if got.Name != name || got.Driver != "local" || len(got.Options) > 0 {
		return "", false, fmt.Errorf("deps volume %s: driver %q with %d options, want a plain local volume", name, got.Driver, len(got.Options))
	}
	for k, want := range v.labels() {
		if got.Labels[k] != want {
			return "", false, fmt.Errorf("deps volume %s exists and is not Agentium's (label %s is %q, want %q): remove it, or use another data folder", name, k, got.Labels[k], want)
		}
	}
	return got.CreatedAt, true, nil
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// Seed is a host folder copied into a deps volume, at To (slash-separated, relative to /deps). Its entries go in owned
// by the grade's user, links as links (never followed), as WriteTar writes a tree. Skip leaves out files and links that
// are in the volume already, by their name under /deps: a seed only adds, since rewriting a file a grade reads would
// tear it. A From that does not exist is no seed.
type Seed struct {
	From string
	To   string
	Skip func(name string) bool
}

// SeedEntry is an entry Agentium writes into a deps volume itself: a folder (Dir), a symbolic link (Link, its target),
// or a file (Body). Name is slash-separated and relative to /deps.
type SeedEntry struct {
	Name string
	Dir  bool
	Link string
	Body []byte
}

// SeedLimits cap a seed's stream: host caches are larger than a grading copy.
func SeedLimits() CopyLimits {
	return CopyLimits{Bytes: 64 << 30, Entries: 5_000_000, Timeout: 60 * time.Minute}
}

// cleanRel checks a name under /deps: relative, clean, never leaving it.
func cleanRel(name string) (string, error) {
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if name == "" || path.IsAbs(name) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("deps seed: %q is not a folder under /deps", name)
	}
	return clean, nil
}

// SeedTar writes the seed stream: every folder on the way to each entry and seed, listed before it (docker cp would
// make a missing one as root, which the grade's user then cannot write: step 0), then the entries, then each seed's
// tree. Every entry is owned by the grade's user. It returns the files and links it wrote, by name under /deps.
func SeedTar(ctx context.Context, w io.Writer, seeds []Seed, entries []SeedEntry, limits CopyLimits) (TarStats, []string, error) {
	tw := tar.NewWriter(w)
	var stats TarStats
	var written []string
	listed := map[string]bool{}
	dir := func(name string, mode int64) error {
		var parents []string
		for p := name; p != "."; p = path.Dir(p) {
			parents = append(parents, p)
		}
		slices.Reverse(parents)
		for _, p := range parents {
			if listed[p] {
				continue
			}
			listed[p] = true
			m := int64(0o755)
			if p == name && mode != 0 {
				m = mode
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: m, Uid: 65534, Gid: 65534, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
				return err
			}
			stats.Entries++
		}
		return nil
	}
	for _, e := range entries {
		name, err := cleanRel(e.Name)
		if err != nil {
			return stats, nil, err
		}
		if err := ctx.Err(); err != nil {
			return stats, nil, err
		}
		if e.Dir {
			if err := dir(name, 0); err != nil {
				return stats, nil, err
			}
			continue
		}
		if err := dir(path.Dir(name), 0); err != nil {
			return stats, nil, err
		}
		hdr := &tar.Header{Name: name, Uid: 65534, Gid: 65534, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if e.Link != "" {
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, e.Link, 0o777
		} else {
			hdr.Typeflag, hdr.Size, hdr.Mode = tar.TypeReg, int64(len(e.Body)), 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return stats, nil, err
		}
		if _, err := tw.Write(e.Body); err != nil {
			return stats, nil, err
		}
		stats.Entries++
		stats.Bytes += int64(len(e.Body))
		written = append(written, name)
	}
	for _, s := range seeds {
		to, err := cleanRel(s.To)
		if err != nil {
			return stats, nil, err
		}
		r, err := os.OpenRoot(s.From)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return stats, nil, fmt.Errorf("deps seed %s: %w", s.From, err)
		}
		if err := dir(to, 0); err != nil {
			r.Close()
			return stats, nil, err
		}
		left := limits
		left.Bytes -= stats.Bytes
		left.Entries -= stats.Entries + stats.Skipped
		t := &tarWalk{ctx: ctx, r: r, tw: tw, limits: left, prefix: to, skip: s.Skip, written: &written}
		err = t.dir(".")
		r.Close()
		stats.Entries += t.stats.Entries
		stats.Bytes += t.stats.Bytes
		stats.Skipped += t.stats.Skipped
		if err != nil {
			return stats, nil, fmt.Errorf("deps seed %s: %w", s.From, err)
		}
	}
	if err := tw.Close(); err != nil {
		return stats, nil, err
	}
	return stats, written, nil
}

// SeedDeps copies the seed stream (SeedTar) into the volume through a container of the image that is created and
// never started (docker cp writes into its volume; no code runs), then removes the container. The volume must be
// Agentium's (EnsureDepsVolume). It returns the files and links written, for the caller's record of what the volume
// holds (Seed.Skip). The container is labelled like every container of the data folder, so recovery and clean find it
// if Agentium dies before removing it.
func (d *Docker) SeedDeps(ctx context.Context, v DepsVolume, img Image, seeds []Seed, entries []SeedEntry, limits CopyLimits) (written []string, err error) {
	if _, ok, err := d.depsVolumeIs(ctx, v); err != nil || !ok {
		return nil, errors.Join(fmt.Errorf("deps volume %s: make it first (EnsureDepsVolume)", v.Name()), err)
	}
	if !imageID.MatchString(img.ID) || !digestRef.MatchString(img.Ref) && !imageID.MatchString(img.Ref) {
		return nil, fmt.Errorf("deps seed: image not found by digest (%q)", img.Ref)
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	run := "seed-" + hex.EncodeToString(b)
	name := "agentium-" + v.Data + "-" + run + "-seed"
	create := []string{"create", "--pull", "never", "--name", name,
		"--label", LabelData + "=" + v.Data, "--label", LabelRun + "=" + run, "--label", LabelMode + "=" + Mode,
		"--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", MainUser,
		"--log-driver", "none", "--mount", "type=volume,src=" + v.Name() + ",dst=" + DepsDir, "--entrypoint", "true", img.Ref}
	_, stderr, res, err := d.call(ctx, create, nil, controlTimeout)
	if err == nil && res.ExitCode != 0 {
		return nil, fmt.Errorf("deps seed: create %s: exit %d: %s", name, res.ExitCode, firstLine(stderr))
	}
	defer func() {
		if rmErr := d.removeName(ctx, name); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("%w (recovery and clean remove it by its labels): %w", ErrCleanup, rmErr))
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("deps seed: create %s: %w", name, err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	walkCtx, stop := context.WithCancel(ctx)
	defer stop()
	var writeErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, written, writeErr = SeedTar(walkCtx, pw, seeds, entries, limits)
		pw.Close()
	}()
	timeout := limits.Timeout
	if timeout <= 0 {
		timeout = SeedLimits().Timeout
	}
	_, stderr, res, err = d.call(ctx, []string{"cp", "-", name + ":" + DepsDir}, pr, timeout)
	// docker has stopped reading: the walk stops at its next entry, and its next write fails.
	stop()
	pr.Close()
	<-done
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, fmt.Errorf("deps seed into %s: %w", v.Name(), err)
	case res.ExitCode != 0:
		return nil, fmt.Errorf("deps seed into %s: exit %d: %s", v.Name(), res.ExitCode, firstLine(stderr))
	case writeErr != nil:
		return nil, fmt.Errorf("deps seed into %s: %w", v.Name(), writeErr)
	}
	return written, nil
}

// WarmSpec is a deps warm-up's container: the grading image, the project's deps volume mounted read-write, and the
// default network, for commands from a trusted commit (the task's base) only, as host warm-ups are.
type WarmSpec struct {
	Volume   DepsVolume
	Image    Image // the grading image the volume is for (its ID is Volume.Image)
	Limits   Limits
	Deadline time.Duration
}

// Warm gives fn a deps warm-up's container: the grade's shape (no capabilities, no new privileges, a read-only root,
// the grade's user, limits, the inspect check and the probes, removal whatever happens), except that it has the
// default network and the deps volume read-write. Only a trusted tree may go in (a checkout of the task's base, never
// an agent's work): its commands may fetch and write what grades later read.
func (d *Docker) Warm(ctx context.Context, w WarmSpec, fn func(ctx context.Context, c *Container) error) error {
	if err := w.Volume.validate(); err != nil {
		return err
	}
	if w.Image.ID != w.Volume.Image {
		return fmt.Errorf("deps warm-up: the image %s is not the volume's (%s)", w.Image.ID, w.Volume.Image)
	}
	if _, ok, err := d.depsVolumeIs(ctx, w.Volume); err != nil || !ok {
		return errors.Join(fmt.Errorf("deps volume %s: make it first (EnsureDepsVolume)", w.Volume.Name()), err)
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	spec := Spec{Data: w.Volume.Data, Run: "warm-" + hex.EncodeToString(b), Role: "warm", Image: w.Image, Limits: w.Limits, Deadline: w.Deadline,
		Deps: w.Volume.Name(), warm: true}
	return d.run(ctx, spec, fn)
}

// WarmCommand is one command of a deps warm-up: run in Dir under /grade/work, with Env over the image's.
type WarmCommand struct {
	Command string
	Dir     string
	Env     []string
	Timeout time.Duration
}

// WarmRun is Warm with a trusted tree (a checkout of the task's base) copied into /grade/work and the commands run in
// order, their output to out, stopping at the first that fails. It returns the commands that passed, and the one that
// failed ("" when none did); an error is the container's, not a command's.
func (d *Docker) WarmRun(ctx context.Context, w WarmSpec, tree string, cmds []WarmCommand, out io.Writer) (passed int, failed string, err error) {
	err = d.Warm(ctx, w, func(ctx context.Context, c *Container) error {
		if _, err := c.CopyIn(ctx, tree, DefaultCopyLimits()); err != nil {
			return err
		}
		for _, cmd := range cmds {
			res, err := c.Exec(ctx, Command{Command: cmd.Command, Dir: cmd.Dir, Env: cmd.Env, Timeout: cmd.Timeout, Output: out})
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				failed = cmd.Command
				if res.TimedOut {
					failed += " (timed out)"
				}
				return nil
			}
			passed++
		}
		return nil
	})
	return passed, failed, err
}
