package container

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func imageJSON(t *testing.T, id string, digests, tags []string, labels map[string]string, layers []string) string {
	t.Helper()
	return mustJSON(t, map[string]any{"Id": id, "RepoDigests": digests, "RepoTags": tags, "Os": "linux", "Architecture": "arm64", "Size": 900_000_000,
		"Config": map[string]any{"Env": []string{"PATH=/usr/bin:/bin"}, "Labels": labels}, "RootFS": map[string]any{"Layers": layers}})
}

func hexID(c string) string { return "sha256:" + strings.Repeat(c, 64) }

const goodCheck = "== git\ngit version 2.47.3\n== less\nless 668 (PCRE2 regular expressions)\n== packages\ngit=1:2.47.3-0+deb13u1\nless=668-1\n" +
	"== toolchain\ngo1.27.1\n== end\n"

// imageScenario is a daemon with the Go pin's base present (by its index digest, as step 0 pulled it), and a build
// that reports builtID, an image with the recipe's labels on the base's layers.
func imageScenario(t *testing.T) (scenario, Recipe, string) {
	t.Helper()
	p, _ := PinFor(ToolchainGo)
	r, err := NewRecipe(p, "arm64")
	must(t, err)
	builtID := hexID("b")
	sc := goodScenario(t)
	sc.Images = map[string]string{r.Base: imageJSON(t, p.Platforms["arm64"].Config, []string{r.Base}, []string{"golang:1.27"}, nil, []string{"sha256:l1", "sha256:l2"})}
	sc.AfterBuild = map[string]string{builtID: imageJSON(t, builtID, nil, []string{r.Tag},
		map[string]string{LabelRecipe: r.Hash, LabelBase: r.Base, LabelToolchain: "go 1.27"}, []string{"sha256:l1", "sha256:l2", "sha256:l3"})}
	sc.BuildID = builtID
	sc.CheckOut = goodCheck
	return sc, r, builtID
}

func openClient(t *testing.T, f *fake) *Docker {
	t.Helper()
	d, err := Open(context.Background(), f.opts())
	must(t, err)
	return d
}

// argvAfter is a call's argv after the client's own flags (--config, --host), or nil.
func argvAfter(argv []string) []string {
	if len(argv) >= 4 && argv[0] == "--config" && argv[2] == "--host" {
		return argv[4:]
	}
	return argv
}

func (f *fake) commands(t *testing.T, name string) [][]string {
	t.Helper()
	var out [][]string
	for _, argv := range f.calls(t) {
		if a := argvAfter(argv); len(a) > 0 && a[0] == name {
			out = append(out, a)
		}
	}
	return out
}

