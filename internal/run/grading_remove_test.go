package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// UF_IMMUTABLE and UF_APPEND (chflags uchg, uappnd).
const (
	ufImmutable = 0x2
	ufAppend    = 0x4
)

// outsideTargets are the user's files a hostile grade aims at through links: a folder (mode 0o500) and, on macOS, a
// file with the user's immutable flag. check reports any change to them.
type outsideTargets struct {
	dir, file string
}

func newOutsideTargets(t *testing.T, parent string) outsideTargets {
	t.Helper()
	o := outsideTargets{dir: filepath.Join(parent, "outside", "dir"), file: filepath.Join(parent, "outside", "file")}
	must(t, os.MkdirAll(o.dir, 0o700))
	must(t, os.WriteFile(filepath.Join(o.dir, "keep"), []byte("the user's"), 0o600))
	must(t, os.WriteFile(o.file, []byte("the user's"), 0o600))
	must(t, os.Chmod(o.dir, 0o500))
	if runtime.GOOS == "darwin" {
		must(t, setFlags(o.file, ufImmutable))
	}
	t.Cleanup(func() {
		if runtime.GOOS == "darwin" {
			setFlags(o.file, 0)
		}
		os.Chmod(o.dir, 0o700)
	})
	return o
}

func (o outsideTargets) check(t *testing.T) {
	t.Helper()
	if info, err := os.Lstat(o.dir); err != nil || info.Mode().Perm() != 0o500 {
		t.Errorf("the user's folder changed: %v", modeOf(info, err))
	}
	if data, err := os.ReadFile(filepath.Join(o.dir, "keep")); err != nil || string(data) != "the user's" {
		t.Errorf("the user's folder lost its file: %q, %v", data, err)
	}
	info, err := os.Lstat(o.file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the user's file changed: %v", modeOf(info, err))
		return
	}
	if runtime.GOOS == "darwin" && fileFlags(info)&ufImmutable == 0 {
		t.Error("the user's file lost its immutable flag")
	}
}

func modeOf(info os.FileInfo, err error) any {
	if err != nil {
		return err
	}
	return info.Mode()
}

// lchflags sets a link's own flags (chflags -h): the syscall package has no lchflags.
func lchflags(t *testing.T, flags, path string) {
	t.Helper()
	if out, err := exec.Command("/usr/bin/chflags", "-h", flags, path).CombinedOutput(); err != nil {
		t.Fatalf("chflags -h %s %s: %v %s", flags, path, err, out)
	}
}

// F1 and F3 of the step 2 review: what a grade leaves to resist removal (folders without permissions, flags on files,
// folders and links) is cleared on the grade's own entries only. Links into the user's files, from folders that resist
// removal, are removed as links: their targets keep their mode, flags and contents.
func TestRemoveTreeNeverReachesThroughALink(t *testing.T) {
	f := newGradeFixture(t)
	o := newOutsideTargets(t, f.dir)
	cache := filepath.Join(f.root, "cache")
	locked := filepath.Join(cache, "locked")
	must(t, os.MkdirAll(filepath.Join(locked, "deeper"), 0o700))
	for _, dir := range []string{cache, locked, filepath.Join(locked, "deeper")} {
		must(t, os.Symlink(o.dir, filepath.Join(dir, "to-dir")))
		must(t, os.Symlink(o.file, filepath.Join(dir, "to-file")))
	}
	must(t, os.Chmod(filepath.Join(locked, "deeper"), 0o377))
	if runtime.GOOS == "darwin" {
		lchflags(t, "uchg", filepath.Join(locked, "to-file")) // a link with the flag, to a file with it
		lchflags(t, "uchg", filepath.Join(locked, "deeper", "to-dir"))
		must(t, os.WriteFile(filepath.Join(locked, "immutable"), nil, 0o600))
		must(t, setFlags(filepath.Join(locked, "immutable"), ufImmutable))
		must(t, setFlags(filepath.Join(locked, "deeper"), ufAppend))
	}
	must(t, os.Chmod(locked, 0))
	if err := removeTree(f.root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.root); err == nil {
		t.Error("the grade's folder is still there")
	}
	o.check(t)
}

// F2: a link with the immutable flag (ln -s /nonexistent imm && chflags -h uchg imm), alone or in a folder with the
// append-only flag, is removed; so is a tree deeper than PATH_MAX with a folder without permissions at the bottom.
func TestRemoveTreeLinkFlagsAndDepth(t *testing.T) {
	f := newGradeFixture(t)
	cache := filepath.Join(f.root, "cache")
	must(t, os.MkdirAll(filepath.Join(cache, "appendonly"), 0o700))
	if runtime.GOOS == "darwin" {
		for _, link := range []string{filepath.Join(cache, "imm"), filepath.Join(cache, "appendonly", "imm")} {
			must(t, os.Symlink("/nonexistent", link))
			lchflags(t, "uchg", link)
		}
		must(t, setFlags(filepath.Join(cache, "appendonly"), ufAppend))
	}
	root, err := os.OpenRoot(cache)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for i := 0; i < 24; i++ {
		parts = append(parts, strings.Repeat(string(rune('a'+i%26)), 120))
	}
	deep := filepath.Join(parts...) // 24 × 121 bytes: past PATH_MAX (1024)
	must(t, root.MkdirAll(deep, 0o700))
	must(t, root.WriteFile(filepath.Join(deep, "f"), nil, 0o600))
	must(t, root.Chmod(deep, 0))
	must(t, root.Chmod(filepath.Join(parts[:12]...), 0o300))
	root.Close()
	if err := removeTree(f.root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.root); err == nil {
		t.Error("the grade's folder is still there")
	}
}

