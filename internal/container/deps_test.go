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
)

func testVolume() DepsVolume { return DepsVolume{Data: "test", Project: "3", Image: fixtureImageID} }

func depsVolumeJSON(t *testing.T, v DepsVolume, labels map[string]string, options map[string]string) string {
	t.Helper()
	return mustJSON(t, map[string]any{"Name": v.Name(), "Driver": "local", "Options": options, "Labels": labels, "CreatedAt": "2026-10-04T12:00:00Z"})
}

func TestDepsVolumeName(t *testing.T) {
	v := testVolume()
	if got := v.Name(); got != "agentium-deps-test-3-84349ebd3bf9" || !volPattern.MatchString(got) {
		t.Errorf("name %q", got)
	}
	for _, bad := range []DepsVolume{{Data: "TEST", Project: "3", Image: fixtureImageID}, {Data: "test", Project: "../x", Image: fixtureImageID},
		{Data: "test", Project: "3", Image: "golang:1.27"}, {Data: "test", Project: "", Image: fixtureImageID}} {
		if err := bad.validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// A missing deps volume is made, plain and labelled; an existing one must be exactly Agentium's.
func TestEnsureDepsVolume(t *testing.T) {
	t.Parallel()
	v := testVolume()
	sc := goodScenario(t)
	sc.VolumeUntilCreate = true
	sc.Volume = depsVolumeJSON(t, v, v.labels(), nil)
	f, d := openFake(t, sc)
	ctx := context.Background()
	if created, err := d.EnsureDepsVolume(ctx, v); err != nil || created != "2026-10-04T12:00:00Z" {
		t.Fatalf("EnsureDepsVolume = %q, %v", created, err)
	}
	creates := f.commands(t, "volume")
	want := []string{"volume", "create", "--driver", "local", "--label", "agentium.data=test", "--label", "agentium.deps=3",
		"--label", "agentium.image=84349ebd3bf9", "--label", "agentium.mode=container-v1", v.Name()}
	if !slices.ContainsFunc(creates, func(a []string) bool { return slices.Equal(a, want) }) {
		t.Errorf("volume calls %q, want %q", creates, want)
	}
	// Present already: nothing is made.
	f2, d2 := openFake(t, func() scenario { s := sc; s.VolumeUntilCreate = false; return s }())
	if _, err := d2.EnsureDepsVolume(ctx, v); err != nil {
		t.Fatal(err)
	}
	if f2.called(t, "volume", "create") {
		t.Error("made a volume that exists")
	}
	for name, json := range map[string]string{
		"someone else's":  depsVolumeJSON(t, v, map[string]string{"owner": "x"}, nil),
		"another project": depsVolumeJSON(t, v, map[string]string{LabelData: "test", LabelMode: Mode, LabelDeps: "4", LabelImage: "84349ebd3bf9"}, nil),
		"a bind":          depsVolumeJSON(t, v, v.labels(), map[string]string{"type": "none", "o": "bind", "device": "/Users/x"}),
	} {
		t.Run(name, func(t *testing.T) {
			s := sc
			s.VolumeUntilCreate = false
			s.Volume = json
			f, d := openFake(t, s)
			if _, err := d.EnsureDepsVolume(ctx, v); err == nil {
				t.Error("accepted")
			}
			if f.called(t, "volume", "create") {
				t.Error("made a volume over it")
			}
		})
	}
}

// The seed stream lists every folder before what is in it, owned by the grade's user, and leaves out what the volume
// holds already.
func TestSeedTar(t *testing.T) {
	t.Parallel()
	m2 := t.TempDir()
	writeTree(t, m2, map[string]string{"org/x/1.0/x-1.0.jar": "jar", "org/x/1.0/x-1.0.pom": "pom", "org/y/2.0/y-2.0.jar": "y"})
	must(t, os.Symlink("x-1.0.jar", filepath.Join(m2, "org/x/1.0/link.jar")))
	seeds := []Seed{
		{Root: m2, Path: ".", To: "m2", Skip: func(name string) bool { return name == "m2/org/y/2.0/y-2.0.jar" }},
		{Root: filepath.Join(t.TempDir(), "missing"), Path: ".", To: "cargo"},
		{Root: m2, Path: "no/such/folder", To: "gradle-ro"},
	}
	entries := []SeedEntry{{Name: "py", Dir: true}, {Name: "gradle/caches/modules-2", Link: "../../gradle-ro/modules-2"}, {Name: "gradle/gradle.properties", Body: []byte("org.gradle.daemon=false\n")}}
	var buf bytes.Buffer
	stats, written, err := SeedTar(context.Background(), &buf, seeds, entries, SeedLimits())
	must(t, err)
	var names []string
	seen := map[string]bool{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must(t, err)
		name := strings.TrimSuffix(h.Name, "/")
		if parent := filepath.Dir(name); parent != "." && !seen[parent] {
			t.Errorf("%s comes before its folder %s", name, parent)
		}
		seen[name] = true
		if h.Uid != 65534 || h.Gid != 65534 {
			t.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
		names = append(names, h.Name+" "+string(h.Typeflag)+" "+h.Linkname)
	}
	want := []string{"py/ 5 ", "gradle/ 5 ", "gradle/caches/ 5 ", "gradle/caches/modules-2 2 ../../gradle-ro/modules-2", "gradle/gradle.properties 0 ",
		"m2/ 5 ", "m2/org/ 5 ", "m2/org/x/ 5 ", "m2/org/x/1.0/ 5 ", "m2/org/x/1.0/link.jar 2 x-1.0.jar", "m2/org/x/1.0/x-1.0.jar 0 ", "m2/org/x/1.0/x-1.0.pom 0 ",
		"m2/org/y/ 5 ", "m2/org/y/2.0/ 5 "}
	if !slices.Equal(names, want) {
		t.Errorf("entries:\n%q\nwant\n%q", names, want)
	}
	if !slices.Equal(written, []string{"gradle/caches/modules-2", "gradle/gradle.properties", "m2/org/x/1.0/link.jar", "m2/org/x/1.0/x-1.0.jar", "m2/org/x/1.0/x-1.0.pom"}) {
		t.Errorf("written %q", written)
	}
	if stats.Bytes != int64(len("org.gradle.daemon=false\n")+len("jar")+len("pom")) {
		t.Errorf("stats %+v", stats)
	}
	for _, bad := range []SeedEntry{{Name: "/etc/passwd", Body: []byte("x")}, {Name: "../x", Dir: true}, {Name: "", Dir: true}} {
		if _, _, err := SeedTar(context.Background(), io.Discard, nil, []SeedEntry{bad}, SeedLimits()); err == nil {
			t.Errorf("%q accepted", bad.Name)
		}
	}
	if _, _, err := SeedTar(context.Background(), io.Discard, []Seed{{Root: m2, Path: ".", To: "../up"}}, nil, SeedLimits()); err == nil {
		t.Error("a seed outside /deps accepted")
	}
	if _, _, err := SeedTar(context.Background(), io.Discard, []Seed{{Root: m2, Path: ".", To: "m2"}}, nil, CopyLimits{Bytes: 4, Entries: 100}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("past the limit: %v", err)
	}
}

// A seed goes in through a container whose top folders the daemon makes first, and whose own tar then unpacks the
// stream as the grade's user without replacing anything the volume holds; the container is removed after.
func TestSeedDeps(t *testing.T) {
	t.Parallel()
	v := testVolume()
	sc := goodScenario(t)
	sc.Volume = depsVolumeJSON(t, v, v.labels(), nil)
	f, d := openFake(t, sc)
	src := t.TempDir()
	writeTree(t, src, map[string]string{"registry/index/a": "a"})
	img := Image{Ref: goImage, ID: fixtureImageID}
	written, err := d.SeedDeps(context.Background(), v, img, []Seed{{Root: src, Path: ".", To: "cargo"}}, []SeedEntry{{Name: "py", Dir: true}}, SeedLimits())
	must(t, err)
	if !slices.Equal(written, []string{"cargo/registry/index/a"}) {
		t.Errorf("written %q", written)
	}
	var create, cp, start, rm []string
	var cpAt, startAt int
	for n, argv := range f.calls(t) {
		switch a := argvAfter(argv); {
		case a[0] == "create":
			create = a
		case a[0] == "cp":
			cp, cpAt = a, n+1
		case a[0] == "start":
			start, startAt = a, n+1
		case a[0] == "rm":
			rm = a
		case a[0] == "exec" || a[0] == "run":
			t.Errorf("a seed ran something else: %v", a)
		}
	}
	name := create[slices.Index(create, "--name")+1]
	joined := strings.Join(create, " ")
	for _, w := range []string{"--pull never", "--label agentium.data=test", "--interactive", "--network none", "--read-only", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--user 65534:65534", "--mount type=volume,src=" + v.Name() + ",dst=/deps --workdir /deps",
		"--entrypoint tar " + goImage + " --extract --file - --skip-old-files --no-same-owner"} {
		if !strings.Contains(joined, w) {
			t.Errorf("create %q lacks %q", joined, w)
		}
	}
	if !strings.HasPrefix(name, "agentium-test-seed-") || !slices.Equal(cp, []string{"cp", "-", name + ":/deps"}) ||
		!slices.Equal(start, []string{"start", "--attach", "--interactive", name}) || !slices.Equal(rm, []string{"rm", "--force", "--volumes", name}) {
		t.Errorf("name %s, cp %v, start %v, rm %v", name, cp, start, rm)
	}
	if cpAt > startAt {
		t.Error("the top folders were made after the stream")
	}
	if got := tarNames(t, f.stdin(t, cpAt)); !slices.Equal(got, []string{"py/ 5 65534 0755 ", "cargo/ 5 65534 0755 "}) {
		t.Errorf("top folders %q", got)
	}
	if got := tarNames(t, f.stdin(t, startAt)); !slices.Equal(got, []string{"py/ 5 65534 0755 ", "cargo/ 5 65534 0755 ", "cargo/registry/ 5 65534 0755 ",
		"cargo/registry/index/ 5 65534 0755 ", "cargo/registry/index/a 0 65534 0644 "}) {
		t.Errorf("stream %q", got)
	}
	// The volume's tar failing is the seed's failure, and the container still goes.
	sc.SeedExit = 2
	f.set(t, sc)
	if _, err := d.SeedDeps(context.Background(), v, img, []Seed{{Root: src, Path: ".", To: "cargo"}}, nil, SeedLimits()); err == nil || !strings.Contains(err.Error(), "tar exited 2") {
		t.Errorf("a failing tar: %v", err)
	}
	if rms := f.commands(t, "rm"); len(rms) != 2 {
		t.Errorf("removals %v", rms)
	}
	// A volume that is not Agentium's is never seeded.
	sc.SeedExit = 0
	sc.Volume = depsVolumeJSON(t, v, map[string]string{"owner": "x"}, nil)
	f2, d2 := openFake(t, sc)
	if _, err := d2.SeedDeps(context.Background(), v, img, nil, nil, SeedLimits()); err == nil || f2.called(t, "cp") {
		t.Errorf("seeded someone else's volume: %v", err)
	}
}

// A seed's folder, or a folder on its way, that is a link out of its trusted root is refused before anything is
// made or sent: nothing of the folder it points to reaches the volume.
func TestSeedRefusesLinks(t *testing.T) {
	t.Parallel()
	deps, outside := t.TempDir(), t.TempDir()
	writeTree(t, outside, map[string]string{"secret/id_ed25519": "PRIVATE", "registry/cache/x": "x"})
	writeTree(t, deps, map[string]string{"gradle-ro/modules-2/ok.jar": "ok"})
	must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(deps, "m2"))) // the seed's own folder
	must(t, os.Symlink(outside, filepath.Join(deps, "cargo")))                       // a folder on its way
	must(t, os.Symlink("gradle-ro", filepath.Join(deps, "inner")))                   // even one that stays inside
	for name, seed := range map[string]Seed{
		"the folder":         {Root: deps, Path: "m2", To: "m2"},
		"an ancestor":        {Root: deps, Path: "cargo/registry", To: "cargo/registry"},
		"a link inside":      {Root: deps, Path: "inner/modules-2", To: "gradle-ro/modules-2"},
		"a path that climbs": {Root: deps, Path: "../" + filepath.Base(outside), To: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			good := Seed{Root: deps, Path: "gradle-ro/modules-2", To: "gradle-ro/modules-2"}
			_, written, err := SeedTar(context.Background(), &buf, []Seed{good, seed}, []SeedEntry{{Name: "py", Dir: true}}, SeedLimits())
			if err == nil || buf.Len() != 0 || written != nil {
				t.Fatalf("SeedTar = %v, %d bytes, %v", err, buf.Len(), written)
			}
			sc := goodScenario(t)
			v := testVolume()
			sc.Volume = depsVolumeJSON(t, v, v.labels(), nil)
			f, d := openFake(t, sc)
			if _, err := d.SeedDeps(context.Background(), v, Image{Ref: goImage, ID: fixtureImageID}, []Seed{good, seed}, nil, SeedLimits()); err == nil {
				t.Fatal("SeedDeps accepted it")
			}
			if f.called(t, "create") || f.called(t, "cp") {
				t.Errorf("a container was made for a refused seed: %v", f.calls(t))
			}
		})
	}
	// The real folders under the root still seed.
	var buf bytes.Buffer
	_, written, err := SeedTar(context.Background(), &buf, []Seed{{Root: deps, Path: "gradle-ro/modules-2", To: "gradle-ro/modules-2"}}, nil, SeedLimits())
	if err != nil || !slices.Equal(written, []string{"gradle-ro/modules-2/ok.jar"}) {
		t.Errorf("a real folder: %v, %v", written, err)
	}
}

