package container

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWriteTarNeverFollowsLinks: the copy-in's stream holds folders, files and links as they are, owned by the grade's
// user, with setuid and setgid dropped; a link to a host file or folder is written as a link, and what it points to
// is never read; pipes are skipped; the tree is unchanged.
func TestWriteTarNeverFollowsLinks(t *testing.T) {
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("HOST-SECRET"), 0o600))
	must(t, os.WriteFile(filepath.Join(outside, "inner"), []byte("HOST-FOLDER-FILE"), 0o600))
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a/f.txt": "hello", "a/run.sh": "#!/bin/sh\n", "suid": "s"})
	must(t, os.Chmod(filepath.Join(root, "a/run.sh"), 0o755))
	must(t, os.Chmod(filepath.Join(root, "suid"), 0o4755|os.ModeSetuid))
	must(t, os.Chmod(filepath.Join(root, "a"), 0o750))
	must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "leak")))
	must(t, os.Symlink(outside, filepath.Join(root, "leakdir")))
	must(t, os.Symlink("a/f.txt", filepath.Join(root, "rel")))
	must(t, syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600))
	must(t, os.Chtimes(filepath.Join(root, "a/f.txt"), time.Unix(1700000000, 0), time.Unix(1700000000, 0)))
	var buf bytes.Buffer
	stats, err := WriteTar(&buf, root, DefaultCopyLimits())
	must(t, err)
	if stats != (TarStats{Entries: 7, Bytes: int64(len("hello") + len("#!/bin/sh\n") + 1), Skipped: 1}) {
		t.Errorf("stats %+v", stats)
	}
	if bytes.Contains(buf.Bytes(), []byte("HOST-SECRET")) || bytes.Contains(buf.Bytes(), []byte("HOST-FOLDER-FILE")) {
		t.Fatal("a link was followed")
	}
	var got []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must(t, err)
		if h.Uid != 65534 || h.Gid != 65534 {
			t.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
		line := h.Name + " " + string(h.Typeflag) + " " + fmtMode(h.Mode)
		if h.Linkname != "" {
			line += " -> " + strings.Replace(h.Linkname, outside, "<outside>", 1)
		}
		if h.Name == "a/f.txt" {
			body, _ := io.ReadAll(tr)
			line += " " + string(body) + " " + h.ModTime.UTC().Format(time.RFC3339)
		}
		got = append(got, line)
	}
	sort.Strings(got)
	want := []string{
		"a/ 5 0750",
		"a/f.txt 0 0644 hello 2023-11-14T22:13:20Z",
		"a/run.sh 0 0755",
		"leak 2 0777 -> <outside>/secret",
		"leakdir 2 0777 -> <outside>",
		"rel 2 0777 -> a/f.txt",
		"suid 0 0755",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestWriteTarRefusesSwaps: a file swapped for a link, or for a pipe, between the listing and the open is refused
// (never followed, never a hang).
func TestWriteTarRefusesSwaps(t *testing.T) {
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("HOST-SECRET"), 0o600))
	root := t.TempDir()
	must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "swapped")))
	must(t, syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600))
	r, err := os.OpenRoot(root)
	must(t, err)
	defer r.Close()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"swapped", "pipe"} {
		done := make(chan error, 1)
		go func() {
			_, err := writeFile(tw, r, name, &tar.Header{Name: name}, DefaultCopyLimits().Bytes)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: written", name)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the open hung", name)
		}
	}
	if bytes.Contains(buf.Bytes(), []byte("HOST-SECRET")) {
		t.Error("a link was followed")
	}
}

// TestWriteTarLimits: more content, or more entries, than the limits allow is ErrTooLarge; a root that cannot be
// opened is errNoRoot (Agentium's folder, not the agent's content).
func TestWriteTarLimits(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a": strings.Repeat("x", 600), "b": strings.Repeat("y", 600), "d/e": ""})
	if _, err := WriteTar(io.Discard, root, CopyLimits{Bytes: 1000, Entries: 10}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("1200 bytes under a 1000-byte limit: %v", err)
	}
	if stats, err := WriteTar(io.Discard, root, CopyLimits{Bytes: 1200, Entries: 4}); err != nil || stats.Entries != 4 {
		t.Errorf("1200 bytes and 4 entries under limits of 1200 and 4: %+v, %v", stats, err)
	}
	if _, err := WriteTar(io.Discard, root, CopyLimits{Bytes: 1200, Entries: 3}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("4 entries under a limit of 3: %v", err)
	}
	if _, err := WriteTar(io.Discard, filepath.Join(root, "missing"), DefaultCopyLimits()); !errors.Is(err, errNoRoot) {
		t.Errorf("a missing tree: %v", err)
	}
	if l := DefaultCopyLimits(); l.Bytes != 2<<30 || l.Entries != 1_000_000 {
		t.Errorf("default limits: %+v", l)
	}
}
