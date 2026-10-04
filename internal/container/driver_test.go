package container

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/task"
)

// fixtureImageID is the ID of the pinned Go image (goImage), which testdata/inspect.json records.
const fixtureImageID = "sha256:84349ebd3bf9b6ffc6163a249e24a1223608a0fe2b77ef7cbe90f468e3fcb71f"

// fixtureSpec is the spec testdata/inspect.json was recorded for (TestRealFixtures).
func fixtureSpec() Spec {
	return Spec{Data: "test", Run: "fixture", Role: "grade", Image: Image{Ref: goImage, ID: fixtureImageID, Env: fixtureImageEnv()}, Limits: DefaultLimits(4), Deadline: 600 * time.Second}
}

// fixtureImageEnv is the pinned Go image's own environment (its Config.Env).
func fixtureImageEnv() []string {
	return []string{"PATH=/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "GOLANG_VERSION=1.27.1", "GOTOOLCHAIN=local", "GOPATH=/go"}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	must(t, err)
	return string(data)
}

const goodCounters = "== memory.events\nlow 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n== pids.events\nmax 0\n== memory.peak\n104857600\n== end\n"

// goodScenario is a healthy local daemon with the image present and a container that passes every check.
func goodScenario(t *testing.T) scenario {
	return scenario{
		Endpoint: "unix:///var/run/docker.sock",
		Version:  `{"Version":"27.4.0","ApiVersion":"1.47","Os":"linux","Arch":"arm64"}`,
		Info:     `{"ServerVersion":"27.4.0","OSType":"linux","NCPU":4,"MemTotal":8308154368,"CgroupVersion":"2","DefaultRuntime":"runc","SecurityOptions":["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]}`,
		Image:    `{"Id":"` + fixtureImageID + `","RepoDigests":["` + goImage + `"],"Os":"linux","Architecture":"arm64","Config":{"Env":` + mustJSON(t, fixtureImageEnv()) + `}}`,
		Inspect:  readFixture(t, "inspect.json"),
		Probe:    readFixture(t, "probe.txt"),
		Counters: goodCounters,
		Volume:   `{"Name":"agentium-deps-test","Driver":"local","Options":null}`,
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	must(t, err)
	return string(data)
}

// fake is a fake docker client: the test binary linked under the name docker, answering from a scenario.
type fake struct {
	dir string
	bin string
}

func newFake(t *testing.T, sc scenario) *fake {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", fakeDockerName)
	must(t, os.MkdirAll(filepath.Dir(bin), 0o755))
	exe, err := os.Executable()
	must(t, err)
	must(t, os.Symlink(exe, bin))
	f := &fake{dir: dir, bin: bin}
	f.set(t, sc)
	return f
}

func (f *fake) set(t *testing.T, sc scenario) {
	t.Helper()
	data, err := json.Marshal(sc)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(f.dir, "scenario.json"), data, 0o600))
}

// opts gives the fake a user's environment (its own docker configuration among it) and an empty configuration folder
// for every call after the endpoint lookup.
func (f *fake) opts() Options {
	return Options{Bin: f.bin, ConfigDir: f.configDir(), Environ: []string{"PATH=/usr/bin:/bin", "HOME=/home/agentium", "DOCKER_CONFIG=/home/agentium/.docker",
		"DOCKER_CONTEXT=colima", "ANTHROPIC_API_KEY=sk-placeholder", "DOCKER_AUTH_CONFIG={}", "AGENTIUM_HOME=/data", "GOFLAGS=-x"}}
}

func (f *fake) configDir() string {
	dir := filepath.Join(f.dir, "config")
	os.MkdirAll(dir, 0o700)
	return dir
}

// calls is every argv the fake was given, in order.
func (f *fake) calls(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	must(t, err)
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var argv []string
		must(t, json.Unmarshal([]byte(line), &argv))
		calls = append(calls, argv)
	}
	return calls
}

// called reports whether some call's argv contains every one of words, in order and adjacent.
func (f *fake) called(t *testing.T, words ...string) bool {
	for _, argv := range f.calls(t) {
		if strings.Contains("\x00"+strings.Join(argv, "\x00")+"\x00", "\x00"+strings.Join(words, "\x00")+"\x00") {
			return true
		}
	}
	return false
}

func (f *fake) stdin(t *testing.T, n int) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "stdin-"+itoa(n)))
	must(t, err)
	return data
}

func itoa(n int) string { return strconv.Itoa(n) }

func tarNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		must(t, err)
		names = append(names, h.Name+" "+string(h.Typeflag)+" "+itoa(h.Uid)+" "+fmtMode(h.Mode)+" "+h.Linkname)
	}
}

func fmtMode(m int64) string { return "0" + strconv.FormatInt(m, 8) }