// Plan reads, and downloads nothing; Fetch with the base present builds without pulling, from the pinned digest, and
// checks the result offline.
func TestPlanThenFetchBuilds(t *testing.T) {
	t.Parallel()
	sc, r, builtID := imageScenario(t)
	f := newFake(t, sc)
	d := openClient(t, f)
	ctx := context.Background()
	plan, err := d.Plan(ctx, []Pin{r.Pin}, BuiltImages{})
	must(t, err)
	if len(plan.Items) != 1 || plan.Items[0].Ready || !plan.Items[0].BasePresent || plan.Items[0].BaseErr != nil {
		t.Fatalf("plan: %+v", plan.Items)
	}
	if base, apt := plan.Download(); base != 0 || apt != r.Pin.AptEstimate {
		t.Errorf("download: base %d, apt %d", base, apt)
	}
	for _, cmd := range []string{"pull", "build", "run", "create"} {
		if got := f.commands(t, cmd); len(got) > 0 {
			t.Fatalf("Plan ran docker %s: %v", cmd, got)
		}
	}
	var out bytes.Buffer
	built, err := d.Fetch(ctx, plan.Items[0], "test", &out)
	must(t, err)
	if built.ID != builtID || built.Tag != r.Tag || built.Recipe != r.Hash || built.Base != r.Base || built.Inside != "go1.27.1" ||
		!slices.Equal(built.Packages, []string{"git=1:2.47.3-0+deb13u1", "less=668-1"}) {
		t.Errorf("built: %+v", built)
	}
	if got := f.commands(t, "pull"); len(got) > 0 {
		t.Errorf("pulled a present base: %v", got)
	}
	builds := f.commands(t, "build")
	if len(builds) != 1 {
		t.Fatalf("builds: %v", builds)
	}
	b := builds[0]
	iid := b[slices.Index(b, "--iidfile")+1]
	want := []string{"build", "--pull=false", "--force-rm", "--tag", r.Tag, "--iidfile", iid, "--label", LabelRecipe + "=" + r.Hash,
		"--label", LabelBase + "=" + r.Base, "--label", LabelToolchain + "=go 1.27", "-"}
	if !slices.Equal(b, want) {
		t.Errorf("build argv:\n%q\nwant\n%q", b, want)
	}
	if _, err := os.Stat(iid); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the iid file's folder was left: %v", err)
	}
	// The build's context is the Dockerfile alone, which names the base by digest and installs only git and less.
	var stdin []byte
	for n, argv := range f.calls(t) {
		if a := argvAfter(argv); len(a) > 0 && a[0] == "build" {
			stdin = f.stdin(t, n+1)
		}
	}
	tr := tar.NewReader(bytes.NewReader(stdin))
	h, err := tr.Next()
	must(t, err)
	body, _ := io.ReadAll(tr)
	if h.Name != "Dockerfile" || string(body) != r.Dockerfile || !strings.Contains(r.Dockerfile, "FROM golang@sha256:e0174e51") ||
		!strings.Contains(r.Dockerfile, "apt-get install -y --no-install-recommends git less;") {
		t.Errorf("context %s:\n%s", h.Name, body)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("the context holds more than the Dockerfile: %v", err)
	}
	runs := f.commands(t, "run")
	if len(runs) != 1 {
		t.Fatalf("checks: %v", runs)
	}
	check := strings.Join(runs[0], " ")
	for _, w := range []string{"--rm --pull never", "--label agentium.data=test", "--network none", "--read-only", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--user 65534:65534", "--entrypoint sh " + builtID + " -c"} {
		if !strings.Contains(check, w) {
			t.Errorf("check %q lacks %q", check, w)
		}
	}
	if !strings.Contains(out.String(), "Successfully built") {
		t.Errorf("progress: %q", out.String())
	}
	// Recorded, the image is ready: nothing to download.
	plan, err = d.Plan(ctx, []Pin{r.Pin}, BuiltImages{r.Tag: built})
	must(t, err)
	if !plan.Items[0].Ready {
		t.Errorf("not ready after its build: %+v", plan.Items[0])
	}
	if base, apt := plan.Download(); base != 0 || apt != 0 {
		t.Errorf("download when ready: %d, %d", base, apt)
	}
	img, rec, err := d.GradingImage(ctx, r, BuiltImages{r.Tag: built})
	if err != nil || img.Ref != builtID || img.ID != builtID || rec.ID != builtID {
		t.Errorf("grading image %+v, %+v, %v", img, rec, err)
	}
}

// A base that is missing is pulled by its digest (the architecture's manifest while the index is unconfirmed), never
// by its tag, then checked to be the pinned content.
func TestFetchPullsByDigest(t *testing.T) {
	t.Parallel()
	p, _ := PinFor(ToolchainRust)
	r, err := NewRecipe(p, "arm64")
	must(t, err)
	builtID := hexID("e")
	sc := goodScenario(t)
	sc.Images = map[string]string{}
	sc.AfterPull = map[string]string{r.Base: imageJSON(t, p.Platforms["arm64"].Config, []string{r.Base}, nil, nil, []string{"sha256:r1"})}
	sc.AfterBuild = map[string]string{builtID: imageJSON(t, builtID, nil, []string{r.Tag},
		map[string]string{LabelRecipe: r.Hash, LabelBase: r.Base, LabelToolchain: "rust 1.95"}, []string{"sha256:r1", "sha256:r2"})}
	sc.BuildID = builtID
	sc.CheckOut = strings.Replace(goodCheck, "go1.27.1", "rustc 1.95.0 (59807616e 2026-04-14)", 1)
	f := newFake(t, sc)
	d := openClient(t, f)
	ctx := context.Background()
	plan, err := d.Plan(ctx, []Pin{p}, BuiltImages{})
	must(t, err)
	if it := plan.Items[0]; it.Ready || it.BasePresent || it.BaseSize != p.Platforms["arm64"].Size {
		t.Fatalf("plan: %+v", it)
	}
	if base, apt := plan.Download(); base != 279315772 || apt != 40e6 {
		t.Errorf("download %d, %d", base, apt)
	}
	built, err := d.Fetch(ctx, plan.Items[0], "test", io.Discard)
	must(t, err)
	if built.Inside != "rustc 1.95.0 (59807616e 2026-04-14)" {
		t.Errorf("inside: %q", built.Inside)
	}
	pulls := f.commands(t, "pull")
	if len(pulls) != 1 || !slices.Equal(pulls[0], []string{"pull", "rust@" + p.Platforms["arm64"].Manifest}) {
		t.Errorf("pulls: %v", pulls)
	}
	for _, argv := range f.calls(t) {
		if strings.Contains(strings.Join(argv, " "), "rust:1.95") {
			t.Errorf("a call names the tag: %v", argv)
		}
	}
}

