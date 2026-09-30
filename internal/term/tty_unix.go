//go:build darwin || linux

package term

import (
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether f is a terminal. It asks for the window size, which only a terminal answers; unlike a
// character-device check, /dev/null is not a terminal.
func IsTerminal(f *os.File) bool {
	_, ok := winsize(f)
	return ok
}

// Columns is the width of the terminal f, or 0 when f is not a terminal or reports no width.
func Columns(f *os.File) int {
	cols, _ := winsize(f)
	return cols
}

func winsize(f *os.File) (cols int, ok bool) {
	conn, err := f.SyscallConn()
	if err != nil {
		return 0, false
	}
	var ws struct{ rows, cols, xpixel, ypixel uint16 }
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	}); err != nil || errno != 0 {
		return 0, false
	}
	return int(ws.cols), true
}
