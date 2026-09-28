package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