// Every way a pull or a build can be the wrong image, or not work, stops before a record is made.
func TestFetchRefuses(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(sc *scenario, r Recipe, builtID string)
		want   string
	}{
		{"a pulled base with other content", func(sc *scenario, r Recipe, _ string) {
			sc.AfterPull = map[string]string{r.Base: strings.Replace(sc.Images[r.Base], r.Pin.Platforms["arm64"].Config, hexID("9"), 1)}
			sc.Images = map[string]string{}
		}, "not the pinned one"},
		{"a pull that fails", func(sc *scenario, r Recipe, _ string) {
			sc.Images, sc.PullExit = map[string]string{}, 1
		}, "pull golang@sha256"},
		{"a build that fails", func(sc *scenario, _ Recipe, _ string) { sc.BuildExit = 100 }, "Unable to locate package"},
		{"no image ID", func(sc *scenario, _ Recipe, _ string) { sc.BuildID = "" }, "reported no image ID"},
		{"an ID the daemon lacks", func(sc *scenario, _ Recipe, _ string) { sc.BuildID = hexID("c") }, "not in the daemon"},
		{"no labels", func(sc *scenario, r Recipe, id string) {
			sc.AfterBuild[id] = imageJSON(t, id, nil, []string{r.Tag}, nil, []string{"sha256:l1", "sha256:l2", "sha256:l3"})
		}, "label agentium.recipe"},
		{"not on the base", func(sc *scenario, r Recipe, id string) {
			sc.AfterBuild[id] = imageJSON(t, id, nil, []string{r.Tag}, map[string]string{LabelRecipe: r.Hash, LabelBase: r.Base, LabelToolchain: "go 1.27"}, []string{"sha256:x1", "sha256:l2", "sha256:l3"})
		}, "not built on the base"},
		{"untagged", func(sc *scenario, r Recipe, id string) {
			sc.AfterBuild[id] = imageJSON(t, id, nil, nil, map[string]string{LabelRecipe: r.Hash, LabelBase: r.Base, LabelToolchain: "go 1.27"}, []string{"sha256:l1", "sha256:l2", "sha256:l3"})
		}, "is not tagged"},
		{"no less", func(sc *scenario, _ Recipe, _ string) {
			sc.CheckOut = strings.Replace(goodCheck, "less 668 (PCRE2 regular expressions)", "sh: 1: less: not found", 1)
		}, "less printed"},
		{"another Go", func(sc *scenario, _ Recipe, _ string) {
			sc.CheckOut = strings.Replace(goodCheck, "go1.27.1", "go1.28.0", 1)
		}, "holds go 1.28"},
		{"a check that fails", func(sc *scenario, _ Recipe, _ string) { sc.CheckExit = 2 }, "exit 2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sc, r, builtID := imageScenario(t)
			tt.change(&sc, r, builtID)
			f := newFake(t, sc)
			d := openClient(t, f)
			ctx := context.Background()
			plan, err := d.Plan(ctx, []Pin{r.Pin}, BuiltImages{})
			must(t, err)
			built, err := d.Fetch(ctx, plan.Items[0], "test", io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) || built.ID != "" {
				t.Errorf("Fetch = %+v, %v; want an error with %q", built, err, tt.want)
			}
		})
	}
}

// A present base under the pin's name but of other content is not used (and is reported), and a build is never made
// from it.
func TestBaseMismatch(t *testing.T) {
	t.Parallel()
	sc, r, _ := imageScenario(t)
	sc.Images[r.Base] = strings.Replace(sc.Images[r.Base], r.Pin.Platforms["arm64"].Config, hexID("9"), 1)
	f := newFake(t, sc)
	d := openClient(t, f)
	plan, err := d.Plan(context.Background(), []Pin{r.Pin}, BuiltImages{})
	must(t, err)
	if it := plan.Items[0]; !errors.Is(it.BaseErr, ErrBaseMismatch) || it.BasePresent {
		t.Fatalf("plan: %+v", it)
	}
	if _, err := d.Fetch(context.Background(), plan.Items[0], "test", io.Discard); !errors.Is(err, ErrBaseMismatch) {
		t.Errorf("Fetch: %v", err)
	}
	if len(f.commands(t, "build")) > 0 || len(f.commands(t, "pull")) > 0 {
		t.Error("pulled or built over a mismatched base")
	}
	// The containerd image store names an image by its reference's digest: accepted.
	sc.Images[r.Base] = strings.Replace(sc.Images[r.Base], hexID("9"), r.Pin.Index, 1)
	f.set(t, sc)
	if _, err := d.Base(context.Background(), r.Pin); err != nil {
		t.Errorf("the reference's own digest as the ID: %v", err)
	}
}

