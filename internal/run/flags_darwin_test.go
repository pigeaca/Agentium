package run

import (
	"os"
	"syscall"
)

// setFlags is chflags(2); fileFlags reads st_flags.
func setFlags(path string, flags int) error { return syscall.Chflags(path, flags) }

func fileFlags(info os.FileInfo) uint32 { return info.Sys().(*syscall.Stat_t).Flags }
