package sandbox

import (
	"syscall"
	"unsafe"
)

// ipcProbe creates the named POSIX shared memory object (kind "shm") or semaphore ("sem"), then closes and unlinks
// it: the raw system calls, so the probe needs no tool beyond the test binary.
func ipcProbe(kind, name string) error {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	ptr := uintptr(unsafe.Pointer(p))
	switch kind {
	case "shm":
		fd, _, errno := syscall.Syscall(syscall.SYS_SHM_OPEN, ptr, uintptr(syscall.O_CREAT|syscall.O_RDWR), 0o600)
		if errno != 0 {
			return errno
		}
		syscall.Close(int(fd))
		syscall.Syscall(syscall.SYS_SHM_UNLINK, ptr, 0, 0)
	case "sem":
		sem, _, errno := syscall.Syscall6(syscall.SYS_SEM_OPEN, ptr, uintptr(syscall.O_CREAT), 0o600, 1, 0, 0)
		if errno != 0 {
			return errno
		}
		syscall.Syscall(syscall.SYS_SEM_CLOSE, sem, 0, 0)
		syscall.Syscall(syscall.SYS_SEM_UNLINK, ptr, 0, 0)
	default:
		return syscall.EINVAL
	}
	return nil
}
