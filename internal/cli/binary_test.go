package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// binaryAt is a Binary for the file at path as it is now, as RunningBinary makes one for the running program.
func binaryAt(t *testing.T, path string) Binary {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return Binary{path: path, info: info}
}

// A build's identity ends with the digest of its file, is the same each time it is asked, and differs for a file
// with other content.
func TestBinaryIDIsTheFilesDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agentium")
	if err := os.WriteFile(path, []byte("one build"), 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("one build"))
	b := binaryAt(t, path)
	id, err := b.ID()
	if err != nil || !strings.HasSuffix(id, "sha256 "+hex.EncodeToString(sum[:])) {
		t.Fatalf("ID = %q, %v; want it to end with the file's digest", id, err)
	}
	if again, err := b.ID(); err != nil || again != id {
		t.Errorf("asked again: %q, %v", again, err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("another build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if otherID, err := binaryAt(t, other).ID(); err != nil || otherID == id {
		t.Errorf("a file with other content has the identity %q, %v", otherID, err)
	}
	// The running program: the test's own binary.
	running := RunningBinary()
	first, err := running.ID()
	if err != nil || !strings.Contains(first, "sha256 ") {
		t.Fatalf("the running program's ID: %q, %v", first, err)
	}
	if second, err := running.ID(); err != nil || second != first {
		t.Errorf("the running program's ID changed: %v", err)
	}
}

// When the file is not the one the program started from any more, there is no identity: the file's digest would name
// another build than the one that runs.
func TestBinaryIDRefusesAFileThatChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agentium")
	write := func(p, content string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(path, "one build")
	b := binaryAt(t, path)
	if _, err := b.ID(); err != nil {
		t.Fatal(err)
	}

	// Another build installed over it, as go install does: a new file renamed into its place.
	write(path+".new", "one build") // even the same bytes: it is not the file that was started
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ID(); !errors.Is(err, errBinaryChanged) {
		t.Errorf("after another file took its place: %v, want errBinaryChanged", err)
	}

	// Rewritten in place: same file, other content of the same size, a later time.
	b = binaryAt(t, path)
	write(path, "two build")
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ID(); !errors.Is(err, errBinaryChanged) {
		t.Errorf("after it was rewritten in place: %v, want errBinaryChanged", err)
	}

	// Gone, or never found.
	b = binaryAt(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ID(); err == nil {
		t.Error("a removed file has an identity")
	}
	if _, err := (Binary{err: errors.New("not found")}).ID(); err == nil {
		t.Error("a program that was not found has an identity")
	}
}
