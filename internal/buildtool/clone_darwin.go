package buildtool

import (
	"syscall"
	"unsafe"
)

// clonefileat(2), by number: the syscall package has no wrapper, and golang.org/x/sys would be a new module. Darwin's
// system call numbers are not a promised interface (libSystem is), but this one has been 462 since clonefile came, in
// macOS 10.12, and x/sys/unix's table (SYS_CLONEFILEAT) has the same. syscall.Syscall6 adds the BSD class on amd64.
// The flags are <sys/clonefile.h>'s.
const (
	sysClonefileat   = 462
	atFDCWD          = -2     // AT_FDCWD on Darwin
	cloneNoFollow    = 0x0001 // CLONE_NOFOLLOW: a link at src is cloned as the link, never followed
	cloneNoOwnerCopy = 0x0002 // CLONE_NOOWNERCOPY: the clone belongs to the caller
)

// cloneFile clones the folder src to dst, which must not exist, in one clonefile(2) call: the whole tree at once,
// copy-on-write. It reports errCloneUnsupported where the file system cannot clone (not APFS, or src and dst on two
// volumes), so the caller copies instead.
func cloneFile(src, dst string) (bool, error) {
	from, err := syscall.BytePtrFromString(src)
	if err != nil {
		return false, err
	}
	to, err := syscall.BytePtrFromString(dst)
	if err != nil {
		return false, err
	}
	fd := atFDCWD
	_, _, errno := syscall.Syscall6(sysClonefileat, uintptr(fd), uintptr(unsafe.Pointer(from)), uintptr(fd), uintptr(unsafe.Pointer(to)),
		cloneNoFollow|cloneNoOwnerCopy, 0)
	switch {
	case errno == 0:
		return true, nil
	case errno == syscall.ENOTSUP || errno == syscall.EXDEV || errno == syscall.ENOSYS:
		return false, errCloneUnsupported
	}
	return false, errno // EEXIST is os.ErrExist
}
