// Package snapshot saves versions of a project's context as commits in Agentium's own bare repository (one per
// project, in the data folder), so versions can be listed, diffed and applied to run checkouts. The user's
// repository is only read.
package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/source"
)

// NamePattern is what snapshot names may look like; ValidName adds git's ref-name rules.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidName reports whether name can name a snapshot (it also names the git ref refs/agentium/snapshots/<name>).
func ValidName(name string) bool {
	return NamePattern.MatchString(name) && !strings.Contains(name, "..") && !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, ".lock")
}

// KindIncluded marks a document added to a snapshot with --include: not loaded by Claude Code, but part of the version.
const KindIncluded = "included"

// File is one context file in a snapshot.
type File struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Bytes        int    `json:"bytes"`
	StartupBytes int    `json:"startup_bytes"`
	SHA256       string `json:"sha256"`
}

// Manifest describes a snapshot's files and what loads at session start.
type Manifest struct {
	Files        []File   `json:"files"`
	StartupBytes int      `json:"startup_bytes"`
	Warnings     []string `json:"warnings"`
	// Module is the monorepo folder the context was resolved for (BuildIn): what a session started there loads. Empty
	// (and absent from the JSON, so older manifests read the same): the repository's root.
	Module string `json:"module,omitempty"`
}

// Paths lists the manifest's files.
func (m Manifest) Paths() []string {
	paths := make([]string, len(m.Files))
	for i, f := range m.Files {
		paths[i] = f.Path
	}
	sort.Strings(paths)
	return paths
}

// Build writes the context of src, plus the documents in include, into bare as a parentless commit whose tree holds
// only those files. Commit metadata is fixed, so identical content and message give the identical commit.
func Build(ctx context.Context, bare string, src source.Source, message string, include []string) (string, Manifest, error) {
	return BuildIn(ctx, bare, src, message, include, "")
}

// BuildIn is Build of the context a session started in module loads (claudectx.ResolveIn): the root's and the module's
// instruction files and .claude folders, and the folders' between. The manifest records the module.
func BuildIn(ctx context.Context, bare string, src source.Source, message string, include []string, module string) (string, Manifest, error) {
	resolved, err := claudectx.ResolveIn(src, module)
	if err != nil {
		return "", Manifest{}, err
	}
	entries := resolved.Entries
	for _, p := range include {
		p = path.Clean(p)
		switch {
		case !source.Has(src, p):
			return "", Manifest{}, fmt.Errorf("--include %s: no such file in %s", p, src.Describe())
		case !claudectx.IsDocument(p):
			return "", Manifest{}, fmt.Errorf("--include %s: only documents (Markdown, reStructuredText, AsciiDoc; not test data) can be included", p)
		case !slices.Contains(resolved.Paths(), p):
			data, err := src.ReadFile(p)
			if err != nil {
				return "", Manifest{}, fmt.Errorf("--include %s: %w", p, err)
			}
			entries = append(entries, claudectx.Entry{Path: p, Kind: KindIncluded, Bytes: len(data)})
			resolved.Entries = entries // keep Paths() current for duplicate includes
		}
	}
	manifest := Manifest{StartupBytes: resolved.StartupBytes(), Warnings: resolved.Warnings, Module: module}
	var index strings.Builder
	for _, entry := range entries {
		data, err := src.ReadFile(entry.Path)
		if err != nil {
			return "", Manifest{}, fmt.Errorf("read %s from %s: %w", entry.Path, src.Describe(), err)
		}
		blob, err := gitx.Output(ctx, bytes.NewReader(data), "--git-dir", bare, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", Manifest{}, err
		}
		mode := "100644"
		if src.Executable(entry.Path) {
			mode = "100755"
		}
		fmt.Fprintf(&index, "%s %s\t%s\x00", mode, strings.TrimSpace(string(blob)), entry.Path) // NUL: any path is safe
		sum := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, File{Path: entry.Path, Kind: entry.Kind, Bytes: entry.Bytes,
			StartupBytes: entry.StartupBytes, SHA256: hex.EncodeToString(sum[:])})
	}
	scratch, err := os.MkdirTemp("", "agentium-index-")
	if err != nil {
		return "", Manifest{}, fmt.Errorf("temporary index: %w", err)
	}
	defer os.RemoveAll(scratch)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "index"),
		"GIT_AUTHOR_NAME=agentium", "GIT_AUTHOR_EMAIL=agentium@localhost", "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME=agentium", "GIT_COMMITTER_EMAIL=agentium@localhost", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"}
	if _, err := gitx.OutputEnv(ctx, env, strings.NewReader(index.String()), "--git-dir", bare, "update-index", "--add", "-z", "--index-info"); err != nil {
		return "", Manifest{}, err
	}
	tree, err := gitx.OutputEnv(ctx, env, nil, "--git-dir", bare, "write-tree")
	if err != nil {
		return "", Manifest{}, err
	}
	commit, err := gitx.OutputEnv(ctx, env, nil, "--git-dir", bare, "commit-tree", strings.TrimSpace(string(tree)), "-m", message)
	if err != nil {
		return "", Manifest{}, err
	}
	return strings.TrimSpace(string(commit)), manifest, nil
}

