// Package source gives read-only views of a repository state, either a commit or the working tree, so context can be
// resolved the same way from either.
package source

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pigeaca/agentium/internal/gitx"
)

// Source is a repository state: its files (slash-separated paths relative to the root) and their contents. Symbolic
// links are followed within the repository, since that is what an agent reading the file sees.
type Source interface {
	Paths() []string // sorted
	ReadFile(path string) ([]byte, error)
	Executable(path string) bool
	Describe() string // e.g. "working tree" or "commit 1a2b3c4d5e6f"
}

// WorkingTree reads the repository at root as it is on disk: tracked files plus untracked files git does not ignore.
// Ignored files (such as CLAUDE.local.md in most repositories) are invisible, as they are to experiments, and so is
// anything a symbolic link reaches outside those files, as in a commit.
func WorkingTree(ctx context.Context, root string) (Source, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", root, err)
	}
	out, err := gitx.Output(ctx, nil, "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var existing []string
	for _, p := range splitNUL(string(out)) { // tracked files deleted from disk are gone; submodules are directories
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil && (info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
			existing = append(existing, p)
		}
	}
	return &workingTree{root: realRoot, paths: dedupe(existing)}, nil
}

type workingTree struct {
	root  string // symlinks resolved
	paths []string
}

func (w *workingTree) Paths() []string  { return w.paths }
func (w *workingTree) Describe() string { return "working tree" }

// ReadFile follows symbolic links only to other files of the working tree, so a link to a personal file (in the home
// folder, or ignored like CLAUDE.local.md) is never read into a snapshot.
func (w *workingTree) ReadFile(p string) ([]byte, error) {
	if !Has(w, p) {
		return nil, fmt.Errorf("%s: %w", p, os.ErrNotExist)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(w.root, filepath.FromSlash(p)))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(w.root, resolved)
	if err != nil || !Has(w, filepath.ToSlash(rel)) {
		return nil, fmt.Errorf("%s: symbolic link points outside the repository's files", p)
	}
	return os.ReadFile(resolved)
}
func (w *workingTree) Executable(p string) bool {
	info, err := os.Stat(filepath.Join(w.root, filepath.FromSlash(p)))
	return err == nil && info.Mode().Perm()&0o111 != 0
}

// Commit reads commit through git, located by where (e.g. "-C", root or "--git-dir", bare).
func Commit(ctx context.Context, commit string, where ...string) (Source, error) {
	return CommitEnv(ctx, nil, commit, where...)
}

// CommitEnv is Commit with extra environment variables on every git call it makes, listing and reads alike (for
// example GIT_NO_LAZY_FETCH=1, so a read never fetches a missing object from a promisor remote).
func CommitEnv(ctx context.Context, env []string, commit string, where ...string) (Source, error) {
	files, err := list(ctx, env, commit, where)
	if err != nil {
		return nil, err
	}
	return &commitSource{ctx: ctx, env: env, where: where, commit: commit, listing: files}, nil
}

// listing is a commit's files: their sorted paths, and each one's mode and blob ID. Nothing changes it once made.
type listing struct {
	paths []string
	modes map[string]string
	ids   map[string]string
}

// list reads commit's files through git.
func list(ctx context.Context, env []string, commit string, where []string) (listing, error) {
	out, err := gitx.OutputEnv(ctx, env, nil, append(where, "ls-tree", "-r", "-z", commit)...)
	if err != nil {
		return listing{}, err
	}
	files := listing{modes: map[string]string{}, ids: map[string]string{}}
	for _, entry := range splitNUL(string(out)) { // "<mode> <type> <object>\t<path>"
		meta, p, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" { // submodules ("commit") are not files
			continue
		}
		files.modes[p], files.ids[p] = fields[0], fields[2]
		files.paths = append(files.paths, p)
	}
	files.paths = dedupe(files.paths)
	return files, nil
}

type commitSource struct {
	ctx    context.Context // Source methods take no context; reads use the one the view was created with
	env    []string
	where  []string
	commit string
	listing
	objects *Objects // keeps the blobs read, for the sources it made; nil for a source of Commit or CommitEnv
}

func (c *commitSource) Paths() []string { return c.paths }
func (c *commitSource) Describe() string {
	return "commit " + c.commit[:min(12, len(c.commit))]
}
func (c *commitSource) Executable(p string) bool { return c.modes[p] == "100755" }
func (c *commitSource) ReadFile(p string) ([]byte, error) {
	for hops := 0; hops < 8; hops++ {
		mode, ok := c.modes[p]
		if !ok {
			return nil, fmt.Errorf("%s: %w", p, os.ErrNotExist)
		}
		data, err := c.blob(p)
		if err != nil || mode != "120000" {
			return data, err
		}
		target := path.Clean(path.Join(path.Dir(p), string(data))) // a symlink blob holds its target
		if strings.HasPrefix(target, "../") || path.IsAbs(string(data)) {
			return nil, fmt.Errorf("%s: symbolic link points outside the repository", p)
		}
		p = target
	}
	return nil, fmt.Errorf("%s: too many symbolic links", p)
}

// blob reads the blob the listing has at p: through git, or once per blob for a source of an Objects.
func (c *commitSource) blob(p string) ([]byte, error) {
	return c.objects.blob(c.ctx, c.ids[p], func() ([]byte, error) {
		return gitx.OutputEnv(c.ctx, c.env, nil, append(c.where, "cat-file", "blob", c.commit+":"+p)...)
	})
}

// Link reports whether p is a symbolic link in src and, if it is, the target as stored: not cleaned, not followed and
// possibly outside the repository. Sources that do not record links (in-memory test sources) report false. Only the
// link itself is read, through git for a commit, so a hostile target is data, never opened.
func Link(src Source, p string) (target string, ok bool, err error) {
	if l, isLinker := src.(linker); isLinker && Has(src, p) {
		return l.link(p)
	}
	return "", false, nil
}

// linker is a Source that knows which of its files are symbolic links.
type linker interface {
	link(p string) (string, bool, error)
}

func (c *commitSource) link(p string) (string, bool, error) {
	if c.modes[p] != "120000" {
		return "", false, nil
	}
	data, err := c.blob(p)
	if err != nil {
		return "", true, fmt.Errorf("read the symbolic link %s: %w", p, err)
	}
	return string(data), true, nil
}

func (w *workingTree) link(p string) (string, bool, error) {
	full := filepath.Join(w.root, filepath.FromSlash(p))
	if info, err := os.Lstat(full); err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false, nil
	}
	target, err := os.Readlink(full)
	if err != nil {
		return "", true, fmt.Errorf("read the symbolic link %s: %w", p, err)
	}
	return filepath.ToSlash(target), true, nil
}

// Has reports whether p is a file in src.
func Has(src Source, p string) bool {
	paths := src.Paths()
	i := sort.SearchStrings(paths, p)
	return i < len(paths) && paths[i] == p
}

func splitNUL(text string) []string {
	var parts []string
	for _, part := range strings.Split(text, "\x00") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func dedupe(paths []string) []string {
	sort.Strings(paths)
	out := paths[:0]
	for i, p := range paths {
		if i == 0 || p != paths[i-1] {
			out = append(out, p)
		}
	}
	return out
}
