package container

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrTooLarge: the grading copy holds more than the copy-in's limit.
var ErrTooLarge = errors.New("the grading copy is too large to copy in")

// errNoRoot: the tree's root could not be opened. That is Agentium's own folder, not the agent's content.
var errNoRoot = errors.New("the tree cannot be opened")

// CopyLimits cap what a copy-in sends.
type CopyLimits struct {
	Bytes   int64         // file content
	Entries int           // every listed entry, skipped ones too, so a tree of countless empty files or pipes cannot stream or be walked without end
	Timeout time.Duration // the whole copy-in; 0 is the default's
}

// DefaultCopyLimits are 2 GiB of content, 1,000,000 entries and 10 minutes (step 0's largest tree: 177 MB in 9,092
// files, in 0.53 s).
func DefaultCopyLimits() CopyLimits {
	return CopyLimits{Bytes: 2 << 30, Entries: 1_000_000, Timeout: 10 * time.Minute}
}

// TarStats counts what a tar stream holds.
type TarStats struct {
	Entries int   // folders, files and links written
	Bytes   int64 // file content
	Skipped int   // sockets, pipes and devices, which are left out
}

// walkHooks let tests act at fixed points of the walk: a swap between a folder's listing and its open, or a slow tree.
// Production code leaves them nil.
type walkHooks struct {
	entry   func(name string) // before an entry is looked at
	openDir func(name string) // after a folder's header is written, before the folder is opened to be listed
}

// WriteTar writes the tree at root to w as a tar stream for the container: folders, regular files and symbolic links,
// owned by the grade's user, with their permission bits (setuid, setgid and sticky dropped) and times, in name order.
// It never follows a link: a link is written as a link, with its target as text, and the tree is read through an
// os.Root, so neither a link nor a folder swapped for one mid-walk leads outside root. Nothing is opened in a way that
// can block: a folder is opened as a folder only, and a file without blocking, and each must still be what it was
// listed as (a pipe swapped in for either is refused, never waited on). Sockets, pipes and devices are skipped. More
// than limits allow (every listed entry counts, skipped ones too) is ErrTooLarge. The walk checks ctx before each
// entry and stops with its cause. The tree is only read.
func WriteTar(ctx context.Context, w io.Writer, root string, limits CopyLimits) (TarStats, error) {
	return writeTar(ctx, w, root, limits, walkHooks{}, nil)
}

// writeTar is WriteTar; it sets begun (when not nil) once the tree is opened, from when the walk reads the agent's
// input. A walk stopped before that never opens the tree.
func writeTar(ctx context.Context, w io.Writer, root string, limits CopyLimits, hooks walkHooks, begun *atomic.Bool) (TarStats, error) {
	t := &tarWalk{ctx: ctx, limits: limits, hooks: hooks}
	if err := t.stopped(); err != nil {
		return t.stats, fmt.Errorf("tar %s: %w", root, err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return t.stats, fmt.Errorf("tar %s: %w: %w", root, errNoRoot, err)
	}
	if begun != nil {
		begun.Store(true)
	}
	defer r.Close()
	t.r, t.tw = r, tar.NewWriter(w)
	if err := t.dir("."); err != nil {
		return t.stats, fmt.Errorf("tar %s: %w", root, err)
	}
	if err := t.tw.Close(); err != nil {
		return t.stats, fmt.Errorf("tar %s: %w", root, err)
	}
	return t.stats, nil
}

// tarWalk is one WriteTar's walk.
type tarWalk struct {
	ctx    context.Context
	r      *os.Root
	tw     *tar.Writer
	limits CopyLimits
	hooks  walkHooks
	stats  TarStats
	// A seed's walk (seedTar): its entries' names start with prefix; skip leaves out files and links by that full name
	// (they are in the volume already), and written collects the ones written.
	prefix  string
	skip    func(name string) bool
	written *[]string
}

// tarName is an entry's name in the stream.
func (t *tarWalk) tarName(name string) string {
	if t.prefix == "" {
		return name
	}
	return path.Join(t.prefix, name)
}

// stopped is the walk's cancellation, with its cause (CopyIn's errCopyEnded once docker has stopped reading).
func (t *tarWalk) stopped() error {
	if t.ctx.Err() == nil {
		return nil
	}
	return context.Cause(t.ctx)
}

// dir writes the entries of the folder name, in name order.
func (t *tarWalk) dir(name string) error {
	names, err := t.list(name)
	if err != nil {
		return err
	}
	for _, entry := range names {
		if err := t.stopped(); err != nil {
			return err
		}
		if err := t.entry(path.Join(name, entry)); err != nil {
			return err
		}
	}
	return nil
}

// list opens the folder name as a folder only, without following a link and without blocking (a pipe swapped in for
// it would otherwise hang the open beyond any cancel), checks that the open file is still a folder, and returns its
// entries' names, sorted. More names than the entries left is ErrTooLarge, before they are all held in memory.
func (t *tarWalk) list(name string) ([]string, error) {
	f, err := t.r.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: no longer a folder", path.Clean(name))
	}
	room := t.limits.Entries - t.stats.Entries - t.stats.Skipped
	var names []string
	for {
		if err := t.stopped(); err != nil {
			return nil, err
		}
		batch, err := f.Readdirnames(1024)
		names = append(names, batch...)
		if len(names) > room {
			return nil, fmt.Errorf("%w: more than %d entries", ErrTooLarge, t.limits.Entries)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(names)
	return names, nil
}

// entry writes one listed entry, and a folder's own entries after it.
func (t *tarWalk) entry(name string) error {
	if t.hooks.entry != nil {
		t.hooks.entry(name)
	}
	if t.stats.Entries+t.stats.Skipped >= t.limits.Entries {
		return fmt.Errorf("%w: more than %d entries", ErrTooLarge, t.limits.Entries)
	}
	info, err := t.r.Lstat(name)
	if err != nil {
		return err
	}
	full := t.tarName(name)
	hdr := &tar.Header{Name: full, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(), Uid: 65534, Gid: 65534, Format: tar.FormatPAX}
	if !info.IsDir() && t.skip != nil && t.skip(full) {
		return nil // a seed's entry already in the volume: rewriting it would tear what grades read
	}
	switch {
	case info.IsDir():
		hdr.Typeflag, hdr.Name = tar.TypeDir, full+"/"
		if err := t.tw.WriteHeader(hdr); err != nil {
			return err
		}
		t.stats.Entries++
		if t.hooks.openDir != nil {
			t.hooks.openDir(name)
		}
		return t.dir(name)
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := t.r.Readlink(name)
		if err != nil {
			return err
		}
		hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, target, 0o777
		t.stats.Entries++
		t.wrote(full)
		return t.tw.WriteHeader(hdr)
	case info.Mode().IsRegular():
		n, err := writeFile(t.tw, t.r, name, hdr, t.limits.Bytes-t.stats.Bytes)
		t.stats.Bytes += n
		if err == nil {
			t.stats.Entries++
			t.wrote(full)
		}
		return err
	default:
		t.stats.Skipped++
		return nil
	}
}

// wrote notes a seed's file or link written.
func (t *tarWalk) wrote(name string) {
	if t.written != nil {
		*t.written = append(*t.written, name)
	}
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
