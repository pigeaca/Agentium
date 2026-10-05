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
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
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

// Seed is a host folder copied into a deps volume, at To (slash-separated, relative to /deps): the folder Path
// (slash-separated, relative) under Root, a trusted cache folder (the project's deps folder, the user's Go module
// cache). Root is opened as it is; Path is resolved through it one folder at a time without following any link, so a
// link anywhere on the way (to an unrelated folder of the host, which grades could then read) refuses the whole seed
// stream before a byte is written. Its entries go in owned by the grade's user, links as links (never followed), as
// WriteTar writes a tree. Skip leaves out files and links by their name under /deps (those the caller knows the volume
// holds, or does not want): the volume's own tar never replaces a file there anyway (SeedDeps). A Root or Path that
// does not exist is no seed.
type Seed struct {
	Root string
	Path string
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

// SeedTar writes the seed stream: every folder on the way to each entry and seed, listed before it, then the entries,
// then each seed's tree. Every entry is owned by the grade's user. It returns the files and links it wrote, by name
// under /deps. Every seed's folder is opened first (openSeeds): a link on its way refuses the stream, empty.
func SeedTar(ctx context.Context, w io.Writer, seeds []Seed, entries []SeedEntry, limits CopyLimits) (TarStats, []string, error) {
	opened, err := openSeeds(seeds)
	if err != nil {
		return TarStats{}, nil, err
	}
	defer closeSeeds(opened)
	return seedTar(ctx, w, opened, entries, limits)
}

// openSeed is a seed whose folder is open (nil: it does not exist).
type openSeed struct {
	Seed
	to string
	r  *os.Root
}

// openSeeds opens every seed's folder through its trusted root, never following a link (openUnder), and checks every
// To; any refusal closes them all.
func openSeeds(seeds []Seed) ([]openSeed, error) {
	var out []openSeed
	for _, s := range seeds {
		to, err := cleanRel(s.To)
		if err != nil {
			closeSeeds(out)
			return nil, err
		}
		r, err := openSeedRoot(s)
		if err != nil {
			closeSeeds(out)
			return nil, err
		}
		out = append(out, openSeed{Seed: s, to: to, r: r})
	}
	return out, nil
}

func closeSeeds(seeds []openSeed) {
	for _, s := range seeds {
		if s.r != nil {
			s.r.Close()
		}
	}
}

// openSeedRoot opens a seed's folder; nil, nil when it (or its root) does not exist.
func openSeedRoot(s Seed) (*os.Root, error) {
	base, err := os.OpenRoot(s.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("deps seed %s: %w", s.Root, err)
	}
	defer base.Close()
	r, err := openUnder(base, s.Path, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("deps seed %s in %s: %w", s.Path, s.Root, err)
	}
	return r, nil
}

// errSeedLink: a seed's folder, or one on its way, is not a real folder (a link, say): it could lead anywhere on the
// host.
var errSeedLink = errors.New("not a real folder (a link is never followed)")

// openUnder opens the folder rel under base one component at a time, without following a link and without
// blocking. For each component:
//   - it must be a real folder by Lstat (a link is refused, even one that stays inside base, which os.Root follows);
//   - it is opened as a folder only and without blocking (openDir, the copy-in's own open: a pipe swapped in fails at
//     once), and the open folder must be the one Lstat saw (a link or another folder swapped in is refused);
//   - its os.Root is made from that open folder itself (/dev/fd/N names the open file, not a path), so nothing can be
//     swapped in between, and it is checked to be the same folder again.
//
// hook, when set (tests), runs between a component's Lstat and its open. The caller closes the result; base stays
// open.
func openUnder(base *os.Root, rel string, hook func(component string)) (*os.Root, error) {
	clean := path.Clean(rel)
	if rel == "" || path.IsAbs(rel) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(rel, 0) {
		return nil, fmt.Errorf("%q: not a folder under the root", rel)
	}
	cur, err := base.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if clean == "." {
		return cur, nil
	}
	for _, c := range strings.Split(clean, "/") {
		next, err := openComponent(cur, c, hook)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// openComponent is one step of openUnder.
func openComponent(cur *os.Root, c string, hook func(string)) (*os.Root, error) {
	want, err := cur.Lstat(c)
	if err != nil {
		return nil, err
	}
	if !want.IsDir() {
		return nil, fmt.Errorf("%s: %w (%s)", c, errSeedLink, want.Mode().Type())
	}
	if hook != nil {
		hook(c)
	}
	f, err := openDir(cur, c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", c, errSeedLink, err)
	}
	defer f.Close()
	got, err := f.Stat()
	if err != nil || !os.SameFile(want, got) {
		return nil, fmt.Errorf("%s: %w: it changed while it was opened", c, errSeedLink)
	}
	next, err := os.OpenRoot("/dev/fd/" + strconv.FormatUint(uint64(f.Fd()), 10))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c, err)
	}
	if again, err := next.Stat("."); err != nil || !os.SameFile(got, again) {
		next.Close()
		return nil, fmt.Errorf("%s: %w: it changed while it was opened", c, errSeedLink)
	}
	return next, nil
}

// seedTar is SeedTar for opened seeds.
func seedTar(ctx context.Context, w io.Writer, seeds []openSeed, entries []SeedEntry, limits CopyLimits) (TarStats, []string, error) {
	tw := tar.NewWriter(w)
	var stats TarStats
	var written []string
	listed := map[string]bool{}
	dir := func(name string) error {
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
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: 0o755, Uid: 65534, Gid: 65534, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
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
			if err := dir(name); err != nil {
				return stats, nil, err
			}
			continue
		}
		if err := dir(path.Dir(name)); err != nil {
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
		if s.r == nil {
			continue
		}
		if err := dir(s.to); err != nil {
			return stats, nil, err
		}
		left := limits
		left.Bytes -= stats.Bytes
		left.Entries -= stats.Entries + stats.Skipped
		t := &tarWalk{ctx: ctx, r: s.r, tw: tw, limits: left, prefix: s.to, skip: s.Skip, written: &written}
		err := t.dir(".")
		stats.Entries += t.stats.Entries
		stats.Bytes += t.stats.Bytes
		stats.Skipped += t.stats.Skipped
		if err != nil {
			return stats, nil, fmt.Errorf("deps seed %s in %s: %w", s.Path, s.Root, err)
		}
	}
	if err := tw.Close(); err != nil {
		return stats, nil, err
	}
	return stats, written, nil
}

// seedTops are the volume's top folders the seed stream writes into: the grade's user cannot make them, since the
// volume's root is root's, so the daemon makes them first (docker cp).
func seedTops(seeds []openSeed, entries []SeedEntry) []string {
	var tops []string
	add := func(name string) {
		if clean, err := cleanRel(name); err == nil {
			top, _, _ := strings.Cut(clean, "/")
			if !slices.Contains(tops, top) {
				tops = append(tops, top)
			}
		}
	}
	for _, e := range entries {
		add(e.Name)
	}
	for _, s := range seeds {
		add(s.To)
	}
	return tops
}

// topsTar is the trusted tar of the volume's top folders, owned by the grade's user.
func topsTar(tops []string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, t := range tops {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: t + "/", Mode: 0o755, Uid: 65534, Gid: 65534, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// seedStage is the volume's top folder where a seed's stream is unpacked before any of it is moved into place.
const seedStage = ".seed"

// seedScript runs in the seed's container, as the grade's user, with the stream on stdin and a fresh folder name as
// $1. It first removes what earlier seeds that were cut short left in the staging folder (the caller holds the
// volume's lock, and SeedDeps has removed every other seed container of the volume first, so no other seed runs). It
// unpacks the whole stream there, and only once tar has read all of it does it put each file and link into place: a
// hard link to its final name (link(2) never replaces an existing name, and the file is whole by then; -T makes the
// final name the link itself, never a name inside a folder, or a link to one, that is there already), then the staging
// name goes. A name the volume holds already, whoever wrote it, stays as it is; ln's own message says why a name that
// is still missing could not be placed. A stream cut mid-file makes tar fail, and nothing of it reaches a final name; its staging folder goes with the
// next seed. Folders on the way are made as needed.
const seedScript = `set -eu
umask 022
stage=/deps/` + seedStage + `
find "$stage" -mindepth 1 -maxdepth 1 -exec chmod -R u+w {} + -exec rm -rf {} +
mkdir "$stage/$1"
tar --extract --file - --directory "$stage/$1" --no-same-owner
cd "$stage/$1"
find . ! -type d -exec sh -c '
  for f do
    dest=/deps/${f#./}
    mkdir -p "${dest%/*}"
    if ! err=$(ln -P -T "$f" "$dest" 2>&1) && [ ! -e "$dest" ] && [ ! -L "$dest" ]; then
      echo "seed: cannot place ${f#./}: $err" >&2
      exit 1
    fi
  done' sh {} +
cd /
chmod -R u+w "$stage/$1"
rm -rf "$stage/$1"
`

// SeedDeps copies the seed stream (SeedTar) into the volume, never replacing what it holds (a file there may be one a
// grade is reading, or one a warm-up wrote) and never leaving a part of a file under its final name. Every seed's
// folder is opened first, so a refused seed makes nothing. A container of the image is created (no network, no
// capabilities, a read-only root, as the grade's user), the daemon makes the volume's top folders in it (docker cp of a
// trusted tar: the volume's root is root's), and then it runs seedScript: the stream is unpacked into a staging folder
// in the volume, and each file is put into place only once all of it arrived. The container is removed after, and is
// labelled like every container of the data folder, so recovery and clean find it if Agentium dies first. The caller
// holds the volume's lock (run's warm-up lock): seedScript removes earlier seeds' staging folders. The volume must be
// Agentium's (EnsureDepsVolume). It returns the files and links sent (placed, or kept as the volume had them), for the
// caller's record of what the volume holds (Seed.Skip).
func (d *Docker) SeedDeps(ctx context.Context, v DepsVolume, img Image, seeds []Seed, entries []SeedEntry, limits CopyLimits) (written []string, err error) {
	if _, ok, err := d.depsVolumeIs(ctx, v); err != nil || !ok {
		return nil, errors.Join(fmt.Errorf("deps volume %s: make it first (EnsureDepsVolume)", v.Name()), err)
	}
	if !imageID.MatchString(img.ID) || !digestRef.MatchString(img.Ref) && !imageID.MatchString(img.Ref) {
		return nil, fmt.Errorf("deps seed: image not found by digest (%q)", img.Ref)
	}
	opened, err := openSeeds(seeds)
	if err != nil {
		return nil, err
	}
	defer closeSeeds(opened)
	for _, e := range entries {
		if _, err := cleanRel(e.Name); err != nil {
			return nil, err
		}
	}
	tops, err := topsTar(append([]string{seedStage}, seedTops(opened, entries)...))
	if err != nil {
		return nil, err
	}
	// A seed container of this volume that a dead Agentium left can still be running (or start again): it would clear
	// this seed's staging folder, or this one its. They are Agentium's own, by their labels and their mount of this
	// volume, so they go, and none may be left.
	if err := d.removeSeeders(ctx, v); err != nil {
		return nil, err
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	run := "seed-" + hex.EncodeToString(b)
	name := "agentium-" + v.Data + "-" + run + "-seed"
	create := []string{"create", "--pull", "never", "--name", name,
		"--label", LabelData + "=" + v.Data, "--label", LabelRun + "=" + run, "--label", LabelMode + "=" + Mode,
		"--interactive", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", User,
		"--log-driver", "none", "--mount", "type=volume,src=" + v.Name() + ",dst=" + DepsDir, "--workdir", DepsDir,
		"--entrypoint", "sh", img.Ref, "-c", seedScript, "sh", hex.EncodeToString(b)}
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
	if _, stderr, res, err := d.call(ctx, []string{"cp", "-", name + ":" + DepsDir}, bytes.NewReader(tops), controlTimeout); err != nil || res.ExitCode != 0 {
		return nil, errors.Join(fmt.Errorf("deps seed: the volume's top folders: %s", firstLine(stderr)), err)
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
		var w io.Writer = pw
		if d.hooks.seedStream != nil {
			w = d.hooks.seedStream(pw)
		}
		_, written, writeErr = seedTar(walkCtx, w, opened, entries, limits)
		pw.Close()
	}()
	timeout := limits.Timeout
	if timeout <= 0 {
		timeout = SeedLimits().Timeout
	}
	_, stderr, res, err = d.call(ctx, []string{"start", "--attach", "--interactive", name}, pr, timeout)
	// The volume's tar has stopped reading: the walk stops at its next entry, and its next write fails.
	stop()
	pr.Close()
	<-done
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, fmt.Errorf("deps seed into %s: %w", v.Name(), err)
	case res.ExitCode != 0:
		return nil, fmt.Errorf("deps seed into %s: exit %d (nothing of a file cut short is in place): %s", v.Name(), res.ExitCode, firstLine(stderr))
	case writeErr != nil:
		return nil, fmt.Errorf("deps seed into %s: %w", v.Name(), writeErr)
	}
	return written, nil
}

// removeSeeders removes every seed container of the volume (labelled with the data folder and a seed's run, named as
// SeedDeps names them, and mounting the volume), whatever its state, and confirms each is gone; one that stays is an
// error, and nothing is seeded.
func (d *Docker) removeSeeders(ctx context.Context, v DepsVolume) error {
	out, err := d.output(ctx, "ps", "--all", "--no-trunc", "--filter", "label="+LabelData+"="+v.Data, "--filter", "label="+LabelMode+"="+Mode,
		"--filter", "volume="+v.Name(), "--format", `{{.Names}}	{{.Label "agentium.run"}}`)
	if err != nil {
		return fmt.Errorf("deps seed: find earlier seeds of %s: %w", v.Name(), err)
	}
	for _, line := range lines(out) {
		name, run, _ := strings.Cut(line, "\t")
		if !strings.HasPrefix(run, "seed-") || name != "agentium-"+v.Data+"-"+run+"-seed" {
			continue
		}
		if err := d.removeName(ctx, name); err != nil {
			return fmt.Errorf("deps seed: an earlier seed of %s (%s) could not be removed, so nothing is seeded: %w", v.Name(), name, err)
		}
	}
	return nil
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
