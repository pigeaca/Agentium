package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

// Binary is the running program's file, as it was when the program started (RunningBinary, the first thing main
// does): what tells this build from any other is read from it later, and a build installed in the meantime must not
// be mistaken for the one that runs.
type Binary struct {
	path string
	info os.FileInfo
	err  error
}

// RunningBinary finds the running program's file and notes which file it is now.
func RunningBinary() Binary {
	path, err := os.Executable()
	if err != nil {
		return Binary{err: fmt.Errorf("find the running program: %w", err)}
	}
	info, err := os.Stat(path)
	if err != nil {
		return Binary{err: fmt.Errorf("find the running program: %w", err)}
	}
	return Binary{path: path, info: info}
}

// errBinaryChanged is ID's error for a file that is no longer the one the program started from.
var errBinaryChanged = errors.New("the running program's file changed since it started")

// ID is what tells this build of Agentium from any other, for what a build keeps between commands (Env.BuildID): the
// build information the Go toolchain put into the running program (its module, version, revision and settings) and
// the SHA-256 of its file. The digest tells apart builds the information cannot (uncommitted changes, another
// toolchain, a build overlay); the information is the running code's own, whatever is on disk.
//
// It fails when the file is not the one RunningBinary saw any more (another build was installed over it, or it was
// rewritten, even while it was read): its digest would name a build that is not the one running. Only a file replaced
// in the instant between the program's start and RunningBinary goes unnoticed, and then only by a build with the same
// build information.
func (b Binary) ID() (string, error) {
	if b.err != nil {
		return "", b.err
	}
	f, err := os.Open(b.path)
	if err != nil {
		return "", fmt.Errorf("read the running program: %w", err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read the running program: %w", err)
	}
	if !unchanged(b.info, before) {
		return "", errBinaryChanged
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("read the running program: %w", err)
	}
	after, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read the running program: %w", err)
	}
	if !unchanged(before, after) {
		return "", errBinaryChanged
	}
	built := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		built = info.String()
	}
	return built + "sha256 " + hex.EncodeToString(sum.Sum(nil)), nil
}

// unchanged reports whether a and b are one file with one content, as far as its place on disk, size and
// modification time can tell.
func unchanged(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
