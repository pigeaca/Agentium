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
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

const contextUsage = `Usage:
  agentium context show [--ref REF]              what Claude Code loads (default: the working tree)
  agentium context snapshot NAME [--ref REF | --working-tree] [--include PATH]...
                                                 save a version (default: --ref HEAD); --include adds a
                                                 document (Markdown, rst, AsciiDoc)
  agentium context lint [--ref REF]              free check, no agent runs: size change since the last snapshot,
                                                 broken @imports, AGENTS.md over Codex's 32 KiB limit and show's
                                                 warnings (default: the working tree); always exit 0
  agentium context lint --print-hook             the Claude Code hook that runs it after you edit context files
                                                 (you add it to ~/.claude/settings.json; Agentium never does)
  agentium context list                          saved versions
  agentium context show|snapshot|list|diff|lint ... --json
                                                 one JSON document instead of text (docs/guide.md, "Scripting and automation")
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
	case "lint":
		return contextLint(ctx, env, args[1:])
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
	// toolchain is the host's build-tool versions (hostToolchain), detected once per command.
	toolchain task.Toolchain
}

// openProject opens the project containing env.Dir; it must have been registered with `agentium init`.
func openProject(ctx context.Context, env Env) (*workspace, error) {
	return openProjectFor(ctx, env, false)
}

// openProjectFor is openProject; readOnly leaves a missing project repository in the data folder uncreated (a dry run
// writes nothing; a project without one has no tasks to read there).
func openProjectFor(ctx context.Context, env Env, readOnly bool) (*workspace, error) {
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
	env.noteRoot(root)
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
		return nil, notRegisteredError{root}
	} else if err != nil {
		db.Close()
		return nil, err
	}
	w := &workspace{db: db, project: project, layout: layout, root: root, bare: layout.ProjectRepo(project.ID)}
	if readOnly {
		return w, nil
	}
	if err := gitx.InitBare(ctx, w.bare); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}

func (w *workspace) Close() { w.db.Close() }

// service is the workspace as the services in internal/experiment take it.
func (w *workspace) service() experiment.Project {
	return experiment.Project{DB: w.db, ID: w.project.ID, Layout: w.layout, Root: w.root, Bare: w.bare}
}

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
	if env.JSON {
		return env.emit(contextShowDocument(env, w.project.Name, src.Describe(), resolved, len(aboveRepository(w.root))))
	}
	st := env.style()
	if err := printContext(env.Stdout, st, w.project.Name, src.Describe(), resolved); err != nil {
		return fail(env, err)
	}
	for _, file := range aboveRepository(w.root) {
		fmt.Fprintln(env.Stdout, warning(st, file+" is above the repository: Claude Code loads it when you work here, but it is not the project's context and experiments exclude it."))
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

func printContext(out io.Writer, st term.Style, project, where string, resolved claudectx.Context) error {
	startup := resolved.StartupBytes()
	fmt.Fprintln(out, st.Heading(fmt.Sprintf("Claude Code context of %s (%s)", project, where)))
	fmt.Fprintf(out, "At session start: about %d tokens (%s, estimated)\n", claudectx.EstimateTokens(startup), sizeLabel(int64(startup)))
	files := func() *term.Table {
		table := term.NewTable(st, term.Left(""), term.Left(""), term.Right(""), term.Left(""))
		table.Indent = "  "
		return table
	}
	atStart, onDemand := files(), files()
	var harness []claudectx.Entry
	demand := 0
	for _, e := range resolved.Entries {
		switch {
		case e.Kind == claudectx.KindHarness:
			harness = append(harness, e)
		case e.StartupBytes == 0:
			onDemand.Row(e.Path, e.Kind, sizeLabel(int64(e.Bytes)))
			demand++
		default:
			var notes []string
			if e.Via != "" {
				notes = append(notes, "via "+e.Via)
			}
			if e.StartupBytes != e.Bytes {
				notes = append(notes, fmt.Sprintf("(description only; %s in full)", sizeLabel(int64(e.Bytes))))
			}
			atStart.Row(e.Path, e.Kind, sizeLabel(int64(e.StartupBytes)), st.Note(strings.Join(notes, " ")))
		}
	}
	if err := atStart.Write(out); err != nil {
		return err
	}
	if demand > 0 {
		fmt.Fprintln(out, st.Heading("On demand:"))
		if err := onDemand.Write(out); err != nil {
			return err
		}
	}
	if len(harness) > 0 {
		fmt.Fprintln(out, st.Heading("Harness (changes what runs, not what the model reads):"))
		for _, e := range harness {
			fmt.Fprintf(out, "  %s\n", e.Path)
		}
	}
	if len(resolved.Linked) > 0 {
		fmt.Fprintln(out, st.Heading("Linked (read only if the agent opens them; in a snapshot only with --include):"))
		for _, p := range resolved.Linked {
			fmt.Fprintf(out, "  %s\n", p)
		}
	}
	for _, w := range resolved.Warnings {
		fmt.Fprintln(out, warning(st, w))
	}
	return nil
}

type contextEntryDoc struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Bytes        int    `json:"bytes"`
	StartupBytes int    `json:"startup_bytes"`
	Via          string `json:"via"`
}

type contextShowDoc struct {
	header
	Project  string            `json:"project"`
	Where    string            `json:"where"` // "working tree" or "commit <short hash>"
	Context  contextSizeDoc    `json:"context"`
	Entries  []contextEntryDoc `json:"entries"`
	Linked   []string          `json:"linked"`
	Warnings []string          `json:"warnings"`
	// AboveRepository counts instruction files in folders above the repository (personal; never named).
	AboveRepository int `json:"above_repository_files"`
}

func contextShowDocument(env Env, name, where string, resolved claudectx.Context, above int) contextShowDoc {
	doc := contextShowDoc{header: hdr("context show"), Project: name, Where: where, Context: contextSize(resolved), Entries: []contextEntryDoc{},
		Linked: list(resolved.Linked), Warnings: env.redactAll(resolved.Warnings), AboveRepository: above}
	for _, e := range resolved.Entries {
		doc.Entries = append(doc.Entries, contextEntryDoc{Path: e.Path, Kind: e.Kind, Bytes: e.Bytes, StartupBytes: e.StartupBytes, Via: e.Via})
	}
	return doc
}

type snapshotInfo struct {
	Name          string   `json:"name"`
	Source        string   `json:"source"`
	Commit        string   `json:"commit"`
	Files         int      `json:"files"`
	StartupTokens int      `json:"startup_tokens_estimated"`
	Warnings      []string `json:"warnings"`
}

type snapshotDoc struct {
	header
	snapshotInfo
	// NotIncludedLinked are documents the context links to that this snapshot lacks (add them with --include).
	NotIncludedLinked []string `json:"not_included_linked"`
	// UncapturedChanges are changed files that are not context and so not in a --working-tree snapshot; always empty without it.
	UncapturedChanges []string `json:"uncaptured_changes"`
}

type snapshotListDoc struct {
	header
	Snapshots []snapshotInfo `json:"snapshots"`
}

type diffDoc struct {
	header
	From        string  `json:"from"`
	To          string  `json:"to"`
	FromTokens  int     `json:"from_tokens_estimated"`
	ToTokens    int     `json:"to_tokens_estimated"`
	DeltaTokens int     `json:"delta_tokens_estimated"`
	Changed     bool    `json:"changed"`
	Stat        string  `json:"stat"`
	Patch       *string `json:"patch"` // with --patch, else null
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// reservedSnapshotName says why a valid name cannot name a snapshot: "base" names each task's own context, and a name
// that reads as a model would make experiment new --b a model A/B (and be refused there as ambiguous). "" when it can.
func reservedSnapshotName(name string) string {
	switch {
	case name == experiment.BaseContext:
		return `"base" names each task's own context`
	case experiment.IsModel(name):
		return fmt.Sprintf("%q reads as a model to experiment new --b (a model A/B)", name)
	}
	return ""
}

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
	if why := reservedSnapshotName(name); why != "" {
		fmt.Fprintf(env.Stderr, "agentium context snapshot: %s; choose another name\n", why)
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
	manifest, err := saveSnapshot(ctx, env, w, name, label, commit, src, include)
	if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		return snapshotJSON(ctx, env, w, src, manifest, snapshotInfo{Name: name, Source: label, Commit: commit}, *workingTree)
	}
	st := env.style()
	fmt.Fprintf(env.Stdout, "Saved snapshot %s from %s (%s): %d file(s); about %d tokens at session start\n",
		name, label, experiment.ShortCommit(commit), len(manifest.Files), claudectx.EstimateTokens(manifest.StartupBytes))
	if *workingTree {
		changes, err := snapshot.UncapturedChanges(ctx, w.root, manifest.Paths())
		if err != nil {
			return fail(env, err)
		}
		if len(changes) > 0 {
			fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("%d changed file(s) are not context and are not in this snapshot (e.g. %s)", len(changes), changes[0])))
		}
	}
	if linked := notIncluded(src, manifest); len(linked) > 0 {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("%d file(s) linked from the context are not in this snapshot (e.g. %s); add them with --include PATH", len(linked), linked[0])))
	}
	for _, w := range manifest.Warnings {
		fmt.Fprintln(env.Stdout, warning(st, w))
	}
	return ExitOK
}