// TestDriverArgvGolden pins every argv the driver gives docker, through the whole path: the usability check, create,
// the inspect check, the skeleton, start, the probes, the tar copy-in, a command with its events watch, the counters,
// the removal, and recovery's listing and removal by label. A change here is a change to the container's isolation, to be read line
// by line, never regenerated blindly. The probe and counters scripts show as placeholders; they are constants checked
// on a real daemon.
func TestDriverArgvGolden(t *testing.T) {
	t.Parallel()
	f := newFake(t, goodScenario(t))
	ctx := context.Background()
	d, images, err := Usable(ctx, f.opts(), DefaultLimits(4), goImage)
	must(t, err)
	if d.Engine() != (Engine{Version: "27.4.0", APIVersion: "1.47", OS: "linux", Arch: "arm64", CgroupVersion: "2", NCPU: 4, MemTotal: 8308154368, DefaultRuntime: "runc"}) {
		t.Errorf("engine: %+v", d.Engine())
	}
	tree := t.TempDir()
	writeTree(t, tree, map[string]string{"go.mod": "module m\n", "sub/a_test.go": "package sub\n"})
	spec := fixtureSpec()
	spec.Image = images[0]
	var out strings.Builder
	var digest string
	err = d.Run(ctx, spec, func(ctx context.Context, c *Container) error {
		digest = c.InspectDigest()
		if env := c.ImageEnv(); len(env) == 0 || !strings.HasPrefix(env[0], "PATH=") {
			t.Errorf("image env: %v", env)
		}
		if _, err := c.CopyIn(ctx, tree, DefaultCopyLimits()); err != nil {
			return err
		}
		res, err := c.Exec(ctx, Command{Command: "go test ./...", Dir: "sub", Env: []string{"GOPROXY=off", "HOME=/grade/cache/home"}, Timeout: time.Minute, Output: &out})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 || res.Counters != (Counters{MemoryPeak: 104857600}) {
			t.Errorf("result: %+v", res)
		}
		return nil
	})
	must(t, err)
	sc := goodScenario(t)
	sc.PS = "agentium-test-orphan-grade\tcreated\torphan\n"
	sc.Volumes = strings.Repeat("v", 64) + "\torphan\n"
	f.set(t, sc)
	left, err := d.Leftovers(ctx, "test")
	must(t, err)
	if len(left) != 2 || left[0] != (Leftover{Kind: "container", Name: "agentium-test-orphan-grade", Run: "orphan", State: "created"}) ||
		left[1] != (Leftover{Kind: "volume", Name: strings.Repeat("v", 64), Run: "orphan"}) {
		t.Errorf("leftovers: %+v", left)
	}
	sc.PS = strings.Repeat("d", 64) + "\n"
	sc.Volumes = strings.Repeat("v", 64) + "\n"
	sc.Inspect = ""
	f.set(t, sc)
	must(t, d.RemoveRun(ctx, "test", "orphan"))

	var golden strings.Builder
	calls := f.calls(t)
	for i := 0; i+1 < len(calls); i++ {
		// The command's events watch starts beside its exec, so the two calls come in either order; the golden has the
		// watch first.
		if slices.Contains(calls[i+1], "events") && slices.ContainsFunc(calls[i], func(a string) bool { return strings.HasPrefix(a, "agentium-exec-") }) {
			calls[i], calls[i+1] = calls[i+1], calls[i]
		}
	}
	for _, argv := range calls {
		for i, a := range argv {
			switch {
			case a == probeScript:
				argv[i] = "<probeScript>"
			case a == countersScript:
				argv[i] = "<countersScript>"
			case a == f.configDir():
				argv[i] = "<empty config>"
			case strings.HasPrefix(a, "agentium-exec-") && len(a) == len("agentium-exec-")+16:
				argv[i] = "<nonce>"
			}
		}
		enc := json.NewEncoder(&golden)
		enc.SetEscapeHTML(false)
		must(t, enc.Encode(argv))
	}
	path := filepath.Join("testdata", "argv.golden")
	if *update {
		must(t, os.WriteFile(path, []byte(golden.String()), 0o644))
	}
	want, err := os.ReadFile(path)
	must(t, err)
	if golden.String() != string(want) {
		t.Errorf("argv changed (go test ./internal/container -run TestDriverArgvGolden -update, then read the diff line by line):\n%s", golden.String())
	}
	if out.String() != "" || !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("output %q, digest %q", out.String(), digest)
	}
	// The skeleton (docker cp's stdin) and the copy-in (tar's stdin).
	for i, argv := range f.calls(t) {
		switch {
		case len(argv) > 4 && argv[4] == "cp":
			if got := tarNames(t, f.stdin(t, i+1)); strings.Join(got, ",") != "work/ 5 65534 0700 ,cache/ 5 65534 0700 " {
				t.Errorf("skeleton: %q", got)
			}
		case len(argv) > 5 && argv[4] == "exec" && argv[5] == "--interactive":
			if got := tarNames(t, f.stdin(t, i+1)); strings.Join(got, ",") != "go.mod 0 65534 0644 ,sub/ 5 65534 0755 ,sub/a_test.go 0 65534 0644 " {
				t.Errorf("copy-in: %q", got)
			}
		}
	}
}

// TestClientEnvironment: the endpoint lookup sees only its allowlist (no credentials, no Agentium settings), and every
// later call sees only PATH, with an empty configuration (--config: the user's config.json, whose proxies the client
// would copy into the container, is never read) and the endpoint found once (--host).
func TestClientEnvironment(t *testing.T) {
	t.Parallel()
	sc := goodScenario(t)
	sc.Env = true
	f := newFake(t, sc)
	opts := f.opts()
	opts.Environ = append(opts.Environ, "DOCKER_HOST=unix:///var/run/docker.sock")
	if _, err := Open(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	envOf := func(argv []string) string { return argv[len(argv)-1] }
	first := envOf(calls[0])
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/agentium", "DOCKER_CONTEXT=colima", "DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_CONFIG="} {
		if !strings.Contains(first, want) {
			t.Errorf("the endpoint lookup lacks %q: %s", want, first)
		}
	}
	for _, argv := range calls {
		env := envOf(argv)
		for _, banned := range []string{"ANTHROPIC_API_KEY", "DOCKER_AUTH_CONFIG", "AGENTIUM_HOME", "GOFLAGS"} {
			if strings.Contains(env, banned) {
				t.Errorf("docker %v saw %s", argv[:2], banned)
			}
		}
	}
	if len(calls) < 3 {
		t.Fatalf("calls: %v", calls)
	}
	for _, argv := range calls[1:] {
		if argv[0] != "--config" || argv[1] != opts.ConfigDir || argv[2] != "--host" || argv[3] != "unix:///var/run/docker.sock" || envOf(argv) != "ENV:PATH=/usr/bin:/bin" {
			t.Errorf("a call after the lookup is not pinned to the empty configuration, the endpoint and PATH: %v", argv)
		}
	}
	// Without a folder given, Open makes an empty one and Close removes it.
	opts.ConfigDir = ""
	d, err := Open(context.Background(), opts)
	must(t, err)
	if entries, err := os.ReadDir(d.config); err != nil || len(entries) != 0 || !strings.HasPrefix(filepath.Base(d.config), "agentium-docker-config-") {
		t.Errorf("own configuration folder %s: %v, %v", d.config, entries, err)
	}
	must(t, d.Close())
	if _, err := os.Stat(d.config); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("not removed: %v", err)
	}
	// A folder that holds a configuration is refused.
	opts.ConfigDir = t.TempDir()
	must(t, os.WriteFile(filepath.Join(opts.ConfigDir, "config.json"), []byte(`{"proxies":{}}`), 0o600))
	if _, err := Open(context.Background(), opts); err == nil {
		t.Error("a configuration folder with a config.json was accepted")
	}
}

// TestRemoteDaemonRefused: a daemon that is not on a Unix socket is refused before anything is sent to it (decision 9).
func TestRemoteDaemonRefused(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"tcp://192.0.2.1:2376", "ssh://builder@192.0.2.1", "npipe:////./pipe/docker_engine", "fd://", "unix://", "unix://relative.sock", "http://192.0.2.1", ""} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			sc := goodScenario(t)
			sc.Endpoint = endpoint
			f := newFake(t, sc)
			_, err := Open(context.Background(), f.opts())
			if !errors.Is(err, ErrRemote) {
				t.Fatalf("Open with %q: %v, want ErrRemote", endpoint, err)
			}
			if strings.Contains(err.Error(), "192.0.2.1") {
				t.Errorf("the error names the endpoint: %v", err)
			}
			if n := len(f.calls(t)); n != 1 {
				t.Errorf("%d calls, want only the endpoint lookup", n)
			}
		})
	}
	if err := CheckLocal("unix:///var/run/docker.sock"); err != nil {
		t.Errorf("a local socket: %v", err)
	}
}

