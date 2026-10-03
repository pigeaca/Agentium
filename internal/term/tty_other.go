//go:build !darwin && !linux

package term

import "os"

// IsTerminal reports false: terminal detection is implemented for macOS and Linux only.
func IsTerminal(*os.File) bool { return false }

// Columns reports 0: terminal detection is implemented for macOS and Linux only.
func Columns(*os.File) int { return 0 }

// Size reports zeros: terminal detection is implemented for macOS and Linux only.
func Size(*os.File) (cols, rows int) { return 0, 0 }
