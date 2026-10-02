//go:build cgo

package run

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// F-E: a grade's folder swapped for a link never widens the sweep to where the link points: only the folder's parent
// is resolved (/var to /private/var), never the folder itself.
func TestSweepRootsResolveOnlyTheParent(t *testing.T) {
	dir := t.TempDir()
	users := filepath.Join(dir, "users")
	must(t, os.MkdirAll(users, 0o700))
	grade := filepath.Join(dir, "grade")
	must(t, os.Symlink(users, grade))
	roots := sweepRoots([]string{grade})
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(roots, grade) || !slices.Contains(roots, filepath.Join(realDir, "grade")) {
		t.Errorf("roots = %q", roots)
	}
	for _, r := range roots {
		if underAny(filepath.Join(realDir, "users", "f"), []string{r}) {
			t.Errorf("root %s reaches the link's target", r)
		}
	}
	if p, ok := startOf(os.Getpid()); !ok || p.command == "" || p.sec == 0 {
		t.Errorf("startOf(self) = %+v, %t", p, ok)
	}
}
