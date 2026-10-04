package buildtool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A monorepo project measures one module: a folder of the repository (store.Settings.Module, slash-separated and
// relative to the root) whose build files decide its profiles. Everything keyed per project (warm-up stamps, Python
// venvs, grading seeds) is keyed by the module too, so two modules of one repository never share what the other warmed.

// ModuleKey is the part of a stamp's, a venv's or a seed's key that names the module: "" for the repository's root (the
// keys are then exactly what they were before modules), else "m" and a short hash of the path, which is safe in a file
// name whatever the path holds.
func ModuleKey(module string) string {
	if module == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(module))
	return "m" + hex.EncodeToString(sum[:])[:8]
}

// moduleVenvKey is a venv's key (the hash of its inputs) for a module: unchanged at the root.
func moduleVenvKey(key, module string) string {
	if module == "" {
		return key
	}
	sum := sha256.Sum256([]byte(key + "\x00" + module))
	return hex.EncodeToString(sum[:])[:16]
}

// InModule is paths (slash-separated, relative to the repository root) narrowed to the module's folder and made
// relative to it. With no module it is paths itself.
func InModule(paths []string, module string) []string {
	if module == "" {
		return paths
	}
	prefix := strings.TrimSuffix(module, "/") + "/"
	var out []string
	for _, p := range paths {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			out = append(out, rest)
		}
	}
	return out
}

// ModuleDir is the folder commands run in for a checkout: the module's folder inside it (checkout itself without a
// module). The checkout is the agent's to change, so the path is checked as it is now, component by component, with
// Lstat: every component must exist and be a real folder, and none may be a link, which could lead anywhere (a
// module the agent deleted, or replaced by a link to another folder). It never follows a link.
func ModuleDir(checkout, module string) (string, error) {
	if module == "" {
		return checkout, nil
	}
	dir := checkout
	for _, part := range strings.Split(module, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("module %q is not a plain relative folder", module)
		}
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("module %q: %s is missing from the checkout", module, part)
		case err != nil:
			// The error names the checkout's location, which notes (shared with reports) must not: only what failed.
			var pe *fs.PathError
			if errors.As(err, &pe) {
				return "", fmt.Errorf("module %q: %s: %w", module, part, pe.Err)
			}
			return "", fmt.Errorf("module %q: %w", module, err)
		case info.Mode()&fs.ModeSymlink != 0:
			return "", fmt.Errorf("module %q: %s is a link, not a folder", module, part)
		case !info.IsDir():
			return "", fmt.Errorf("module %q: %s is not a folder", module, part)
		}
	}
	return dir, nil
}
