//go:build cgo && (darwin || linux)

package run

/*
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
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

// ag_noaclat removes a name's access list without following a link: setattrlistat(ATTR_CMN_EXTENDED_SECURITY,
// FSOPT_NOFOLLOW) with a file security header that holds no list (kauth_filesec with KAUTH_FILESEC_NOACL; the kernel's
// struct is not in the user headers, so its header part is spelled out). An owner can always do this, even when the
// list denies everyone "writesecurity", and a list that denies "delete" or "delete_child" would otherwise block
// removal, and a rename into the quarantine too. Elsewhere there is nothing to do: Linux's lists do not block the owner.
static int ag_noaclat(int dfd, const char *name) {
#ifdef __APPLE__
	struct ag_filesec {
		uint32_t magic;           // KAUTH_FILESEC_MAGIC
		unsigned char owner[16];  // guid_t: zero, unchanged
		unsigned char group[16];
		uint32_t entrycount;      // KAUTH_FILESEC_NOACL: no list
		uint32_t flags;
	};
	struct __attribute__((packed)) { attrreference_t ref; struct ag_filesec fs; } buf;
	memset(&buf, 0, sizeof(buf));
	buf.ref.attr_dataoffset = sizeof(attrreference_t);
	buf.ref.attr_length = sizeof(struct ag_filesec);
	buf.fs.magic = 0x012cc16d;
	buf.fs.entrycount = (uint32_t)-1;
	struct attrlist al = {0};
	al.bitmapcount = ATTR_BIT_MAP_COUNT;
	al.commonattr = ATTR_CMN_EXTENDED_SECURITY;
	if (setattrlistat(dfd, name, &al, &buf, sizeof(buf), FSOPT_NOFOLLOW) != 0) return -errno;
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
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// blockingFlags are the owner's file flags that make removal fail: UF_IMMUTABLE and UF_APPEND (uchg, uappnd). Only
// they are cleared: the others carry meaning (UF_COMPRESSED clears to a file with no data, and a grade may hard-link
// one of the user's compressed files into its folders), and the system's (SF_*) need the superuser.
const blockingFlags = 0x2 | 0x4

// removeWalks says the removal walks folder descriptors (removeTreeRacing calls its hook): with cgo, on macOS and Linux.
const removeWalks = true

func errnoOf(r C.int) error {
	if r >= 0 {
		return nil
	}
	return syscall.Errno(-r)
}

// removeTreeAt removes root by walking it through folder descriptors, one name at a time, never following a link and
// never resolving a path the grade can change: it clears what resists removal on each entry (the blocking flags and
// the access list on any entry, links included; the owner's permissions on folders) by name in its parent's descriptor, opens each folder
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
	var errs firstErrors
	removeAt(parent, filepath.Base(root), raced, &errs)
	if err := errs.err(); err != nil {
		return fmt.Errorf("remove %s: %w", root, err)
	}
	return nil
}

// firstErrors keeps the first error and counts the rest: a grade can make any number of entries resist, and a warning
// must stay one line.
type firstErrors struct {
	first error
	more  int
}

func (e *firstErrors) add(err error) {
	switch {
	case err == nil:
	case e.first == nil:
		e.first = err
	default:
		e.more++
	}
}

func (e *firstErrors) err() error {
	if e.first == nil || e.more == 0 {
		return e.first
	}
	return fmt.Errorf("%w (and %d more)", e.first, e.more)
}

// belowError is a removal error named by its path below the root. The grade picks the names and the depth, so the
// message keeps the first and the last two names of a deep path and quotes each one (a name may hold a newline or a
// terminal escape).
type belowError struct {
	path []string
	err  error
}

func (e *belowError) Error() string {
	quoted := make([]string, len(e.path))
	for i, name := range e.path {
		quoted[i] = strconv.Quote(name)
	}
	if len(quoted) > 3 {
		quoted = []string{quoted[0], "…", quoted[len(quoted)-2], quoted[len(quoted)-1]}
	}
	return strings.Join(quoted, "/") + ": " + e.err.Error()
}

func (e *belowError) Unwrap() error { return e.err }

// removeAt removes the entry name of the folder dfd, and what is under it (raced: see removeTreeRacing). What cannot
// be removed is added to errs, named by its path below root.
func removeAt(dfd int, name string, raced func(name string), errs *firstErrors) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	failed := func(err error) { errs.add(&belowError{path: []string{name}, err: err}) }
	// The access list goes first (best effort, no-follow, any type): on macOS a folder whose list denies readattr or
	// readsecurity cannot even be looked at. Not every file system keeps access lists; if a list was not cleared, what
	// follows fails and says so.
	C.ag_noaclat(C.int(dfd), cname)
	var mode, flags C.uint32_t
	if err := errnoOf(C.ag_lstatat(C.int(dfd), cname, &mode, &flags)); errors.Is(err, fs.ErrNotExist) {
		return
	} else if err != nil {
		failed(err)
		return
	}
	if raced != nil {
		raced(name)
	}
	if flags&blockingFlags != 0 {
		if err := errnoOf(C.ag_setflagsat(C.int(dfd), cname, flags&^blockingFlags)); err != nil {
			failed(fmt.Errorf("clear its flags: %w", err))
			return
		}
	}
	if uint32(mode)&syscall.S_IFMT != syscall.S_IFDIR {
		if err := errnoOf(C.ag_unlinkat(C.int(dfd), cname, 0)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed(err)
		}
		return
	}
	if perm := uint32(mode) & 0o7777; perm&0o700 != 0o700 {
		if err := errnoOf(C.ag_chmodat(C.int(dfd), cname, C.uint32_t(perm|0o700))); err != nil {
			failed(err)
			return
		}
	}
	fd := C.ag_openat_dir(C.int(dfd), cname)
	if err := errnoOf(fd); err != nil {
		failed(err)
		return
	}
	dir := os.NewFile(uintptr(fd), name) // owns fd: closed below
	names, err := dir.Readdirnames(-1)
	if err != nil {
		failed(err)
	}
	var below firstErrors
	for _, child := range names {
		removeAt(int(fd), child, raced, &below)
	}
	dir.Close()
	if below.first != nil {
		if b, ok := below.first.(*belowError); ok {
			b.path = append([]string{name}, b.path...)
			errs.add(b)
		} else {
			errs.add(&belowError{path: []string{name}, err: below.first})
		}
		errs.more += below.more
		return
	}
	if err := errnoOf(C.ag_unlinkat(C.int(dfd), cname, 1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		failed(err)
	}
}
