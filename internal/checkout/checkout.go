// Package checkout makes working copies for validation and runs: a fresh repository holding one commit (depth 1)
// fetched from Agentium's bare repository, with no hooks. Through git, nothing else there (other commits, hidden tests,
// reference solutions) is reachable from the copy, and the copy does not record where it came from. The file system is
// another matter: copies live in the data folder next to the bare repository, so agent runs must also be denied it.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/source"
)

// New creates dir, which must not exist yet, as a checkout of commit from the bare repository. On failure, dir is
// removed.
func New(ctx context.Context, bare, commit, dir string) (err error) {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("checkout %s: already exists", dir)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	// An empty --template: the user's init.templateDir (which may hold hooks) is not copied in.
	if _, err := gitx.Run(ctx, "init", "--quiet", "--template=", dir); err != nil {
		return err
	}
	if err := gitx.FetchCommit(ctx, bare, commit, "", "-C", dir); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, "-C", dir, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", commit); err != nil {
		return err
	}
	// FETCH_HEAD names the bare repository, which holds hidden tests and solutions.
	if err := os.Remove(filepath.Join(dir, ".git", "FETCH_HEAD")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checkout %s: %w", dir, err)
	}
	return nil
}

// Write makes each of paths in dir match src: written with src's content and executable bit (symbolic links are
// written as the files they point to), or deleted when src does not have it.
func Write(dir string, src source.Source, paths []string) error {
	for _, p := range paths {
		if err := safePath(p); err != nil {
			return err
		}
		if err := noSymlinkedParent(dir, p); err != nil {
			return err
		}
		full := filepath.Join(dir, filepath.FromSlash(p))
		if !source.Has(src, p) {
			if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("delete %s: %w", p, err)
			}
			continue
		}
		data, err := src.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s from %s: %w", p, src.Describe(), err)
		}
		mode := os.FileMode(0o644)
		if src.Executable(p) {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) { // replaces a symlink, never follows it
			return fmt.Errorf("write %s: %w", p, err)
		}
		if err := os.WriteFile(full, data, mode); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}
	return nil
}

// noSymlinkedParent refuses a path whose folders include a symbolic link: writing through it (a repository may contain
// pkg -> /elsewhere) would leave the checkout.
func noSymlinkedParent(dir, p string) error {
	current := dir
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil // created fresh by MkdirAll
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("write %s: %s is a symbolic link", p, strings.Join(parts[:len(parts)-1], "/"))
		}
	}
	return nil
}

// safePath refuses paths that could leave the checkout or reach its .git folder.
func safePath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("unsafe path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("unsafe path %q", p)
		}
	}
	return nil
}
