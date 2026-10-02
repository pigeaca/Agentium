package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// contextLint is a free, advisory check: it reports problems as warnings and still exits 0, like `context show` and
// `context snapshot` do with the same warnings, so it can run anywhere (including a hook) without failing a pipeline.
// Only a usage error or an unreadable repository fails.
func contextLint(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("context lint", flag.ContinueOnError)
	ref := fs.String("ref", "", "lint the context at this commit instead of the working tree")
	hook := fs.Bool("hook", false, "read a Claude Code PostToolUse payload on stdin; check only when it edits a context file")
	printHook := fs.Bool("print-hook", false, "print the settings snippet that runs --hook after edits")
	rest, code, ok := parseArgs(env, fs, args, contextUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 || (*hook && *printHook) || ((*hook || *printHook) && *ref != "") {
		fmt.Fprint(env.Stderr, contextUsage)
		return ExitUsage
	}
	if env.JSON && (*hook || *printHook) {
		fmt.Fprintln(env.Stderr, "agentium context lint: --hook and --print-hook print their own JSON for Claude Code; they do not take --json")
		return ExitUsage
	}
	switch {
	case *printHook:
		return printHookSettings(env)
	case *hook:
		return lintHook(ctx, env)
	}
	root, err := repoRoot(ctx, env.Dir)
	if err != nil {
		return fail(env, err)
	}
	env.noteRoot(root)
	result, err := lintTree(ctx, root, *ref)
	if err != nil {
		return fail(env, err)
	}
	result.compare(ctx, env, root)
	if env.JSON {
		return env.emit(result.document(env, filepath.Base(root)))
	}
	result.print(env.Stdout, env.style(), "Context lint of "+filepath.Base(root)+" ("+result.where+")")
	return ExitOK
}

type lintDoc struct {
	header
	Project       string       `json:"project"`
	Where         string       `json:"where"`
	StartupTokens int          `json:"startup_tokens_estimated"`
	Snapshot      *lintBaseDoc `json:"snapshot"`      // the last snapshot it was compared with; null when there is none
	NoComparison  string       `json:"no_comparison"` // why there is no snapshot comparison
	Problems      []string     `json:"problems"`
	Warnings      []string     `json:"warnings"`
}

type lintBaseDoc struct {
	Name          string `json:"name"`
	StartupTokens int    `json:"startup_tokens_estimated"`
	DeltaTokens   int    `json:"delta_tokens_estimated"`
}

func (r lintResult) document(env Env, project string) lintDoc {
	doc := lintDoc{header: hdr("context lint"), Project: project, Where: r.where, StartupTokens: claudectx.EstimateTokens(r.StartupBytes),
		NoComparison: env.redact(r.noBase), Problems: env.redactAll(r.Problems), Warnings: env.redactAll(r.Warnings)}
	if r.base != "" {
		was := claudectx.EstimateTokens(r.baseBytes)
		doc.Snapshot = &lintBaseDoc{Name: r.base, StartupTokens: was, DeltaTokens: doc.StartupTokens - was}
	}
	return doc
}

// lintResult is a lint of one repository state and, when there is one, its comparison with the last snapshot.
type lintResult struct {
	claudectx.Lint
	where     string
	base      string // the last snapshot's name; empty when there is none to compare with
	baseBytes int
	noBase    string // why there is no comparison
}

// lintTree lints the context in the working tree of the repository at root, or at ref when it is not empty.
func lintTree(ctx context.Context, root, ref string) (lintResult, error) {
	var src source.Source
	var err error
	if ref == "" {
		src, err = source.WorkingTree(ctx, root)
	} else {
		var commit string
		// --end-of-options: a ref such as "--output=x" is a name to look up, never an option.
		if commit, err = gitx.Run(ctx, "-C", root, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}"); err != nil || commit == "" {
			if ctx.Err() != nil {
				return lintResult{}, ctx.Err()
			}
			return lintResult{}, fmt.Errorf("%q is not a commit in %s", ref, root)
		}
		src, err = source.Commit(ctx, commit, "-C", root)
	}
	if err != nil {
		return lintResult{}, err
	}
	lint, err := claudectx.LintContext(src)
	if err != nil {
		return lintResult{}, err
	}
	return lintResult{Lint: lint, where: src.Describe()}, nil
}

