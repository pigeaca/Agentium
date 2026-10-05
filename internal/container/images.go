package container

import (
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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

const (
	// pullTimeout bounds one pull of a base.
	pullTimeout = 30 * time.Minute
	// buildTimeout bounds one local build (apt's lists and two packages).
	buildTimeout = 30 * time.Minute
	// checkTimeout bounds the built image's check.
	checkTimeout = 2 * time.Minute
)

// ErrBaseMismatch: the image the daemon holds under a pin's digest is not the pinned content for this architecture.
var ErrBaseMismatch = errors.New("the base image is not the pinned one")

// Base finds a pin's base on the daemon by its pinned reference (never a tag; never pulling), and checks that it is
// the pinned content: its image ID is the architecture's config digest (the classic image store), or the reference's
// own digest (the containerd image store, which names images by their manifest).
func (d *Docker) Base(ctx context.Context, p Pin) (Image, error) {
	ref, err := p.Ref(d.engine.Arch)
	if err != nil {
		return Image{}, err
	}
	img, err := d.inspectImage(ctx, ref)
	if err != nil {
		return Image{}, err
	}
	_, refDigest, _ := strings.Cut(ref, "@")
	plat := p.Platforms[d.engine.Arch]
	if img.ID != plat.Config && img.ID != refDigest {
		return Image{}, fmt.Errorf("%w: %s is %s on this daemon, and the pin names %s for %s", ErrBaseMismatch, ref, img.ID, plat.Config, d.engine.Arch)
	}
	return Image{Ref: ref, ID: img.ID, Env: img.Config.Env, Arch: img.Architecture}, nil
}

// Built is a grading image built from a recipe and checked, as the data folder records it (BuiltImages).
type Built struct {
	Tag       string    `json:"tag"`
	ID        string    `json:"id"`     // the image ID: what grades run (Image.Ref), never the tag
	Recipe    string    `json:"recipe"` // Recipe.Hash
	Base      string    `json:"base"`   // the pinned base's reference
	BaseID    string    `json:"base_id"`
	Toolchain string    `json:"toolchain"` // the pin's name, "go 1.27"
	Inside    string    `json:"inside"`    // what the check read of the toolchain, "go1.27.1"
	Packages  []string  `json:"packages"`  // what apt installed, name=version
	Arch      string    `json:"arch"`
	At        time.Time `json:"built_at"`
}

// BuiltImages are the grading images built for a data folder, by tag.
type BuiltImages map[string]Built

// LoadBuilt reads the record of built images; a missing file is none.
func LoadBuilt(path string) (BuiltImages, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return BuiltImages{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("built images: %w", err)
	}
	b := BuiltImages{}
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("built images %s: %w", path, err)
	}
	return b, nil
}

// Save writes the record whole (a temp file renamed over it), owner-only.
func (b BuiltImages) Save(path string) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("built images: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("built images: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("built images: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("built images: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("built images: %w", err)
	}
	return nil
}

// GradingImage finds the grading image of a recipe: recorded as built, present by its image ID, and still labelled
// with the recipe and base it was built from. What a grade runs is that ID (Image.Ref), never the tag. A missing one is
// ErrImageMissing, naming the command that builds it.
func (d *Docker) GradingImage(ctx context.Context, r Recipe, built BuiltImages) (Image, Built, error) {
	rec, ok := built[r.Tag]
	if !ok || rec.Recipe != r.Hash || !imageID.MatchString(rec.ID) {
		return Image{}, Built{}, fmt.Errorf("%w: the grading image %s is not built (agentium images pull builds it, with consent)", ErrImageMissing, r.Pin.Name())
	}
	img, err := d.inspectImage(ctx, rec.ID)
	if err != nil {
		if errors.Is(err, ErrImageMissing) {
			return Image{}, Built{}, fmt.Errorf("%w: the grading image %s was removed (agentium images pull builds it again, with consent)", ErrImageMissing, r.Pin.Name())
		}
		return Image{}, Built{}, err
	}
	if err := r.checkLabels(img); err != nil {
		return Image{}, Built{}, err
	}
	return Image{Ref: rec.ID, ID: img.ID, Env: img.Config.Env, Arch: img.Architecture}, rec, nil
}

// checkLabels refuses an image that does not carry the recipe's labels.
func (r Recipe) checkLabels(img imageInfo) error {
	for _, l := range r.labels() {
		if got := img.Config.Labels[l[0]]; got != l[1] {
			return fmt.Errorf("%w: the grading image %s (%s) has label %s %q, want %q", ErrBaseMismatch, r.Tag, img.ID, l[0], got, l[1])
		}
	}
	return nil
}