// TestUnusableDaemonRefused: no daemon, or one that cannot isolate a grade, is refused.
func TestUnusableDaemonRefused(t *testing.T) {
	t.Parallel()
	info := func(fields string) string {
		return `{"ServerVersion":"27.4.0","NCPU":4,"MemTotal":8308154368` + fields + `}`
	}
	const linux = `,"OSType":"linux"`
	const seccomp = `,"SecurityOptions":["name=seccomp,profile=builtin"]`
	cases := []struct {
		name    string
		version string
		info    string
		want    error
	}{
		{"no daemon", "", "", ErrUnavailable},
		{"info has server errors", "", `{"ServerErrors":["Cannot connect"]}`, ErrUnavailable},
		{"windows", `{"Version":"27.4.0","ApiVersion":"1.47","Os":"windows","Arch":"amd64"}`, info(`,"OSType":"windows","CgroupVersion":"2"` + seccomp), ErrUnsupported},
		{"no seccomp", "", info(linux + `,"CgroupVersion":"2","SecurityOptions":["name=apparmor"]`), ErrUnsupported},
		{"seccomp unconfined", "", info(linux + `,"CgroupVersion":"2","SecurityOptions":["name=seccomp,profile=unconfined"]`), ErrUnsupported},
		{"cgroup v1", "", info(linux + `,"CgroupVersion":"1"` + seccomp), ErrUnsupported},
		{"old API", `{"Version":"20.10.0","ApiVersion":"1.40","Os":"linux","Arch":"arm64"}`, info(linux + `,"CgroupVersion":"2"` + seccomp), ErrUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sc := goodScenario(t)
			if c.version != "" || c.name == "no daemon" {
				sc.Version = c.version
			}
			if c.info != "" {
				sc.Info = c.info
			}
			_, err := Open(context.Background(), newFake(t, sc).opts())
			if !errors.Is(err, c.want) {
				t.Fatalf("%v, want %v", err, c.want)
			}
		})
	}
	sc := goodScenario(t)
	f := newFake(t, sc)
	_, err := Open(context.Background(), Options{Bin: filepath.Join(f.dir, "no-such-docker"), Environ: f.opts().Environ})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("no client: %v", err)
	}
}

// TestFits: a daemon with less memory or fewer CPUs than one grade's limits is refused (open decision 8).
func TestFits(t *testing.T) {
	d := &Docker{engine: Engine{NCPU: 2, MemTotal: 2 << 30}}
	if err := d.Fits(DefaultLimits(2)); !errors.Is(err, ErrTooSmall) {
		t.Errorf("2 GiB against 4 GiB: %v", err)
	}
	d.engine.MemTotal = 8 << 30
	if err := d.Fits(DefaultLimits(2)); err != nil {
		t.Errorf("8 GiB, 2 CPUs: %v", err)
	}
	if err := d.Fits(DefaultLimits(8)); !errors.Is(err, ErrTooSmall) {
		t.Errorf("4 CPUs on 2: %v", err)
	}
	if l := DefaultLimits(12); l.CPUs != 4 || l.Memory != 4<<30 || l.Pids != 4096 {
		t.Errorf("default limits: %+v", l)
	}
}

// TestImageMissingRefusedWithoutPull: a missing image, an image of another digest, or a tag is refused, and nothing
// is ever pulled.
func TestImageMissingRefusedWithoutPull(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		ref   string
		image string
		want  error
	}{
		{"missing", goImage, "", ErrImageMissing},
		{"another digest", goImage, `{"Id":"` + fixtureImageID + `","RepoDigests":["golang@sha256:` + strings.Repeat("0", 64) + `"],"Os":"linux","Architecture":"arm64"}`, ErrImageMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sc := goodScenario(t)
			sc.Image = c.image
			f := newFake(t, sc)
			_, _, err := Usable(context.Background(), f.opts(), DefaultLimits(4), c.ref)
			if !errors.Is(err, c.want) {
				t.Fatalf("%v, want %v", err, c.want)
			}
			for _, argv := range f.calls(t) {
				if strings.Contains(strings.Join(argv, " "), "pull ") && !strings.Contains(strings.Join(argv, " "), "--pull never") {
					t.Errorf("pulled: %v", argv)
				}
			}
		})
	}
	f := newFake(t, goodScenario(t))
	d, err := Open(context.Background(), f.opts())
	must(t, err)
	before := len(f.calls(t))
	for _, ref := range []string{"golang:1.27", "golang", "golang@sha256:abc", "sha256:" + strings.Repeat("a", 63)} {
		if _, err := d.Image(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "not pinned") {
			t.Errorf("%q: %v", ref, err)
		}
	}
	if len(f.calls(t)) != before {
		t.Error("an unpinned reference reached docker")
	}
	sc := goodScenario(t)
	sc.Image = strings.Replace(sc.Image, `"Architecture":"arm64"`, `"Architecture":"amd64"`, 1)
	f.set(t, sc)
	if _, err := d.Image(context.Background(), goImage); err == nil || !strings.Contains(err.Error(), "amd64") {
		t.Errorf("another architecture: %v", err)
	}
	// By ID (a local build, step 3): the daemon's ID must be the one asked for.
	sc = goodScenario(t)
	f.set(t, sc)
	if img, err := d.Image(context.Background(), fixtureImageID); err != nil || img.ID != fixtureImageID {
		t.Errorf("by ID: %+v, %v", img, err)
	}
	if _, err := d.Image(context.Background(), "sha256:"+strings.Repeat("b", 64)); err == nil {
		t.Error("an image answering with another ID was accepted")
	}
}

// TestModeIsTasks: the label's mode is task.GraderContainer.
func TestModeIsTasks(t *testing.T) {
	if Mode != task.GraderContainer {
		t.Errorf("Mode %q, task.GraderContainer %q", Mode, task.GraderContainer)
	}
}

func openFake(t *testing.T, sc scenario) (*fake, *Docker) {
	t.Helper()
	f := newFake(t, sc)
	d, err := Open(context.Background(), f.opts())
	must(t, err)
	return f, d
}

