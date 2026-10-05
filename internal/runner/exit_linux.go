package runner

import (
	"errors"
	"syscall"
	"unsafe"
)

// canWaitWithoutReaping: whether this system's waitExit works; without it Run refuses to start a command.
const canWaitWithoutReaping = true

// The waitid(2) values Run uses (linux/wait.h).
const (
	pPID     = 1
	wExited  = 0x4
	wNoWait  = 0x1000000
	siginfoN = 128
)

// waitExit returns once the child process pid has exited, without reaping it (waitid with WNOWAIT): it stays a zombie,
// holding its ID, until cmd.Wait.
func waitExit(pid int) error {
	var info [siginfoN]byte
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), wExited|wNoWait, 0, 0)
		if errno == 0 {
			return nil
		}
		if !errors.Is(errno, syscall.EINTR) {
			return errno
		}
	}
}
