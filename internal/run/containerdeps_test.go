package run

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/container"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
)

// fakeContainerDeps is the Docker driver's deps part, recording what the warm-up asked of it. A seed writes what
// container.SeedTar would, so the record of what was seeded is real.
type fakeContainerDeps struct {
	created  string
	volumes  []string
	seeded   [][]string
	warms    [][]string
	trees    [][]string // what each warm-up's tree held at its root
	failed   string
	warmErr  error
	warmSpec container.WarmSpec
}

func (f *fakeContainerDeps) EnsureDepsVolume(_ context.Context, v container.DepsVolume) (string, error) {
	f.volumes = append(f.volumes, v.Name())
	return f.created, nil
}

func (f *fakeContainerDeps) SeedDeps(ctx context.Context, _ container.DepsVolume, _ container.Image, seeds []container.Seed, entries []container.SeedEntry, limits container.CopyLimits) ([]string, error) {
	_, written, err := container.SeedTar(ctx, io.Discard, seeds, entries, limits)
	f.seeded = append(f.seeded, written)
	return written, err
}

func (f *fakeContainerDeps) WarmRun(_ context.Context, w container.WarmSpec, tree string, cmds []container.WarmCommand, _ io.Writer) (int, string, error) {
	f.warmSpec = w
	var names []string
	for _, c := range cmds {
		names = append(names, c.Command)
	}
	f.warms = append(f.warms, names)
	entries, _ := os.ReadDir(tree)
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	f.trees = append(f.trees, files)
	if f.warmErr != nil {
		return 0, "", f.warmErr
	}
	if f.failed != "" {
		return 0, f.failed, nil
	}
	return len(cmds), "", nil
}

