package buildtool

import (
	"crypto/sha256"
	"encoding/hex"
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
