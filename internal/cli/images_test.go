package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/container"
)

// fakeDocker is the Docker driver for the images and clean commands: the pins' state on an arm64 daemon, and a record
// of what was fetched and removed.
type fakeDocker struct {
	mu        sync.Mutex
	rmHook    func()          // runs in RemoveImage, outside the lock
	present   map[string]bool // bases present, by toolchain
	later     map[string]bool // present from the second Plan on, when set
	fetched   []string
	fetchErr  error
	local     []container.LocalImage
	imagesRm  []string
	items     []container.Item
	removed   []string
	runs      []string
	idleErr   map[string]error
	closed    int
	planCalls int
}

func (f *fakeDocker) Engine() container.Engine { return container.Engine{OS: "linux", Arch: "arm64"} }
func (f *fakeDocker) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeDocker) Plan(_ context.Context, pins []container.Pin, built container.BuiltImages) (container.ImagePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planCalls++
	present := f.present
	if f.later != nil && f.planCalls > 1 {
		present = f.later
	}
	plan := container.ImagePlan{Arch: "arm64"}
	for _, p := range pins {
		r, err := container.NewRecipe(p, "arm64")
		if err != nil {
			return plan, err
		}
		it := container.PlanItem{Pin: p, Recipe: r, BasePresent: present[p.Toolchain], BaseSize: p.Platforms["arm64"].Size}
		if b, ok := built[r.Tag]; ok {
			it.Ready, it.Built = true, b
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}

func (f *fakeDocker) Fetch(_ context.Context, it container.PlanItem, data string, out io.Writer) (container.Built, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		return container.Built{}, f.fetchErr
	}
	f.fetched = append(f.fetched, it.Pin.Name())
	io.WriteString(out, "Step 1/2 : FROM "+it.Recipe.Base+"\n")
	return container.Built{Tag: it.Recipe.Tag, ID: "sha256:" + strings.Repeat("b", 64), Recipe: it.Recipe.Hash, Base: it.Recipe.Base, Toolchain: it.Pin.Name(),
		Inside: "go1.27.1", Packages: []string{"git=1:2.47.3-0+deb13u1", "less=668-1"}, Arch: "arm64", At: time.Unix(0, 0).UTC()}, nil
}

func (f *fakeDocker) LocalImages(context.Context) ([]container.LocalImage, error) {
	return f.local, nil
}

func (f *fakeDocker) RemoveImage(_ context.Context, ref string) error {
	if f.rmHook != nil {
		f.rmHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imagesRm = append(f.imagesRm, ref)
	return nil
}

func (f *fakeDocker) Inventory(context.Context, string) ([]container.Item, error) {
	return f.items, nil
}

func (f *fakeDocker) RemoveIdle(_ context.Context, _ string, it container.Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, it.Name)
	return f.idleErr[it.Name]
}

func (f *fakeDocker) RemoveRun(_ context.Context, _ string, run string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, run)
	return nil
}

// imagesEnv runs agentium outside any repository, with a data folder of its own and the fake Docker.
func imagesEnv(t *testing.T, d *fakeDocker, stdin string, terminal bool) (func(args ...string) cliResult, string) {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	dir := t.TempDir()
	return func(args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		env := Env{Args: args, Stdout: &stdout, Stderr: &stderr, Dir: dir, DefaultGrader: "host", Terminal: terminal, StdinTerminal: terminal,
			Stdin: strings.NewReader(stdin), Getenv: func(k string) string { return map[string]string{"AGENTIUM_HOME": data, "HOME": dir}[k] },
			Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH")} }, Now: time.Now,
			Docker: func(context.Context, []string) (DockerClient, error) { return d, nil }}
		code := Run(context.Background(), env)
		return cliResult{code, stdout.String(), stderr.String()}
	}, data
}

