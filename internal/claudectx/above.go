package claudectx

import (
	"os"
	"path/filepath"
)

// InstructionFilesAbove lists the instruction files (CLAUDE.md, CLAUDE.local.md, AGENTS.md) in the folders above dir,
// which Claude Code would load into a session started in dir. Runs and judge calls refuse to start when any exist.
func InstructionFilesAbove(dir string) []string {
	var found []string
	for d := filepath.Dir(dir); d != filepath.Dir(d); d = filepath.Dir(d) {
		for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"} {
			if info, err := os.Stat(filepath.Join(d, name)); err == nil && !info.IsDir() {
				found = append(found, filepath.Join(d, name))
			}
		}
	}
	return found
}
