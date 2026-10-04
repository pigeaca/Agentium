package container

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"syscall"
	"time"
)

// ErrTooLarge: the grading copy holds more than the copy-in's limit.
var ErrTooLarge = errors.New("the grading copy is too large to copy in")

// errNoRoot: the tree's root could not be opened. That is Agentium's own folder, not the agent's content.
var errNoRoot = errors.New("the tree cannot be opened")

// CopyLimits cap what a copy-in sends.
type CopyLimits struct {
	Bytes   int64 // file content
	Entries int   // folders, files and links, so a tree of countless empty files cannot stream without end
}

// DefaultCopyLimits are 2 GiB of content and 1,000,000 entries (step 0's largest tree: 177 MB in 9,092 files).
func DefaultCopyLimits() CopyLimits { return CopyLimits{Bytes: 2 << 30, Entries: 1_000_000} }

// TarStats counts what a tar stream holds.
type TarStats struct {
	Entries int   // folders, files and links written
	Bytes   int64 // file content
	Skipped int   // sockets, pipes and devices, which are left out
}

// WriteTar writes the tree at root to w as a tar stream for the container: folders, regular files and symbolic links,
// owned by the grade's user, with their permission bits (setuid, setgid and sticky dropped) and times. It never follows
// a link: a link is written as a link, with its target as text, and the tree is read through an os.Root, so neither a
// link nor a folder swapped for one mid-walk leads outside root. A file is opened without following a link and must
// still be the regular file it was listed as. Sockets, pipes and devices are skipped. More than limits allow is
// ErrTooLarge. The tree is only read.
func WriteTar(w io.Writer, root string, limits CopyLimits) (TarStats, error) {
	var stats TarStats
	r, err := os.OpenRoot(root)
	if err != nil {
		return stats, fmt.Errorf("tar %s: %w: %w", root, errNoRoot, err)
	}
	defer r.Close()
	tw := tar.NewWriter(w)
	walkErr := fs.WalkDir(r.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if stats.Entries >= limits.Entries {
			return fmt.Errorf("%w: more than %d entries", ErrTooLarge, limits.Entries)
		}
		info, err := r.Lstat(name)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(), Uid: 65534, Gid: 65534, Format: tar.FormatPAX}
		switch {
		case info.IsDir():
			hdr.Typeflag, hdr.Name = tar.TypeDir, name+"/"
			stats.Entries++
			return tw.WriteHeader(hdr)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := r.Readlink(name)
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, target, 0o777
			stats.Entries++
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			n, err := writeFile(tw, r, name, hdr, limits.Bytes-stats.Bytes)
			stats.Bytes += n
			if err == nil {
				stats.Entries++
			}
			return err
		default:
			stats.Skipped++
			return nil
		}
	})
	if walkErr != nil {
		return stats, fmt.Errorf("tar %s: %w", root, walkErr)
	}
	if err := tw.Close(); err != nil {
		return stats, fmt.Errorf("tar %s: %w", root, err)
	}
	return stats, nil
}

// writeFile writes one regular file: opened without following a link and without blocking (a pipe swapped in would
// otherwise hang the open), then checked to still be a regular file, and copied for exactly the size it has now.
func writeFile(tw *tar.Writer, r *os.Root, name string, hdr *tar.Header, room int64) (int64, error) {
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s: no longer a regular file", path.Clean(name))
	}
	if info.Size() > room {
		return 0, fmt.Errorf("%w: %s", ErrTooLarge, path.Clean(name))
	}
	hdr.Typeflag, hdr.Size, hdr.Mode, hdr.ModTime = tar.TypeReg, info.Size(), int64(info.Mode().Perm()), info.ModTime()
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, err
	}
	n, err := io.CopyN(tw, f, info.Size())
	if err != nil {
		return n, fmt.Errorf("%s: %w (it changed while it was read)", path.Clean(name), err)
	}
	return n, nil
}

// skeletonTar is the trusted two-entry tar docker cp puts into /grade before the container starts: work/ and
// cache/, owned by the grade's user, mode 0700. The volume's root stays root's, so the grade can write only there.
func skeletonTar(w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, name := range []string{"work/", "cache/"} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o700, Uid: 65534, Gid: 65534, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err != nil {
			return err
		}
	}
	return tw.Close()
}
