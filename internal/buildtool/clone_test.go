package buildtool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// writeTree writes files (path: content) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree reads every regular file under root (path: content), and names links and folders.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case d.Type()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			got[rel] = "link to " + target
		case d.IsDir():
			got[rel+"/"] = ""
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			got[rel] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sameTree(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d entries, want %d: %v", what, len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s = %q, want %q", what, k, got[k], v)
		}
	}
}

// A clone is the seed's tree, links kept as links, and independent of it both ways: writing the clone in place,
// appending, truncating, removing, renaming, changing a mode or adding a file never changes the seed, and a later
// change to the seed never shows in the clone. Every file in the clone is a file of its own (another inode, one link):
// not a hard link a write could reach the seed through. On macOS it is one clonefile(2) call.
func TestCloneFolderIsIndependentOfTheSeed(t *testing.T) {
	dir := t.TempDir()
	seed, clone := filepath.Join(dir, "seed"), filepath.Join(dir, "grades", "g1", "cache")
	writeTree(t, seed, map[string]string{"go-build/00/a-d": "compiled", "gradle/gradle.properties": "org.gradle.daemon=false\n",
		"gradle/wrapper/dists/x.zip": "wrapper", "pycache/.keep": ""})
	if err := os.Symlink("../gradle/gradle.properties", filepath.Join(seed, "pycache", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(seed, "gradle", "wrapper", "dists", "x.zip"), 0o400); err != nil {
		t.Fatal(err)
	}
	before := readTree(t, seed)
	how, err := CloneFolder(context.Background(), seed, clone)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" && how != CloneFile {
		t.Errorf("made by %q, want one clonefile call (APFS)", how)
	}
	sameTree(t, "the clone", readTree(t, clone), before)
	if info, err := os.Lstat(filepath.Join(clone, "gradle", "wrapper", "dists", "x.zip")); err != nil || info.Mode().Perm() != 0o400 {
		t.Errorf("modes are kept: %v, %v", info, err)
	}
	for _, name := range []string{"go-build/00/a-d", "gradle/gradle.properties"} {
		s, c := stat(t, filepath.Join(seed, name)), stat(t, filepath.Join(clone, name))
		if s.Ino == c.Ino || c.Nlink != 1 || s.Nlink != 1 {
			t.Errorf("%s: the clone shares the seed's file (inodes %d and %d, links %d and %d)", name, s.Ino, c.Ino, s.Nlink, c.Nlink)
		}
	}

	// The clone's grade writes everything it can.
	inPlace, err := os.OpenFile(filepath.Join(clone, "go-build", "00", "a-d"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inPlace.WriteAt([]byte("POISON!!"), 0); err != nil {
		t.Fatal(err)
	}
	inPlace.Close()
	appendTo, err := os.OpenFile(filepath.Join(clone, "gradle", "gradle.properties"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	appendTo.WriteString("org.gradle.caching=true\n")
	appendTo.Close()
	must(t, os.Chmod(filepath.Join(clone, "gradle", "wrapper", "dists", "x.zip"), 0o600))
	must(t, os.Truncate(filepath.Join(clone, "gradle", "wrapper", "dists", "x.zip"), 0))
	must(t, os.Remove(filepath.Join(clone, "pycache", "link")))
	must(t, os.Symlink("/etc/passwd", filepath.Join(clone, "pycache", "link")))
	must(t, os.Rename(filepath.Join(clone, "pycache", ".keep"), filepath.Join(clone, "pycache", "moved")))
	must(t, os.WriteFile(filepath.Join(clone, "go-build", "planted"), []byte("x"), 0o600))
	must(t, os.Chmod(filepath.Join(clone, "go-build"), 0o755))
	sameTree(t, "the seed after the grade wrote its clone", readTree(t, seed), before)
	if info, _ := os.Lstat(filepath.Join(seed, "go-build")); info.Mode().Perm() != 0o700 {
		t.Errorf("the seed's folder mode changed: %v", info.Mode())
	}

	// And the other way: a later seed change does not reach an existing clone.
	must(t, os.WriteFile(filepath.Join(seed, "go-build", "00", "a-d"), []byte("reseeded"), 0o600))
	if data, _ := os.ReadFile(filepath.Join(clone, "go-build", "00", "a-d")); string(data) != "POISON!!" {
		t.Errorf("the clone follows the seed: %q", data)
	}
}

func stat(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t)
}

// A clone never replaces what is at its destination (another grade's cache), never follows a link at its source, and
// takes only absolute paths; a cancelled context clones nothing.
func TestCloneFolderRefuses(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	writeTree(t, seed, map[string]string{"f": "seed"})
	other := filepath.Join(dir, "other")
	writeTree(t, other, map[string]string{"f": "another grade's cache"})
	if _, err := CloneFolder(context.Background(), seed, other); err == nil {
		t.Error("cloned over an existing folder")
	}
	if data, _ := os.ReadFile(filepath.Join(other, "f")); string(data) != "another grade's cache" {
		t.Errorf("the existing folder changed: %q", data)
	}
	link := filepath.Join(dir, "link")
	must(t, os.Symlink(seed, link))
	if _, err := CloneFolder(context.Background(), link, filepath.Join(dir, "from-link")); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("a link as the seed: %v", err)
	}
	if _, err := CloneFolder(context.Background(), filepath.Join(seed, "f"), filepath.Join(dir, "from-file")); err == nil {
		t.Error("a file as the seed was cloned")
	}
	if _, err := CloneFolder(context.Background(), "seed", filepath.Join(dir, "rel")); err == nil {
		t.Error("a relative seed was accepted")
	}
	if _, err := CloneFolder(context.Background(), seed, filepath.Join(dir, "new", "parent", "cache")); err != nil {
		t.Errorf("a missing parent is created: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CloneFolder(ctx, seed, filepath.Join(dir, "cancelled")); err == nil {
		t.Error("a cancelled clone went ahead")
	}
	for _, p := range []string{filepath.Join(dir, "from-link"), filepath.Join(dir, "from-file"), filepath.Join(dir, "cancelled")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was left behind", p)
		}
	}
}