// TestRunRemovesWhateverHappens: after fn's error, a panic, or a failed check before start, the container is removed
// (by name, with its volumes) before Run returns; a failed check never starts it, and fn never runs.
func TestRunRemovesWhateverHappens(t *testing.T) {
	t.Parallel()
	spec := fixtureSpec()
	rm := []string{"rm", "--force", "--volumes", Name(spec)}
	t.Run("fn error", func(t *testing.T) {
		t.Parallel()
		f, d := openFake(t, goodScenario(t))
		boom := errors.New("boom")
		if err := d.Run(context.Background(), spec, func(context.Context, *Container) error { return boom }); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if !f.called(t, rm...) {
			t.Error("not removed")
		}
	})
	t.Run("panic", func(t *testing.T) {
		t.Parallel()
		f, d := openFake(t, goodScenario(t))
		func() {
			defer func() {
				if r := recover(); r != "boom" {
					t.Errorf("recovered %v", r)
				}
			}()
			_ = d.Run(context.Background(), spec, func(context.Context, *Container) error { panic("boom") })
		}()
		if !f.called(t, rm...) {
			t.Error("not removed after a panic")
		}
	})
	t.Run("inspect mismatch", func(t *testing.T) {
		t.Parallel()
		sc := goodScenario(t)
		sc.Inspect = strings.Replace(sc.Inspect, `"NetworkMode": "none"`, `"NetworkMode": "bridge"`, 1)
		f, d := openFake(t, sc)
		ran := false
		err := d.Run(context.Background(), spec, func(context.Context, *Container) error { ran = true; return nil })
		if !errors.Is(err, ErrMismatch) || errors.Is(err, ErrUnjudgeable) || ran {
			t.Fatalf("%v (fn ran: %v)", err, ran)
		}
		if f.called(t, "start") || f.called(t, "cp") || !f.called(t, rm...) {
			t.Errorf("calls: %v", f.calls(t))
		}
	})
	t.Run("probe failure", func(t *testing.T) {
		t.Parallel()
		sc := goodScenario(t)
		sc.Probe = strings.Replace(sc.Probe, "NoNewPrivs:\t1", "NoNewPrivs:\t0", 1)
		f, d := openFake(t, sc)
		ran := false
		err := d.Run(context.Background(), spec, func(context.Context, *Container) error { ran = true; return nil })
		if !errors.Is(err, ErrProbe) || errors.Is(err, ErrUnjudgeable) || ran || !f.called(t, rm...) {
			t.Fatalf("%v (fn ran: %v)", err, ran)
		}
	})
	t.Run("create failed", func(t *testing.T) {
		t.Parallel()
		sc := goodScenario(t)
		sc.Inspect = ""
		f, d := openFake(t, sc)
		if err := d.Run(context.Background(), spec, func(context.Context, *Container) error { return nil }); err == nil {
			t.Fatal("no error")
		}
		if !f.called(t, rm...) {
			t.Error("a container that may exist was not removed")
		}
	})
	t.Run("create cancelled", func(t *testing.T) {
		t.Parallel()
		// The client was killed mid-create: the daemon may have made the container, so it is removed.
		sc := goodScenario(t)
		sc.CreateBlock = true
		f, d := openFake(t, sc)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(300*time.Millisecond, cancel)
		err := d.Run(ctx, spec, func(context.Context, *Container) error { return nil })
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnjudgeable) {
			t.Fatalf("%v", err)
		}
		if !f.called(t, rm...) {
			t.Error("a container the cancelled create may have made was not removed")
		}
	})
	t.Run("create refused", func(t *testing.T) {
		t.Parallel()
		// The name is in use: that container is not this grade's, and must not be removed.
		sc := goodScenario(t)
		sc.CreateExit = 125
		f, d := openFake(t, sc)
		if err := d.Run(context.Background(), spec, func(context.Context, *Container) error { return nil }); err == nil || !strings.Contains(err.Error(), "already in use") {
			t.Fatalf("%v", err)
		}
		if f.called(t, "rm") {
			t.Error("removed a container this grade did not create")
		}
	})
	t.Run("deps volume", func(t *testing.T) {
		t.Parallel()
		withDeps := spec
		withDeps.Deps = "agentium-deps-test"
		sc := goodScenario(t)
		sc.Volume = `{"Driver":"local","Options":{"type":"none","o":"bind","device":"/Users"}}`
		f, d := openFake(t, sc)
		if err := d.Run(context.Background(), withDeps, func(context.Context, *Container) error { return nil }); err == nil || !strings.Contains(err.Error(), "plain local volume") {
			t.Fatalf("a bind-backed deps volume: %v", err)
		}
		sc.Volume = ""
		f.set(t, sc)
		if err := d.Run(context.Background(), withDeps, func(context.Context, *Container) error { return nil }); err == nil {
			t.Fatal("a missing deps volume was accepted")
		}
		if f.called(t, "create") {
			t.Error("created despite the deps volume")
		}
	})
}