// A pull never downloads without consent: off a terminal and without --yes it shows the download and stops; at a
// terminal, only "yes" goes on.
func TestImagesPullNeedsConsent(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		args     []string
		stdin    string
		terminal bool
		fetched  bool
	}{
		{"no terminal, no --yes", []string{"images", "pull", "rust", "go"}, "", false, false},
		{"answered no", []string{"images", "pull", "rust", "go"}, "n\n", true, false},
		{"answered nothing", []string{"images", "pull", "rust", "go"}, "", true, false},
		{"answered yes", []string{"images", "pull", "rust", "go"}, "yes\n", true, true},
		{"--yes", []string{"images", "pull", "--yes", "rust", "go"}, "", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &fakeDocker{present: map[string]bool{"go": true}}
			run, data := imagesEnv(t, d, tt.stdin, tt.terminal)
			res := run(tt.args...)
			// The consent names every download: the base pulled by its digest, with its size and registry, and apt's.
			expect(t, res, map[bool]int{true: ExitOK, false: ExitError}[tt.fetched],
				"would download, on this machine's Docker (linux/arm64)",
				"go 1.27      base present; then build it, installing git and less through apt (about 15.0 MB)",
				"rust 1.95    pull rust:1.95-slim@039d198bab66 (279.3 MB compressed, from Docker Hub); then build it, installing git and less through apt (about 40.0 MB)",
				"Total: about 334.3 MB: 279.3 MB of base images")
			if got := len(d.fetched) > 0; got != tt.fetched {
				t.Fatalf("fetched %v, want %v:\n%s", d.fetched, tt.fetched, res.stdout)
			}
			if !tt.fetched {
				if !strings.Contains(res.stdout, "Nothing was downloaded") {
					t.Errorf("no refusal:\n%s", res.stdout)
				}
				if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the data folder was written without consent: %v", err)
				}
				if !strings.Contains(res.stdout, "agentium images pull --yes "+strings.Join(tt.args[2:], " ")) {
					t.Errorf("no command to agree with:\n%s", res.stdout)
				}
				return
			}
			if !slices.Equal(d.fetched, []string{"go 1.27", "rust 1.95"}) {
				t.Errorf("fetched %v", d.fetched)
			}
			built, err := container.LoadBuilt(builtImagesPathFor(data))
			if err != nil || len(built) != 2 {
				t.Errorf("record %v, %v", built, err)
			}
			if !strings.Contains(res.stdout, "The grading images are ready: go 1.27, rust 1.95.") {
				t.Errorf("summary:\n%s", res.stdout)
			}
			// Once built, a pull downloads nothing and asks nothing.
			again := run("images", "pull", "rust", "go")
			expect(t, again, ExitOK, "The grading images are ready: go 1.27, rust 1.95.")
			if strings.Contains(again.stdout, "would download") || len(d.fetched) != 2 {
				t.Errorf("a second pull: %s (fetched %v)", again.stdout, d.fetched)
			}
		})
	}
}

// A base removed between the consent and the pull is not pulled: that download was not agreed to.
func TestImagesPullStaysWithinConsent(t *testing.T) {
	t.Parallel()
	d := &fakeDocker{present: map[string]bool{"go": true}, later: map[string]bool{}}
	run, _ := imagesEnv(t, d, "", false)
	res := run("images", "pull", "--yes", "go")
	expect(t, res, ExitError, "go 1.27      base present", "go 1.27: it changed since you agreed")
	if len(d.fetched) > 0 {
		t.Errorf("fetched %v", d.fetched)
	}
}

