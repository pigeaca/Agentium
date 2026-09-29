package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

const taskUsage = `Usage:
  agentium task add NAME --base REF (--instruction TEXT | --instruction-file FILE) [--solution REF]
                         [--setup CMD]... [--verify CMD]...
                         a task by hand; with --solution, its test-file changes are the hidden tests
  agentium task import (--commit REF | --pr N) [--name NAME] [--setup CMD]... [--verify CMD]...
                         a task from history: the base is the parent, test-file changes are the hidden tests,
                         the rest is the reference solution (a PR must be merged; read through gh)
  agentium task list
  agentium task show NAME
  agentium task edit NAME [--instruction TEXT | --instruction-file FILE] [--setup CMD... | --no-setup]
                         [--verify CMD]... [--reviewed]
  agentium task validate NAME [--snapshot NAME]... [--timeout DURATION] [--keep]
                         the hidden tests fail on the base and the reference passes them, in the base's own
                         context and with each snapshot applied (without a solution: the base passes)
  agentium task rm NAME

--verify defaults to the test commands found by agentium init. --setup commands run first in every fresh
checkout (for example, building assets the code embeds); they must pass.
`

func runTask(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, taskUsage)
		return ExitUsage
	}
	commands := map[string]func(context.Context, Env, []string) int{
		"add": taskAdd, "import": taskImport, "list": taskList, "show": taskShow, "edit": taskEdit,
		"validate": taskValidate, "rm": taskRemove,
	}
	if run, ok := commands[args[0]]; ok {
		return run(ctx, env, args[1:])
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(env.Stdout, taskUsage)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "agentium task: unknown subcommand %q\n\n%s", args[0], taskUsage)
	return ExitUsage
}

