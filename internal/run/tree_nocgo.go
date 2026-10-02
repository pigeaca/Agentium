//go:build !(cgo && (darwin || linux))

package run

import "os"

// removeTreeAt without cgo (Agentium's SQLite driver needs cgo, so only a partial build lands here) is os.RemoveAll,
// which never follows a link either but clears nothing that resists removal: such a folder is quarantined.
func removeTreeAt(root string) error {
	return os.RemoveAll(root)
}

// removeTreeRacing is removeTreeAt; raced is never called (see the cgo build).
func removeTreeRacing(root string, raced func(name string)) error {
	return os.RemoveAll(root)
}
