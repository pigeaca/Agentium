// Package home locates Agentium's data folder. It lives outside every user repository, so Agentium never writes to the
// repositories it measures.
package home

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout is the data folder: the SQLite database and per-project artifacts (checkouts, transcripts, reports).
type Layout struct {
	Root      string
	Database  string
	Artifacts string
}

// Resolve returns the layout under $AGENTIUM_HOME, or ~/.agentium when that is unset. getenv is os.Getenv outside tests.
func Resolve(getenv func(string) string) (Layout, error) {
	root := getenv("AGENTIUM_HOME")
	if root == "" {
		userHome := getenv("HOME")
		if userHome == "" {
			return Layout{}, fmt.Errorf("resolve data folder: neither AGENTIUM_HOME nor HOME is set")
		}
		root = filepath.Join(userHome, ".agentium")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve data folder %q: %w", root, err)
	}
	return Layout{Root: root, Database: filepath.Join(root, "agentium.db"), Artifacts: filepath.Join(root, "artifacts")}, nil
}

// Ensure creates the folders readable only by the owner: transcripts and checkouts contain source code.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.Artifacts} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restrict %s: %w", dir, err)
		}
	}
	return nil
}
