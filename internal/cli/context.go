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
  agentium context show [--ref REF]                          what Claude Code loads (default: the working tree)
  agentium context snapshot NAME [--ref REF | --working-tree] save a version (default: --ref HEAD)
  agentium context list                                      saved versions
  agentium context diff A B [--patch]                        compare two saved versions
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
	root, err := gitx.Run(ctx, "-C", env.Dir, "rev-parse", "--show-toplevel")
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

// read returns a view of the working tree (ref == "") or of ref, and the user commit it corresponds to. Commits are
// copied into Agentium's bare repository first, so later reads never touch the user's repository.
func (w *workspace) read(ctx context.Context, ref string) (source.Source, string, error) {
	target := ref
	if target == "" {
		target = "HEAD"
	}
	commit, err := gitx.Run(ctx, "-C", w.root, "rev-parse", "--verify", "--quiet", target+"^{commit}")
	if err != nil || commit == "" {
		return nil, "", fmt.Errorf("%q is not a commit in %s", target, w.root)
	}
	if err := gitx.FetchCommit(ctx, w.bare, w.root, commit); err != nil {
		return nil, "", err
	}
	if ref == "" {
		src, err := source.WorkingTree(ctx, w.root)
		return src, commit, err
	}
	src, err := source.Commit(ctx, commit, "--git-dir", w.bare)
	return src, commit, err
}

func contextShow(ctx context.Context, env Env, args []string) int {
	fs := newFlags("context show", env.Stderr)
	ref := fs.String("ref", "", "show the context at this commit instead of the working tree")
	rest, err := parseInterspersed(fs, args)
	if err != nil || len(rest) != 0 {
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
	for _, warning := range resolved.Warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
}

func contextSnapshot(ctx context.Context, env Env, args []string) int {
	fs := newFlags("context snapshot", env.Stderr)
	ref := fs.String("ref", "", "snapshot the context at this commit (default HEAD)")
	workingTree := fs.Bool("working-tree", false, "snapshot the context in the working tree, including uncommitted edits")
	rest, err := parseInterspersed(fs, args)
	if err != nil || len(rest) != 1 || (*workingTree && *ref != "") {
		fmt.Fprint(env.Stderr, contextUsage)
		return ExitUsage
	}
	name := rest[0]
	if !snapshot.NamePattern.MatchString(name) {
		fmt.Fprintf(env.Stderr, "agentium context snapshot: name %q must be lowercase letters, digits, '.', '_' or '-' (up to 63)\n", name)
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
	commitID, manifest, err := snapshot.Build(ctx, w.bare, src, "snapshot "+name+" of "+label+" at "+commit)
	if err != nil {
		return fail(env, err)
	}
	if _, err := gitx.Run(ctx, "--git-dir", w.bare, "update-ref", "refs/agentium/snapshots/"+name, commitID); err != nil {
		return fail(env, err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fail(env, fmt.Errorf("encode manifest: %w", err))
	}
	if _, err := w.db.SaveSnapshot(ctx, store.Snapshot{ProjectID: w.project.ID, Name: name, Source: label, SourceCommit: commit,
		CommitID: commitID, Manifest: encoded, CreatedAt: env.Now()}); err != nil {
		return fail(env, err)
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
	for _, warning := range manifest.Warnings {
		fmt.Fprintf(env.Stdout, "warning: %s\n", warning)
	}
	return ExitOK
}

func contextList(ctx context.Context, env Env, args []string) int {
	if len(args) != 0 {
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
	fs := newFlags("context diff", env.Stderr)
	patch := fs.Bool("patch", false, "print the full diff")
	rest, err := parseInterspersed(fs, args)
	if err != nil || len(rest) != 2 {
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

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseInterspersed parses flags anywhere among the arguments (the flag package stops at the first positional one).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