// A recorded grading image that was removed, or relabelled, is not used.
func TestGradingImageRefuses(t *testing.T) {
	t.Parallel()
	sc, r, builtID := imageScenario(t)
	f := newFake(t, sc)
	d := openClient(t, f)
	ctx := context.Background()
	rec := BuiltImages{r.Tag: {Tag: r.Tag, ID: builtID, Recipe: r.Hash}}
	if _, _, err := d.GradingImage(ctx, r, BuiltImages{}); !errors.Is(err, ErrImageMissing) || !strings.Contains(err.Error(), "agentium images pull") {
		t.Errorf("not built: %v", err)
	}
	if _, _, err := d.GradingImage(ctx, r, rec); !errors.Is(err, ErrImageMissing) {
		t.Errorf("removed: %v", err) // AfterBuild answers only once a build ran
	}
	sc.Images[builtID] = imageJSON(t, builtID, nil, []string{r.Tag}, map[string]string{LabelRecipe: "x", LabelBase: r.Base, LabelToolchain: "go 1.27"}, nil)
	f.set(t, sc)
	if _, _, err := d.GradingImage(ctx, r, rec); !errors.Is(err, ErrBaseMismatch) {
		t.Errorf("relabelled: %v", err)
	}
	stale := BuiltImages{r.Tag: {Tag: r.Tag, ID: builtID, Recipe: "another recipe"}}
	if _, _, err := d.GradingImage(ctx, r, stale); !errors.Is(err, ErrImageMissing) {
		t.Errorf("another recipe's record: %v", err)
	}
}

func TestBuiltImagesFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "images.json")
	b, err := LoadBuilt(path)
	if err != nil || len(b) != 0 {
		t.Fatalf("missing file: %v, %v", b, err)
	}
	b["agentium-grade:go1.27-x"] = Built{Tag: "agentium-grade:go1.27-x", ID: hexID("a"), Packages: []string{"git=1"}, At: time.Unix(1, 0).UTC()}
	must(t, b.Save(path))
	got, err := LoadBuilt(path)
	if err != nil || got["agentium-grade:go1.27-x"].ID != hexID("a") {
		t.Errorf("round trip: %+v, %v", got, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	must(t, os.WriteFile(path, []byte("{"), 0o600))
	if _, err := LoadBuilt(path); err == nil {
		t.Error("a damaged record was read")
	}
}

func TestLocalImagesAndRemove(t *testing.T) {
	t.Parallel()
	sc, r, builtID := imageScenario(t)
	old := hexID("d")
	user, other := hexID("e"), hexID("f")
	sc.Images[builtID] = imageJSON(t, builtID, nil, []string{r.Tag}, nil, []string{"sha256:l1", "sha256:l2", "sha256:l3"})
	sc.Images[old] = imageJSON(t, old, nil, []string{"agentium-grade:go1.27-000000000000"}, nil, nil)
	// The user's image built FROM a grading image inherits its labels; another data folder's grading image is not in
	// this record: both are foreign.
	sc.Images[user] = imageJSON(t, user, nil, []string{"myapp:dev"}, map[string]string{LabelRecipe: r.Hash}, nil)
	sc.Images[other] = imageJSON(t, other, nil, []string{"agentium-grade:go1.27-ffffffffffff"}, nil, nil)
	sc.ImageLS = builtID + "\t" + r.Tag + "\tgo 1.27\n" + old + "\tagentium-grade:go1.27-000000000000\tgo 1.27\n" + user + "\tmyapp:dev\tgo 1.27\n" +
		other + "\tagentium-grade:go1.27-ffffffffffff\tgo 1.27\n"
	f := newFake(t, sc)
	d := openClient(t, f)
	ctx := context.Background()
	list, err := d.LocalImages(ctx, BuiltImages{r.Tag: {ID: builtID}, "agentium-grade:go1.27-000000000000": {ID: old}})
	must(t, err)
	var got []string
	for _, l := range list {
		got = append(got, l.Kind+" "+l.Ref+" "+map[bool]string{true: "current", false: "old"}[l.Current])
	}
	want := []string{"base " + r.Base + " current", "grading " + r.Tag + " current", "grading agentium-grade:go1.27-000000000000 old",
		"foreign myapp:dev old", "foreign agentium-grade:go1.27-ffffffffffff old"}
	if !slices.Equal(got, want) {
		t.Errorf("images:\n%q\nwant\n%q", got, want)
	}
	// The base is also the user's golang:1.27: removing Agentium's reference frees nothing.
	if !slices.Equal(list[0].OtherTags, []string{"golang:1.27"}) || len(list[1].OtherTags) != 0 {
		t.Errorf("other tags: %v, %v", list[0].OtherTags, list[1].OtherTags)
	}
	must(t, d.RemoveImage(ctx, r.Tag))
	if rm := f.commands(t, "image"); !slices.ContainsFunc(rm, func(a []string) bool { return slices.Equal(a, []string{"image", "rm", r.Tag}) }) {
		t.Errorf("no plain image rm: %v", rm)
	}
	sc.ImageRmErr = "Error response from daemon: conflict: unable to remove repository reference (must force) - container x is using its referenced image"
	f.set(t, sc)
	if err := d.RemoveImage(ctx, r.Tag); err == nil || !strings.Contains(err.Error(), "must force") {
		t.Errorf("a refused removal: %v", err)
	}
	sc.ImageRmErr = "Error response from daemon: No such image: x"
	f.set(t, sc)
	if err := d.RemoveImage(ctx, r.Tag); err != nil {
		t.Errorf("an image already gone: %v", err)
	}
}

// What an image is built on is read from its layers, never from its labels.
func TestBuiltOn(t *testing.T) {
	base := LocalImage{Layers: []string{"a", "b"}}
	for _, tt := range []struct {
		layers      []string
		on, known   bool
		description string
	}{
		{[]string{"a", "b", "c"}, true, true, "on it"},
		{[]string{"a", "x", "c"}, false, true, "another base"},
		{[]string{"a", "b"}, false, true, "the base itself"},
		{nil, false, false, "layers unknown"},
	} {
		if on, known := BuiltOn(base, LocalImage{Layers: tt.layers}); on != tt.on || known != tt.known {
			t.Errorf("%s: %v %v", tt.description, on, known)
		}
	}
	if _, known := BuiltOn(LocalImage{}, LocalImage{Layers: []string{"a"}}); known {
		t.Error("a base without layers is known")
	}
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[string]int64{"1.2GB": 1_200_000_000, "512MB": 512_000_000, "64kB": 64_000, "12B": 12, "0B": 0, "N/A": -1, "": -1, "-1B": -1} {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%q) = %d, want %d", in, got, want)
		}
	}
}