func TestWarmContainerDeps(t *testing.T) {
	ctx := context.Background()
	bare, base := bareWith(t, map[string]string{"go.mod": "module m\n", "go.sum": "example.com/a v1.0.0 h1:x=\n", "pyproject.toml": "[project]\nname='m'\n", "uv.lock": "version = 1\n"})
	data, userHome := t.TempDir(), t.TempDir()
	write := func(root, name, body string) {
		p := filepath.Join(root, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	write(userHome, "go/pkg/mod/cache/download/example.com/a/@v/v1.0.0.zip", "zip")
	write(userHome, "go/pkg/mod/cache/download/example.com/a/@v/v1.0.0.mod", "mod")
	layout := home.Layout{Root: data, Cache: filepath.Join(data, "cache"), Deps: filepath.Join(data, "deps")}
	env := Env{Layout: layout, Bare: bare, Home: userHome, Environ: []string{"HOME=" + userHome}}
	f := &fakeContainerDeps{created: "2026-10-04T10:00:00Z"}
	img := container.Image{Ref: "sha256:" + strings.Repeat("a", 64), ID: "sha256:" + strings.Repeat("a", 64)}
	in := ContainerDepsInput{Docker: f, Data: "test", Image: img, Profiles: buildtool.Select([]string{"go", "python"}), Base: base,
		Limits: container.DefaultLimits(4), LogPath: filepath.Join(data, "warm.log")}
	project := filepath.Base(filepath.Dir(bare))
	volume := "agentium-deps-test-" + project + "-aaaaaaaaaaaa"
	s, err := env.WarmContainerDeps(ctx, in)
	must(t, err)
	if s.Volume != volume || s.Created != f.created || !strings.HasPrefix(s.Venv, "/deps/py/") || len(s.Notes) != 0 {
		t.Errorf("state %+v", s)
	}
	if !slices.Equal(f.volumes, []string{volume}) || len(f.seeded) != 1 || len(f.warms) != 1 {
		t.Fatalf("volumes %v, seeds %v, warms %v", f.volumes, f.seeded, f.warms)
	}
	if !slices.Contains(f.seeded[0], "gomod/cache/download/example.com/a/@v/v1.0.0.zip") {
		t.Errorf("seeded %v", f.seeded[0])
	}
	if !slices.Equal(f.warms[0], []string{"go mod download", "uv sync --frozen --no-install-project --no-install-workspace --no-install-local"}) {
		t.Errorf("warm-up %v", f.warms[0])
	}
	// The warm-up ran the base's checkout (a repository holding the base alone: builds may run git describe), which is
	// gone after.
	if !slices.Contains(f.trees[0], "uv.lock") || !slices.Contains(f.trees[0], ".git") {
		t.Errorf("tree %v", f.trees[0])
	}
	if left, _ := os.ReadDir(filepath.Join(layout.Cache, "warm")); len(left) != 0 {
		t.Errorf("the checkout is left: %v", left)
	}
	if f.warmSpec.Volume.Name() != volume || f.warmSpec.Image.ID != img.ID {
		t.Errorf("warm spec %+v", f.warmSpec)
	}
	if ContainerDepsUsed(layout, volume).IsZero() {
		t.Error("no last use")
	}
	// Stamped: the same base warms no more.
	again, err := env.WarmContainerDeps(ctx, in)
	if err != nil || again.Venv != s.Venv || len(f.seeded) != 1 || len(f.warms) != 1 {
		t.Errorf("again: %+v, %v; seeds %d, warms %d", again, err, len(f.seeded), len(f.warms))
	}
	// Another base: only what the volume lacks is seeded, and nothing it holds is written again.
	write(userHome, "go/pkg/mod/cache/download/example.com/a/@v/v1.1.0.zip", "zip")
	bare2Base := commitOn(t, bare, map[string]string{"go.mod": "module m\n", "go.sum": "example.com/a v1.0.0 h1:x=\nexample.com/a v1.1.0 h1:y=\n",
		"pyproject.toml": "[project]\nname='m'\n", "uv.lock": "version = 1\n"})
	in.Base = bare2Base
	if _, err := env.WarmContainerDeps(ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(f.seeded) != 2 || !slices.Equal(f.seeded[1], []string{"gomod/cache/download/example.com/a/@v/v1.1.0.zip"}) {
		t.Errorf("the second seed wrote %v", f.seeded[len(f.seeded)-1])
	}
	// A volume made since (the old one removed): its record and stamps do not hold, so it is seeded whole and warmed.
	f.created = "2026-10-05T10:00:00Z"
	in.Base = base
	if _, err := env.WarmContainerDeps(ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(f.seeded) != 3 || !slices.Contains(f.seeded[2], "gomod/cache/download/example.com/a/@v/v1.0.0.zip") || len(f.warms) != 3 {
		t.Errorf("a new volume: seeds %v, warms %d", f.seeded, len(f.warms))
	}
	// A failed step is a note, and the base stays unstamped.
	f.created, f.failed = "2026-10-06T10:00:00Z", "uv sync --frozen"
	s, err = env.WarmContainerDeps(ctx, in)
	if err != nil || len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "uv sync --frozen") {
		t.Errorf("failed: %+v, %v", s, err)
	}
	f.failed = ""
	if _, err := env.WarmContainerDeps(ctx, in); err != nil || len(f.warms) != 5 {
		t.Errorf("after a failure the base warms again: %d warms, %v", len(f.warms), err)
	}
	// The container's own failure is an error, and stamps nothing.
	f.created, f.warmErr = "2026-10-07T10:00:00Z", container.ErrProbe
	if _, err := env.WarmContainerDeps(ctx, in); !errors.Is(err, container.ErrProbe) {
		t.Errorf("a container failure: %v", err)
	}
	f.warmErr = nil
	if _, err := env.WarmContainerDeps(ctx, in); err != nil || len(f.warms) != 7 {
		t.Errorf("after a container failure: %d warms, %v", len(f.warms), err)
	}
	// Forgotten with its volume: the state goes.
	must(t, ForgetContainerDeps(layout, volume))
	if !ContainerDepsUsed(layout, volume).IsZero() {
		t.Error("state left after ForgetContainerDeps")
	}
	if err := ForgetContainerDeps(layout, "../x"); err != nil {
		t.Error(err)
	}
	if _, err := env.WarmContainerDeps(ctx, ContainerDepsInput{Docker: f, Data: "test", Image: img, Base: "HEAD"}); err == nil {
		t.Error("a base that is not a full commit was accepted")
	}
}

// commitOn adds a commit of files to the bare repository and returns it.
func commitOn(t *testing.T, bare string, files map[string]string) string {
	t.Helper()
	ctx := context.Background()
	user := goldenRepo(t, files)
	commit, err := gitx.Run(ctx, "-C", user, "rev-parse", "HEAD")
	must(t, err)
	must(t, gitx.FetchCommit(ctx, user, commit, gitx.SourceRef(commit), "--git-dir", bare))
	return commit
}

// Clean removes a deps volume only holding its warm-up lock, and not one used since it was listed; its state goes
// after it, and its lock stays.
func TestRemoveContainerDeps(t *testing.T) {
	ctx := context.Background()
	layout := home.Layout{Cache: filepath.Join(t.TempDir(), "cache")}
	const volume = "agentium-deps-test-3-aaaaaaaaaaaa"
	state := containerDepsState(layout, volume)
	must(t, os.MkdirAll(state, 0o700))
	for _, name := range []string{"used", "seeded", "stamp-x"} {
		must(t, os.WriteFile(filepath.Join(state, name), nil, 0o600))
	}
	listed := ContainerDepsUsed(layout, volume)
	calls := 0
	remove := func() error { calls++; return nil }
	// A warm-up holds the lock.
	unlock, err := lockFile(ctx, filepath.Join(state, "lock"), nil)
	must(t, err)
	if err := RemoveContainerDeps(ctx, layout, volume, listed, remove); !errors.Is(err, ErrCleanBusy) || calls != 0 {
		t.Errorf("busy: %v, %d removals", err, calls)
	}
	unlock()
	// Used since it was listed.
	if err := RemoveContainerDeps(ctx, layout, volume, listed.Add(-time.Hour), remove); !errors.Is(err, ErrCleanUsed) || calls != 0 {
		t.Errorf("used since: %v, %d removals", err, calls)
	}
	// Docker's refusal is the error, and the volume keeps its state.
	refused := errors.New("in use")
	if err := RemoveContainerDeps(ctx, layout, volume, listed, func() error { return refused }); !errors.Is(err, refused) {
		t.Errorf("refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "seeded")); err != nil {
		t.Errorf("a volume that stays lost its record: %v", err)
	}
	must(t, RemoveContainerDeps(ctx, layout, volume, listed, remove))
	entries, _ := os.ReadDir(state)
	if calls != 1 || len(entries) != 1 || entries[0].Name() != "lock" {
		t.Errorf("after removal: %d calls, %v", calls, entries)
	}
}