// snapshotJSON prints context snapshot's --json document: what the text reports, and warns about.
func snapshotJSON(ctx context.Context, env Env, w *workspace, src source.Source, manifest snapshot.Manifest, info snapshotInfo, workingTree bool) int {
	info.Files, info.StartupTokens, info.Warnings = len(manifest.Files), claudectx.EstimateTokens(manifest.StartupBytes), env.redactAll(manifest.Warnings)
	doc := snapshotDoc{header: hdr("context snapshot"), snapshotInfo: info, NotIncludedLinked: list(notIncluded(src, manifest)), UncapturedChanges: []string{}}
	if workingTree {
		changes, err := snapshot.UncapturedChanges(ctx, w.root, manifest.Paths())
		if err != nil {
			return fail(env, err)
		}
		doc.UncapturedChanges = list(changes)
	}
	return env.emit(doc)
}

// saveSnapshot builds the snapshot's commit in Agentium's repository and records it under name.
func saveSnapshot(ctx context.Context, env Env, w *workspace, name, label, commit string, src source.Source, include []string) (snapshot.Manifest, error) {
	commitID, manifest, err := snapshot.Build(ctx, w.bare, src, "snapshot "+name+" of "+label+" at "+commit, include)
	if err != nil {
		return manifest, err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return manifest, fmt.Errorf("encode manifest: %w", err)
	}
	// The database decides who owns a name (a concurrent snapshot with the same name fails here); the ref then keeps
	// the commit from garbage collection.
	if _, err := w.db.SaveSnapshot(ctx, store.Snapshot{ProjectID: w.project.ID, Name: name, Source: label, SourceCommit: commit,
		CommitID: commitID, Manifest: encoded, CreatedAt: env.Now()}); errors.Is(err, store.ErrExists) {
		return manifest, fmt.Errorf("snapshot %q already exists; choose another name", name)
	} else if err != nil {
		return manifest, err
	}
	if _, err := gitx.Run(ctx, "--git-dir", w.bare, "update-ref", "refs/agentium/snapshots/"+name, commitID); err != nil {
		return manifest, errors.Join(err, w.db.DeleteSnapshot(ctx, w.project.ID, name))
	}
	return manifest, nil
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
	if env.JSON {
		doc := snapshotListDoc{header: hdr("context list"), Snapshots: []snapshotInfo{}}
		for _, snap := range snaps {
			var manifest snapshot.Manifest
			if err := json.Unmarshal(snap.Manifest, &manifest); err != nil {
				return fail(env, fmt.Errorf("snapshot %s: %w", snap.Name, err))
			}
			doc.Snapshots = append(doc.Snapshots, snapshotInfo{Name: snap.Name, Source: snap.Source, Commit: snap.SourceCommit, Files: len(manifest.Files),
				StartupTokens: claudectx.EstimateTokens(manifest.StartupBytes), Warnings: env.redactAll(manifest.Warnings)})
		}
		return env.emit(doc)
	}
	if len(snaps) == 0 {
		fmt.Fprintln(env.Stdout, "No snapshots yet: "+env.style().Command("agentium context snapshot NAME"))
		return ExitOK
	}
	table := term.NewTable(env.style(), term.Left("NAME"), term.Left("SOURCE"), term.Left("COMMIT"), term.Right("FILES"), term.Right("START TOKENS"))
	for _, snap := range snaps {
		var manifest snapshot.Manifest
		if err := json.Unmarshal(snap.Manifest, &manifest); err != nil {
			return fail(env, fmt.Errorf("snapshot %s: %w", snap.Name, err))
		}
		table.Row(snap.Name, snap.Source, experiment.ShortCommit(snap.SourceCommit), strconv.Itoa(len(manifest.Files)),
			fmt.Sprintf("~%d", claudectx.EstimateTokens(manifest.StartupBytes)))
	}
	if err := table.Write(env.Stdout); err != nil {
		return fail(env, err)
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
	if env.JSON {
		doc := diffDoc{header: hdr("context diff"), From: rest[0], To: rest[1], FromTokens: from, ToTokens: to, DeltaTokens: to - from,
			Changed: strings.TrimSpace(stat) != "", Stat: strings.TrimSpace(stat)}
		if *patch {
			doc.Patch = &fullPatch
		}
		return env.emit(doc)
	}
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