// A removal and a pull at once: the pull waits for the removal's lock, so the image it builds stays in the record.
func TestImagesRemoveKeepsAConcurrentPull(t *testing.T) {
	t.Parallel()
	d := &fakeDocker{present: map[string]bool{"rust": true},
		local: []container.LocalImage{{Kind: "grading", Ref: "agentium-grade:go1.27-aaaaaaaaaaaa", Toolchain: "go 1.27", Size: 920e6, Current: true}}}
	run, data := imagesEnv(t, d, "", false)
	must(t, os.MkdirAll(data, 0o700))
	must(t, container.BuiltImages{"agentium-grade:go1.27-aaaaaaaaaaaa": {Tag: "agentium-grade:go1.27-aaaaaaaaaaaa"}}.Save(builtImagesPathFor(data)))
	removing, release := make(chan struct{}), make(chan struct{})
	d.rmHook = func() { close(removing); <-release }
	removed := make(chan cliResult, 1)
	go func() { removed <- run("images", "remove", "--yes", "go") }()
	<-removing // the removal has read the record and is removing
	pulled := make(chan cliResult, 1)
	go func() { pulled <- run("images", "pull", "--yes", "rust") }()
	select {
	case res := <-pulled:
		t.Fatalf("the pull finished while the removal held the record: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	expect(t, <-removed, ExitOK, "removed grading agentium-grade:go1.27-aaaaaaaaaaaa")
	expect(t, <-pulled, ExitOK, "The grading images are ready: rust 1.95.")
	built, err := container.LoadBuilt(builtImagesPathFor(data))
	must(t, err)
	rust, _ := container.PinFor("rust")
	r, _ := container.NewRecipe(rust, "arm64")
	if _, ok := built[r.Tag]; !ok || len(built) != 1 {
		t.Errorf("record %v: the pull's image is lost", built)
	}
}

// Without a toolchain named, images looks up the project in the current folder and writes nothing: no data folder
// for an unregistered repository, and none of a registered project's files.
func TestImagesProjectLookupWritesNothing(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	data := filepath.Join(t.TempDir(), "data")
	d := &fakeDocker{}
	run := func(dir string, args ...string) cliResult {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), Env{Args: args, Stdout: &stdout, Stderr: &stderr, Dir: dir, DefaultGrader: "host",
			Getenv:  func(k string) string { return map[string]string{"AGENTIUM_HOME": data, "HOME": repo}[k] },
			Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH")} }, Now: time.Now,
			Docker: func(context.Context, []string) (DockerClient, error) { return d, nil }})
		return cliResult{code, stdout.String(), stderr.String()}
	}
	expect(t, run(repo, "images", "pull"), ExitUsage)
	expect(t, run(repo, "images"), ExitOK)
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the data folder was made: %v", err)
	}
	// A registered project: its needs are read, and its data folder stays as it was.
	f := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	*f.docker = func(context.Context, []string) (DockerClient, error) { return d, nil }
	writeFile(t, f.repo, "go.mod", "module m\n\ngo 1.27\n")
	before := tree(t, f.data)
	expect(t, f.run(context.Background(), "images"), ExitOK, "this project")
	if after := tree(t, f.data); after != before {
		t.Errorf("images wrote to the data folder:\nbefore\n%s\nafter\n%s", before, after)
	}
	if len(d.fetched) > 0 {
		t.Error("fetched")
	}
}

func builtImagesPathFor(data string) string { return filepath.Join(data, "container-images.json") }

func TestImagesUsage(t *testing.T) {
	t.Parallel()
	d := &fakeDocker{}
	run, _ := imagesEnv(t, d, "", false)
	expect(t, run("images", "pull", "node"), ExitUsage)
	if res := run("images", "pull"); res.code != ExitUsage || !strings.Contains(res.stderr, "name the toolchains") {
		t.Errorf("pull outside a project: %+v", res)
	}
	expect(t, run("images", "frob"), ExitUsage)
	if len(d.fetched) > 0 {
		t.Error("fetched")
	}
	// Without Docker, the images commands fail and say so.
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), Env{Args: []string{"images"}, Stdout: &stdout, Stderr: &stderr, Dir: t.TempDir(), DefaultGrader: "host",
		Getenv: func(k string) string { return map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "d")}[k] }})
	if code != ExitError || !strings.Contains(stderr.String(), "Docker is not set up") {
		t.Errorf("no Docker: %d %s", code, stderr.String())
	}
}