// Diff returns the file summary and the patch between two snapshot commits.
func Diff(ctx context.Context, bare, from, to string) (stat, patch string, err error) {
	// Personal diff settings (external tools, text conversion) must not change or run during the comparison.
	if stat, err = gitx.Run(ctx, "--git-dir", bare, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--stat", from, to); err != nil {
		return "", "", err
	}
	out, err := gitx.Output(ctx, nil, "--git-dir", bare, "diff", "--no-ext-diff", "--no-textconv", "--no-color", from, to)
	return stat, string(out), err
}

// Overlay turns a checkout of a task's base into an experiment arm: write every file of the snapshot, then delete the
// base's files that load by being present (instruction files, .claude, .mcp.json) and that the snapshot lacks. Files
// the base merely imports stay: they are repository files that simply stop loading.
type Overlay struct {
	Writes  []string
	Deletes []string
	// HarnessChanged lists settings, hooks and MCP files that differ between the base and the arm: they change what
	// runs, not what the model reads, so experiments must report them.
	HarnessChanged []string
}

// ErrTouchesNonContext means a snapshot would change a base file that is neither an instruction file nor a document
// (for example a CLAUDE.md importing @package.json with other content), so arms would differ in what they build and
// test, not only in their context.
var ErrTouchesNonContext = errors.New("the snapshot changes files that are code or configuration in the base")

// ErrArmMismatch means the base with the snapshot applied would not load exactly the snapshot's context.
var ErrArmMismatch = errors.New("the base with the snapshot applied would load a different context")

// PlanOverlay works out how to apply snap (a snapshot commit, holding only context files) to a checkout of base, and
// checks the result: it refuses to change base files other than instruction files and documents, even ones the base
// imports, and the arm must load exactly the snapshot's context.
func PlanOverlay(base, snap source.Source) (Overlay, error) {
	return PlanOverlayIn(base, snap, "")
}

// PlanOverlayIn is PlanOverlay for a task in module (store.Task.Module), whose agent starts in the module's folder:
// what loads, and so what is deleted when snap lacks it and what the arm must load, is what a session started there
// loads (claudectx.ResolveIn, LoadsByPresenceIn). snap's own module (Manifest.Module) does not matter: a snapshot
// is a set of files, and an arm loads exactly what a session in the task's module would load of them. At the root it
// is PlanOverlay.
func PlanOverlayIn(base, snap source.Source, module string) (Overlay, error) {
	baseContext, err := claudectx.ResolveIn(base, module)
	if err != nil {
		return Overlay{}, err
	}
	overlay := Overlay{Writes: snap.Paths()}
	var conflicts []string
	for _, p := range overlay.Writes {
		if claudectx.LoadsByPresenceIn(p, module) || claudectx.IsDocument(p) || !source.Has(base, p) {
			continue
		}
		baseData, err := base.ReadFile(p)
		if err != nil {
			return Overlay{}, fmt.Errorf("read %s from %s: %w", p, base.Describe(), err)
		}
		snapData, err := snap.ReadFile(p)
		if err != nil {
			return Overlay{}, fmt.Errorf("read %s from %s: %w", p, snap.Describe(), err)
		}
		if !bytes.Equal(baseData, snapData) {
			conflicts = append(conflicts, p)
		}
	}
	if len(conflicts) > 0 {
		return Overlay{}, fmt.Errorf("%w: %s", ErrTouchesNonContext, strings.Join(conflicts, ", "))
	}
	for _, p := range baseContext.Paths() {
		if !source.Has(snap, p) && claudectx.LoadsByPresenceIn(p, module) {
			overlay.Deletes = append(overlay.Deletes, p)
		}
	}
	arm, err := claudectx.ResolveIn(&applied{base: base, snap: snap, deleted: overlay.Deletes}, module)
	if err != nil {
		return Overlay{}, err
	}
	want, err := claudectx.ResolveIn(snap, module)
	if err != nil {
		return Overlay{}, err
	}
	if got, wanted := arm.Paths(), want.Paths(); !slices.Equal(got, wanted) {
		return Overlay{}, fmt.Errorf("%w: it would load %s instead of %s", ErrArmMismatch, strings.Join(got, ", "), strings.Join(wanted, ", "))
	}
	if overlay.HarnessChanged, err = harnessChanges(base, snap, baseContext, want); err != nil {
		return Overlay{}, err
	}
	return overlay, nil
}

