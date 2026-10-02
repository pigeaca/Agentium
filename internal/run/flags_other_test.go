//go:build !darwin

package run

import "os"

// setFlags and fileFlags: file flags are macOS's (the tests that need them skip elsewhere).
func setFlags(string, int) error { return nil }

func fileFlags(os.FileInfo) uint32 { return 0 }