// TestCancelKillsAndRemoves: a cancel during a command removes the container at once, inside Exec (the docker
// client's death alone leaves the command running in the container), and Run returns the cancellation.
func TestCancelKillsAndRemoves(t *testing.T) {
	t.Parallel()
	sc := goodScenario(t)
	sc.CommandBlock = true
	f, d := openFake(t, sc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec := fixtureSpec()
	start := time.Now()
	err := d.Run(ctx, spec, func(ctx context.Context, c *Container) error {
		time.AfterFunc(300*time.Millisecond, cancel)
		_, err := c.Exec(ctx, Command{Command: "sleep 300"})
		if !f.called(t, "rm", "--force", "--volumes", Name(spec)) {
			t.Error("Exec returned from a cancel without removing the container")
		}
		// Bounded: were the container still there, the fake would block this exec too.
		if _, err := c.Exec(context.Background(), Command{Command: "true", Timeout: time.Second}); !errors.Is(err, ErrGone) {
			t.Errorf("exec after the cancel: %v", err)
		}
		return err
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnjudgeable) {
		t.Fatalf("Run: %v (Agentium's own cancel is not the grade's doing)", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("the cancel took %s", time.Since(start))
	}
}

// TestTimeoutReadsCountersThenRemoves: a command past its timeout is a timed-out result with the counters read, and
// the container is removed.
func TestTimeoutReadsCountersThenRemoves(t *testing.T) {
	t.Parallel()
	sc := goodScenario(t)
	sc.CommandBlock = true
	sc.Counters = strings.Replace(goodCounters, "oom_kill 0", "oom_kill 2", 1)
	f, d := openFake(t, sc)
	spec := fixtureSpec()
	err := d.Run(context.Background(), spec, func(ctx context.Context, c *Container) error {
		res, err := c.Exec(ctx, Command{Command: "sleep 300", Timeout: 300 * time.Millisecond})
		if err != nil {
			return err
		}
		if !res.TimedOut || res.ExitCode != -1 || res.Counters.OOMKills != 2 || !res.Counters.Hit() {
			t.Errorf("result: %+v", res)
		}
		if !f.called(t, "rm", "--force", "--volumes", Name(spec)) {
			t.Error("the container was not removed after the timeout")
		}
		return nil
	})
	must(t, err)
}

// TestExecRefusesBadInput: a bare NAME (docker would pass the client's own value through), a NAME that is not one, and
// a folder outside the copy are refused before docker runs.
func TestExecRefusesBadInput(t *testing.T) {
	t.Parallel()
	f, d := openFake(t, goodScenario(t))
	err := d.Run(context.Background(), fixtureSpec(), func(ctx context.Context, c *Container) error {
		before := len(f.calls(t))
		for _, cmd := range []Command{
			{Command: "true", Env: []string{"ANTHROPIC_API_KEY"}},
			{Command: "true", Env: []string{"A B=1"}},
			{Command: "true", Env: []string{"=1"}},
			{Command: "true", Dir: "../cache"},
			{Command: "true", Dir: "/etc"},
			{Command: "true", Dir: "a/../../x"},
		} {
			if _, err := c.Exec(ctx, cmd); err == nil {
				t.Errorf("accepted %+v", cmd)
			}
		}
		if len(f.calls(t)) != before {
			t.Error("a refused command reached docker")
		}
		return nil
	})
	must(t, err)
}

// TestCopyInFailures: a tree the stream refuses (too much content, too many entries), that the image's tar fails on,
// or whose streaming reaches the timeout is ErrUnjudgeable (the agent controls the tree; a retry would be a re-roll),
// and removes the container. A root that cannot be opened is Agentium's own failure, not the agent's.
func TestCopyInFailures(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	writeTree(t, tree, map[string]string{"big": strings.Repeat("x", 4096), "a/b": "", "a/c": ""})
	cases := map[string]struct {
		root       string
		limits     CopyLimits
		tarExit    int
		unjudgable bool
		also       error
		tarBlock   bool
	}{
		"too much content": {tree, CopyLimits{Bytes: 1024, Entries: 100}, 0, true, ErrTooLarge, false},
		"too many entries": {tree, CopyLimits{Bytes: 1 << 20, Entries: 3}, 0, true, ErrTooLarge, false},
		"tar fails":        {tree, DefaultCopyLimits(), 2, true, nil, false},
		// A tree slow enough to reach the timeout once streaming is the agent's doing: no retry.
		"tar hangs past the timeout": {tree, CopyLimits{Bytes: 1 << 20, Entries: 100, Timeout: 500 * time.Millisecond}, 0, true, nil, true},
		"no root":                    {filepath.Join(tree, "missing"), DefaultCopyLimits(), 0, false, nil, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := goodScenario(t)
			s.TarExit = c.tarExit
			s.TarBlock = c.tarBlock
			f, d := openFake(t, s)
			err := d.Run(context.Background(), fixtureSpec(), func(ctx context.Context, ct *Container) error {
				_, err := ct.CopyIn(ctx, c.root, c.limits)
				if !f.called(t, "rm", "--force", "--volumes", Name(fixtureSpec())) {
					t.Error("a partial copy was left in place")
				}
				return err
			})
			if err == nil || errors.Is(err, ErrUnjudgeable) != c.unjudgable || c.also != nil && !errors.Is(err, c.also) {
				t.Fatalf("%v (want unjudgeable %v, also %v)", err, c.unjudgable, c.also)
			}
		})
	}
}

// TestUnjudgeableAfterTheGradesCode: after the grade's code ran, every way it can make the result unreadable is
// ErrUnjudgeable, never a plain (retried) error: a fork bomb left running (the counters' exec cannot start), the
// counters' read killed, the container ended mid-command (its main process killed), and a timeout whose counters
// cannot be read. Each removes the container.
func TestUnjudgeableAfterTheGradesCode(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*scenario){
		"fork bomb":                   func(sc *scenario) { sc.CommandExit = 1; sc.CountersExit = 126 },
		"counters killed":             func(sc *scenario) { sc.CommandExit = 1; sc.CountersExit = 137 },
		"counters cut short":          func(sc *scenario) { sc.CommandExit = 1; sc.Counters = "== memory.events\noom_kill 0\n" },
		"container ended mid-command": func(sc *scenario) { sc.CommandGone = true },
		"timeout, counters unread":    func(sc *scenario) { sc.CommandBlock = true; sc.CountersExit = 126 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sc := goodScenario(t)
			change(&sc)
			f, d := openFake(t, sc)
			var execErr error
			err := d.Run(context.Background(), fixtureSpec(), func(ctx context.Context, c *Container) error {
				_, execErr = c.Exec(ctx, Command{Command: "go test ./...", Timeout: 2 * time.Second})
				if !f.called(t, "rm", "--force", "--volumes", Name(fixtureSpec())) {
					t.Error("the container was not removed")
				}
				return execErr
			})
			if !errors.Is(execErr, ErrUnjudgeable) || !errors.Is(err, ErrUnjudgeable) {
				t.Fatalf("Exec: %v; Run: %v; want ErrUnjudgeable", execErr, err)
			}
		})
	}
}

// TestOpenErrors: a failed endpoint lookup is ErrUnavailable without the socket's path, and a cancelled Open keeps
// the cancellation in its error chain.
func TestOpenErrors(t *testing.T) {
	t.Parallel()
	sc := goodScenario(t)
	sc.ContextFail = "Cannot connect to the Docker daemon at unix:///home/someone/.colima/default/docker.sock. Is the docker daemon running?"
	f := newFake(t, sc)
	_, err := Open(context.Background(), f.opts())
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "someone") || !strings.Contains(err.Error(), "<docker endpoint>") {
		t.Errorf("a failed lookup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, newFake(t, goodScenario(t)).opts()); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
		t.Errorf("a cancelled Open: %v", err)
	}
}

// TestRedactsEndpoint: docker's messages name the socket's path (a home path under colima); errors never do.
func TestRedactsEndpoint(t *testing.T) {
	d := &Docker{host: "unix:///home/someone/.colima/default/docker.sock"}
	got := d.redact("Cannot connect to the Docker daemon at unix:///home/someone/.colima/default/docker.sock. Is it running? dial /home/someone/.colima/default/docker.sock" +
		` error during connect: Get "http://%2Fhome%2Fsomeone%2F.colima%2Fdefault%2Fdocker.sock/v1.47/exec/x/json": EOF`)
	if strings.Contains(got, "someone") {
		t.Errorf("redact: %q", got)
	}
}