// What a pull or a build prints reaches the terminal with the home folder as ~ and the endpoint hidden, partial lines
// included.
func TestStreamRedactsTheHome(t *testing.T) {
	t.Parallel()
	sc, r, _ := imageScenario(t)
	sc.Images = map[string]string{}
	sc.PullExit = 1
	sc.PullStderr = "error during connect: Get \"http://%2Fhome%2Fagentium%2F.colima%2Fdefault%2Fdocker.sock/v1.47/images\": dial unix /home/agentium/.colima/default/docker.sock: connect\n" +
		"config in /home/agentium/.docker and /home/agentiumx stays; last line /home/agentium"
	f := newFake(t, sc)
	d := openClient(t, f)
	var out bytes.Buffer
	err := d.pull(context.Background(), r.Base, &out)
	if err == nil || strings.Contains(err.Error(), "/home/agentium/") {
		t.Errorf("error %v", err)
	}
	got := out.String()
	for _, leak := range []string{"/home/agentium/", "%2Fhome%2Fagentium", "/home/agentium\n"} {
		if strings.Contains(got, leak) {
			t.Errorf("output names the home folder (%q):\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "~/.docker and /home/agentiumx stays; last line ~") || !strings.Contains(got, "pulled "+r.Base) {
		t.Errorf("output:\n%s", got)
	}
}

// Each stream is redacted on its own: a partial line of stderr ending in the home folder is never joined to stdout's
// next line (which would hide the path's end), in either order of delivery.
func TestRedactingStreamsStayApart(t *testing.T) {
	d := &Docker{home: "/home/agentium"}
	for name, order := range map[string][]int{"stderr first": {1, 0, 2}, "stdout first": {0, 1, 2}} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			out, errw := shownStreams(&buf, d.redact)
			steps := []func(){
				func() { io.WriteString(out, "pulled golang@sha256:x\n") },
				func() { io.WriteString(errw, "last line /home/agentium") },
				func() { out.Flush(); errw.Flush() },
			}
			for _, i := range order {
				steps[i]()
			}
			got := buf.String()
			if strings.Contains(got, "/home/agentium") || !strings.Contains(got, "last line ~") || !strings.Contains(got, "pulled golang@sha256:x\n") {
				t.Errorf("output %q", got)
			}
		})
	}
}
