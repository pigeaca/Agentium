package run

import (
	"context"
	"errors"
	"fmt"
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

// UF_IMMUTABLE and UF_APPEND (chflags uchg, uappnd), and UF_COMPRESSED.
const (
	ufImmutable  = 0x2
	ufAppend     = 0x4
	ufCompressed = 0x20
)

// outsideTargets are the user's files a hostile grade aims at through links: a writable folder (0o700) holding a file,
// a read-only subfolder (0o500) with a file and, on macOS, a file with the user's immutable flag; and a file of its own
// (immutable on macOS). A removal that followed a link into the folder could delete, chmod or unflag all of them, so
// check reports any change.
type outsideTargets struct {
	dir, file string
}

func newOutsideTargets(t *testing.T, parent string) outsideTargets {
	t.Helper()
	o := outsideTargets{dir: filepath.Join(parent, "outside", "dir"), file: filepath.Join(parent, "outside", "file")}
	must(t, os.MkdirAll(filepath.Join(o.dir, "sub"), 0o700))
	must(t, os.WriteFile(filepath.Join(o.dir, "keep"), []byte("the user's"), 0o600))
	must(t, os.WriteFile(filepath.Join(o.dir, "sub", "keep"), []byte("the user's"), 0o600))
	must(t, os.WriteFile(filepath.Join(o.dir, "immutable"), []byte("the user's"), 0o600))
	must(t, os.WriteFile(o.file, []byte("the user's"), 0o600))
	must(t, os.Chmod(filepath.Join(o.dir, "sub"), 0o500))
	if runtime.GOOS == "darwin" {
		must(t, setFlags(o.file, ufImmutable))
		must(t, setFlags(filepath.Join(o.dir, "immutable"), ufImmutable))
	}
	t.Cleanup(func() {
		if runtime.GOOS == "darwin" {
			setFlags(o.file, 0)
			setFlags(filepath.Join(o.dir, "immutable"), 0)
		}
		os.Chmod(filepath.Join(o.dir, "sub"), 0o700)
	})
	return o
}

func (o outsideTargets) check(t *testing.T) {
	t.Helper()
	for path, mode := range map[string]os.FileMode{o.dir: 0o700, filepath.Join(o.dir, "sub"): 0o500} {
		if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != mode {
			t.Errorf("the user's folder %s changed: %v", path, modeOf(info, err))
		}
	}
	for _, name := range []string{filepath.Join(o.dir, "keep"), filepath.Join(o.dir, "sub", "keep"), filepath.Join(o.dir, "immutable"), o.file} {
		if data, err := os.ReadFile(name); err != nil || string(data) != "the user's" {
			t.Errorf("the user's file %s changed: %q, %v", name, data, err)
		}
	}
	for _, name := range []string{filepath.Join(o.dir, "immutable"), o.file} {
		info, err := os.Lstat(name)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the user's file %s changed: %v", name, modeOf(info, err))
			continue
		}
		if runtime.GOOS == "darwin" && fileFlags(info)&ufImmutable == 0 {
			t.Errorf("the user's file %s lost its immutable flag", name)
		}
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
	if !removeWalks {
		t.Skip("the removal walks folders (and calls the hook) only with cgo")
	}
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

// aclDeny adds an access list entry to path (chmod +a), macOS only.
func aclDeny(t *testing.T, rights, path string) {
	t.Helper()
	if out, err := exec.Command("/bin/chmod", "+a", "everyone deny "+rights, path).CombinedOutput(); err != nil {
		t.Fatalf("chmod +a %s %s: %v %s", rights, path, err, out)
	}
}

// resistByACL makes dir and a file in it refuse deletion by access lists, as a grade can (the sandbox allows it): the
// folder denies deletion of itself and of its entries, the file of itself.
func resistByACL(t *testing.T, dir string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, "stuck"), nil, 0o600))
	aclDeny(t, "delete", filepath.Join(dir, "stuck"))
	aclDeny(t, "delete,delete_child,writesecurity", dir)
}