// F1's race: a grade process swaps a folder that resists removal for a link to the user's folder, again and again,
// while the removal runs. Whatever the timing, the user's folder keeps its mode.
func TestRemoveTreeSwapRace(t *testing.T) {
	f := newGradeFixture(t)
	o := newOutsideTargets(t, f.dir)
	for i := 0; i < 40; i++ {
		cache := filepath.Join(f.root, "cache")
		d := filepath.Join(cache, "d")
		must(t, os.MkdirAll(filepath.Join(d, "inner"), 0o700))
		must(t, os.WriteFile(filepath.Join(d, "inner", "f"), nil, 0o600))
		must(t, os.Chmod(filepath.Join(d, "inner"), 0o377))
		must(t, os.Chmod(d, 0o377))
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if os.Rename(d, d+".real") == nil {
					os.Symlink(o.dir, d)
					runtime.Gosched()
					os.Remove(d)
					os.Rename(d+".real", d)
				}
				inner := filepath.Join(d, "inner")
				if os.Rename(inner, inner+".real") == nil {
					os.Symlink(o.dir, inner)
					runtime.Gosched()
					os.Remove(inner)
					os.Rename(inner+".real", inner)
				}
			}
		})
		time.Sleep(time.Millisecond)
		removeTree(f.root) // may fail mid-race; the folder is removed below
		close(stop)
		wg.Wait()
		if err := removeTree(f.root); err != nil {
			t.Fatal(err)
		}
		o.check(t)
		if t.Failed() {
			t.Fatalf("iteration %d", i)
		}
	}
}

// F1 at the exact moment: right after the removal looked at an entry (a folder that resists removal, carrying flags),
// the grade swaps it for a link to the user's folder or file. What the removal then changes is the link, never what it
// points to.
func TestRemoveTreeSwapAfterTheLook(t *testing.T) {
	f := newGradeFixture(t)
	o := newOutsideTargets(t, f.dir)
	for _, target := range []string{o.dir, o.file} {
		cache := filepath.Join(f.root, "cache")
		d := filepath.Join(cache, "d")
		must(t, os.MkdirAll(d, 0o700))
		must(t, os.WriteFile(filepath.Join(d, "f"), nil, 0o600))
		must(t, os.Chmod(d, 0o377))
		if runtime.GOOS == "darwin" {
			must(t, setFlags(filepath.Join(d, "f"), ufImmutable))
		}
		swapped := false
		err := removeTreeRacing(f.root, func(name string) {
			if name == "d" && !swapped {
				swapped = true
				must(t, os.Rename(d, filepath.Join(cache, "moved")))
				must(t, os.Symlink(target, d))
			}
		})
		if !swapped {
			t.Fatal("the swap never happened")
		}
		o.check(t)
		if err == nil {
			if _, statErr := os.Lstat(f.root); statErr == nil {
				t.Error("removal reported success and left the folder")
			}
		}
		if err := removeTree(f.root); err != nil { // what the swap moved away
			t.Fatal(err)
		}
		o.check(t)
	}
}

