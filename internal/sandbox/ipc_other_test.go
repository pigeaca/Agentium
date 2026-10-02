//go:build !darwin

package sandbox

import "errors"

// ipcProbe is darwin's only: the sandbox tests that call it skip elsewhere.
func ipcProbe(kind, name string) error { return errors.New("POSIX IPC probes run on macOS only") }
