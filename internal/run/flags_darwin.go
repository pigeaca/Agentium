package run

import (
	"os"
	"syscall"
)

// clearFlags clears a file's or folder's flags (chflags), which removal needs gone: the user's immutable and
// append-only flags (uchg, uappnd) make unlink fail even for the owner. info is p's Lstat: links are never passed here,
// since chflags(2) follows them.
func clearFlags(p string, info os.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Flags != 0 {
		syscall.Chflags(p, 0)
	}
}