// keepCommit resolves ref in the user's repository and copies that one commit (depth 1) into Agentium's bare
// repository, so the task survives rebases and garbage collection there.
func (w *workspace) keepCommit(ctx context.Context, ref string) (string, error) {
	commit, err := gitx.Run(ctx, "-C", w.root, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || commit == "" {
		return "", fmt.Errorf("%q is not a commit in %s", ref, w.root)
	}
	if err := gitx.FetchCommit(ctx, w.root, commit, gitx.SourceRef(commit), "--git-dir", w.bare); err != nil {
		return "", err
	}
	return commit, nil
}

// defaultVerify is the project's detected test commands.
func (w *workspace) defaultVerify() []string {
	var info project.Info
	if err := json.Unmarshal(w.project.Discovery, &info); err != nil {
		return nil
	}
	return info.TestCommands
}

// instructionFlags reads --instruction or --instruction-file.
type instructionFlags struct {
	text, file *string
}

func addInstructionFlags(fs *flag.FlagSet) instructionFlags {
	return instructionFlags{
		text: fs.String("instruction", "", "what the agent is asked to do"),
		file: fs.String("instruction-file", "", "read the instruction from this file"),
	}
}

// value returns the instruction, or "" when neither flag was given.
func (f instructionFlags) value(env Env) (string, error) {
	switch {
	case *f.text != "" && *f.file != "":
		return "", errors.New("give --instruction or --instruction-file, not both")
	case *f.file != "":
		file := *f.file
		if !filepath.IsAbs(file) {
			file = filepath.Join(env.Dir, file)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read instruction: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(*f.text), nil
}

func taskAdd(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("task add", flag.ContinueOnError)
	base := fs.String("base", "", "the commit the agent starts from")
	solution := fs.String("solution", "", "a commit that solves the task: its test-file changes become the hidden tests")
	instruction := addInstructionFlags(fs)
	var verify, setup stringList
	fs.Var(&verify, "verify", "a verification command (repeatable)")
	fs.Var(&setup, "setup", "a command a fresh checkout needs first (repeatable)")
	rest, code, ok := parseArgs(env, fs, args, taskUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 || *base == "" {
		fmt.Fprint(env.Stderr, taskUsage)
		return ExitUsage
	}
	text, err := instruction.value(env)
	if err != nil || text == "" {
		fmt.Fprintf(env.Stderr, "agentium task add: an instruction is required (--instruction or --instruction-file)%s\n", errSuffix(err))
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	t := store.Task{ProjectID: w.project.ID, Name: rest[0], Instruction: text, Source: "manual", Verify: verify, Setup: setup, CreatedAt: env.Now()}
	if t.BaseCommit, err = w.keepCommit(ctx, *base); err != nil {
		return fail(env, err)
	}
	if *solution != "" {
		if t.SolutionCommit, err = w.keepCommit(ctx, *solution); err != nil {
			return fail(env, err)
		}
	}
	return saveTask(ctx, env, w, t)
}

func taskImport(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("task import", flag.ContinueOnError)
	commitRef := fs.String("commit", "", "import this commit")
	pr := fs.Int("pr", 0, "import this merged pull request (read through gh)")
	name := fs.String("name", "", "task name (default: from the commit subject)")
	var verify, setup stringList
	fs.Var(&verify, "verify", "a verification command (repeatable)")
	fs.Var(&setup, "setup", "a command a fresh checkout needs first (repeatable)")
	rest, code, ok := parseArgs(env, fs, args, taskUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 || (*commitRef == "") == (*pr == 0) {
		fmt.Fprint(env.Stderr, "agentium task import: give --commit REF or --pr N\n\n"+taskUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	t := store.Task{ProjectID: w.project.ID, Name: *name, Verify: verify, Setup: setup, NeedsReview: true, CreatedAt: env.Now()}
	var solutionRef string
	if *pr != 0 {
		merged, err := pullRequest(ctx, env, w.root, *pr)
		if err != nil {
			return fail(env, err)
		}
		solutionRef, t.Instruction, t.Source = merged.commit, merged.instruction, fmt.Sprintf("pr #%d", *pr)
		if err := checkWholePR(ctx, w.root, merged); err != nil {
			return fail(env, err)
		}
	} else {
		solutionRef = *commitRef
	}
	if t.SolutionCommit, err = w.keepCommit(ctx, solutionRef); err != nil {
		return fail(env, err)
	}
	parent, err := gitx.Run(ctx, "-C", w.root, "rev-parse", "--verify", "--quiet", t.SolutionCommit+"^1")
	if err != nil || parent == "" {
		return fail(env, fmt.Errorf("commit %s has no parent to start from", shortCommit(t.SolutionCommit)))
	}
	if t.BaseCommit, err = w.keepCommit(ctx, parent); err != nil {
		return fail(env, err)
	}
	if *pr == 0 {
		t.Source = "commit " + shortCommit(t.SolutionCommit)
		if t.Instruction, err = gitx.Run(ctx, "-C", w.root, "log", "-1", "--format=%B", t.SolutionCommit); err != nil {
			return fail(env, err)
		}
	}
	if t.Name == "" {
		subject, _, _ := strings.Cut(t.Instruction, "\n")
		t.Name = taskName(subject, t.SolutionCommit)
	}
	return saveTask(ctx, env, w, t)
}

// saveTask splits the solution, fills in defaults, stores the task and reports it.
func saveTask(ctx context.Context, env Env, w *workspace, t store.Task) int {
	if !snapshot.ValidName(t.Name) {
		fmt.Fprintf(env.Stderr, "agentium task: name %q must be lowercase letters, digits, '.', '_' or '-' (up to 63)\n", t.Name)
		return ExitUsage
	}
	if len(t.Verify) == 0 {
		if t.Verify = w.defaultVerify(); len(t.Verify) == 0 {
			return fail(env, errors.New("no test commands were detected for this project: pass --verify CMD"))
		}
	}
	if t.SolutionCommit != "" {
		var err error
		if t.HiddenTests, t.Reference, err = task.Split(ctx, t.BaseCommit, t.SolutionCommit, "--git-dir", w.bare); err != nil {
			return fail(env, err)
		}
		switch {
		case len(t.HiddenTests) == 0:
			return fail(env, fmt.Errorf("%s changes no test files, so there are no hidden tests to check a solution with", shortCommit(t.SolutionCommit)))
		case len(t.Reference) == 0:
			return fail(env, fmt.Errorf("%s changes only test files, so there is nothing for an agent to implement", shortCommit(t.SolutionCommit)))
		}
	}
	saved, err := w.db.SaveTask(ctx, t)
	if errors.Is(err, store.ErrExists) {
		return fail(env, fmt.Errorf("task %q already exists; choose another name with --name", t.Name))
	} else if err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Added task %s (%s): base %s, %d hidden test file(s), %d reference file(s)\n",
		saved.Name, saved.Source, shortCommit(saved.BaseCommit), len(saved.HiddenTests), len(saved.Reference))
	if len(saved.Setup) > 0 {
		fmt.Fprintf(env.Stdout, "  setup:  %s\n", strings.Join(saved.Setup, "; "))
	}
	fmt.Fprintf(env.Stdout, "  verify: %s\n", strings.Join(saved.Verify, "; "))
	if saved.NeedsReview {
		fmt.Fprintf(env.Stdout, "Review the instruction for solution leaks (it came from history): agentium task show %s, then task edit %s --instruction-file FILE or --reviewed\n", saved.Name, saved.Name)
	}
	fmt.Fprintf(env.Stdout, "Next: agentium task validate %s\n", saved.Name)
	return ExitOK
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// taskName makes a name from a commit subject: a slug of at most 40 characters, cut between words, then the short
// commit.
func taskName(subject, commit string) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(subject), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:40]
		if cut := strings.LastIndex(slug, "-"); cut > 0 {
			slug = slug[:cut]
		}
	}
	if slug == "" {
		return "c" + shortCommit(commit)[:7]
	}
	return slug + "-" + shortCommit(commit)[:7]
}

// mergedPR is a merged pull request as gh reports it.
type mergedPR struct {
	number      int
	commit      string // the merge (or squash) commit
	instruction string
	files       map[string]lineCounts // by path (the new path for renames)
}

// lineCounts are a file's added and deleted lines; -1 when unknown (binary files in git's numstat).
type lineCounts struct{ added, deleted int }

// pullRequest reads a merged pull request through gh, run in the repository (read-only: `gh pr view`).
func pullRequest(ctx context.Context, env Env, root string, number int) (mergedPR, error) {
	gh, err := env.LookPath("gh")
	if err != nil {
		return mergedPR{}, errors.New("gh (GitHub CLI) was not found on PATH; import the merge commit with --commit instead")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "pr", "view", strconv.Itoa(number), "--json", "number,title,body,state,mergeCommit,files")
	cmd.Dir = root
	cmd.Env = gitx.Environ(os.Environ()) // gh keeps its own login; only GIT_* is dropped
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return mergedPR{}, fmt.Errorf("gh pr view %d: %s", number, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return mergedPR{}, fmt.Errorf("gh pr view %d: %w", number, err)
	}
	var view struct {
		Number             int
		Title, Body, State string
		MergeCommit        *struct{ Oid string }
		Files              []struct {
			Path                 string
			Additions, Deletions int
		}
	}
	if err := json.Unmarshal(out, &view); err != nil {
		return mergedPR{}, fmt.Errorf("gh pr view %d: %w", number, err)
	}
	if view.State != "MERGED" || view.MergeCommit == nil || view.MergeCommit.Oid == "" {
		return mergedPR{}, fmt.Errorf("pull request #%d is %s, not merged: only merged pull requests have a reference solution", number, strings.ToLower(view.State))
	}
	pr := mergedPR{number: number, commit: view.MergeCommit.Oid, instruction: strings.TrimSpace(view.Title + "\n\n" + view.Body),
		files: map[string]lineCounts{}}
	for _, f := range view.Files {
		pr.files[f.Path] = lineCounts{f.Additions, f.Deletions}
	}
	return pr, nil
}

// checkWholePR makes sure the merge commit holds exactly the pull request's changes: every file with the same line
// counts. A rebase merge's last commit holds only its own changes, so its base would already contain part of the
// solution. Renames are detected on both sides, as GitHub reports them.
func checkWholePR(ctx context.Context, root string, pr mergedPR) error {
	if _, err := gitx.Run(ctx, "-C", root, "cat-file", "-e", "--end-of-options", pr.commit+"^{commit}"); err != nil {
		return fmt.Errorf("pull request #%d was merged as %s, which is not in your repository yet: fetch it first", pr.number, shortCommit(pr.commit))
	}
	out, err := gitx.Output(ctx, nil, "-C", root, "diff", "--numstat", "-z", "-M", pr.commit+"^1", pr.commit)
	if err != nil {
		return err
	}
	local, err := parseNumstat(string(out))
	if err != nil {
		return err
	}
	mismatch := func(detail string) error {
		return fmt.Errorf("pull request #%d does not match its merge commit %s (%s): it was probably rebase-merged; import its commits with --commit",
			pr.number, shortCommit(pr.commit), detail)
	}
	if len(local) != len(pr.files) {
		return mismatch(fmt.Sprintf("%d file(s) in the pull request, %d in the commit", len(pr.files), len(local)))
	}
	for path, counts := range pr.files {
		got, ok := local[path]
		switch {
		case !ok:
			return mismatch(path + " is not in the commit")
		case got.added >= 0 && got != counts:
			return mismatch(fmt.Sprintf("%s: +%d -%d in the pull request, +%d -%d in the commit", path, counts.added, counts.deleted, got.added, got.deleted))
		}
	}
	return nil
}

// parseNumstat reads `git diff --numstat -z` output: "added\tdeleted\tpath\0", or for a rename
// "added\tdeleted\t\0old\0new\0". Binary files have "-" counts, kept as -1.
func parseNumstat(out string) (map[string]lineCounts, error) {
	files := map[string]lineCounts{}
	records := strings.Split(out, "\x00")
	for i := 0; i < len(records); i++ {
		if records[i] == "" {
			continue
		}
		fields := strings.SplitN(records[i], "\t", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected git diff --numstat record %q", records[i])
		}
		path := fields[2]
		if path == "" { // a rename: the old and the new path follow
			if i+2 >= len(records) {
				return nil, fmt.Errorf("truncated git diff --numstat rename record")
			}
			path, i = records[i+2], i+2
		}
		counts := lineCounts{-1, -1}
		if fields[0] != "-" {
			added, err1 := strconv.Atoi(fields[0])
			deleted, err2 := strconv.Atoi(fields[1])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("unexpected git diff --numstat counts %q", records[i])
			}
			counts = lineCounts{added, deleted}
		}
		files[path] = counts
	}
	return files, nil
}

func taskList(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("task list", flag.ContinueOnError), args, taskUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		fmt.Fprint(env.Stderr, taskUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	tasks, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(env.Stdout, "No tasks yet: agentium task import --commit REF, or agentium task add NAME ...")
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "%-50s %-16s %5s %5s  %s\n", "NAME", "SOURCE", "TESTS", "FILES", "STATUS")
	for _, t := range tasks {
		status := validationStatus(t)
		if t.NeedsReview {
			status += " (instruction not reviewed)"
		}
		fmt.Fprintf(env.Stdout, "%-50s %-16s %5d %5d  %s\n", t.Name, t.Source, len(t.HiddenTests), len(t.Reference), status)
	}
	return ExitOK
}

func validationStatus(t store.Task) string {
	if t.Validation == nil {
		return "not validated"
	}
	var v task.Validation
	if err := json.Unmarshal(t.Validation, &v); err != nil {
		return "unreadable validation"
	}
	return v.Summary()
}

func taskShow(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("task show", flag.ContinueOnError), args, taskUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, taskUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	t, err := w.db.TaskByName(ctx, w.project.ID, rest[0])
	if err != nil {
		return fail(env, err)
	}
	out := env.Stdout
	fmt.Fprintf(out, "Task %s (%s)\n  base       %s\n", t.Name, t.Source, t.BaseCommit)
	if t.SolutionCommit != "" {
		fmt.Fprintf(out, "  solution   %s\n", t.SolutionCommit)
	}
	if len(t.Setup) > 0 {
		fmt.Fprintf(out, "  setup      %s\n", strings.Join(t.Setup, "; "))
	}
	fmt.Fprintf(out, "  verify     %s\n", strings.Join(t.Verify, "; "))
	fmt.Fprintf(out, "  hidden     %s\n", orNone(strings.Join(t.HiddenTests, ", ")))
	fmt.Fprintf(out, "  reference  %s\n", orNone(strings.Join(t.Reference, ", ")))
	fmt.Fprintf(out, "  status     %s\n", validationStatus(t))
	if t.NeedsReview {
		fmt.Fprintln(out, "Instruction (from history; review it for solution leaks, then task edit):")
	} else {
		fmt.Fprintln(out, "Instruction:")
	}
	for _, line := range strings.Split(t.Instruction, "\n") {
		fmt.Fprintf(out, "  %s\n", line)
	}
	for _, p := range t.Reference { // names of reference files in the instruction tell the agent where the fix goes
		if strings.Contains(t.Instruction, p) || strings.Contains(t.Instruction, filepath.Base(p)) {
			fmt.Fprintf(out, "note: the instruction names reference file %s\n", p)
		}
	}
	return ExitOK
}

func taskEdit(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("task edit", flag.ContinueOnError)
	instruction := addInstructionFlags(fs)
	var verify, setup stringList
	fs.Var(&verify, "verify", "replace the verification commands (repeatable)")
	fs.Var(&setup, "setup", "replace the setup commands (repeatable)")
	noSetup := fs.Bool("no-setup", false, "remove the setup commands")
	reviewed := fs.Bool("reviewed", false, "mark the instruction as reviewed for solution leaks")
	rest, code, ok := parseArgs(env, fs, args, taskUsage)
	if !ok {
		return code
	}
	text, err := instruction.value(env)
	if err != nil || len(rest) != 1 || (len(setup) > 0 && *noSetup) ||
		(text == "" && len(verify) == 0 && len(setup) == 0 && !*noSetup && !*reviewed) {
		fmt.Fprintf(env.Stderr, "agentium task edit: give NAME and at least one of --instruction, --instruction-file, --setup, --no-setup, --verify or --reviewed%s\n", errSuffix(err))
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	t, err := w.db.TaskByName(ctx, w.project.ID, rest[0])
	if err != nil {
		return fail(env, err)
	}
	if text != "" {
		t.Instruction, t.NeedsReview = text, false
	}
	if *reviewed {
		t.NeedsReview = false
	}
	if len(verify) > 0 {
		t.Verify, t.Validation = verify, nil // a validation of other commands no longer applies
	}
	if len(setup) > 0 || *noSetup {
		t.Setup, t.Validation = setup, nil
	}
	if err := w.db.UpdateTask(ctx, t, env.Now()); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Updated task %s\n", t.Name)
	return ExitOK
}

func taskValidate(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("task validate", flag.ContinueOnError)
	var snapshots stringList
	fs.Var(&snapshots, "snapshot", "also validate with this context snapshot applied (repeatable)")
	timeout := fs.Duration("timeout", 10*time.Minute, "time limit for each verification command")
	keep := fs.Bool("keep", false, "keep the checkouts for inspection")
	rest, code, ok := parseArgs(env, fs, args, taskUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, taskUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	t, err := w.db.TaskByName(ctx, w.project.ID, rest[0])
	if err != nil {
		return fail(env, err)
	}
	arms := []task.Arm{{Name: "base"}}
	for i, name := range snapshots {
		if name == "base" || slices.Contains(snapshots[:i], name) { // arm names name checkouts and logs
			fmt.Fprintf(env.Stderr, "agentium task validate: --snapshot %q is repeated or reserved (\"base\" is the task's own context)\n", name)
			return ExitUsage
		}
		snap, err := w.db.SnapshotByName(ctx, w.project.ID, name)
		if err != nil {
			return fail(env, err)
		}
		arms = append(arms, task.Arm{Name: name, Snapshot: snap.CommitID})
	}
	folder := filepath.Join(w.layout.Artifacts, "tasks", strconv.FormatInt(t.ID, 10), env.Now().UTC().Format("20060102T150405Z"))
	v := task.Validator{Bare: w.bare, WorkDir: filepath.Join(folder, "checkouts"), LogDir: filepath.Join(folder, "logs"),
		Timeout: *timeout, Keep: *keep, Env: run.BuildEnv(w.layout), Progress: env.Stdout, Now: env.Now}
	fmt.Fprintf(env.Stdout, "Validating %s in %d arm(s): %s\n", t.Name, len(arms), strings.Join(t.Verify, "; "))
	result, err := v.Validate(ctx, task.Spec{Base: t.BaseCommit, Solution: t.SolutionCommit, HiddenTests: t.HiddenTests,
		Reference: t.Reference, Setup: t.Setup, Verify: t.Verify}, arms)
	if err != nil {
		return fail(env, err)
	}
	if t.Validation, err = json.Marshal(result); err != nil {
		return fail(env, fmt.Errorf("encode validation: %w", err))
	}
	if err := w.db.UpdateTask(ctx, t, env.Now()); err != nil {
		return fail(env, err)
	}
	for arm, files := range result.HarnessChanged {
		fmt.Fprintf(env.Stdout, "note: arm %s changes what runs, not only what the model reads: %s\n", arm, strings.Join(files, ", "))
	}
	fmt.Fprintf(env.Stdout, "Result: %s (logs: %s)\n", result.Summary(), v.LogDir)
	if result.Status == task.StatusInvalid {
		return ExitError
	}
	return ExitOK
}

func taskRemove(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("task rm", flag.ContinueOnError), args, taskUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, taskUsage)
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	if err := w.db.DeleteTask(ctx, w.project.ID, rest[0]); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Removed task %s\n", rest[0])
	return ExitOK
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}
