package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/store"
)

const contextUsage = `Usage:
  agentium context show [--ref REF]              what Claude Code loads (default: the working tree)
  agentium context snapshot NAME [--ref REF | --working-tree] [--include PATH]... [--include-linked]
                                                 save a version (default: --ref HEAD); --include adds a
                                                 document (Markdown, rst, AsciiDoc), --include-linked every
                                                 document the context links to
  agentium context list                          saved versions
  agentium context diff A B [--patch]            compare two saved versions
`

func runContext(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, contextUsage)
		return ExitUsage
	}
	switch args[0] {
	case "show":
		return contextShow(ctx, env, args[1:])
	case "snapshot":
		return contextSnapshot(ctx, env, args[1:])
	case "list":
		return contextList(ctx, env, args[1:])
	case "diff":
		return contextDiff(ctx, env, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, contextUsage)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "agentium context: unknown subcommand %q\n\n%s", args[0], contextUsage)
		return ExitUsage
	}
}

// workspace is a registered project opened for a command.
type workspace struct {
	db      *store.Store
	project store.Project
	layout  home.Layout
	root    string
	bare    string
}

// openProject opens the project containing env.Dir; it must have been registered with `agentium init`.
func openProject(ctx context.Context, env Env) (*workspace, error) {
	if env.Dir == "" {
		return nil, errors.New("the current folder cannot be read: run agentium inside your repository")
	}
	root, err := gitx.Run(ctx, "-C", env.Dir, "rev-parse", "--show-toplevel")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository", env.Dir)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
		return nil, err
	}
	if err := layout.CheckOutside(root); err != nil {
		return nil, err
	}
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, layout.Database)
	if err != nil {
		return nil, err
	}
	project, err := db.ProjectByRoot(ctx, root)
	if errors.Is(err, store.ErrNotFound) {
		db.Close()
		return nil, fmt.Errorf("%s is not registered: run `agentium init` first", root)
	} else if err != nil {
		db.Close()
		return nil, err
	}
	w := &workspace{db: db, project: project, layout: layout, root: root, bare: layout.ProjectRepo(project.ID)}
	if err := gitx.InitBare(ctx, w.bare); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}

func (w *workspace) Close() { w.db.Close() }

// read returns a read-only view of the working tree (ref == "") or of ref, and the commit HEAD or ref names. Nothing
// is copied: commits are read in place with ls-tree and cat-file.
func (w *workspace) read(ctx context.Context, ref string) (source.Source, string, error) {
	target := ref
	if target == "" {
		target = "HEAD"
	}
	// --end-of-options: a ref such as "--output=x" is a name to look up, never an option.
	commit, err := gitx.Run(ctx, "-C", w.root, "rev-parse", "--verify", "--quiet", "--end-of-options", target+"^{commit}")
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if err != nil || commit == "" {
		return nil, "", fmt.Errorf("%q is not a commit in %s", target, w.root)
	}
	if ref == "" {
		src, err := source.WorkingTree(ctx, w.root)
		return src, commit, err
	}
	src, err := source.Commit(ctx, commit, "-C", w.root)
	return src, commit, err
}

func contextShow(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("context show", flag.ContinueOnError)
	ref := fs.String("ref", "", "show the context at this commit instead of the working tree")
	rest, code, ok := parseArgs(env, fs, args, contextUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		fmt.Fprint(env.Stderr, contextUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	src, _, err := w.read(ctx, *ref)
	if err != nil {
		return fail(env, err)
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return fail(env, err)
	}
	printContext(env.Stdout, w.project.Name, src.Describe(), resolved)
	for _, file := range aboveRepository(w.root) {
		fmt.Fprintf(env.Stdout, "warning: %s is above the repository: Claude Code loads it when you work here, but it is not the project's context and experiments exclude it.\n", file)
	}
	return ExitOK
}

// aboveRepository lists instruction files in the folders above root, by name only (they are personal; never read).
func aboveRepository(root string) []string {
	var found []string
	for dir := filepath.Dir(root); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"} {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
				found = append(found, filepath.Join(dir, name))
			}
		}
	}
	return found
}

