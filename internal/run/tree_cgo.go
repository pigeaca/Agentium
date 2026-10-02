//go:build cgo && (darwin || linux)

package run

/*
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <unistd.h>
#ifdef __APPLE__
#include <sys/attr.h>
#endif

// Each returns 0 (or a descriptor) on success and -errno on failure. Every one acts on one name in the folder dfd
// refers to and never follows a link at that name: the folder's entries are the grade's, which may swap any of them
// for a link at any time, and nothing here may reach what a link points to.

static int ag_openat_dir(int dfd, const char *name) {
	int fd = openat(dfd, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
	return fd < 0 ? -errno : fd;
}

static int ag_lstatat(int dfd, const char *name, uint32_t *mode, uint32_t *flags) {
	struct stat st;
	if (fstatat(dfd, name, &st, AT_SYMLINK_NOFOLLOW) != 0) return -errno;
	*mode = st.st_mode;
#ifdef __APPLE__
	*flags = st.st_flags;
#else
	*flags = 0;
#endif
	return 0;
}

// ag_setflagsat sets a name's file flags (chflags) without following a link: setattrlistat with FSOPT_NOFOLLOW, the
// only fd-relative way macOS has (no chflagsat), which needs no open and so works on an entry without permissions.
static int ag_setflagsat(int dfd, const char *name, uint32_t flags) {
#ifdef __APPLE__
	struct attrlist al = {0};
	al.bitmapcount = ATTR_BIT_MAP_COUNT;
	al.commonattr = ATTR_CMN_FLAGS;
	if (setattrlistat(dfd, name, &al, &flags, sizeof(flags), FSOPT_NOFOLLOW) != 0) return -errno;
#endif
	return 0;
}

static int ag_chmodat(int dfd, const char *name, uint32_t mode) {
	return fchmodat(dfd, name, (mode_t)mode, AT_SYMLINK_NOFOLLOW) == 0 ? 0 : -errno;
}

static int ag_unlinkat(int dfd, const char *name, int dir) {
	return unlinkat(dfd, name, dir ? AT_REMOVEDIR : 0) == 0 ? 0 : -errno;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// userFlags are the file flags an owner may set and clear (UF_SETTABLE): uchg and uappnd among them, which make
// removal fail. The system's own (SF_*) need the superuser and are kept.
const userFlags = 0x0000ffff

func errnoOf(r C.int) error {
	if r >= 0 {
		return nil
	}
	return syscall.Errno(-r)
}

// removeTreeAt removes root by walking it through folder descriptors, one name at a time, never following a link and
// never resolving a path the grade can change: it clears what resists removal on each entry (the owner's flags on any
// entry, links included; the owner's permissions on folders) by name in its parent's descriptor, opens each folder
// with O_NOFOLLOW|O_DIRECTORY, and removes from the bottom up. A swap of any entry for a link at any moment changes
// only what that link itself is: its flags or mode may be cleared and the link removed, never its target. Paths deeper
// than PATH_MAX are no limit, since each call names one entry. root's parent folder (Agentium's own, which a grade
// cannot write) is opened by path.
func removeTreeAt(root string) error {
	return removeTreeRacing(root, nil)
}

// removeTreeRacing is removeTreeAt with raced, when set, called after each entry's stat and before anything changes it:
// where a grade's process may swap the entry. Tests use it to swap at exactly that moment; it is nil otherwise.
func removeTreeRacing(root string, raced func(name string)) error {
	parent, err := syscall.Open(filepath.Dir(root), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("remove %s: %w", root, err)
	}
	defer syscall.Close(parent)
	if err := removeAt(parent, filepath.Base(root), raced); err != nil {
		return fmt.Errorf("remove %s: %w", root, err)
	}
	return nil
}

// removeAt removes the entry name of the folder dfd, and what is under it (raced: see removeTreeRacing).
func removeAt(dfd int, name string, raced func(name string)) error {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var mode, flags C.uint32_t
	if err := errnoOf(C.ag_lstatat(C.int(dfd), cname, &mode, &flags)); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if raced != nil {
		raced(name)
	}
	if flags&userFlags != 0 {
		if err := errnoOf(C.ag_setflagsat(C.int(dfd), cname, flags&^userFlags)); err != nil {
			return fmt.Errorf("%s: clear its flags: %w", name, err)
		}
	}
	if uint32(mode)&syscall.S_IFMT != syscall.S_IFDIR {
		return errnoOf(C.ag_unlinkat(C.int(dfd), cname, 0))
	}
	if perm := uint32(mode) & 0o7777; perm&0o700 != 0o700 {
		if err := errnoOf(C.ag_chmodat(C.int(dfd), cname, C.uint32_t(perm|0o700))); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	fd := C.ag_openat_dir(C.int(dfd), cname)
	if err := errnoOf(fd); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	dir := os.NewFile(uintptr(fd), name) // owns fd: closed below
	names, err := dir.Readdirnames(-1)
	var errs []error
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
	}
	for _, child := range names {
		if err := removeAt(int(fd), child, raced); err != nil {
			errs = append(errs, fmt.Errorf("%s/%w", name, err))
		}
	}
	dir.Close()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return errnoOf(C.ag_unlinkat(C.int(dfd), cname, 1))
}