// TestSpecValidation: names and labels carry only what docker and recovery can match, and a spec without a pinned
// image or a deadline is refused.
func TestSpecValidation(t *testing.T) {
	if got := Name(fixtureSpec()); got != "agentium-test-fixture-grade" {
		t.Errorf("name %q", got)
	}
	if id := DataID("/somewhere/.agentium"); len(id) != 8 || id == DataID("/elsewhere/.agentium") {
		t.Errorf("data ID %q", id)
	}
	mutate := []func(*Spec){
		func(s *Spec) { s.Data = "Test" },
		func(s *Spec) { s.Data = "" },
		func(s *Spec) { s.Run = "-x" },
		func(s *Spec) { s.Run = "a/b" },
		func(s *Spec) { s.Role = "Grade" },
		func(s *Spec) { s.Image.Ref = "golang:1.27" },
		func(s *Spec) { s.Image.ID = "" },
		func(s *Spec) { s.Deadline = 0 },
		func(s *Spec) { s.Deadline = 48 * time.Hour },
		func(s *Spec) { s.Deps = "/host/path" },
		func(s *Spec) { s.Limits.Memory = 0 },
	}
	for i, m := range mutate {
		s := fixtureSpec()
		m(&s)
		if err := s.validate(); err == nil {
			t.Errorf("mutation %d accepted: %+v", i, s)
		}
	}
	if err := fixtureSpec().validate(); err != nil {
		t.Error(err)
	}
	s := fixtureSpec()
	s.Deadline = 1500 * time.Millisecond
	if s.deadlineSeconds() != "2" {
		t.Errorf("deadline rounds to %s", s.deadlineSeconds())
	}
}

// runGuarded runs Run with fn and fails the test if it does not return within limit: a copy-in or a command that hangs
// must show as a failure, not as a test binary that never ends.
func runGuarded(t *testing.T, d *Docker, ctx context.Context, limit time.Duration, fn func(ctx context.Context, c *Container) error) error {
	t.Helper()
	return runSpecGuarded(t, d, ctx, fixtureSpec(), limit, fn)
}

// runSpecGuarded is runGuarded for spec. On a hang the test fails at once, so its cleanups (the real tests' removal of
// leftovers by label) still run.
func runSpecGuarded(t *testing.T, d *Docker, ctx context.Context, spec Spec, limit time.Duration, fn func(ctx context.Context, c *Container) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, spec, fn) }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("Run hung for %s", limit)
		return nil
	}
}

// TestCopyInFolderSwappedForPipe: the agent swaps a folder for a pipe after its header is written and before it is
// opened to be listed. The open never blocks (a blocking open of a pipe outlives every timeout and cancel, and cleanup
// would never run): the copy-in returns at once with the tree refused (ErrUnjudgeable), or with the cancel, and the
// container is removed. With a slow walk, the timeout and the cancel fire first, and still nothing hangs.
func TestCopyInFolderSwappedForPipe(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		pause      time.Duration // after the swap, before the open
		timeout    time.Duration
		cancel     time.Duration // 0: no cancel
		unjudgable bool
	}{
		"swap":                 {0, 5 * time.Second, 0, true},
		"swap past a timeout":  {time.Second, 300 * time.Millisecond, 0, true},
		"swap past the cancel": {time.Second, 5 * time.Second, 300 * time.Millisecond, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tree := t.TempDir()
			writeTree(t, tree, map[string]string{"a/inner.go": "package a\n", "b.go": "package b\n"})
			f, d := openFake(t, goodScenario(t))
			d.hooks.openDir = func(name string) {
				if name != "a" {
					return
				}
				must(t, os.Rename(filepath.Join(tree, "a"), filepath.Join(tree, "moved")))
				must(t, syscall.Mkfifo(filepath.Join(tree, "a"), 0o600))
				time.Sleep(c.pause)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancel > 0 {
				time.AfterFunc(c.cancel, cancel)
			}
			start := time.Now()
			err := runGuarded(t, d, ctx, 30*time.Second, func(ctx context.Context, ct *Container) error {
				_, err := ct.CopyIn(ctx, tree, CopyLimits{Bytes: 1 << 20, Entries: 100, Timeout: c.timeout})
				return err
			})
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("the copy-in took %s", took)
			}
			if err == nil || errors.Is(err, ErrUnjudgeable) != c.unjudgable {
				t.Fatalf("%v (want unjudgeable %v)", err, c.unjudgable)
			}
			if c.cancel > 0 && !errors.Is(err, context.Canceled) {
				t.Errorf("a cancelled copy-in: %v", err)
			}
			if !f.called(t, "rm", "--force", "--volumes", Name(fixtureSpec())) {
				t.Error("a partial copy was left in place")
			}
		})
	}
}

// TestCopyInStopsTheWalkWhenDockerStops: a tree of nothing but pipes, which are skipped and so never written, keeps
// the walk away from the stream it would otherwise fail on; once the copy-in times out, the walk stops at its next
// entry instead of walking the whole tree. Not a byte reached docker, but the walk read the agent's tree (slowly
// enough to reach the timeout), so the copy-in is ErrUnjudgeable, and so is a removal that then fails: a retry would
// be a re-roll.
func TestCopyInStopsTheWalkWhenDockerStops(t *testing.T) {
	t.Parallel()
	for name, rmFail := range map[string]bool{"removed": false, "removal fails": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tree := t.TempDir()
			for i := range 300 {
				must(t, syscall.Mkfifo(filepath.Join(tree, fmt.Sprintf("p%03d", i)), 0o600))
			}
			sc := goodScenario(t)
			sc.RmFail = rmFail
			f, d := openFake(t, sc)
			d.hooks.entry = func(string) { time.Sleep(10 * time.Millisecond) } // 3 s for the whole tree
			start := time.Now()
			err := runGuarded(t, d, context.Background(), 30*time.Second, func(ctx context.Context, ct *Container) error {
				_, err := ct.CopyIn(ctx, tree, CopyLimits{Bytes: 1 << 20, Entries: 1000, Timeout: 300 * time.Millisecond})
				return err
			})
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("the walk went on after docker stopped: %s", took)
			}
			if !errors.Is(err, ErrUnjudgeable) || !f.called(t, "rm", "--force", "--volumes", Name(fixtureSpec())) || errors.Is(err, ErrCleanup) != rmFail {
				t.Fatalf("%v", err)
			}
		})
	}
}

