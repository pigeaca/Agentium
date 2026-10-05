package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests use a real local daemon and only images already present (the driver's goImage, and whatever else of the
// pin table step 0 left); they never pull or build, except TestRealBuild, which needs AGENTIUM_TEST_ALLOW_BUILD=1.

// The plan reads the daemon and downloads nothing: a pinned base present by its digest is the pinned content (its
// image ID is the config digest read from the registry's manifest), and the others are missing.
func TestRealPlan(t *testing.T) {
	d, _ := realDocker(t)
	ctx := context.Background()
	plan, err := d.Plan(ctx, Pins(), BuiltImages{})
	must(t, err)
	for _, it := range plan.Items {
		t.Logf("%s: base present %v (%v), %d bytes to pull, apt about %d", it.Pin.Name(), it.BasePresent, it.BaseErr, it.BaseSize, it.Pin.AptEstimate)
		if it.BaseErr != nil {
			t.Errorf("%s: %v", it.Pin.Name(), it.BaseErr)
		}
	}
	if !plan.Items[0].BasePresent {
		t.Error("the Go base (goImage) is present, and the plan says not")
	}
	base, apt := plan.Download()
	t.Logf("download: %d bytes of bases, about %d through apt", base, apt)
}

// LocalImages' image ls format runs on the real client against every image present (no label filter, so it has rows
// to format): docker's image formatter has fewer fields than its container one. It only lists.
func TestRealImageListFormat(t *testing.T) {
	d, _ := realDocker(t)
	out, err := d.output(context.Background(), "image", "ls", "--no-trunc", "--format", imageListFormat)
	if err != nil {
		t.Fatalf("docker image ls --format %q: %v", imageListFormat, err)
	}
	found := false
	for _, line := range lines(out) {
		f := strings.Split(line, "\t")
		if len(f) != 2 || !imageID.MatchString(f[0]) {
			t.Errorf("a row of another shape: %q", line)
		}
		found = found || strings.HasPrefix(f[1], "golang:")
	}
	if !found {
		t.Errorf("the Go image is not listed:\n%s", out)
	}
}

// The whole life of a deps volume on a real daemon: made with its labels, seeded through a container that never
// starts (every folder owned by the grade's user), warmed in a container that may write it, read-only to a grade, never
// removed while a container uses it, and removed once idle; a created, never-started container is found and removed.
//
// AGENTIUM_TEST_IMAGE runs it with another image that is present already (by its digest reference, never pulled): the
// JDK base, eclipse-temurin@sha256:..., is Ubuntu with Rust coreutils, where the seed's script must work too.
func TestRealDepsVolume(t *testing.T) {
	d, img := realDocker(t)
	ctx := context.Background()
	if ref := os.Getenv("AGENTIUM_TEST_IMAGE"); ref != "" {
		var err error
		if img, err = d.Image(ctx, ref); err != nil {
			t.Skipf("AGENTIUM_TEST_IMAGE is not usable here (never pulled by tests): %v", err)
		}
	}
	data := testData(t, d)
	v := DepsVolume{Data: data, Project: "t1", Image: img.ID}
	t.Cleanup(func() { // before testData's check, which would find the volume: its containers first, then it
		ctx := context.Background()
		if left, err := d.Leftovers(ctx, data); err == nil {
			for _, l := range left {
				_ = d.RemoveRun(ctx, data, l.Run)
			}
		}
		if err := d.RemoveIdle(ctx, data, Item{Kind: "volume", Name: v.Name()}); err != nil {
			t.Errorf("the test's deps volume: %v", err)
		}
	})
	created, err := d.EnsureDepsVolume(ctx, v)
	must(t, err)
	if again, err := d.EnsureDepsVolume(ctx, v); err != nil || again != created || created == "" { // twice: the second finds it
		t.Fatalf("again: %q (first %q), %v", again, created, err)
	}
	src := t.TempDir()
	writeTree(t, src, map[string]string{"registry/index/config.json": "{}", "registry/cache/a.crate": "crate"})
	written, err := d.SeedDeps(ctx, v, img, []Seed{{Root: src, Path: ".", To: "cargo"}}, []SeedEntry{{Name: "py", Dir: true}}, SeedLimits())
	must(t, err)
	if !slices.Equal(written, []string{"cargo/registry/cache/a.crate", "cargo/registry/index/config.json"}) {
		t.Errorf("written %q", written)
	}
	// A second seed adds only what is new: what the volume holds is never rewritten.
	writeTree(t, src, map[string]string{"registry/cache/b.crate": "b"})
	again, err := d.SeedDeps(ctx, v, img, []Seed{{Root: src, Path: ".", To: "cargo", Skip: func(name string) bool { return slices.Contains(written, name) }}}, nil, SeedLimits())
	must(t, err)
	if !slices.Equal(again, []string{"cargo/registry/cache/b.crate"}) {
		t.Errorf("second seed wrote %q", again)
	}
	limits := DefaultLimits(d.Engine().NCPU)
	limits.Memory = 1 << 30
	var out bytes.Buffer
	err = d.Warm(ctx, WarmSpec{Volume: v, Image: img, Limits: limits, Deadline: 5 * time.Minute}, func(ctx context.Context, c *Container) error {
		tree := t.TempDir()
		writeTree(t, tree, map[string]string{"go.mod": "module m\n"})
		if _, err := c.CopyIn(ctx, tree, DefaultCopyLimits()); err != nil {
			return err
		}
		res, err := c.Exec(ctx, Command{Command: "id -u; stat -c '%u %a %n' /deps/py /deps/cargo /deps/cargo/registry; echo warmed > /deps/py/stamp; echo warm > /deps/cargo/registry/cache/c.crate; ls /sys/class/net", Timeout: time.Minute, Output: &out})
		if err == nil && res.ExitCode != 0 {
			err = errors.New("warm command failed: " + out.String())
		}
		return err
	})
	must(t, err)
	// A seed never replaces a file the volume holds, even one no record names (the warm-up wrote it), nor one a seed
	// wrote: the volume's own tar skips them.
	writeTree(t, src, map[string]string{"registry/cache/c.crate": "host", "registry/cache/a.crate": "changed"})
	if _, err := d.SeedDeps(ctx, v, img, []Seed{{Root: src, Path: ".", To: "cargo"}}, nil, SeedLimits()); err != nil {
		t.Fatal(err)
	}
	// A name that is a folder, or a link to one, already is never linked into: the name stays as it was (ln -T).
	linkEntries := []SeedEntry{{Name: "gradle-ro/modules-2", Dir: true}, {Name: "gradle/caches/modules-2", Link: "../../gradle-ro/modules-2"}}
	for range 2 {
		if _, err := d.SeedDeps(ctx, v, img, nil, append(linkEntries, SeedEntry{Name: "cargo/registry", Body: []byte("a file over a folder")}), SeedLimits()); err != nil {
			t.Fatal(err)
		}
	}
	// A seed container a dead Agentium left running on this volume is removed before a seed, so it can clear no
	// staging folder of the new one.
	stuck := "agentium-" + data + "-seed-stuck-seed"
	if _, err := d.output(ctx, "create", "--pull", "never", "--name", stuck, "--label", LabelData+"="+data, "--label", LabelRun+"=seed-stuck",
		"--label", LabelMode+"="+Mode, "--network", "none", "--user", User, "--mount", "type=volume,src="+v.Name()+",dst=/deps", "--entrypoint", "sleep", img.Ref, "600"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.output(ctx, "start", stuck); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeedDeps(ctx, v, img, []Seed{{Root: src, Path: ".", To: "cargo"}}, nil, SeedLimits()); err != nil {
		t.Fatal(err)
	}
	if _, stderr, res, err := d.call(ctx, []string{"container", "inspect", stuck}, nil, controlTimeout); err != nil || res.ExitCode == 0 || !noSuchContainer(stderr) {
		t.Errorf("the stuck seed container is still there: %v %s", err, stderr)
	}
	// A seed cut mid-file places nothing of that file, and the next seed delivers it whole and clears what the cut one
	// left in the staging folder.
	big := strings.Repeat("0123456789abcdef", 8<<10) // 128 KiB
	writeTree(t, src, map[string]string{"registry/cache/big.crate": big})
	d.hooks.seedStream = func(w io.Writer) io.Writer { return &cutWriter{w: w, left: 32 << 10} }
	if written, err := d.SeedDeps(ctx, v, img, []Seed{{Root: src, Path: ".", To: "cargo"}}, nil, SeedLimits()); err == nil || written != nil {
		t.Fatalf("a seed cut mid-file: %v, %v", written, err)
	}
	d.hooks.seedStream = nil
	var probe bytes.Buffer
	err = d.Warm(ctx, WarmSpec{Volume: v, Image: img, Limits: limits, Deadline: 5 * time.Minute}, func(ctx context.Context, c *Container) error {
		_, err := c.Exec(ctx, Command{Command: "test -e /deps/cargo/registry/cache/big.crate && echo PLACED; ls /deps/.seed | wc -l", Timeout: time.Minute, Output: &probe})
		return err
	})
	must(t, err)
	if got := probe.String(); strings.Contains(got, "PLACED") || strings.TrimSpace(got) != "1" {
		t.Errorf("after a cut seed: %q (want nothing placed, one staging folder left)", got)
	}
	if _, err := d.SeedDeps(ctx, v, img, []Seed{{Root: src, Path: ".", To: "cargo"}}, nil, SeedLimits()); err != nil {
		t.Fatal(err)
	}
	// WarmRun stops at the first command that fails, and says which.
	tree := t.TempDir()
	writeTree(t, tree, map[string]string{"sub/x": "x"})
	var warmOut bytes.Buffer
	passed, failed, err := d.WarmRun(ctx, WarmSpec{Volume: v, Image: img, Limits: limits, Deadline: 5 * time.Minute}, tree,
		[]WarmCommand{{Command: "test -f x && echo warm-ran >> /deps/py/log", Dir: "sub", Timeout: time.Minute}, {Command: "exit 3", Timeout: time.Minute},
			{Command: "echo never >> /deps/py/log", Timeout: time.Minute}}, &warmOut)
	if err != nil || passed != 1 || failed != "exit 3" {
		t.Errorf("WarmRun = %d, %q, %v: %s", passed, failed, err, warmOut.String())
	}
	for _, w := range []string{"65534\n", "65534 755 /deps/py", "65534 755 /deps/cargo\n", "eth0"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("warm-up output lacks %q:\n%s", w, out.String())
		}
	}
	// A grade reads the volume and cannot write it; the volume is in use meanwhile, and stays.
	spec := realSpec(data, "deps", img, d)
	spec.Deps = v.Name()
	out.Reset()
	err = d.Run(ctx, spec, func(ctx context.Context, c *Container) error {
		res, err := c.Exec(ctx, Command{Command: "cat /deps/py/stamp /deps/cargo/registry/cache/b.crate; echo; cat /deps/py/log /deps/cargo/registry/cache/c.crate /deps/cargo/registry/cache/a.crate; echo; stat -c '%u' /deps/cargo/registry/cache/c.crate; sha256sum < /deps/cargo/registry/cache/big.crate; ls -A /deps/.seed | wc -l; readlink /deps/gradle/caches/modules-2; test -e /deps/gradle-ro/modules-2/modules-2 && echo SELFLINK; test -d /deps/cargo/registry && test ! -e /deps/cargo/registry/registry && echo FOLDER-KEPT; if touch /deps/py/x 2>/dev/null; then echo WROTE; fi", Timeout: time.Minute, Output: &out})
		if err == nil && res.ExitCode != 0 {
			err = errors.New("grade command failed: " + out.String())
		}
		if err != nil {
			return err
		}
		items, err := d.Inventory(ctx, data)
		if err != nil {
			return err
		}
		for _, dec := range Decide(items, time.Now().Add(48*time.Hour), time.Hour, time.Hour, nil) {
			if dec.Remove && (dec.Name == v.Name() || dec.Name == Name(spec)) {
				t.Errorf("planned to remove %s, which a grade uses: %+v", dec.Name, dec)
			}
		}
		if err := d.RemoveIdle(ctx, data, Item{Kind: "volume", Name: v.Name()}); !errors.Is(err, ErrInUse) {
			t.Errorf("removing the volume a grade mounts: %v", err)
		}
		if err := d.RemoveIdle(ctx, data, Item{Kind: "container", Name: Name(spec)}); !errors.Is(err, ErrInUse) {
			t.Errorf("removing a running grade: %v", err)
		}
		return nil
	})
	must(t, err)
	sum := sha256.Sum256([]byte(big))
	if got := out.String(); !strings.Contains(got, "warmed\nb\nwarm-ran\nwarm\ncrate\n65534\n"+hex.EncodeToString(sum[:])+"  -\n0\n../../gradle-ro/modules-2\nFOLDER-KEPT\n") || strings.Contains(got, "SELFLINK") || strings.Contains(got, "WROTE") || strings.Contains(got, "never") {
		t.Errorf("grade output: %q", got)
	}
	// A seed's container left behind (Agentium died between create and removal): created, never started.
	name := "agentium-" + data + "-seed-dead-seed"
	if _, err := d.output(ctx, "create", "--pull", "never", "--name", name, "--label", LabelData+"="+data, "--label", LabelRun+"=seed-dead",
		"--label", LabelMode+"="+Mode, "--network", "none", "--mount", "type=volume,src="+v.Name()+",dst=/deps", "--entrypoint", "true", img.Ref); err != nil {
		t.Fatal(err)
	}
	items, err := d.Inventory(ctx, data)
	must(t, err)
	decisions := map[string]Decision{}
	for _, dec := range Decide(items, time.Now(), time.Hour, time.Hour, nil) {
		decisions[dec.Name] = dec
	}
	if dec := decisions[name]; dec.Remove || dec.State != "created" {
		t.Errorf("a container made just now: %+v", dec)
	}
	if dec := decisions[v.Name()]; dec.Remove || dec.Users != 1 {
		t.Errorf("the volume the leftover mounts: %+v", dec)
	}
	later := map[string]Decision{}
	for _, dec := range Decide(items, time.Now().Add(2*time.Hour), time.Hour, time.Hour, nil) {
		later[dec.Name] = dec
	}
	if !later[name].Remove {
		t.Errorf("the leftover, later: %+v", later[name])
	}
	must(t, d.RemoveIdle(ctx, data, later[name].Item))
	must(t, d.RemoveIdle(ctx, data, Item{Kind: "volume", Name: v.Name()}))
	items, err = d.Inventory(ctx, data)
	must(t, err)
	if len(items) != 0 {
		t.Errorf("left: %+v", items)
	}
}

// A real build downloads apt's lists and two packages, so it runs only with AGENTIUM_TEST_ALLOW_BUILD=1 (the user's
// consent); it builds the Go pin's grading image from the present base, checks it, and removes it.
func TestRealBuild(t *testing.T) {
	if os.Getenv("AGENTIUM_TEST_ALLOW_BUILD") != "1" {
		t.Skip("a build downloads through apt: set AGENTIUM_TEST_ALLOW_BUILD=1 to allow it")
	}
	d, _ := realDocker(t)
	ctx := context.Background()
	data := testData(t, d)
	p, _ := PinFor(ToolchainGo)
	plan, err := d.Plan(ctx, []Pin{p}, BuiltImages{})
	must(t, err)
	if !plan.Items[0].BasePresent {
		t.Skip("the Go base is not present, and this test never pulls")
	}
	built, err := d.Fetch(ctx, plan.Items[0], data, io.Discard)
	must(t, err)
	t.Cleanup(func() { _ = d.RemoveImage(context.Background(), built.Tag) })
	t.Logf("built %+v", built)
	if _, _, err := d.GradingImage(ctx, plan.Items[0].Recipe, BuiltImages{built.Tag: built}); err != nil {
		t.Error(err)
	}
}
