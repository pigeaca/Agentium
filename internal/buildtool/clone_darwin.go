//go:build darwin && cgo

package buildtool

/*
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <sys/clonefile.h>

// ag_clonefile clones src to dst with clonefileat(2), through libSystem (the interface Apple promises), never following
// a link at src, the clone owned by the caller. It returns 0 or the errno.
static int ag_clonefile(const char *src, const char *dst) {
	return clonefileat(AT_FDCWD, src, AT_FDCWD, dst, CLONE_NOFOLLOW | CLONE_NOOWNERCOPY) == 0 ? 0 : errno;
}
*/
import "C"

import (
	"syscall"
	"unsafe"
)

// cloneFile clones the folder src to dst, which must not exist, in one clonefileat(2) call: the whole tree at once,
// copy-on-write. It reports errCloneUnsupported where the file system cannot clone (not APFS, or src and dst on two
// volumes), so the caller copies instead. cgo is already required (the SQLite driver), so this adds no dependency.
func cloneFile(src, dst string) (bool, error) {
	from, to := C.CString(src), C.CString(dst)
	defer C.free(unsafe.Pointer(from))
	defer C.free(unsafe.Pointer(to))
	switch errno := syscall.Errno(C.ag_clonefile(from, to)); errno {
	case 0:
		return true, nil
	case syscall.ENOTSUP, syscall.EXDEV:
		return false, errCloneUnsupported
	default:
		return false, errno // EEXIST is os.ErrExist
	}
}