// F-C: access lists a grade sets (on its copy itself, its entries, its cache) are cleared by the removal: the grade's
// folder goes, after withGrading and in recovery, with no quarantine and no warning. So does a host grade's copy with one.
func TestAccessListsAreCleared(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS access lists")
	}
	f := newGradeFixture(t)
	t.Cleanup(func() { exec.Command("/bin/chmod", "-RN", f.dir).Run() })
	q := quarantine(f.env.Layout)
	var warnings []string
	in := f.input(f.root, "", "go")
	in.Quarantine, in.Warn = q, func(w string) { warnings = append(warnings, w) }
	if err := withGrading(context.Background(), in, func(g grading) error {
		resistByACL(t, g.Copy)
		resistByACL(t, g.Cache)
		// A folder whose list denies readattr and readsecurity cannot even be looked at until the list is gone.
		blind := filepath.Join(g.Copy, "blind")
		must(t, os.MkdirAll(filepath.Join(blind, "sub"), 0o700))
		for _, right := range []string{"readattr", "readsecurity"} {
			if out, err := exec.Command("/bin/chmod", "+a", "everyone deny "+right, blind).CombinedOutput(); err != nil {
				t.Fatalf("chmod +a %s: %v %s", right, err, out)
			}
		}
		must(t, os.Symlink("/nonexistent", filepath.Join(g.Temp, "link")))
		if out, err := exec.Command("/bin/chmod", "-h", "+a", "everyone deny delete", filepath.Join(g.Temp, "link")).CombinedOutput(); err != nil {
			t.Fatalf("chmod -h +a: %v %s", err, out)
		}
		// Without the clearing, the copy itself could not even be moved aside.
		if err := os.Rename(g.Copy, g.Copy+"-moved"); err == nil {
			t.Error("a copy that denies deletion was renamed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.root); err == nil {
		t.Error("the grade's folder is still there")
	}
	if len(warnings) > 0 {
		t.Errorf("warnings = %q", warnings)
	}
	if entries, _ := os.ReadDir(q); len(entries) != 0 {
		t.Errorf("quarantine = %v", entries)
	}

	// Recovery of a dead run whose grade folder and host-mode copy deny deletion.
	dir := filepath.Join(f.env.Layout.Records, "r9")
	for _, sub := range []string{filepath.Join(gradingFolder, "copy"), "verify"} {
		must(t, os.MkdirAll(filepath.Join(dir, sub), 0o700))
		resistByACL(t, filepath.Join(dir, sub))
	}
	aclDeny(t, "delete,delete_child", filepath.Join(dir, gradingFolder))
	must(t, (Env{}).writeStart(start{Record: Record{ID: "r9", RecordsDir: dir}, Workspace: filepath.Join(f.env.Layout.Workspaces, "r9"), AgentStarted: true}))
	orphans, err := RecoverWarn(context.Background(), f.env.Layout, func(string) (bool, error) { return false, nil }, "", time.Now(),
		func(w string) { warnings = append(warnings, w) })
	if err != nil || len(orphans) != 1 || len(warnings) > 0 {
		t.Fatalf("RecoverWarn = %v, %v, warnings %q", orphans, err, warnings)
	}
	for _, gone := range []string{filepath.Join(dir, gradingFolder), filepath.Join(dir, "verify")} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
}

// F2 and F-C: a grade folder that still resists removal is moved into the quarantine whole, with a warning naming its
// run, never an error: the copy nested in it moves with it even when the copy itself denies being moved. A later
// recovery removes it.
func TestResistingGradeIsQuarantined(t *testing.T) {
	f := newGradeFixture(t)
	t.Cleanup(func() { exec.Command("/bin/chmod", "-RN", f.dir).Run() })
	q := quarantine(f.env.Layout)
	g, err := prepareGrading(context.Background(), f.input(f.root, "", "go"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		resistByACL(t, g.Copy)
	}
	fails := func(string) error { return errors.New("it resists") }
	warning, err := quarantineAfter(f.root, q, fails)
	if err != nil || !strings.Contains(warning, "it resists") || !strings.Contains(warning, filepath.Join(q, "r1-"+gradingFolder+"-")) {
		t.Fatalf("quarantineAfter = %q, %v", warning, err)
	}
	if _, err := os.Lstat(f.root); err == nil {
		t.Error("the grade's folder is still in the records")
	}
	entries, _ := os.ReadDir(q)
	if len(entries) != 1 {
		t.Fatalf("quarantine = %v", entries)
	}
	if _, err := os.Lstat(filepath.Join(q, entries[0].Name(), "copy", "main.go")); err != nil {
		t.Errorf("the copy did not move with its folder: %v", err)
	}
	// Neither removed nor movable (no quarantine folder): an error, which callers turn into a warning.
	other := filepath.Join(f.env.Layout.Records, "r2", gradingFolder)
	must(t, os.MkdirAll(other, 0o700))
	if _, err := quarantineAfter(other, "", fails); err == nil {
		t.Error("no quarantine and no removal: no error")
	}
	// A later recovery empties the quarantine: the removal clears the access lists.
	if w := emptyQuarantine(q); w != "" {
		t.Errorf("emptyQuarantine = %q", w)
	}
	if entries, _ := os.ReadDir(q); len(entries) != 0 {
		t.Errorf("quarantine after emptying = %v", entries)
	}
}

// F-D: however many entries resist, the error (and the warning made of it) stays one bounded line: the first error
// and a count.
func TestRemovalErrorsAreBounded(t *testing.T) {
	if !removeWalks {
		t.Skip("the removal walks folders only with cgo")
	}
	f := newGradeFixture(t)
	cache := filepath.Join(f.root, "cache")
	must(t, os.MkdirAll(cache, 0o700))
	for i := 0; i < 300; i++ {
		must(t, os.WriteFile(filepath.Join(cache, fmt.Sprintf("f%03d", i)), nil, 0o600))
	}
	must(t, os.Chmod(cache, 0o500)) // os.RemoveAll fails; the walk runs
	// Each file becomes a folder with an entry once looked at: unlinking it as a file fails.
	remove := func(root string) error {
		return removeTreeRacing(root, func(name string) {
			if strings.HasPrefix(name, "f") {
				p := filepath.Join(cache, name)
				os.Remove(p)
				os.MkdirAll(filepath.Join(p, "x"), 0o700)
			}
		})
	}
	err := remove(f.root)
	if err == nil || !strings.Contains(err.Error(), "(and 299 more)") || len(err.Error()) > 1000 {
		t.Fatalf("error = %v", err)
	}
	warning, qErr := quarantineAfter(f.root, quarantine(f.env.Layout), func(string) error { return err })
	if qErr != nil || len(warning) > 1500 || !strings.Contains(warning, "more)") {
		t.Errorf("warning (%d bytes) = %q, %v", len(warning), warning, qErr)
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
	var warnings []string
	in := f.input(f.root, "", "go")
	in.Warn = func(w string) { warnings = append(warnings, w) }
	if err := withGrading(context.Background(), in, func(g grading) error {
		_, inCopy = leftover(t, g.Copy, "")
		_, holding = leftover(t, "/", filepath.Join(g.Cache, "held"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	endsSoon(t, "a process working in the copy", inCopy)
	endsSoon(t, "a process holding a file in the cache", holding)
	// Each killed process is reported (F-E), by ID and command, quoted (the grade picks the name).
	if n := strings.Count(strings.Join(warnings, "\n"), "it was stopped: "); n != 2 || !strings.Contains(strings.Join(warnings, "\n"), ` "sleep"`) {
		t.Errorf("warnings = %q", warnings)
	}

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

// F-F: the removal clears only the flags that block it (uchg, uappnd). A grade may hard-link one of the user's
// compressed files into its folder and flag it: clearing every flag would clear UF_COMPRESSED too and leave the user's
// file without its data.
func TestRemoveTreeKeepsOtherFlags(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS file flags and compression")
	}
	f := newGradeFixture(t)
	plain := filepath.Join(f.dir, "plain")
	must(t, os.WriteFile(plain, []byte(strings.Repeat("a", 200000)), 0o600))
	users := filepath.Join(f.dir, "users-compressed")
	if out, err := exec.Command("/usr/bin/ditto", "--hfsCompression", plain, users).CombinedOutput(); err != nil {
		t.Skipf("ditto --hfsCompression: %v %s", err, out)
	}
	info, err := os.Lstat(users)
	if err != nil || fileFlags(info)&ufCompressed == 0 {
		t.Skip("this file system did not compress the file")
	}
	cache := filepath.Join(f.root, "cache")
	must(t, os.MkdirAll(cache, 0o700))
	must(t, os.Link(users, filepath.Join(cache, "hard")))
	must(t, setFlags(filepath.Join(cache, "hard"), int(fileFlags(info))|ufImmutable))
	t.Cleanup(func() { setFlags(users, 0) })
	if err := removeTree(f.root); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(users)
	if err != nil || fileFlags(info)&ufCompressed == 0 {
		t.Errorf("the user's file lost UF_COMPRESSED: %v", err)
	}
	if data, err := os.ReadFile(users); err != nil || string(data) != strings.Repeat("a", 200000) {
		t.Errorf("the user's compressed file lost its data (%d bytes, %v)", len(data), err)
	}
}