// compare sets the comparison with the project's most recent snapshot, or why there is none. The repository need not
// be registered, and nothing here is an error. It only reads Agentium's database (OpenReadOnly): a hook must not
// create the data folder, migrate, register anything or wait on a lock.
func (r *lintResult) compare(ctx context.Context, env Env, root string) {
	r.base, r.baseBytes, r.noBase = lastSnapshot(ctx, env, root)
}

func lastSnapshot(ctx context.Context, env Env, root string) (name string, startupBytes int, why string) {
	const busy = "no snapshot comparison: the database is busy or unreadable"
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
		return "", 0, "no snapshot comparison: " + err.Error()
	}
	if _, err := os.Stat(layout.Database); err != nil {
		return "", 0, "not registered with Agentium (agentium init), so no snapshot comparison"
	}
	// OpenReadOnly may read the file without locks (no other process had it open); if anything changed while we
	// read, a writer started meanwhile and the result is not trusted.
	before := store.StateOf(layout.Database)
	db, err := store.OpenReadOnly(ctx, layout.Database)
	switch {
	case errors.Is(err, store.ErrSchemaNewer):
		return "", 0, "no snapshot comparison: the database was made by a newer Agentium"
	case errors.Is(err, store.ErrSchema):
		return "", 0, "no snapshot comparison: the database was made by another Agentium version; run any other agentium command first"
	case err != nil:
		return "", 0, busy
	}
	defer db.Close()
	name, startupBytes, why, trusted := readLastSnapshot(ctx, db, root)
	if !trusted || store.StateOf(layout.Database) != before {
		return "", 0, busy
	}
	return name, startupBytes, why
}

// readLastSnapshot finds the registered project's last snapshot; trusted is false when the database could not be read.
func readLastSnapshot(ctx context.Context, db *store.Store, root string) (name string, startupBytes int, why string, trusted bool) {
	project, err := db.ProjectByRoot(ctx, root)
	if errors.Is(err, store.ErrNotFound) {
		return "", 0, "not registered with Agentium (agentium init), so no snapshot comparison", true
	} else if err != nil {
		return "", 0, "", false
	}
	snaps, err := db.Snapshots(ctx, project.ID)
	if err != nil {
		return "", 0, "", false
	}
	if len(snaps) == 0 {
		return "", 0, "no snapshot yet (agentium context snapshot NAME), so nothing to compare with", true
	}
	last := snaps[len(snaps)-1]
	var manifest snapshot.Manifest
	if err := json.Unmarshal(last.Manifest, &manifest); err != nil {
		return "", 0, "no snapshot comparison: snapshot " + last.Name + " is unreadable", true
	}
	return last.Name, manifest.StartupBytes, "", true
}

func (r lintResult) print(out io.Writer, st term.Style, heading string) {
	fmt.Fprintln(out, st.Heading(heading))
	line := fmt.Sprintf("At session start: about %d tokens (%s, estimated)", claudectx.EstimateTokens(r.StartupBytes), sizeLabel(int64(r.StartupBytes)))
	if r.base != "" {
		delta := claudectx.EstimateTokens(r.StartupBytes) - claudectx.EstimateTokens(r.baseBytes)
		line += fmt.Sprintf("; %+d tokens against snapshot %s (about %d)", delta, r.base, claudectx.EstimateTokens(r.baseBytes))
	}
	fmt.Fprintln(out, line)
	if r.noBase != "" {
		fmt.Fprintln(out, note(st, r.noBase))
	}
	for _, p := range append(append([]string{}, r.Problems...), r.Warnings...) {
		fmt.Fprintln(out, warning(st, p))
	}
	if len(r.Problems)+len(r.Warnings) == 0 {
		fmt.Fprintln(out, "No problems found.")
	}
}

// repoRoot is the git root containing dir, symbolic links resolved.
func repoRoot(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		return "", errors.New("the current folder cannot be read: run agentium inside your repository")
	}
	root, err := gitx.Run(ctx, "-C", dir, "rev-parse", "--show-toplevel")
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", dir)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	return root, nil
}

// hookTimeout bounds the whole hook: Claude Code waits for it after every edit, so a slow check stays silent.
const hookTimeout = 2 * time.Second

