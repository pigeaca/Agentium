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
	"path/filepath"
	"strings"

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
	workingTree := fs.Bool("working-tree", false, "lint the working tree, including uncommitted edits (default: HEAD)")
	hook := fs.Bool("hook", false, "read a Claude Code PostToolUse payload on stdin; check only when it edits a context file")
	printHook := fs.Bool("print-hook", false, "print the settings snippet that runs --hook after edits")
	rest, code, ok := parseArgs(env, fs, args, contextUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 || (*hook && *printHook) || ((*hook || *printHook) && *workingTree) {
		fmt.Fprint(env.Stderr, contextUsage)
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
	result, err := lintRepo(ctx, env, root, *workingTree)
	if err != nil {
		return fail(env, err)
	}
	result.print(env.Stdout, env.style(), "Context lint of "+filepath.Base(root)+" ("+result.where+")")
	return ExitOK
}

// lintResult is a lint of one repository state and, when there is one, its comparison with the last snapshot.
type lintResult struct {
	claudectx.Lint
	where     string
	base      string // the last snapshot's name; empty when there is none to compare with
	baseBytes int
	noBase    string // why there is no comparison
}

// lintRepo lints the context at HEAD or in the working tree of the repository at root. The snapshot comparison needs
// the repository to be registered; without that, or without a snapshot, it is skipped with a reason, never an error.
func lintRepo(ctx context.Context, env Env, root string, working bool) (lintResult, error) {
	var src source.Source
	var err error
	if working {
		src, err = source.WorkingTree(ctx, root)
	} else {
		var commit string
		if commit, err = gitx.Run(ctx, "-C", root, "rev-parse", "--verify", "--quiet", "--end-of-options", "HEAD^{commit}"); err != nil || commit == "" {
			return lintResult{}, fmt.Errorf("%s has no commit yet: commit, or use --working-tree", root)
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
	r := lintResult{Lint: lint, where: src.Describe()}
	r.base, r.baseBytes, r.noBase = lastSnapshot(ctx, env, root)
	return r, nil
}

// lastSnapshot finds the registered project's most recent snapshot. It only reads Agentium's database: a hook must
// not create the data folder or register anything.
func lastSnapshot(ctx context.Context, env Env, root string) (name string, startupBytes int, why string) {
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
		return "", 0, "no snapshot comparison: " + err.Error()
	}
	if _, err := os.Stat(layout.Database); err != nil {
		return "", 0, "not registered with Agentium (agentium init), so no snapshot comparison"
	}
	db, err := store.Open(ctx, layout.Database)
	if err != nil {
		return "", 0, "no snapshot comparison: " + err.Error()
	}
	defer db.Close()
	project, err := db.ProjectByRoot(ctx, root)
	if errors.Is(err, store.ErrNotFound) {
		return "", 0, "not registered with Agentium (agentium init), so no snapshot comparison"
	} else if err != nil {
		return "", 0, "no snapshot comparison: " + err.Error()
	}
	snaps, err := db.Snapshots(ctx, project.ID)
	if err != nil || len(snaps) == 0 {
		return "", 0, "no snapshot yet (agentium context snapshot NAME), so nothing to compare with"
	}
	last := snaps[len(snaps)-1]
	var manifest snapshot.Manifest
	if err := json.Unmarshal(last.Manifest, &manifest); err != nil {
		return "", 0, "no snapshot comparison: snapshot " + last.Name + " is unreadable"
	}
	return last.Name, manifest.StartupBytes, ""
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

// lintHook is the PostToolUse hook: it never fails and prints only when the edited file is a context file.
func lintHook(ctx context.Context, env Env) int {
	var data []byte
	if env.Stdin != nil {
		data, _ = io.ReadAll(io.LimitReader(env.Stdin, 17<<20))
	}
	if msg := hookMessage(ctx, env, data); msg != "" {
		fmt.Fprintln(env.Stdout, claudectx.HookOutput(msg))
	}
	return ExitOK
}

// hookMessage is the text to show the user for payload data, or "" to stay silent. The repository comes from the
// edited file's path (then the payload's cwd), not from the process's folder: Claude Code may run the hook elsewhere.
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
	folder := nearestFolder(file)
	realFolder, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return ""
	}
	root, err := repoRoot(ctx, realFolder)
	if err != nil {
		return "" // not in a git repository: not ours
	}
	below, _ := filepath.Rel(folder, file)
	rel, err := filepath.Rel(root, filepath.Join(realFolder, below))
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	result, err := lintRepo(ctx, env, root, true)
	if err != nil {
		return "Agentium context lint skipped: " + err.Error()
	}
	if !result.Contains(rel) {
		return ""
	}
	var out bytes.Buffer
	result.print(&out, term.Detect(false, env.Getenv), "Agentium context lint: "+filepath.ToSlash(rel)+" changed")
	return out.String()
}

// nearestFolder is the closest existing folder of file's path.
func nearestFolder(file string) string {
	dir := filepath.Dir(file)
	for dir != filepath.Dir(dir) {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			break
		}
		dir = filepath.Dir(dir)
	}
	return dir
}

func printHookSettings(env Env) int {
	snippet, err := claudectx.HookSettings()
	if err != nil {
		return fail(env, err)
	}
	fmt.Fprintln(env.Stderr, "Add this to the \"hooks\" of your own ~/.claude/settings.json (merge it with any hooks you have).")
	fmt.Fprintln(env.Stderr, "After each Edit, Write or MultiEdit of a context file, Claude Code then shows its size change and warnings.")
	fmt.Fprintln(env.Stderr, "Agentium never writes your settings. Its own runs load project settings only, so the hook never fires inside them.")
	fmt.Fprint(env.Stdout, string(snippet))
	return ExitOK
}