func printContext(out io.Writer, project, where string, resolved claudectx.Context) {
	startup := resolved.StartupBytes()
	fmt.Fprintf(out, "Claude Code context of %s (%s)\n", project, where)
	fmt.Fprintf(out, "At session start: about %d tokens (%s, estimated)\n", claudectx.EstimateTokens(startup), sizeLabel(int64(startup)))
	var onDemand, harness []claudectx.Entry
	for _, e := range resolved.Entries {
		switch {
		case e.Kind == claudectx.KindHarness:
			harness = append(harness, e)
		case e.StartupBytes == 0:
			onDemand = append(onDemand, e)
		default:
			note := ""
			if e.Via != "" {
				note = " via " + e.Via
			}
			if e.StartupBytes != e.Bytes {
				note += fmt.Sprintf(" (description only; %s in full)", sizeLabel(int64(e.Bytes)))
			}
			fmt.Fprintf(out, "  %-44s %-12s %8s%s\n", e.Path, e.Kind, sizeLabel(int64(e.StartupBytes)), note)
		}
	}
	if len(onDemand) > 0 {
		fmt.Fprintln(out, "On demand:")
		for _, e := range onDemand {
			fmt.Fprintf(out, "  %-44s %-12s %8s\n", e.Path, e.Kind, sizeLabel(int64(e.Bytes)))
		}
	}
	if len(harness) > 0 {
		fmt.Fprintln(out, "Harness (changes what runs, not what the model reads):")
		for _, e := range harness {
			fmt.Fprintf(out, "  %s\n", e.Path)
		}
	}
	if len(resolved.Linked) > 0 {
		fmt.Fprintln(out, "Linked (read only if the agent opens them; in a snapshot only with --include):")
		for _, p := range resolved.Linked {
			fmt.Fprintf(out, "  %s\n", p)
		}
	}
	for _, warning := range resolved.Warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func contextSnapshot(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("context snapshot", flag.ContinueOnError)
	ref := fs.String("ref", "", "snapshot the context at this commit (default HEAD)")
	workingTree := fs.Bool("working-tree", false, "snapshot the context in the working tree, including uncommitted edits")
	var include stringList
	fs.Var(&include, "include", "also capture this document (repeatable)")
	includeLinked := fs.Bool("include-linked", false, "also capture every document the context links to")
	rest, code, ok := parseArgs(env, fs, args, contextUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 || (*workingTree && *ref != "") {
		fmt.Fprint(env.Stderr, contextUsage)
		return ExitUsage
	}
	name := rest[0]
	if !snapshot.ValidName(name) {
		fmt.Fprintf(env.Stderr, "agentium context snapshot: name %q must be lowercase letters, digits, '.', '_' or '-' (up to 63; no \"..\", no trailing \".\" or \".lock\")\n", name)
		return ExitUsage
	}
	if !*workingTree && *ref == "" {
		*ref = "HEAD"
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	if _, err := w.db.SnapshotByName(ctx, w.project.ID, name); err == nil {
		return fail(env, fmt.Errorf("snapshot %q already exists; choose another name", name))
	}
	src, commit, err := w.read(ctx, *ref)
	if err != nil {
		return fail(env, err)
	}
	label := *ref
	if *workingTree {
		label = "working tree"
	}
	if *includeLinked {
		resolved, err := claudectx.Resolve(src)
		if err != nil {
			return fail(env, err)
		}
		include = append(include, resolved.Linked...) // documents only
	}
	commitID, manifest, err := snapshot.Build(ctx, w.bare, src, "snapshot "+name+" of "+label+" at "+commit, include)
	if err != nil {
		return fail(env, err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fail(env, fmt.Errorf("encode manifest: %w", err))
	}
	// The database decides who owns a name (a concurrent snapshot with the same name fails here); the ref then keeps
	// the commit from garbage collection.
	if _, err := w.db.SaveSnapshot(ctx, store.Snapshot{ProjectID: w.project.ID, Name: name, Source: label, SourceCommit: commit,
		CommitID: commitID, Manifest: encoded, CreatedAt: env.Now()}); errors.Is(err, store.ErrExists) {
		return fail(env, fmt.Errorf("snapshot %q already exists; choose another name", name))
	} else if err != nil {
		return fail(env, err)
	}
	if _, err := gitx.Run(ctx, "--git-dir", w.bare, "update-ref", "refs/agentium/snapshots/"+name, commitID); err != nil {
		return fail(env, errors.Join(err, w.db.DeleteSnapshot(ctx, w.project.ID, name)))
	}
	fmt.Fprintf(env.Stdout, "Saved snapshot %s from %s (%s): %d file(s); about %d tokens at session start\n",
		name, label, shortCommit(commit), len(manifest.Files), claudectx.EstimateTokens(manifest.StartupBytes))
	if *workingTree {
		changes, err := snapshot.UncapturedChanges(ctx, w.root, manifest.Paths())
		if err != nil {
			return fail(env, err)
		}
		if len(changes) > 0 {
			fmt.Fprintf(env.Stdout, "note: %d changed file(s) are not context and are not in this snapshot (e.g. %s)\n", len(changes), changes[0])
		}
	}
	if linked := notIncluded(src, manifest); len(linked) > 0 {
		fmt.Fprintf(env.Stdout, "note: %d file(s) linked from the context are not in this snapshot (e.g. %s); add them with --include PATH\n", len(linked), linked[0])
	}
	for _, warning := range manifest.Warnings {
		fmt.Fprintf(env.Stdout, "warning: %s\n", warning)
	}
	return ExitOK
}

// notIncluded lists linked documents of src that the snapshot does not capture.
func notIncluded(src source.Source, manifest snapshot.Manifest) []string {
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return nil
	}
	captured := map[string]bool{}
	for _, p := range manifest.Paths() {
		captured[p] = true
	}
	var missing []string
	for _, p := range resolved.Linked {
		if !captured[p] {
			missing = append(missing, p)
		}
	}
	return missing
}

func contextList(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("context list", flag.ContinueOnError), args, contextUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		fmt.Fprint(env.Stderr, contextUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	snaps, err := w.db.Snapshots(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	if len(snaps) == 0 {
		fmt.Fprintln(env.Stdout, "No snapshots yet: agentium context snapshot NAME")
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "%-20s %-16s %-14s %6s %14s\n", "NAME", "SOURCE", "COMMIT", "FILES", "START TOKENS")
	for _, snap := range snaps {
		var manifest snapshot.Manifest
		if err := json.Unmarshal(snap.Manifest, &manifest); err != nil {
			return fail(env, fmt.Errorf("snapshot %s: %w", snap.Name, err))
		}
		fmt.Fprintf(env.Stdout, "%-20s %-16s %-14s %6d %14s\n", snap.Name, snap.Source, shortCommit(snap.SourceCommit),
			len(manifest.Files), fmt.Sprintf("~%d", claudectx.EstimateTokens(manifest.StartupBytes)))
	}
	return ExitOK
}

func contextDiff(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("context diff", flag.ContinueOnError)
	patch := fs.Bool("patch", false, "print the full diff")
	rest, code, ok := parseArgs(env, fs, args, contextUsage)
	if !ok {
		return code
	}
	if len(rest) != 2 {
		fmt.Fprint(env.Stderr, contextUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	var snaps [2]store.Snapshot
	var manifests [2]snapshot.Manifest
	for i, name := range rest {
		if snaps[i], err = w.db.SnapshotByName(ctx, w.project.ID, name); err != nil {
			return fail(env, err)
		}
		if err := json.Unmarshal(snaps[i].Manifest, &manifests[i]); err != nil {
			return fail(env, fmt.Errorf("snapshot %s: %w", name, err))
		}
	}
	stat, fullPatch, err := snapshot.Diff(ctx, w.bare, snaps[0].CommitID, snaps[1].CommitID)
	if err != nil {
		return fail(env, err)
	}
	from, to := claudectx.EstimateTokens(manifests[0].StartupBytes), claudectx.EstimateTokens(manifests[1].StartupBytes)
	fmt.Fprintf(env.Stdout, "%s -> %s: session-start context about %d -> %d tokens (%+d, estimated)\n", rest[0], rest[1], from, to, to-from)
	if strings.TrimSpace(stat) == "" {
		fmt.Fprintln(env.Stdout, "No differences.")
		return ExitOK
	}
	fmt.Fprintln(env.Stdout, stat)
	if *patch {
		fmt.Fprint(env.Stdout, fullPatch)
	}
	return ExitOK
}