// lintHook is the PostToolUse hook: it never fails and prints only when the edited file is a context file.
func lintHook(ctx context.Context, env Env) int {
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	var data []byte
	if env.Stdin != nil {
		data, _ = io.ReadAll(io.LimitReader(env.Stdin, claudectx.MaxHookPayload+1))
		_, _ = io.Copy(io.Discard, env.Stdin) // an oversize payload: drain it so Claude Code never gets EPIPE
	}
	if len(data) > claudectx.MaxHookPayload {
		return ExitOK // a huge Write is not worth parsing
	}
	if msg := hookMessage(ctx, env, data); msg != "" && ctx.Err() == nil {
		fmt.Fprintln(env.Stdout, claudectx.HookOutput(msg))
	}
	return ExitOK
}

// hookMessage is the text to show the user for payload data, or "" to stay silent. The repository comes from the
// edited file's path (then the payload's cwd), not from the process's folder: Claude Code may run the hook elsewhere.
// It decides relevance before the costly steps: the path alone rules out most edits (source files), then the context
// is resolved, and only for a context file does it read the database.
func hookMessage(ctx context.Context, env Env, data []byte) string {
	payload, relevant, err := claudectx.ParseHookPayload(data)
	if err != nil {
		return "Agentium context lint skipped: " + err.Error()
	}
	if !relevant {
		return ""
	}
	file := payload.ToolInput.FilePath
	if !filepath.IsAbs(file) {
		file = filepath.Join(payload.Cwd, file)
	}
	if !mayBeContext(file) {
		return "" // decided from the path alone, before git runs
	}
	if _, err := os.Stat(file); err != nil {
		return "" // an edit leaves the file in place; a path that is gone is not something to lint
	}
	folder := filepath.Dir(file)
	realFolder, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return ""
	}
	root, err := repoRoot(ctx, realFolder)
	if err != nil {
		return "" // not in a git repository (or out of time): not ours
	}
	below, _ := filepath.Rel(folder, file)
	rel, err := filepath.Rel(root, filepath.Join(realFolder, below))
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if !claudectx.LoadsByPresence(rel) && !claudectx.IsDocumentExt(rel) {
		return "" // only documents can be imported or linked as context
	}
	result, err := lintTree(ctx, root, "")
	if ctx.Err() != nil {
		return ""
	}
	if err != nil {
		return "Agentium context lint skipped: " + err.Error()
	}
	if !result.Reaches(root, rel) {
		return ""
	}
	result.compare(ctx, env, root)
	var out bytes.Buffer
	result.print(&out, term.Style{}, "Agentium context lint: "+rel+" changed") // plain: Claude Code shows the text as is
	return out.String()
}

// mayBeContext is the broad path check made before git starts: the name or extension of a context file, or a place
// Claude Code reads configuration from. The exact check follows.
func mayBeContext(file string) bool {
	slash := filepath.ToSlash(file)
	switch path.Base(slash) {
	case "CLAUDE.md", "AGENTS.md":
		return true
	}
	return claudectx.IsDocumentExt(slash) || strings.Contains(slash, "/.claude/") || strings.HasSuffix(slash, ".mcp.json")
}

// hookCommand is the hook's command line: the absolute path of this binary, so a non-interactive shell without the
// user's PATH still finds it, quoted for the shell.
func hookCommand() string {
	exe, err := os.Executable() // not resolved: a link such as Homebrew's survives upgrades
	if err != nil {
		exe = "agentium"
	}
	return shellQuote(exe) + " context lint --hook"
}

// underTempDir reports whether exe lives in a temporary folder, as a binary built by `go run` does.
func underTempDir(exe string) bool {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		real = exe
	}
	return strings.HasPrefix(real, tmp+string(filepath.Separator))
}

func shellQuote(s string) string {
	if strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func printHookSettings(env Env) int {
	snippet, err := claudectx.HookSettings(hookCommand())
	if err != nil {
		return fail(env, err)
	}
	if exe, err := os.Executable(); err == nil && underTempDir(exe) {
		fmt.Fprintf(env.Stderr, "warning: %s is in a temporary folder (as with `go run`) and will disappear: install agentium (go install ./cmd/agentium) and print the hook again.\n", exe)
	}
	fmt.Fprintln(env.Stderr, "Merge this object into your own ~/.claude/settings.json (add the PostToolUse entry to any \"hooks\" you already have).")
	fmt.Fprintln(env.Stderr, "After each Edit, Write or MultiEdit of a context file, Claude Code then shows its size change and warnings.")
	fmt.Fprintln(env.Stderr, "Agentium never writes your settings. Its own runs load project settings only, so the hook never fires inside them.")
	fmt.Fprint(env.Stdout, string(snippet))
	return ExitOK
}
