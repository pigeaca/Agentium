package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestResolve(t *testing.T) {
	override := t.TempDir()
	layout, err := Resolve(env(map[string]string{"AGENTIUM_HOME": override, "HOME": "/ignored"}))
	if err != nil {
		t.Fatal(err)
	}
	if layout.Root != override || layout.Database != filepath.Join(override, "agentium.db") || layout.Artifacts != filepath.Join(override, "artifacts") {
		t.Errorf("override layout = %+v", layout)
	}
	layout, err = Resolve(env(map[string]string{"HOME": "/Users/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	if layout.Root != "/Users/someone/.agentium" {
		t.Errorf("default root = %q", layout.Root)
	}
	// Agents read the deps folder, so it is none of the folders they are denied (cache, projects, records, artifacts).
	if layout.Deps != "/Users/someone/.agentium/deps" {
		t.Errorf("deps folder = %q", layout.Deps)
	}
	if got := layout.ProjectRepo(7); got != "/Users/someone/.agentium/projects/7/repo.git" {
		t.Errorf("project repository = %q", got)
	}
	if _, err := Resolve(env(nil)); err == nil {
		t.Error("expected an error without AGENTIUM_HOME or HOME")
	}
}

func TestEnsureIsOwnerOnly(t *testing.T) {
	layout, err := Resolve(env(map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data")}))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{layout.Root, layout.Artifacts} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want 0700", dir, info.Mode().Perm())
		}
	}
	if err := layout.Ensure(); err != nil {
		t.Errorf("Ensure is not idempotent: %v", err)
	}
}

func TestEnsureNeverChangesAnExistingFolder(t *testing.T) {
	open := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	layout, err := Resolve(env(map[string]string{"AGENTIUM_HOME": open}))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("an existing folder open to others: err = %v", err)
	}
	if info, _ := os.Stat(open); info.Mode().Perm() != 0o755 {
		t.Errorf("mode changed to %v", info.Mode().Perm())
	}
	if _, err := os.Stat(layout.Artifacts); err == nil {
		t.Error("created a folder inside a refused data folder")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{Root: file, Artifacts: filepath.Join(file, "artifacts")}).Ensure(); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("a file as data folder: err = %v", err)
	}
}

func TestCheckOutside(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, _ = filepath.EvalSymlinks(repo) // as discovery reports it
	link := filepath.Join(base, "link-to-sub")
	if err := os.Symlink(filepath.Join(repo, "sub"), link); err != nil {
		t.Fatal(err)
	}
	for root, inside := range map[string]bool{
		repo:                                     true,
		filepath.Join(repo, ".agentium", "data"): true, // does not exist yet
		filepath.Join(link, "data"):              true, // through a symlink
		filepath.Join(base, "repo-data"):         false,
		filepath.Join(base, "elsewhere", "data"): false,
	} {
		err := Layout{Root: root}.CheckOutside(repo)
		if (err != nil) != inside {
			t.Errorf("CheckOutside(%s) = %v, want inside=%v", root, err, inside)
		}
	}
}

func TestLockRunsIsExclusiveAndReleased(t *testing.T) {
	l, err := Resolve(func(key string) string {
		return map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data")}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Before any run, the lock file is missing: not busy, and the probe does not create it (a dry run writes nothing).
	if l.RunsBusy() {
		t.Error("RunsBusy without a lock file")
	}
	if _, err := os.Stat(filepath.Join(l.Root, "runs.lock")); !os.IsNotExist(err) {
		t.Errorf("RunsBusy created the lock file: %v", err)
	}
	release, err := l.LockRuns()
	if err != nil {
		t.Fatal(err)
	}
	// flock locks belong to the open file: a second open, as another process would make, is refused.
	if _, err := l.LockRuns(); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("second lock: %v", err)
	}
	if !l.RunsBusy() {
		t.Error("RunsBusy while the lock is held")
	}
	release()
	if l.RunsBusy() {
		t.Error("RunsBusy after release")
	}
	// A probe holding its shared lock does not make a start fail: LockRuns waits it out.
	probe, err := os.Open(filepath.Join(l.Root, "runs.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(200 * time.Millisecond); probe.Close() }()
	again, err := l.LockRuns()
	if err != nil {
		t.Fatalf("after a probe: %v", err)
	}
	again()
}

func TestRunTemp(t *testing.T) {
	layout, err := Resolve(env(map[string]string{"AGENTIUM_HOME": "/data"}))
	if err != nil {
		t.Fatal(err)
	}
	root := layout.RunTemp("e1-s2-t1")
	if layout.Temp != "/tmp" || filepath.Dir(root) != "/tmp" || !IsRunTempName(filepath.Base(root)) || len(root) != len("/tmp/ag-0123456789") {
		t.Errorf("RunTemp = %q (Temp %q)", root, layout.Temp)
	}
	// Predictable from the data folder and the workspace's name, distinct across both.
	other, _ := Resolve(env(map[string]string{"AGENTIUM_HOME": "/elsewhere"}))
	if layout.RunTemp("e1-s2-t1") != root || layout.RunTemp("e1-s2-t2") == root || other.RunTemp("e1-s2-t1") == root {
		t.Error("RunTemp must depend on the data folder and the workspace only")
	}
	// Case-insensitive file systems (macOS, Windows): one data folder, however its path is cased.
	upper, lower := Layout{Root: "/Golden/Data", Temp: "/tmp"}, Layout{Root: "/golden/data", Temp: "/tmp"}
	if same := upper.RunTemp("r1") == lower.RunTemp("r1"); same != (runtime.GOOS == "darwin" || runtime.GOOS == "windows") {
		t.Errorf("on %s, data folders differing in case share temp roots: %v", runtime.GOOS, same)
	}
	if (Layout{Root: "/data"}).RunTemp("r1") != "" {
		t.Error("a layout without Temp has no temp roots")
	}
	for name, want := range map[string]bool{"ag-0123456789": true, "ag-abcdef0123": true, "ag-012345678": false, "ag-ABCDEF0123": false,
		"ag-0123456789x": false, "claude-501": false, "xag-0123456789": false} {
		if IsRunTempName(name) != want {
			t.Errorf("IsRunTempName(%q) = %v", name, !want)
		}
	}
}

// Draft calls' folders hold replies that may name what only a reference solution has: they are inside the artifacts
// folder, which runs deny their agents, and outside the records folder, whose folders recovery reads as runs.
func TestDraftsAreInsideArtifactsAndOutsideRecords(t *testing.T) {
	l, err := Resolve(env(map[string]string{"AGENTIUM_HOME": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.Drafts(), l.Artifacts+string(filepath.Separator)) || strings.HasPrefix(l.Drafts(), l.Records+string(filepath.Separator)) {
		t.Errorf("drafts at %s; artifacts %s, records %s", l.Drafts(), l.Artifacts, l.Records)
	}
}
