//go:build !darwin || !cgo

package buildtool

// cloneFile has no clonefile(2) outside macOS (or without cgo, which Agentium's SQLite driver needs anyway): the caller
// copies (copyTree: a reflink copy on Linux where the file system can).
func cloneFile(src, dst string) (bool, error) {
	return false, errCloneUnsupported
}
