package codex

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ProbeFile is the instruction file a calibration appends its codeword to, as a path relative to the checkout repo
// (slash-separated), or "" when Codex loads none at start. Codex loads AGENTS.md from the repository's root down to
// the folder it starts in (module: "" for the root), where a folder's AGENTS.override.md replaces its AGENTS.md, up to
// 32 KiB in all (the plan's findings); this is the first file of that chain, the root's when there is one. Only
// regular files count: a link is not followed. This is no context resolver (the plan's step 4): what Codex loads in
// full, and in which order, is not checked here.
func ProbeFile(repo, module string) string {
	dirs := []string{""}
	if module != "" {
		parts := strings.Split(path.Clean(module), "/")
		for i := range parts {
			dirs = append(dirs, path.Join(parts[:i+1]...))
		}
	}
	for _, dir := range dirs {
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
			rel := path.Join(dir, name)
			if info, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(rel))); err == nil && info.Mode().IsRegular() {
				return rel
			}
		}
	}
	return ""
}