// TestDaemonWithoutExecRecordsFailsAtStart: a daemon whose events cannot judge a command (the stream refused, or
// events without exec IDs and exit codes, as an old engine's) fails at start, before any of the grade's input is
// used: infrastructure, retryable, never ErrUnjudgeable, and fn never runs.
func TestDaemonWithoutExecRecordsFailsAtStart(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*scenario){
		"events refused":                 func(sc *scenario) { sc.EventsFail = true },
		"no exec IDs or exit codes":      func(sc *scenario) { sc.EventsBare = true },
		"no exec IDs, every event twice": func(sc *scenario) { sc.EventsBare = true; sc.EventsTwice = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sc := goodScenario(t)
			change(&sc)
			f, d := openFake(t, sc)
			d.statusWait = time.Second
			ran := false
			err := runGuarded(t, d, context.Background(), 30*time.Second, func(context.Context, *Container) error { ran = true; return nil })
			if err == nil || errors.Is(err, ErrUnjudgeable) || ran || !f.called(t, "rm", "--force", "--volumes", Name(fixtureSpec())) {
				t.Fatalf("%v (fn ran: %v)", err, ran)
			}
			t.Logf("%v", err)
		})
	}
}

// TestMarkerFailureBeforeTheCommand: the marker before a command fails (its exec cannot start, as when a fork bomb
// left by an earlier command holds every process slot) or its record never arrives. No command ran, but once the
// grade's tree is in, a retry would still be a re-roll: ErrUnjudgeable, and the container is removed. Without a
// copy-in first, none of the grade's input was used, and the error is plain (retryable); the container is removed too.
func TestMarkerFailureBeforeTheCommand(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	writeTree(t, tree, map[string]string{"go.mod": "module m\n"})
	for name, change := range map[string]func(*scenario){
		"marker exec fails":     func(sc *scenario) { sc.MarkerExit = 126 },
		"marker record missing": func(sc *scenario) { sc.MarkerUnrecorded = true },
	} {
		for _, copyIn := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, copy-in %v", name, copyIn), func(t *testing.T) {
				t.Parallel()
				sc := goodScenario(t)
				change(&sc)
				f, d := openFake(t, sc)
				d.statusWait = time.Second
				var execErr error
				err := runGuarded(t, d, context.Background(), 30*time.Second, func(ctx context.Context, c *Container) error {
					if copyIn {
						if _, err := c.CopyIn(ctx, tree, DefaultCopyLimits()); err != nil {
							return err
						}
					}
					_, execErr = c.Exec(ctx, Command{Command: "go test ./..."})
					if !f.called(t, "rm", "--force", "--volumes", Name(fixtureSpec())) {
						t.Error("the container was not removed")
					}
					return execErr
				})
				if execErr == nil || errors.Is(execErr, ErrUnjudgeable) != copyIn || errors.Is(err, ErrUnjudgeable) != copyIn ||
					errors.Is(execErr, context.Canceled) || errors.Is(execErr, ErrCleanup) {
					t.Fatalf("Exec: %v; Run: %v; want unjudgeable %v", execErr, err, copyIn)
				}
				if f.called(t, "go test ./...") {
					t.Error("the command ran after its marker failed")
				}
				t.Logf("%v", execErr)
			})
		}
	}
}

// TestExecWatchRecords: the daemon can deliver an event twice (from the replay and live), so a repeat with the same
// exec ID and exit code is the same record; a second exec ID for one marker, or a second exit code for one exec, is
// not, and nothing is judged from it.
func TestExecWatchRecords(t *testing.T) {
	const id = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	line := func(action string, attrs map[string]string) string {
		data, err := json.Marshal(map[string]any{"Type": "container", "Action": action, "Actor": map[string]any{"ID": id, "Attributes": attrs}})
		must(t, err)
		return string(data) + "\n"
	}
	start := func(execID string) string {
		return line("exec_start: sh -c true agentium-exec-n", map[string]string{"execID": execID})
	}
	die := func(execID, code string) string {
		return line("exec_die", map[string]string{"execID": execID, "exitCode": code})
	}
	cases := []struct {
		name   string
		events []string
		exit   int
		bad    bool
	}{
		{"once", []string{start("e1"), die("e1", "1")}, 1, false},
		{"twice", []string{start("e1"), start("e1"), die("e1", "1"), die("e1", "1")}, 1, false},
		{"another container", []string{strings.Replace(start("e2"), id, strings.Repeat("d", 64), 1), start("e1"), die("e1", "0")}, 0, false},
		{"two IDs for one marker", []string{start("e1"), start("e2"), die("e1", "0")}, 0, true},
		{"two exit codes", []string{start("e1"), die("e1", "0"), die("e1", "1")}, 0, true},
		{"no exit code", []string{start("e1"), die("e1", "")}, 0, true},
		{"split across writes", []string{start("e1")[:20], start("e1")[20:] + die("e1", "3")}, 3, false},
	}
	for _, c := range cases {
		w := newExecWatch(id, "agentium-exec-n")
		for _, ev := range c.events {
			w.Write([]byte(ev))
		}
		exit, err := w.wait(context.Background(), "agentium-exec-n", 10*time.Millisecond)
		if (err != nil) != c.bad || !c.bad && exit != c.exit {
			t.Errorf("%s: exit %d, %v", c.name, exit, err)
		}
	}
}