func TestImagesList(t *testing.T) {
	t.Parallel()
	d := &fakeDocker{present: map[string]bool{"go": true, "jdk": true}}
	goPin, _ := container.PinFor("go")
	ref, _ := goPin.Ref("arm64")
	d.local = []container.LocalImage{{Kind: "base", Ref: ref, Toolchain: "go 1.27", Size: 900e6, Current: true},
		{Kind: "grading", Ref: "agentium-grade:go1.27-000000000000", Toolchain: "go 1.27", Size: 920e6}}
	run, _ := imagesEnv(t, d, "", false)
	res := run("images")
	expect(t, res, ExitOK, "Grading images (linux/arm64): each built here from its pinned base, adding git and less",
		"go 1.27", "golang:1.27@e0174e51e812", "present, 900.0 MB", "not built (apt: about 15.0 MB)",
		"rust 1.95", "rust:1.95-slim@039d198bab66", "missing: 279.3 MB to pull",
		"python 3.12", "ghcr.io/astral-sh/uv:0.11.28-python3.12-trixie-slim@4a60515ef927", "missing: 69.2 MB to pull",
		"grading images of an earlier recipe: agentium-grade:go1.27-000000000000",
		"rust 1.95: still to confirm: the index digest", "agentium images pull shows the download and asks")
	if len(d.fetched) > 0 {
		t.Error("list fetched")
	}
}

// Removal lists without --yes, and removes only grading images unless --bases.
func TestImagesRemove(t *testing.T) {
	t.Parallel()
	goPin, _ := container.PinFor("go")
	ref, _ := goPin.Ref("arm64")
	d := &fakeDocker{local: []container.LocalImage{{Kind: "base", Ref: ref, Toolchain: "go 1.27", Size: 900e6, Current: true, OtherTags: []string{"golang:1.27"}},
		{Kind: "grading", Ref: "agentium-grade:go1.27-aaaaaaaaaaaa", Toolchain: "go 1.27", Size: 920e6, Current: true},
		{Kind: "grading", Ref: "agentium-grade:jdk21-bbbbbbbbbbbb", Toolchain: "jdk 21", Size: 600e6, Current: true}}}
	run, data := imagesEnv(t, d, "", false)
	dry := run("images", "remove", "go")
	expect(t, dry, ExitOK, "a dry run: nothing was removed", "agentium-grade:go1.27-aaaaaaaaaaaa", "agentium images remove --yes")
	if len(d.imagesRm) > 0 || strings.Contains(dry.stdout, ref) || strings.Contains(dry.stdout, "jdk21") {
		t.Errorf("dry run: removed %v\n%s", d.imagesRm, dry.stdout)
	}
	must(t, os.MkdirAll(data, 0o700))
	must(t, container.BuiltImages{"agentium-grade:go1.27-aaaaaaaaaaaa": {Tag: "agentium-grade:go1.27-aaaaaaaaaaaa"}, "agentium-grade:jdk21-bbbbbbbbbbbb": {}}.Save(builtImagesPathFor(data)))
	expect(t, run("images", "remove", "--yes", "go"), ExitOK, "removed grading agentium-grade:go1.27-aaaaaaaaaaaa")
	if !slices.Equal(d.imagesRm, []string{"agentium-grade:go1.27-aaaaaaaaaaaa"}) {
		t.Errorf("removed %v", d.imagesRm)
	}
	built, _ := container.LoadBuilt(builtImagesPathFor(data))
	if _, ok := built["agentium-grade:go1.27-aaaaaaaaaaaa"]; ok || len(built) != 1 {
		t.Errorf("record %v", built)
	}
	d.imagesRm = nil
	// A base that is also the user's own tag frees nothing, and says so.
	expect(t, run("images", "remove", "--bases"), ExitOK, "would free up to 1.52 GB",
		"base     go 1.27      "+ref+" (stays as golang:1.27, your own tag: frees nothing)")
	expect(t, run("images", "remove", "--yes", "--bases"), ExitOK, "untagged base "+ref+": it stays as golang:1.27, your own tag")
	if !slices.Contains(d.imagesRm, ref) || len(d.imagesRm) != 3 {
		t.Errorf("with --bases: %v", d.imagesRm)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
