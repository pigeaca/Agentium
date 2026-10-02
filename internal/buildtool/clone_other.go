//go:build !darwin

package buildtool

// cloneFile has no clonefile(2) outside macOS: the caller copies (copyTree: a reflink copy on Linux where the file
// system can).
func cloneFile(src, dst string) (bool, error) {
	return false, errCloneUnsupported
}