// TestExecTrustsOnlyTheDaemonsRecord: the docker client exits 1 both when the command did and when its own API calls
// failed. A command's exit counts only when the daemon's record of that exec agrees and the client reported nothing;
// a client failure is ErrUnjudgeable (its input was used, so it is left out, never retried) and removes the
// container, even with the counters healthy. A genuine exit 1 is a result.
func TestExecTrustsOnlyTheDaemonsRecord(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		change  func(*scenario)
		timeout time.Duration
		judged  bool
	}{
		"genuine exit 1":                       {func(sc *scenario) { sc.CommandExit = 1 }, 0, true},
		"genuine exit 0":                       {func(sc *scenario) {}, 0, true},
		"client fails after a passing command": {func(sc *scenario) { sc.ClientFail = true }, 0, false},
		"client fails after a failing command": {func(sc *scenario) { sc.CommandExit = 1; sc.ClientFail = true }, 0, false},
		"client exits 1 silently on a pass":    {func(sc *scenario) { sc.ClientExit = 1 }, 0, false},
		"client fails before the exec starts":  {func(sc *scenario) { sc.ClientFailEarly = true }, 0, false},
		"events end after the command started": {func(sc *scenario) { sc.EventsEndAfterStart = true; sc.CommandExit = 1 }, 0, false},
		"every event twice, exit 1":            {func(sc *scenario) { sc.EventsTwice = true; sc.CommandExit = 1 }, 0, true},
		// The watch fails after it saw the command start; the client then stalls until the timeout. Nothing shows the
		// command was still running, so the timeout does not settle.
		"timeout after the events ended": {func(sc *scenario) { sc.EventsEndAfterStart = true; sc.CommandBlock = true }, time.Second, false},
		"timeout, every event twice":     {func(sc *scenario) { sc.EventsTwice = true; sc.CommandBlock = true }, time.Second, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sc := goodScenario(t)
			c.change(&sc)
			f, d := openFake(t, sc)
			d.statusWait = time.Second
			var res Result
			var execErr error
			err := runGuarded(t, d, context.Background(), 20*time.Second, func(ctx context.Context, ct *Container) error {
				res, execErr = ct.Exec(ctx, Command{Command: "go test ./...", Timeout: c.timeout})
				removed := f.called(t, "rm", "--force", "--volumes", Name(fixtureSpec()))
				if removed != (!c.judged || c.timeout > 0) {
					t.Errorf("removed inside Exec: %v", removed)
				}
				return execErr
			})
			if c.judged {
				want := sc.CommandExit
				if c.timeout > 0 {
					want = -1
				}
				if execErr != nil || res.ExitCode != want || res.TimedOut != (c.timeout > 0) || res.Counters != (Counters{MemoryPeak: 104857600}) {
					t.Fatalf("result %+v, %v; want exit %d with its counters", res, execErr, want)
				}
				return
			}
			if !errors.Is(execErr, ErrUnjudgeable) || !errors.Is(err, ErrUnjudgeable) {
				t.Fatalf("Exec: %v; Run: %v; want ErrUnjudgeable", execErr, err)
			}
			t.Logf("%v", execErr)
		})
	}
}

// TestCleanupFailureIsClassified: a removal that fails (the daemon stopped answering) is ErrCleanup, and never a plain
// error that a caller might retry after the grade's input was used: then it is ErrUnjudgeable too. A settled result
// (Exec's nil error) stands, and its removal is retried by Run. Before the input was used, and on Agentium's own
// cancel, it is not ErrUnjudgeable.
func TestCleanupFailureIsClassified(t *testing.T) {
	t.Parallel()
	rm := []string{"rm", "--force", "--volumes", Name(fixtureSpec())}
	t.Run("settled exit, then the removal fails", func(t *testing.T) {
		t.Parallel()
		sc := goodScenario(t)
		sc.CommandExit, sc.RmFail = 1, true
		_, d := openFake(t, sc)
		var res Result
		var execErr error
		err := d.Run(context.Background(), fixtureSpec(), func(ctx context.Context, c *Container) error {
			res, execErr = c.Exec(ctx, Command{Command: "go test ./..."})
			return execErr
		})
		if execErr != nil || res.ExitCode != 1 {
			t.Fatalf("Exec: %+v, %v", res, execErr)
		}
		if !errors.Is(err, ErrCleanup) || !errors.Is(err, ErrUnjudgeable) {
			t.Fatalf("Run: %v; want ErrCleanup and ErrUnjudgeable", err)
		}
	})
	t.Run("settled timeout, then the removal fails", func(t *testing.T) {
		t.Parallel()
		sc := goodScenario(t)
		sc.CommandBlock, sc.RmFail = true, true
		f, d := openFake(t, sc)
		err := d.Run(context.Background(), fixtureSpec(), func(ctx context.Context, c *Container) error {
			res, err := c.Exec(ctx, Command{Command: "sleep 300", Timeout: time.Second})
			if err != nil || !res.TimedOut || res.ExitCode != -1 {
				t.Errorf("a settled timeout: %+v, %v", res, err)
			}
			if _, err := c.Exec(ctx, Command{Command: "true"}); !errors.Is(err, ErrGone) {
				t.Errorf("an exec after a failed removal: %v, want ErrGone", err)
			}
			return nil
		})
		if !errors.Is(err, ErrCleanup) || !errors.Is(err, ErrUnjudgeable) {
			t.Fatalf("Run: %v; want ErrCleanup and ErrUnjudgeable", err)
		}
		n := 0
		for _, argv := range f.calls(t) {
			if slices.Contains(argv, "rm") {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%d removals, want 2 (Exec's, then Run's)", n)
		}
	})
	t.Run("unjudgeable, and the removal fails", func(t *testing.T) {
		t.Parallel()
		sc := goodScenario(t)
		sc.ClientFail, sc.RmFail = true, true
		_, d := openFake(t, sc)
		err := d.Run(context.Background(), fixtureSpec(), func(ctx context.Context, c *Container) error {
			_, err := c.Exec(ctx, Command{Command: "go test ./..."})
			return err
		})
		if !errors.Is(err, ErrCleanup) || !errors.Is(err, ErrUnjudgeable) {
			t.Fatalf("Run: %v", err)
		}
	})
	t.Run("before the input, the removal fails", func(t *testing.T) {
		t.Parallel()
		sc := goodScenario(t)
		sc.Probe = strings.Replace(sc.Probe, "NoNewPrivs:\t1", "NoNewPrivs:\t0", 1)
		sc.RmFail = true
		f, d := openFake(t, sc)
		err := d.Run(context.Background(), fixtureSpec(), func(context.Context, *Container) error { return nil })
		if !errors.Is(err, ErrProbe) || !errors.Is(err, ErrCleanup) || errors.Is(err, ErrUnjudgeable) || !f.called(t, rm...) {
			t.Fatalf("Run: %v; want ErrProbe and ErrCleanup, not ErrUnjudgeable", err)
		}
	})
	t.Run("cancelled after the input, the removal fails", func(t *testing.T) {
		t.Parallel()
		sc := goodScenario(t)
		sc.CommandBlock, sc.RmFail = true, true
		_, d := openFake(t, sc)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := d.Run(ctx, fixtureSpec(), func(ctx context.Context, c *Container) error {
			time.AfterFunc(300*time.Millisecond, cancel)
			_, err := c.Exec(ctx, Command{Command: "sleep 300"})
			return err
		})
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCleanup) || errors.Is(err, ErrUnjudgeable) {
			t.Fatalf("Run: %v; want the cancel and ErrCleanup, not ErrUnjudgeable", err)
		}
	})
}
