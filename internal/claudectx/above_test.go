package claudectx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestInstructionFilesAbove(t *testing.T) {
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	found := InstructionFilesAbove(filepath.Join(outer, "data", "workspaces", "r1", "repo"))
	if !slices.Contains(found, filepath.Join(outer, "AGENTS.md")) {
		t.Errorf("found = %v", found)
	}
}
