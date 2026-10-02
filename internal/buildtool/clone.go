package buildtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// How CloneFolder made its copy: CloneFile is one clonefile(2) call on the whole folder (macOS, APFS); otherwise it
// returns the cp command of copyTree's that succeeded.
const CloneFile = "clonefile"

// CloneFolder makes dst, which must not exist, an independent copy of the folder src, and returns how (CloneFile, or
// the cp command used). Writing in either never changes the other: a clone shares data blocks copy-on-write, and every
// file in it is a new file of its own (not a hard link), so a write in place, a truncation, a removal or a change of
// mode in dst leaves src as it was.
//
// On macOS one clonefile(2) call clones the whole tree, which is what makes a per-grade cache affordable: in the
// isolation plan's step 0 it took 0.03-0.14 s for a Go build cache or a Gradle home of a few thousand files, and about
// half a second for 2.4 GB in 25,650 files, where cp -Rc (which clones file by file) took 1-22 s, about as long as a plain
// copy. Where clonefile cannot clone (another file system than APFS, another volume than src's, another system than
// macOS) the fallback is copyTree's cp: correct, only slower.
//
// src must be a real folder, not a link to one (the link itself is never followed). dst's parent folder is created,
// owner-only, when missing. A dst that exists is refused and left alone. On a failure dst is removed: it did not exist.
func CloneFolder(ctx context.Context, src, dst string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(src) || !filepath.IsAbs(dst) {
		return "", fmt.Errorf("clone %s to %s: the paths must be absolute", src, dst)
	}
	info, err := os.Lstat(src)
	if err != nil {
		return "", fmt.Errorf("clone %s: %w", src, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("clone %s: not a folder (a link is not followed)", src)
	}
	if _, err := os.Lstat(dst); err == nil {
		return "", fmt.Errorf("clone %s: %s exists", src, dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("clone %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("clone %s: %w", src, err)
	}
	cloned, err := cloneFile(src, dst)
	switch {
	case cloned:
		return CloneFile, nil
	case errors.Is(err, os.ErrExist): // made meanwhile by someone else: never replaced
		return "", fmt.Errorf("clone %s: %s exists", src, dst)
	case err != nil && !errors.Is(err, errCloneUnsupported):
		os.RemoveAll(dst) // a clone that failed part way (out of space) may leave a part
		return "", fmt.Errorf("clone %s: %w", src, err)
	}
	how, err := copyTree(ctx, src, dst)
	if err != nil {
		os.RemoveAll(dst)
		return "", fmt.Errorf("clone %s: %w", src, err)
	}
	return how, nil
}

// errCloneUnsupported means clonefile(2) cannot clone here (not macOS, not APFS, two volumes): copy instead.
var errCloneUnsupported = errors.New("clonefile is not supported here")