// Apply returns base as an arm's checkout holds it: snap written over it, and the files that load by being present but
// that snap lacks deleted. It makes PlanOverlay's checks, so it refuses what an arm would refuse.
func Apply(base, snap source.Source) (source.Source, error) {
	return ApplyIn(base, snap, "")
}

// ApplyIn is Apply for a task in module (PlanOverlayIn).
func ApplyIn(base, snap source.Source, module string) (source.Source, error) {
	overlay, err := PlanOverlayIn(base, snap, module)
	if err != nil {
		return nil, err
	}
	return &applied{base: base, snap: snap, deleted: overlay.Deletes}, nil
}

// harnessChanges lists harness files added, removed or changed between base and snap.
func harnessChanges(base, snap source.Source, baseContext, snapContext claudectx.Context) ([]string, error) {
	paths := map[string]bool{}
	for _, c := range []claudectx.Context{baseContext, snapContext} {
		for _, e := range c.Entries {
			if e.Kind == claudectx.KindHarness {
				paths[e.Path] = true
			}
		}
	}
	var changed []string
	for p := range paths {
		if !source.Has(base, p) || !source.Has(snap, p) {
			changed = append(changed, p)
			continue
		}
		a, err := base.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s from %s: %w", p, base.Describe(), err)
		}
		b, err := snap.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s from %s: %w", p, snap.Describe(), err)
		}
		if !bytes.Equal(a, b) {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// applied is base with a snapshot written over it and some files deleted, as an arm's checkout would be.
type applied struct {
	base, snap source.Source
	deleted    []string
}

func (a *applied) Paths() []string {
	var paths []string
	for _, p := range a.base.Paths() {
		if !slices.Contains(a.deleted, p) {
			paths = append(paths, p)
		}
	}
	paths = append(paths, a.snap.Paths()...)
	sort.Strings(paths)
	return slices.Compact(paths)
}
func (a *applied) ReadFile(p string) ([]byte, error) {
	if source.Has(a.snap, p) {
		return a.snap.ReadFile(p)
	}
	return a.base.ReadFile(p)
}
func (a *applied) Executable(p string) bool {
	if source.Has(a.snap, p) {
		return a.snap.Executable(p)
	}
	return a.base.Executable(p)
}
func (a *applied) Describe() string { return a.snap.Describe() + " over " + a.base.Describe() }

// UncapturedChanges lists working-tree changes (against HEAD, including untracked files) that are not context, so
// they are not part of a snapshot taken from the working tree. git status may apply clean filters that the user's own
// git config defines (git-lfs, for example), as any git status would; a repository cannot define one itself.
func UncapturedChanges(ctx context.Context, root string, contextPaths []string) ([]string, error) {
	out, err := gitx.Output(ctx, nil, "-C", root, "status", "--porcelain", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=all")
	if err != nil {
		return nil, err
	}
	captured := map[string]bool{}
	for _, p := range contextPaths {
		captured[p] = true
	}
	var changes []string
	for _, record := range strings.Split(string(out), "\x00") {
		if len(record) < 4 { // "XY path"
			continue
		}
		if p := record[3:]; !captured[p] {
			changes = append(changes, p)
		}
	}
	sort.Strings(changes)
	return changes, nil
}
