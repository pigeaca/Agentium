// Package home locates Agentium's data folder. It lives outside every user repository, so Agentium never writes to the
// repositories it measures.
package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// Ensure creates missing folders readable only by the owner: transcripts and checkouts contain source code. It never
// changes an existing folder (AGENTIUM_HOME may point anywhere, even at the home folder); an existing data folder that
// other users can open is refused instead.
func (l Layout) Ensure() error {
	info, err := os.Stat(l.Root)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("data folder %s is not a folder", l.Root)
	case err == nil && info.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("data folder %s is open to other users (mode %04o): run `chmod 700 %s`, or set AGENTIUM_HOME to a new folder",
			l.Root, info.Mode().Perm(), l.Root)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("data folder %s: %w", l.Root, err)
	}
	if err := os.MkdirAll(filepath.Dir(l.Root), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(l.Root), err)
	}
	for _, dir := range []string{l.Root, l.Artifacts} {
		err := os.Mkdir(dir, 0o700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil { // Mkdir applies the umask; this folder is new, so restrict it
			return fmt.Errorf("restrict %s: %w", dir, err)
		}
	}
	return nil
}

// CheckOutside refuses a data folder that is the repository at repoRoot (symlinks resolved) or inside it: writing
// there would change the repository Agentium promises only to read. Call it before Ensure creates anything.
func (l Layout) CheckOutside(repoRoot string) error {
	// Lower case: on case-insensitive file systems (macOS, Windows) "Repo" and "repo" are the same folder.
	rel, err := filepath.Rel(strings.ToLower(repoRoot), strings.ToLower(realPath(l.Root)))
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("the data folder %s is inside the repository %s: set AGENTIUM_HOME to a folder outside it", l.Root, repoRoot)
	}
	return nil
}

// realPath resolves symbolic links in the longest existing prefix of p, which may not exist yet.
func realPath(p string) string {
	var missing []string
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, missing...)...)
		}
		missing = append([]string{filepath.Base(p)}, missing...)
		p = parent
	}
}