// PlanItem is one pin's state on the daemon, and what getting its grading image takes.
type PlanItem struct {
	Pin    Pin
	Recipe Recipe
	// Ready: the grading image is built, present and labelled (Built holds its record); nothing to download.
	Ready bool
	Built Built
	// BasePresent: the base is present by its digest and is the pinned content. BaseSize is what pulling it downloads
	// at most (its compressed layers on this architecture); layers the daemon already has are not downloaded again.
	BasePresent bool
	BaseSize    int64
	BaseErr     error // set when the base's state could not be read, or it is not the pinned content
}

// ImagePlan is what `agentium images pull` would download and build.
type ImagePlan struct {
	Arch  string
	Items []PlanItem
}

// Download is the plan's download: base layers (an upper bound) and apt (an estimate), in bytes.
func (p ImagePlan) Download() (base, apt int64) {
	for _, it := range p.Items {
		if it.Ready {
			continue
		}
		if !it.BasePresent {
			base += it.BaseSize
		}
		apt += it.Pin.AptEstimate
	}
	return base, apt
}

// Pending is the items not ready.
func (p ImagePlan) Pending() []PlanItem {
	var out []PlanItem
	for _, it := range p.Items {
		if !it.Ready {
			out = append(out, it)
		}
	}
	return out
}

