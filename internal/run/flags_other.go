//go:build !darwin

package run

import "os"

// clearFlags does nothing outside macOS: Linux's immutable attribute needs a privilege a grade does not have.
func clearFlags(string, os.FileInfo) {}