// A deps warm-up's container has the network and its deps volume read-write; a grade's never does, and Run refuses a
// warm-up's spec.
func TestWarmShapeIsNeverAGrade(t *testing.T) {
	t.Parallel()
	grade := fixtureSpec()
	grade.Deps = "agentium-deps-test-3-84349ebd3bf9"
	warm := grade
	warm.warm = true
	g, w := strings.Join(createArgs(grade), " "), strings.Join(createArgs(warm), " ")
	if !strings.Contains(g, "--network none") || !strings.Contains(g, "dst=/deps,readonly") {
		t.Errorf("grade: %s", g)
	}
	if !strings.Contains(w, "--network bridge") || strings.Contains(w, "readonly") || !strings.Contains(w, "dst=/deps --entrypoint") {
		t.Errorf("warm: %s", w)
	}
	for _, flag := range []string{"--cap-drop ALL", "--security-opt no-new-privileges", "--read-only", "--user 65533:65533", "--rm", "--init", "--ipc private"} {
		if !strings.Contains(w, flag) {
			t.Errorf("warm lacks %s", flag)
		}
	}
	// The daemon's record of each: accepted for its own spec, refused for the other.
	record := func(network string, rw bool) []byte {
		return inspectWith(t, func(c map[string]any) {
			h := obj(c, "HostConfig")
			h["NetworkMode"] = network
			c["NetworkSettings"] = map[string]any{"Networks": map[string]any{network: map[string]any{}}}
			h["Mounts"] = append(h["Mounts"].([]any), map[string]any{"Type": "volume", "Source": grade.Deps, "Target": DepsDir, "ReadOnly": !rw})
			c["Mounts"] = append(c["Mounts"].([]any), map[string]any{"Type": "volume", "Name": grade.Deps, "Destination": DepsDir, "RW": rw, "Driver": "local"})
		})
	}
	if _, _, err := checkInspect(record("none", false), grade, "runc"); err != nil {
		t.Errorf("a grade's record: %v", err)
	}
	if _, _, err := checkInspect(record("bridge", true), warm, "runc"); err != nil {
		t.Errorf("a warm-up's record: %v", err)
	}
	for name, raw := range map[string][]byte{"networked": record("bridge", false), "deps writable": record("none", true), "a warm-up's": record("bridge", true)} {
		if _, _, err := checkInspect(raw, grade, "runc"); !errors.Is(err, ErrMismatch) {
			t.Errorf("a grade with %s: %v", name, err)
		}
	}
	if _, _, err := checkInspect(record("none", false), warm, "runc"); !errors.Is(err, ErrMismatch) {
		t.Errorf("a warm-up in a grade's shape: %v", err)
	}
	// The probes: a writable /deps only for a warm-up.
	good := readFixture(t, "probe.txt")
	writable := strings.Replace(strings.Replace(good, "/deps missing", "/deps denied", 1), "== devlog", "/dev/root /deps ext4 rw,relatime 0 0\n== devlog", 1)
	networked := strings.Replace(writable, "== net\nlo\n", "== net\neth0\nlo\n", 1)
	if err := checkProbesFor(networked, true, true); err != nil {
		t.Errorf("a warm-up's probes: %v", err)
	}
	if err := checkProbesFor(writable, true, false); !errors.Is(err, ErrProbe) {
		t.Errorf("a grade with /deps writable: %v", err)
	}
	if err := checkProbesFor(strings.Replace(networked, "rw,relatime", "ro,relatime", 1), true, false); !errors.Is(err, ErrProbe) {
		t.Errorf("a grade with a network: %v", err)
	}
	if err := checkProbesFor(strings.Replace(networked, "rw,relatime", "ro,relatime", 1), true, true); !errors.Is(err, ErrProbe) {
		t.Errorf("a warm-up with /deps read-only: %v", err)
	}
	// Run never makes a warm-up's container.
	f, d := openFake(t, goodScenario(t))
	if err := d.Run(context.Background(), warm, func(context.Context, *Container) error { return nil }); err == nil || f.called(t, "create") {
		t.Errorf("Run with a warm-up's spec: %v", err)
	}
}

// Warm refuses a volume that is not the image's, or not Agentium's, before any container is made.
func TestWarmRefuses(t *testing.T) {
	t.Parallel()
	v := testVolume()
	sc := goodScenario(t)
	sc.Volume = depsVolumeJSON(t, v, v.labels(), nil)
	f, d := openFake(t, sc)
	img := Image{Ref: goImage, ID: hexID("a")}
	fn := func(context.Context, *Container) error { return nil }
	if err := d.Warm(context.Background(), WarmSpec{Volume: v, Image: img, Limits: DefaultLimits(4), Deadline: 600e9}, fn); err == nil {
		t.Error("another image's volume accepted")
	}
	sc.Volume = depsVolumeJSON(t, v, map[string]string{"owner": "x"}, nil)
	f.set(t, sc)
	if err := d.Warm(context.Background(), WarmSpec{Volume: v, Image: Image{Ref: goImage, ID: fixtureImageID}, Limits: DefaultLimits(4), Deadline: 600e9}, fn); err == nil {
		t.Error("someone else's volume accepted")
	}
	if f.called(t, "create") {
		t.Error("a container was made")
	}
}