// Plan reads what each pin's grading image needs on this daemon. It downloads nothing.
func (d *Docker) Plan(ctx context.Context, pins []Pin, built BuiltImages) (ImagePlan, error) {
	plan := ImagePlan{Arch: d.engine.Arch}
	for _, p := range pins {
		r, err := NewRecipe(p, d.engine.Arch)
		if err != nil {
			return ImagePlan{}, err
		}
		it := PlanItem{Pin: p, Recipe: r, BaseSize: p.Platforms[d.engine.Arch].Size}
		if _, rec, err := d.GradingImage(ctx, r, built); err == nil {
			it.Ready, it.Built = true, rec
		} else if !errors.Is(err, ErrImageMissing) && !errors.Is(err, ErrBaseMismatch) {
			return ImagePlan{}, err
		}
		if _, err := d.Base(ctx, p); err == nil {
			it.BasePresent = true
		} else if !errors.Is(err, ErrImageMissing) {
			it.BaseErr = err
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}

// Fetch gets one plan item's grading image: the base pulled by its digest when it is missing, then built and checked.
// Only call it with the user's consent to the plan's download (Agentium never pulls or builds on its own). Progress
// goes to out. data is the data folder's ID, which labels the check's container.
func (d *Docker) Fetch(ctx context.Context, it PlanItem, data string, out io.Writer) (Built, error) {
	if it.Ready {
		return it.Built, nil
	}
	if it.BaseErr != nil {
		return Built{}, it.BaseErr
	}
	if !it.BasePresent {
		if err := d.pull(ctx, it.Recipe.Base, out); err != nil {
			return Built{}, err
		}
	}
	return d.build(ctx, it.Recipe, data, out)
}

// pull pulls a base by its digest (never a tag) and anonymously (the empty configuration holds no registry login).
func (d *Docker) pull(ctx context.Context, ref string, out io.Writer) error {
	if !digestRef.MatchString(ref) {
		return fmt.Errorf("pull %q: not pinned by digest", ref)
	}
	stderr, res, err := d.stream(ctx, []string{"pull", ref}, nil, out, pullTimeout)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("pull %s: exit %d: %s", ref, res.ExitCode, firstLine(stderr))
	}
	return nil
}

// build builds a recipe's grading image from its base (present and pinned), then checks the result: it is in the
// daemon under the ID the build reported, carries the recipe's labels, is built on the base's layers, and its git,
// less and toolchain run, offline, as the grade's user (checkScript).
func (d *Docker) build(ctx context.Context, r Recipe, data string, out io.Writer) (Built, error) {
	if !dataPattern.MatchString(data) {
		return Built{}, fmt.Errorf("build: data ID %q", data)
	}
	base, err := d.Base(ctx, r.Pin)
	if err != nil {
		return Built{}, err
	}
	baseInfo, err := d.inspectImage(ctx, base.Ref)
	if err != nil {
		return Built{}, err
	}
	dir, err := os.MkdirTemp("", "agentium-build-")
	if err != nil {
		return Built{}, fmt.Errorf("build %s: %w", r.Tag, err)
	}
	defer os.RemoveAll(dir)
	iid := filepath.Join(dir, "iid")
	buildContext, err := r.contextTar()
	if err != nil {
		return Built{}, fmt.Errorf("build %s: %w", r.Tag, err)
	}
	stderr, res, err := d.stream(ctx, r.buildArgs(iid), bytes.NewReader(buildContext), out, buildTimeout)
	if err != nil {
		return Built{}, fmt.Errorf("build %s: %w", r.Tag, err)
	}
	if res.ExitCode != 0 {
		return Built{}, fmt.Errorf("build %s: exit %d: %s", r.Tag, res.ExitCode, firstLine(stderr))
	}
	idBytes, err := os.ReadFile(iid)
	if err != nil {
		return Built{}, fmt.Errorf("build %s: the build reported no image ID: %w", r.Tag, err)
	}
	id := strings.TrimSpace(string(idBytes))
	if !imageID.MatchString(id) {
		return Built{}, fmt.Errorf("build %s: the build reported %q, not an image ID", r.Tag, id)
	}
	img, err := d.inspectImage(ctx, id)
	if err != nil {
		return Built{}, fmt.Errorf("build %s: the image it reported is not in the daemon: %w", r.Tag, err)
	}
	if err := r.checkLabels(img); err != nil {
		return Built{}, err
	}
	if !slices.Contains(img.RepoTags, r.Tag) {
		return Built{}, fmt.Errorf("build %s: the image %s is not tagged %s", r.Tag, id, r.Tag)
	}
	if len(img.RootFS.Layers) <= len(baseInfo.RootFS.Layers) || !slices.Equal(img.RootFS.Layers[:len(baseInfo.RootFS.Layers)], baseInfo.RootFS.Layers) {
		return Built{}, fmt.Errorf("build %s: the image %s is not built on the base %s's layers", r.Tag, id, base.Ref)
	}
	packages, inside, err := d.checkBuilt(ctx, r, id, data)
	if err != nil {
		return Built{}, err
	}
	return Built{Tag: r.Tag, ID: id, Recipe: r.Hash, Base: r.Base, BaseID: base.ID, Toolchain: r.Pin.Name(), Inside: inside, Packages: packages,
		Arch: img.Architecture, At: time.Now().UTC().Truncate(time.Second)}, nil
}

// checkBuilt runs the built image's check in a throwaway container: offline, read-only, no capabilities, as the
// grade's user, labelled like every container of the data folder so that clean finds it if Agentium dies first.
func (d *Docker) checkBuilt(ctx context.Context, r Recipe, id, data string) (packages []string, inside string, err error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	runID := "images-" + hex.EncodeToString(b)
	name := "agentium-" + data + "-" + runID + "-check"
	args := []string{"run", "--rm", "--pull", "never", "--name", name,
		"--label", LabelData + "=" + data, "--label", LabelRun + "=" + runID, "--label", LabelMode + "=" + Mode,
		"--network", "none", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=67108864", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", User, "--memory", "1073741824", "--pids-limit", "256",
		"--env", "HOME=/tmp", "--entrypoint", "sh", id, "-c", r.checkScript()}
	out, stderr, res, err := d.call(ctx, args, nil, checkTimeout)
	if rmErr := d.removeName(ctx, name); rmErr != nil { // --rm already did, unless the client was cut short
		err = errors.Join(err, rmErr)
	}
	if err != nil {
		return nil, "", fmt.Errorf("check %s: %w", r.Tag, err)
	}
	if res.ExitCode != 0 {
		return nil, "", fmt.Errorf("check %s: exit %d: %s", r.Tag, res.ExitCode, firstLine(stderr))
	}
	return r.readCheck(string(out))
}

// stream runs docker with its output going to out as it comes (a pull's or a build's progress), stderr kept too.
func (d *Docker) stream(ctx context.Context, args []string, stdin io.Reader, out io.Writer, timeout time.Duration) (string, runner.Result, error) {
	if out == nil {
		out = io.Discard
	}
	stderr := &capped{max: 64 << 10}
	// What docker prints reaches the terminal redacted (its endpoint, the home folder as ~), a whole line at a time.
	shown := &redactingWriter{w: out, redact: d.redact}
	defer shown.Flush()
	res, err := runner.Run(ctx, runner.Spec{Args: append([]string{d.bin}, d.args(args)...), Environ: d.environ, Timeout: timeout,
		Output: shown, Stderr: io.MultiWriter(shown, stderr), Stdin: stdin})
	if err != nil {
		return d.redact(stderr.String()), res, err
	}
	if res.TimedOut {
		return d.redact(stderr.String()), res, fmt.Errorf("timed out after %s", timeout)
	}
	return d.redact(stderr.String()), res, nil
}

// LocalImage is an image of Agentium's on the daemon: a grading image (by its labels) or a pinned base (by its digest).
type LocalImage struct {
	Kind      string // "grading" or "base"
	Ref       string // a grading image's tag, or a base's pinned reference
	ID        string
	Toolchain string // the pin's name
	Size      int64  // bytes, unpacked, as the daemon reports it
	Current   bool   // a grading image of the current recipe, or a base of a current pin
	// OtherTags are the image's tags that are not Agentium's (a base pulled by you as golang:1.27, say): removing the
	// image by Agentium's reference then only drops that reference, and the image stays.
	OtherTags []string
}

// LocalImages lists the grading images on the daemon (any recipe, current or not) and the pinned bases present.
func (d *Docker) LocalImages(ctx context.Context) ([]LocalImage, error) {
	current := map[string]bool{}
	var list []LocalImage
	for _, p := range pins() {
		if r, err := NewRecipe(p, d.engine.Arch); err == nil {
			current[r.Tag] = true
		}
		img, err := d.Base(ctx, p)
		if errors.Is(err, ErrImageMissing) || errors.Is(err, ErrBaseMismatch) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := d.inspectImage(ctx, img.Ref)
		if err != nil {
			return nil, err
		}
		list = append(list, LocalImage{Kind: "base", Ref: img.Ref, ID: img.ID, Toolchain: p.Name(), Size: info.Size, Current: true, OtherTags: info.RepoTags})
	}
	out, err := d.output(ctx, "image", "ls", "--no-trunc", "--filter", "label="+LabelRecipe, "--format", `{{.ID}}	{{.Repository}}:{{.Tag}}	{{.Label "agentium.toolchain"}}`)
	if err != nil {
		return nil, fmt.Errorf("grading images: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || !imageID.MatchString(f[0]) {
			continue
		}
		info, err := d.inspectImage(ctx, f[0])
		if err != nil {
			if errors.Is(err, ErrImageMissing) {
				continue // removed meanwhile
			}
			return nil, err
		}
		var others []string
		for _, tag := range info.RepoTags {
			if !strings.HasPrefix(tag, GradingRepository+":") {
				others = append(others, tag)
			}
		}
		list = append(list, LocalImage{Kind: "grading", Ref: f[1], ID: f[0], Toolchain: f[2], Size: info.Size, Current: current[f[1]], OtherTags: others})
	}
	return list, nil
}

// RemoveImage removes an image by its tag, pinned reference or ID, never forcing: an image a container uses, or one
// with other tags when named by ID, stays, and the daemon's refusal is the error. An image already gone is not one.
func (d *Docker) RemoveImage(ctx context.Context, ref string) error {
	_, stderr, res, err := d.call(ctx, []string{"image", "rm", ref}, nil, controlTimeout)
	if err != nil {
		return fmt.Errorf("remove image %s: %w", ref, err)
	}
	if res.ExitCode != 0 && !strings.Contains(stderr, "No such image") {
		return fmt.Errorf("remove image %s: %s", ref, firstLine(stderr))
	}
	return nil
}

// HumanSize is a size as docker prints it (1.2GB, 512MB, 64kB, 12B), in bytes; -1 when it cannot be read.
func HumanSize(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"KB", 1e3}, {"B", 1}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			if err != nil || f < 0 {
				return -1
			}
			return int64(f * u.mult)
		}
	}
	return -1
}

// redactingWriter passes whole lines through redact (a partial line waits for its end, or Flush; a line longer than
// 64 KiB goes as it is cut). It is safe for concurrent writes, as stdout and stderr share it.
type redactingWriter struct {
	mu     sync.Mutex
	w      io.Writer
	redact func(string) string
	buf    []byte
}

func (r *redactingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexAny(r.buf, "\n\r")
		if i < 0 {
			if len(r.buf) > 64<<10 {
				i = len(r.buf) - 1
			} else {
				return len(p), nil
			}
		}
		if _, err := io.WriteString(r.w, r.redact(string(r.buf[:i+1]))); err != nil {
			return len(p), err
		}
		r.buf = r.buf[i+1:]
	}
}

// Flush writes what is left of a partial line.
func (r *redactingWriter) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) > 0 {
		io.WriteString(r.w, r.redact(string(r.buf)))
		r.buf = nil
	}
}