// F2: a folder a grade made unremovable (an access list denying deletion) is moved into the quarantine with a warning,
// never an error: withGrading and recovery go on, and a later recovery removes it once it can be removed.
func TestUnremovableGradeIsQuarantined(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS access lists")
	}
	f := newGradeFixture(t)
	acl := func(t *testing.T, args ...string) {
		t.Helper()
		if out, err := exec.Command("/bin/chmod", args...).CombinedOutput(); err != nil {
			t.Fatalf("chmod %v: %v %s", args, err, out)
		}
	}
	deny := func(dir string) {
		must(t, os.WriteFile(filepath.Join(dir, "stuck"), nil, 0o600))
		acl(t, "+a", "everyone deny delete", filepath.Join(dir, "stuck"))
		acl(t, "+a", "everyone deny delete_child", dir)
	}
	q := quarantine(f.env.Layout)
	t.Cleanup(func() { exec.Command("/bin/chmod", "-RN", f.dir).Run() })
	var warnings []string
	in := f.input(f.root, "", "go")
	in.Quarantine, in.Warn = q, func(w string) { warnings = append(warnings, w) }
	if err := withGrading(context.Background(), in, func(g grading) error { deny(g.Cache); return nil }); err != nil {
		t.Fatalf("withGrading: %v", err)
	}
	if _, err := os.Lstat(f.root); err == nil {
		t.Error("the grade's folder is still in the records")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], q) {
		t.Errorf("warnings = %q", warnings)
	}
	if entries, _ := os.ReadDir(q); len(entries) != 1 {
		t.Errorf("quarantine = %v", entries)
	}

	// Recovery: a dead run's grade folder that cannot be removed does not stop it.
	dir := filepath.Join(f.env.Layout.Records, "r9")
	must(t, os.MkdirAll(filepath.Join(dir, gradingFolder, "cache"), 0o700))
	deny(filepath.Join(dir, gradingFolder, "cache"))
	must(t, (Env{}).writeStart(start{Record: Record{ID: "r9", RecordsDir: dir}, Workspace: filepath.Join(f.env.Layout.Workspaces, "r9"), AgentStarted: true}))
	warnings = nil
	orphans, err := RecoverWarn(context.Background(), f.env.Layout, func(string) (bool, error) { return false, nil }, "", time.Now(),
		func(w string) { warnings = append(warnings, w) })
	if err != nil || len(orphans) != 1 {
		t.Fatalf("RecoverWarn = %v, %v", orphans, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, gradingFolder)); err == nil {
		t.Error("recovery left the grade's folder in the records")
	}
	// The quarantine's first folder still resists; its second (recovery's) is now there too.
	if !strings.Contains(strings.Join(warnings, "\n"), "could not be removed") {
		t.Errorf("warnings = %q", warnings)
	}
	// Once the access lists are gone, the next recovery empties the quarantine without a word.
	acl(t, "-RN", q)
	warnings = nil
	if _, err := RecoverWarn(context.Background(), f.env.Layout, func(string) (bool, error) { return true, nil }, "", time.Now(),
		func(w string) { warnings = append(warnings, w) }); err != nil || len(warnings) != 0 {
		t.Errorf("RecoverWarn = %v, warnings %q", err, warnings)
	}
	if entries, _ := os.ReadDir(q); len(entries) != 0 {
		t.Errorf("quarantine after recovery = %v", entries)
	}
}

// leftover starts a process a grade could leave behind: in its own session (setsid, out of the process group runner
// kills), with dir as its working folder and, when held is set, that file open. It returns a channel that receives
// once the process has ended.
func leftover(t *testing.T, dir, held string) (pid int, ended <-chan struct{}) {
	t.Helper()
	script := "exec sleep 300"
	if held != "" {
		script = `exec 3>>"$1"; cd /; exec sleep 300`
	}
	cmd := exec.Command("/bin/sh", "-c", script, "sh", held)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	must(t, cmd.Start())
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { cmd.Process.Kill() })
	time.Sleep(100 * time.Millisecond) // let the shell reach sleep
	return cmd.Process.Pid, done
}

func endsSoon(t *testing.T, what string, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Errorf("%s outlived the grade", what)
	}
}

// F5: what a grade leaves running in its own session (a server in its copy, a daemon holding a file in its cache) is
// stopped before the grade's folders go, by withGrading and by recovery; and a seed's warm step leaves nothing running
// into the published seed.
func TestLeftoverProcessesAreStopped(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the process sweep is macOS's (libproc)")
	}
	f := newGradeFixture(t)
	var inCopy, holding <-chan struct{}
	if err := withGrading(context.Background(), f.input(f.root, "", "go"), func(g grading) error {
		_, inCopy = leftover(t, g.Copy, "")
		_, holding = leftover(t, "/", filepath.Join(g.Cache, "held"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	endsSoon(t, "a process working in the copy", inCopy)
	endsSoon(t, "a process holding a file in the cache", holding)

	// Recovery, for a run whose Agentium died while its grade ran.
	dir := filepath.Join(f.env.Layout.Records, "r8")
	must(t, os.MkdirAll(filepath.Join(dir, gradingFolder, "cache"), 0o700))
	must(t, os.MkdirAll(filepath.Join(dir, "verify"), 0o700))
	must(t, (Env{}).writeStart(start{Record: Record{ID: "r8", RecordsDir: dir}, Workspace: filepath.Join(f.env.Layout.Workspaces, "r8"), AgentStarted: true}))
	_, inVerify := leftover(t, filepath.Join(dir, "verify"), "")
	_, inCache := leftover(t, filepath.Join(dir, gradingFolder, "cache"), "")
	if _, err := Recover(context.Background(), f.env.Layout, func(string) (bool, error) { return false, nil }, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	endsSoon(t, "a dead run's grade process in its copy", inVerify)
	endsSoon(t, "a dead run's grade process in its cache", inCache)

	// A warm step's process does not live on into the published seed.
	var warmLeft <-chan struct{}
	if err := prepareSeed(context.Background(), f.golang, f.deps, f.seed, func(_ context.Context, dir string) error {
		_, warmLeft = leftover(t, dir, "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	endsSoon(t, "a warm step's process", warmLeft)
}
