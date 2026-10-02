package run

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/sandbox"
)

// Under the real grading sandbox, with the grade's own environment and profile: the grade builds into its cache clone
// and its temp root, and cannot read, write or plant anything in the seed (directly, through a link it makes in its
// cache, or by replacing its cache with a link), nor rewrite its profile file. The seed is as it was, and the next grade
// starts from it. The canary passes on the profile as written.
func TestSandboxedGradeCannotPoisonTheSeed(t *testing.T) {
	if testing.Short() {
		t.Skip("starts sandboxed processes")
	}
	if err := exec.Command(sandbox.Exec, "-p", "(version 1)(allow default)", "/usr/bin/true").Run(); err != nil {
		t.Skipf("sandbox-exec cannot apply a profile here (nested in a sandbox?): %v", err)
	}
	f := newGradeFixture(t)
	homeDir := filepath.Join(f.dir, "home")
	must(t, os.MkdirAll(homeDir, 0o700))
	if err := prepareSeed(context.Background(), f.golang, f.deps, f.seed, func(_ context.Context, dir string) error {
		return os.WriteFile(filepath.Join(dir, "entry"), []byte("trusted"), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	seedBefore := tree(t, f.seed)
	g, err := prepareGrading(context.Background(), f.input(f.root, f.seed, "go"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.remove()
	tag, err := sandbox.NewTag()
	if err != nil {
		t.Fatal(err)
	}
	profile, file, digest, err := g.writeProfile(sandbox.Profile{Tag: tag, Home: homeDir, Environ: g.Environ, Data: f.env.Layout.Root})
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Canary(context.Background(), file, digest, profile); err != nil {
		t.Fatalf("the canary: %v", err)
	}
	run := func(command string) int {
		t.Helper()
		var out bytes.Buffer
		spec, err := sandbox.Wrap(runner.Spec{Dir: g.Copy, Command: command, Environ: g.Environ, Timeout: time.Minute, Output: &out}, file)
		if err != nil {
			t.Fatal(err)
		}
		result, err := runner.Run(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: exit %d %s", command, result.ExitCode, out.String())
		return result.ExitCode
	}
	// The grade's own work.
	if code := run(`echo built > "$GOCACHE/new" && echo poisoned > "$GOCACHE/entry" && echo t > "$TMPDIR/t" && echo x > out.txt && cat "$GOCACHE/new"`); code != 0 {
		t.Fatalf("the grade could not write its own folders: exit %d", code)
	}
	seed := f.seed
	for _, command := range []string{
		`cat '` + seed + `/entry'`,              // read the seed
		`echo poisoned >> '` + seed + `/entry'`, // append to it
		`echo poisoned > '` + seed + `/entry'`,  // rewrite it
		`touch '` + seed + `/planted'`,          // plant beside it
		`rm -f '` + seed + `/entry'`,            // remove it
		`ln -s '` + seed + `' "$GOCACHE/to-seed" && echo poisoned > "$GOCACHE/to-seed/entry"`,         // through a link of its own
		`ln '` + seed + `/entry' "$GOCACHE/hard"`,                                                     // a hard link to the seed's file
		`mv "$GOCACHE" "$TMPDIR/moved" ; ln -s '` + seed + `' "$GOCACHE" && touch "$GOCACHE/planted"`, // its cache made a link
		`echo '(allow default)' >> '` + file + `'`,                                                    // its profile
		`touch '` + filepath.Join(filepath.Dir(seed), "next-seed") + `'`,
	} {
		if run(command) == 0 {
			t.Errorf("allowed: %s", command)
		}
	}
	if err := sandbox.CheckFile(file, digest); err != nil {
		t.Errorf("the profile file changed: %v", err)
	}
	if got := tree(t, f.seed); len(got) != len(seedBefore) || got["entry"] != "trusted" {
		t.Errorf("the seed after a hostile grade: %v", got)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(seed), "next-seed")); err == nil {
		t.Error("the grade planted a seed")
	}
	if err := g.remove(); err != nil {
		t.Fatal(err)
	}
	next, err := prepareGrading(context.Background(), f.input(f.root, f.seed, "go"))
	if err != nil {
		t.Fatal(err)
	}
	defer next.remove()
	if got := tree(t, next.Cache); got["entry"] != "trusted" || len(got) != len(seedBefore) {
		t.Errorf("the next grade's cache: %v", got)
	}
}
