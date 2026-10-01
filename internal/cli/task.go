package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/pigeaca/agentium/internal/experiment"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pigeaca/agentium/internal/gitx"
	llmjudge "github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/mine"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

const taskUsage = `Usage:
  agentium task add NAME --base REF (--instruction TEXT | --instruction-file FILE | --ticket-file FILE)
                         [--solution REF [--judge-graded]] [--accept-gaps] [--setup CMD]... [--verify CMD]...
                         a task by hand; with --solution, its test-file changes are the hidden tests.
                         --ticket-file reads an exported ticket (Jira's JSON export of one issue, or Markdown: a
                         title, a description and an "Acceptance criteria" section) into the instruction, keeps its key
                         as the source (ticket ABC-123) and asks for a review of the result; its --solution may change
                         no test files, and the task is then judge-graded. --judge-graded asks the same of a solution
                         without a ticket (without it, such a solution is refused)
  agentium task mine [--since DATE] [--limit N] [--max-files N] [--max-lines N] [--dry-run] [--jobs N]
                     [--timeout DURATION] [--setup CMD]... [--verify CMD]...
                         tasks from history in one go: finds commits on the default branch that change tests and
                         code, small and with a clear message, imports the best --limit (default 10) as task import
                         --commit does, and validates them --jobs at a time (default 2), ending with one table;
                         --dry-run lists the candidates with their scores, and why other commits were set aside,
                         and saves nothing. Commits that are already tasks are skipped. Mined tasks verify with
                         the detected build tools' test commands (go test ./... for Go), not every command init
                         found (linters and docs checks fail at old commits); without a build tool, with those
  agentium task import (--commit REF | --pr N) [--name NAME] [--setup CMD]... [--verify CMD]...
                         a task from history: the base is the parent, test-file changes are the hidden tests,
                         the rest is the reference solution (a PR must be merged; read through gh); a commit's
                         instruction is its subject and body, without trailers such as Co-Authored-By
  agentium task list
  agentium task show NAME
  agentium task edit NAME [--instruction TEXT | --instruction-file FILE] [--setup CMD... | --no-setup]
                         [--verify CMD]... [--reviewed] [--accept-gaps]
  agentium task validate (NAME [--weak-tests [--max-hunks N]] | --all [--status STATUS] [--jobs N])
                         [--snapshot NAME]... [--repeat N] [--timeout DURATION] [--keep]
                         the hidden tests fail on the base and the reference passes them, in the base's own
                         context and with each snapshot applied (without a solution: the base passes);
                         --repeat N (1 to 20) runs every stage N times, and a stage whose runs disagree makes the
                         task flaky, which experiments reject (experiment plan asks for at least 3);
                         --weak-tests removes one hunk of the reference at a time (the first --max-hunks, default 20)
                         and reruns the checks in the base context: hunks that still pass are "not tested by the
                         hidden tests", a warning that leaves the task valid; a later task validate without
                         --weak-tests replaces the stored result, so rerun with it to keep the list;
                         --all validates every task (--status: only the unvalidated, valid, invalid, flaky or
                         unchecked ones), --jobs at a time (default 2), and ends with one table; an interrupt keeps
                         the validations that finished. --jobs above 1 (here and in task mine) assumes the project's
                         tests can run side by side: no fixed ports, shared /tmp paths or databases
  agentium task rm NAME

Judge-graded tasks have no hidden tests: their runs are to be graded by the judge against the reference solution.
task validate checks that the reference changes code and the instruction says something, and runs nothing.
Experiments and run once do not take them yet.

task show and task validate list what the hidden tests require that neither the instruction nor the base code states
(exact texts; for Go also new names); task list counts them. A task that becomes reviewed (task add --solution, task edit --instruction or --reviewed)
with such a list needs --accept-gaps.

--verify defaults to the test commands found by agentium init (task add and task import; task mine uses the build
tools' own). --setup commands run first in every fresh
checkout (for example, building assets the code embeds); they must pass.
`

func runTask(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, taskUsage)
		return ExitUsage
	}
	commands := map[string]func(context.Context, Env, []string) int{
		"add": taskAdd, "import": taskImport, "list": taskList, "show": taskShow, "edit": taskEdit,
		"validate": taskValidate, "rm": taskRemove, "mine": taskMine,
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
	ticketFile := fs.String("ticket-file", "", "read the instruction from an exported ticket (Jira JSON or Markdown)")
	judgeGraded := fs.Bool("judge-graded", false, "with --solution that changes no test files: grade runs with the judge")
	acceptGaps := fs.Bool("accept-gaps", false, "accept the requirements the hidden tests have that nothing states")
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
	if *judgeGraded && *solution == "" {
		fmt.Fprintln(env.Stderr, "agentium task add: --judge-graded needs --solution: the judge compares runs with the reference solution")
		return ExitUsage
	}
	t := store.Task{Name: rest[0], Source: "manual", Verify: verify, Setup: setup, CreatedAt: env.Now()}
	judging := judgeNever
	switch {
	case *judgeGraded:
		judging = judgeRequired
	case *ticketFile != "":
		judging = judgeAllowed
	}
	if *ticketFile != "" {
		if *instruction.text != "" || *instruction.file != "" {
			fmt.Fprintln(env.Stderr, "agentium task add: give --ticket-file or --instruction/--instruction-file, not both")
			return ExitUsage
		}
		ticket, err := readTicket(env, *ticketFile)
		if err != nil {
			return fail(env, err)
		}
		// The text was converted, not written by the user: it is reviewed like an imported one before experiments.
		t.Instruction, t.Source, t.NeedsReview = ticket.Instruction(), strings.TrimSpace("ticket "+ticket.Key), true
	} else {
		text, err := instruction.value(env)
		if err != nil || text == "" {
			fmt.Fprintf(env.Stderr, "agentium task add: an instruction is required (--instruction, --instruction-file or --ticket-file)%s\n", errSuffix(err))
			return ExitUsage
		}
		t.Instruction = text
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	t.ProjectID = w.project.ID
	if t.BaseCommit, err = w.keepCommit(ctx, *base); err != nil {
		return fail(env, err)
	}
	if *solution != "" {
		if t.SolutionCommit, err = w.keepCommit(ctx, *solution); err != nil {
			return fail(env, err)
		}
	}
	return saveTask(ctx, env, w, t, *acceptGaps, judging)
}

// maxTicketBytes bounds a ticket file: one exported issue is far smaller.
const maxTicketBytes = 1 << 20

// readTicket reads and parses an exported ticket file (relative to the working directory).
func readTicket(env Env, file string) (task.Ticket, error) {
	if !filepath.IsAbs(file) {
		file = filepath.Join(env.Dir, file)
	}
	f, err := os.Open(file)
	if err != nil {
		return task.Ticket{}, fmt.Errorf("read ticket: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTicketBytes+1))
	if err != nil {
		return task.Ticket{}, fmt.Errorf("read ticket: %w", err)
	}
	if len(data) > maxTicketBytes {
		return task.Ticket{}, fmt.Errorf("ticket %s is larger than %d KiB: export one issue, without attachments", filepath.Base(file), maxTicketBytes>>10)
	}
	ticket, err := task.ParseTicket(file, data)
	if err != nil {
		return task.Ticket{}, fmt.Errorf("ticket %s: %w", filepath.Base(file), err)
	}
	return ticket, nil
}

// judging says whether a task may, or must, be judge-graded.
type judging int

const (
	judgeNever    judging = iota // a solution without test changes is refused (task import, task add without a ticket)
	judgeAllowed                 // a solution without test changes makes the task judge-graded (task add --ticket-file)
	judgeRequired                // the solution must change no test files (task add --judge-graded)
)

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
	if *pr == 0 {
		if t, err = w.commitTask(ctx, *commitRef, "", t); err != nil {
			return fail(env, err)
		}
		return saveTask(ctx, env, w, t, false, judgeNever)
	}
	merged, err := pullRequest(ctx, env, w.root, *pr)
	if err != nil {
		return fail(env, err)
	}
	t.Instruction, t.Source = merged.instruction, fmt.Sprintf("pr #%d", *pr)
	if err := checkWholePR(ctx, w.root, merged); err != nil {
		return fail(env, err)
	}
	if t, err = w.keepSolution(ctx, merged.commit, t); err != nil {
		return fail(env, err)
	}
	if t.Name == "" {
		subject, _, _ := strings.Cut(t.Instruction, "\n")
		t.Name = taskName(subject, t.SolutionCommit)
	}
	return saveTask(ctx, env, w, t, false, judgeNever)
}

// commitTask fills in t from a commit of the user's repository, as task import --commit and task mine both make
// tasks: the commit is the solution and its first parent the base (both kept in Agentium's repository), the source
// is "commit <hash>", the instruction is the message by mine's rule (the subject, a blank line and the body without
// trailers; instruction, when not empty, must be that rule's text, as a mined candidate gives it), and the name is
// made from the subject unless t has one. The task needs a review: its instruction came from history. completeTask
// does the rest.
func (w *workspace) commitTask(ctx context.Context, ref, instruction string, t store.Task) (store.Task, error) {
	t, err := w.keepSolution(ctx, ref, t)
	if err != nil {
		return t, err
	}
	t.Source, t.NeedsReview = "commit "+experiment.ShortCommit(t.SolutionCommit), true
	if instruction == "" {
		if instruction, err = mine.CommitInstruction(ctx, t.SolutionCommit, "-C", w.root); err != nil {
			return t, err
		}
	}
	t.Instruction = instruction
	if t.Name == "" {
		subject, _, _ := strings.Cut(t.Instruction, "\n")
		t.Name = taskName(subject, t.SolutionCommit)
	}
	return t, nil
}

// keepSolution keeps the commit ref names as t's solution and its first parent as t's base.
func (w *workspace) keepSolution(ctx context.Context, ref string, t store.Task) (store.Task, error) {
	var err error
	if t.SolutionCommit, err = w.keepCommit(ctx, ref); err != nil {
		return t, err
	}
	parent, err := gitx.Run(ctx, "-C", w.root, "rev-parse", "--verify", "--quiet", t.SolutionCommit+"^1")
	if ctx.Err() != nil {
		return t, ctx.Err()
	}
	if err != nil || parent == "" {
		return t, fmt.Errorf("commit %s has no parent to start from", experiment.ShortCommit(t.SolutionCommit))
	}
	if t.BaseCommit, err = w.keepCommit(ctx, parent); err != nil {
		return t, err
	}
	return t, nil
}

// completeTask fills in the verify commands' default, splits the solution into hidden tests and the reference, and
// sets the grading mode, refusing what cannot be a task (inline Rust tests, nothing to implement, no tests to grade
// by unless judging allows it).
func (w *workspace) completeTask(ctx context.Context, t store.Task, judging judging) (store.Task, error) {
	if len(t.Verify) == 0 {
		if t.Verify = w.defaultVerify(); len(t.Verify) == 0 {
			return t, errors.New("no test commands were detected for this project: pass --verify CMD")
		}
	}
	if t.SolutionCommit == "" {
		return t, nil
	}
	var err error
	if t.HiddenTests, t.Reference, err = task.Split(ctx, t.BaseCommit, t.SolutionCommit, "--git-dir", w.bare); err != nil {
		return t, err
	}
	if err := task.RefuseInlineRustTests(ctx, t.BaseCommit, t.SolutionCommit, t.Reference, "--git-dir", w.bare); err != nil {
		return t, err
	}
	switch {
	case len(t.HiddenTests) == 0 && len(t.Reference) == 0:
		return t, fmt.Errorf("%s changes no files against the base, so there is nothing to implement", experiment.ShortCommit(t.SolutionCommit))
	case len(t.HiddenTests) == 0 && judging != judgeNever:
		t.Grading = task.GradingJudge
	case len(t.HiddenTests) == 0:
		return t, fmt.Errorf("%s changes no test files, so there are no hidden tests to check a solution with (to grade it with the judge instead: task add --judge-graded)",
			experiment.ShortCommit(t.SolutionCommit))
	case judging == judgeRequired:
		return t, fmt.Errorf("--judge-graded is for solutions without tests, but %s changes %d test file(s): leave the flag out to grade by them",
			experiment.ShortCommit(t.SolutionCommit), len(t.HiddenTests))
	case len(t.Reference) == 0:
		return t, fmt.Errorf("%s changes only test files, so there is nothing for an agent to implement", experiment.ShortCommit(t.SolutionCommit))
	}
	return t, nil
}

// saveTask completes the task (completeTask), checks its gaps if it is reviewed from the start, stores it and reports
// it.
func saveTask(ctx context.Context, env Env, w *workspace, t store.Task, acceptGaps bool, judging judging) int {
	if !snapshot.ValidName(t.Name) {
		fmt.Fprintf(env.Stderr, "agentium task: name %q must be lowercase letters, digits, '.', '_' or '-' (up to 63)\n", t.Name)
		return ExitUsage
	}
	t, err := w.completeTask(ctx, t, judging)
	if err != nil {
		return fail(env, err)
	}
	if t.SolutionCommit != "" && !t.NeedsReview { // a task added by hand is reviewed from the start
		if ok, err := gapGate(ctx, env, w, t, acceptGaps); err != nil {
			return fail(env, err)
		} else if !ok {
			return ExitError
		}
	}
	saved, err := w.db.SaveTask(ctx, t)
	if errors.Is(err, store.ErrExists) {
		return fail(env, fmt.Errorf("task %q already exists; choose another name with --name", t.Name))
	} else if err != nil {
		return fail(env, err)
	}
	if saved.Grading == task.GradingJudge {
		fmt.Fprintf(env.Stdout, "Added task %s (%s): base %s, judge-graded (no hidden tests), %d reference file(s)\n",
			saved.Name, saved.Source, experiment.ShortCommit(saved.BaseCommit), len(saved.Reference))
	} else {
		fmt.Fprintf(env.Stdout, "Added task %s (%s): base %s, %d hidden test file(s), %d reference file(s)\n",
			saved.Name, saved.Source, experiment.ShortCommit(saved.BaseCommit), len(saved.HiddenTests), len(saved.Reference))
	}
	if len(saved.Setup) > 0 {
		fmt.Fprintf(env.Stdout, "  setup:  %s\n", strings.Join(saved.Setup, "; "))
	}
	fmt.Fprintf(env.Stdout, "  verify: %s\n", strings.Join(saved.Verify, "; "))
	st := env.style()
	if saved.Grading == task.GradingJudge {
		fmt.Fprintln(env.Stdout, note(st, judgeGradedNote))
	}
	if sections := task.SolutionSections(saved.Instruction); saved.NeedsReview && len(sections) > 0 {
		fmt.Fprintln(env.Stdout, st.Warn("The instruction has sections that may give the solution away: "+strings.Join(sections, ", ")))
	}
	if saved.NeedsReview {
		fmt.Fprintf(env.Stdout, "%s: %s, then %s\n", st.Warn("Review the instruction "+reviewReason(saved)),
			st.Command("agentium task show "+saved.Name), st.Command("task edit "+saved.Name+" --instruction-file FILE")+" or "+st.Command("--reviewed"))
	}
	fmt.Fprintf(env.Stdout, "Next: %s\n", st.Command("agentium task validate "+saved.Name))
	return ExitOK
}

// judgeGradedNote says what judge grading means for a task today.
const judgeGradedNote = "runs of this task are graded by the judge, which compares each run's change with the reference solution " +
	"(there are no hidden tests); experiments and run once take judge-graded tasks in a later version"

// reviewReason says why a task's instruction needs a review: a ticket's was converted, history's may leak the solution.
func reviewReason(t store.Task) string {
	if isTicket(t) {
		return "(converted from a ticket: check it reads right and does not leak the solution)"
	}
	return "for solution leaks (it came from history)"
}

func isTicket(t store.Task) bool {
	return t.Source == "ticket" || strings.HasPrefix(t.Source, "ticket ")
}

// grading is a task's grading mode as shown (tasks stored before judge grading have none set by the store's default).
func grading(t store.Task) string {
	if t.Grading == "" {
		return task.GradingTests
	}
	return t.Grading
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
		return "c" + experiment.ShortCommit(commit)[:7]
	}
	return slug + "-" + experiment.ShortCommit(commit)[:7]
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
		return fmt.Errorf("pull request #%d was merged as %s, which is not in your repository yet: fetch it first", pr.number, experiment.ShortCommit(pr.commit))
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
			pr.number, experiment.ShortCommit(pr.commit), detail)
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
		st := env.style()
		fmt.Fprintf(env.Stdout, "No tasks yet: %s, %s, or %s\n", st.Command("agentium task mine"), st.Command("agentium task import --commit REF"),
			st.Command("agentium task add NAME ..."))
		return ExitOK
	}
	fair := task.NewFairness("--git-dir", w.bare)
	st := env.style()
	table := term.NewTable(st, term.Left("NAME"), term.Left("SOURCE"), term.Left("GRADED BY"), term.Right("TESTS"), term.Right("FILES"), term.Left("STATUS"))
	for _, t := range tasks {
		table.Row(t.Name, t.Source, grading(t), strconv.Itoa(len(t.HiddenTests)), strconv.Itoa(len(t.Reference)), taskStatus(ctx, st, fair, t))
	}
	if err := table.Write(env.Stdout); err != nil {
		return fail(env, err)
	}
	return ExitOK
}

// taskStatus is a task's status as task list shows it: the validation's summary, then what still needs attention
// (a review, untested hunks, unstated requirements).
func taskStatus(ctx context.Context, st term.Style, fair *task.Fairness, t store.Task) string {
	status := st.Status(validationStatus(t))
	if t.NeedsReview {
		status += st.Warn(" (instruction not reviewed)")
	}
	if n := untestedCount(t); n > 0 {
		status += st.Warn(fmt.Sprintf(" (%d untested hunk(s))", n))
	}
	if gaps, err := task.Gaps(ctx, fair, t); err != nil {
		status += st.Warn(" (unstated requirements unknown)")
	} else if len(gaps) > 0 {
		status += st.Warn(fmt.Sprintf(" (%d unstated requirement(s))", len(gaps)))
	}
	return status
}

// gapGate refuses to mark t reviewed while its gaps stand, unless they are accepted. It explains why on stderr and
// reports whether to go on.
func gapGate(ctx context.Context, env Env, w *workspace, t store.Task, accept bool) (bool, error) {
	gaps, err := task.Gaps(ctx, task.NewFairness("--git-dir", w.bare), t)
	if err != nil {
		return false, err
	}
	if len(gaps) > 0 && !accept {
		printGaps(env.Stderr, term.Style{}, gaps) // stderr stays plain
		fmt.Fprintf(env.Stderr, "agentium task: %s not marked reviewed; state the gaps in the instruction, or pass --accept-gaps\n", t.Name)
		return false, nil
	}
	return true, nil
}

// printGaps shows the gaps, if any, under a heading that says what to do about them.
func printGaps(out io.Writer, st term.Style, gaps []task.Gap) {
	if len(gaps) == 0 {
		return
	}
	fmt.Fprintln(out, st.Warn(fmt.Sprintf("Unstated requirements (%d): the hidden tests need these, but neither the instruction nor the base code states them.\n"+
		"State them in the instruction (task edit --instruction-file), or accept them (task edit --reviewed --accept-gaps):", len(gaps))))
	for _, g := range gaps {
		fmt.Fprintf(out, "  %s\n", g)
	}
}

// printWeakTests lists the reference hunks no hidden test needs, or says why the check did not run.
func printWeakTests(out io.Writer, st term.Style, w *task.WeakTests) {
	switch {
	case w == nil:
		return
	case w.Reason != "":
		fmt.Fprintln(out, note(st, "weak-tests check skipped: "+w.Reason))
		return
	}
	var skipped string
	if w.Skipped > 0 {
		skipped = fmt.Sprintf("; %d more hunk(s) were skipped (--max-hunks)", w.Skipped)
	}
	if len(w.Untested) == 0 {
		fmt.Fprintln(out, note(st, fmt.Sprintf("weak tests: every one of %d hunk(s) checked is needed by the hidden tests%s", w.Checked, skipped)))
		return
	}
	fmt.Fprintln(out, st.Warn(fmt.Sprintf("Not tested by the hidden tests (%d of %d hunk(s) checked%s): removing each still passes.\n"+
		"Fine for logging, comments and docs; otherwise the tests may miss part of the fix:", len(w.Untested), w.Checked, skipped)))
	for _, h := range w.Untested {
		fmt.Fprintf(out, "  %s\n", h)
	}
}

// untestedCount is how many hunks the stored validation found untested (0 when that check was not run).
func untestedCount(t store.Task) int {
	var v task.Validation
	if t.Validation == nil || json.Unmarshal(t.Validation, &v) != nil || v.WeakTests == nil || v.WeakTests.Reason != "" {
		return 0
	}
	return len(v.WeakTests.Untested)
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
	out, st := env.Stdout, env.style()
	fmt.Fprintf(out, "%s\n  base       %s\n", st.Heading(fmt.Sprintf("Task %s (%s)", t.Name, t.Source)), t.BaseCommit)
	if t.SolutionCommit != "" {
		fmt.Fprintf(out, "  solution   %s\n", t.SolutionCommit)
	}
	if len(t.Setup) > 0 {
		fmt.Fprintf(out, "  setup      %s\n", strings.Join(t.Setup, "; "))
	}
	fmt.Fprintf(out, "  verify     %s\n", strings.Join(t.Verify, "; "))
	fmt.Fprintf(out, "  graded by  %s\n", grading(t))
	if t.Grading == task.GradingJudge {
		fmt.Fprintln(out, "  hidden     none (judge-graded)")
	} else {
		fmt.Fprintf(out, "  hidden     %s\n", orNone(strings.Join(t.HiddenTests, ", ")))
	}
	fmt.Fprintf(out, "  reference  %s\n", orNone(strings.Join(t.Reference, ", ")))
	fmt.Fprintf(out, "  status     %s\n", st.Status(validationStatus(t)))
	var stored task.Validation
	if t.Validation != nil && json.Unmarshal(t.Validation, &stored) == nil {
		printWeakTests(out, st, stored.WeakTests)
	}
	if t.Grading == task.GradingJudge {
		fmt.Fprintln(out, note(st, judgeGradedNote))
	}
	gaps, gapErr := task.Gaps(ctx, task.NewFairness("--git-dir", w.bare), t)
	if t.NeedsReview && isTicket(t) {
		fmt.Fprintln(out, st.Heading("Instruction")+" "+st.Warn("(converted from a ticket; review it, then task edit)")+st.Heading(":"))
	} else if t.NeedsReview {
		fmt.Fprintln(out, st.Heading("Instruction")+" "+st.Warn("(from history; review it for solution leaks, then task edit)")+st.Heading(":"))
	} else {
		fmt.Fprintln(out, st.Heading("Instruction:"))
	}
	for _, line := range strings.Split(t.Instruction, "\n") {
		fmt.Fprintf(out, "  %s\n", line)
	}
	for _, p := range t.Reference { // names of reference files in the instruction tell the agent where the fix goes
		if strings.Contains(t.Instruction, p) || strings.Contains(t.Instruction, filepath.Base(p)) {
			fmt.Fprintln(out, note(st, "the instruction names reference file "+p))
		}
	}
	if gapErr != nil {
		fmt.Fprintln(out, note(st, fmt.Sprintf("unstated requirements could not be checked: %v", gapErr)))
	}
	printGaps(out, st, gaps)
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
	acceptGaps := fs.Bool("accept-gaps", false, "with --reviewed: accept the requirements the hidden tests have that nothing states")
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
		t.Instruction = text
	}
	if text != "" || *reviewed { // the task becomes reviewed: its gaps must be stated or accepted
		if ok, err := gapGate(ctx, env, w, t, *acceptGaps); err != nil {
			return fail(env, err)
		} else if !ok {
			return ExitError
		}
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

// validateArgs is what task validate was asked for.
type validateArgs struct {
	name      string // the task, unless all
	all       bool
	status    string
	jobs      int
	snapshots stringList
	weak      bool
	opts      task.ValidateOptions
	// notApplied are the flags given that only apply to running checks (for a judge-graded task).
	notApplied []string
}

// parseValidate reads and checks task validate's arguments; a mistake is reported and its exit code returned.
func parseValidate(env Env, args []string) (a validateArgs, code int, ok bool) {
	fs := flag.NewFlagSet("task validate", flag.ContinueOnError)
	all := fs.Bool("all", false, "validate every task of the project, --jobs at a time")
	status := fs.String("status", "", "with --all: only tasks with this status ("+strings.Join(task.BatchStatuses, ", ")+")")
	jobs := fs.Int("jobs", 2, "with --all: how many tasks to validate at once")
	fs.Var(&a.snapshots, "snapshot", "also validate with this context snapshot applied (repeatable)")
	repeat := fs.Int("repeat", 1, "run every stage this many times; a stage whose runs disagree makes the task flaky")
	weak := fs.Bool("weak-tests", false, "also remove each hunk of the reference solution and list those no hidden test needs")
	maxHunks := fs.Int("max-hunks", task.DefaultMaxHunks, "with --weak-tests: how many hunks to try, in file and line order")
	timeout := fs.Duration("timeout", 10*time.Minute, "time limit for each verification command")
	keep := fs.Bool("keep", false, "keep the checkouts for inspection")
	rest, code, ok := parseArgs(env, fs, args, taskUsage)
	if !ok {
		return a, code, false
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		given[f.Name] = true
		if f.Name != "weak-tests" && f.Name != "max-hunks" {
			a.notApplied = append(a.notApplied, "--"+f.Name)
		}
	})
	usage := func(format string, args ...any) (validateArgs, int, bool) {
		fmt.Fprintf(env.Stderr, "agentium task validate: "+format+"\n", args...)
		return a, ExitUsage, false
	}
	switch {
	case *all && len(rest) != 0:
		return usage("give a task NAME or --all, not both")
	case *all && *weak:
		return usage("--weak-tests checks one task at a time: give its NAME instead of --all")
	case *all && *jobs < 1:
		return usage("--jobs must be at least 1")
	case *all && given["status"] && !slices.Contains(task.BatchStatuses, *status):
		return usage("--status must be one of %s", strings.Join(task.BatchStatuses, ", "))
	case !*all && (given["status"] || given["jobs"]):
		return usage("--status and --jobs need --all")
	case !*all && len(rest) != 1:
		fmt.Fprint(env.Stderr, taskUsage)
		return a, ExitUsage, false
	case *repeat < 1 || *repeat > 20:
		return usage("--repeat must be 1 to 20")
	case given["max-hunks"] && !*weak:
		return usage("--max-hunks needs --weak-tests")
	case *maxHunks < 1:
		return usage("--max-hunks must be at least 1")
	}
	for i, name := range a.snapshots {
		if name == "base" || slices.Contains(a.snapshots[:i], name) { // arm names name checkouts and logs
			return usage("--snapshot %q is repeated or reserved (\"base\" is the task's own context)", name)
		}
	}
	if len(rest) == 1 {
		a.name = rest[0]
	}
	a.all, a.status, a.jobs, a.weak = *all, *status, *jobs, *weak
	a.opts = task.ValidateOptions{Repeat: *repeat, Weak: *weak, MaxHunks: *maxHunks, Timeout: *timeout, Keep: *keep}
	return a, ExitOK, true
}

func taskValidate(ctx context.Context, env Env, args []string) int {
	a, code, ok := parseValidate(env, args)
	if !ok {
		return code
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	o := a.opts
	if a.all {
		if o.Arms, err = w.validating(nil, env.Now).Arms(ctx, w.project.ID, a.snapshots); err != nil {
			return fail(env, err)
		}
		return validateAll(ctx, env, w, a.status, o, a.jobs)
	}
	t, err := w.db.TaskByName(ctx, w.project.ID, a.name)
	if err != nil {
		return fail(env, err)
	}
	if t.Grading == task.GradingJudge {
		if a.weak {
			fmt.Fprintf(env.Stderr, "agentium task validate: --weak-tests needs hidden tests; %s is judge-graded and has none\n", t.Name)
			return ExitUsage
		}
		return validateJudged(ctx, env, w, t, a.notApplied)
	}
	if a.weak && (t.SolutionCommit == "" || len(t.Reference) == 0 || len(t.HiddenTests) == 0) {
		fmt.Fprintf(env.Stderr, "agentium task validate: --weak-tests needs a task with a solution (hidden tests and a reference); %s has none\n", t.Name)
		return ExitUsage
	}
	if t.SolutionCommit != "" {
		if err := task.RefuseInlineRustTests(ctx, t.BaseCommit, t.SolutionCommit, t.Reference, "--git-dir", w.bare); err != nil {
			return fail(env, err)
		}
	}
	if o.Arms, err = w.validating(nil, env.Now).Arms(ctx, w.project.ID, a.snapshots); err != nil {
		return fail(env, err)
	}
	buildEnv, err := run.BuildEnv(w.layout)
	if err != nil {
		return fail(env, err)
	}
	return validateOne(ctx, env, w, w.validating(buildEnv, env.Now), t, o)
}

// validateOne validates t with o in the arms o names, with a live line and the stages' progress, stores the validation
// and reports it.
func validateOne(ctx context.Context, env Env, w *workspace, val task.Validating, t store.Task, o task.ValidateOptions) int {
	env, live := liveEnv(env)
	defer live.Stop()
	v := val.Validator(t, o)
	v.Progress, v.Style = env.Stdout, env.style()
	v.Started = func(arm, stage string) { live.Step("validating " + t.Name + ": " + arm + ", " + stage) }
	st := env.style()
	fmt.Fprintf(env.Stdout, "%s: %s\n", st.Heading(fmt.Sprintf("Validating %s in %d arm(s)", t.Name, len(o.Arms))), strings.Join(t.Verify, "; "))
	result, err := v.Validate(ctx, task.SpecOf(t), o.Arms)
	live.Stop()
	if err != nil {
		return fail(env, err)
	}
	if t, err = val.StoreValidation(ctx, t, result, env.Now()); err != nil {
		return fail(env, err)
	}
	if gaps, err := task.Gaps(ctx, task.NewFairness("--git-dir", w.bare), t); err != nil {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("unstated requirements could not be checked: %v", err)))
	} else {
		printGaps(env.Stdout, st, gaps)
	}
	for arm, files := range result.HarnessChanged {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("arm %s changes what runs, not only what the model reads: %s", arm, strings.Join(files, ", "))))
	}
	printWeakTests(env.Stdout, st, result.WeakTests)
	fmt.Fprintf(env.Stdout, "Result: %s %s\n", st.Status(result.Summary()), st.Note("(logs: "+v.LogDir+")"))
	if result.Status == task.StatusInvalid || result.Status == task.StatusFlaky {
		return ExitError
	}
	return ExitOK
}

// validating is what validating w's tasks needs; buildEnv is the environment commands run in (run.BuildEnv).
func (w *workspace) validating(buildEnv []string, now func() time.Time) task.Validating {
	return task.Validating{DB: w.db, Bare: w.bare, Artifacts: w.layout.Artifacts, Env: buildEnv, Now: now,
		ReferenceDiff: func(ctx context.Context, base, solution string, reference []string) (string, error) {
			return llmjudge.ReferenceDiff(ctx, w.bare, base, solution, reference)
		}}
}

// validateJudged checks a judge-graded task (judgedValidation), stores the result and reports it. notApplied lists the
// flags given that only apply to running checks.
func validateJudged(ctx context.Context, env Env, w *workspace, t store.Task, notApplied []string) int {
	out, st := env.Stdout, env.style()
	fmt.Fprintln(out, st.Heading(fmt.Sprintf("Checking judge-graded task %s", t.Name)))
	if len(notApplied) > 0 {
		fmt.Fprintln(out, note(st, strings.Join(notApplied, ", ")+" do(es) not apply: nothing runs for a judge-graded task"))
	}
	val := w.validating(nil, env.Now)
	result, diff, err := val.Judged(ctx, t, env.Now())
	if err != nil {
		return fail(env, err)
	}
	if t, err = val.StoreValidation(ctx, t, result, env.Now()); err != nil {
		return fail(env, err)
	}
	words := len(strings.Fields(t.Instruction))
	fmt.Fprintf(out, "  instruction  %d word(s)\n", words)
	fmt.Fprintf(out, "  reference    %d code file(s), %d changed line(s): %s\n", len(result.Judge.CodeFiles), result.Judge.ChangedLines,
		orNone(strings.Join(result.Judge.CodeFiles, ", ")))
	if skipped := len(t.Reference) - len(result.Judge.CodeFiles); skipped > 0 {
		fmt.Fprintln(out, note(st, fmt.Sprintf("%d reference file(s) are documents the judge does not compare", skipped)))
	}
	if chars := utf8.RuneCountInString(diff); chars > llmjudge.MaxDiffChars {
		fmt.Fprintln(out, st.Warn(fmt.Sprintf("The reference diff has %d characters; the judge reads the first %d, so it will see a cut copy "+
			"(the task stays valid)", chars, llmjudge.MaxDiffChars)))
	}
	fmt.Fprintln(out, note(st, "hidden-test checks are skipped: there are no hidden tests"))
	fmt.Fprintln(out, note(st, judgeGradedNote))
	if t.NeedsReview {
		fmt.Fprintf(out, "%s: %s\n", st.Warn("The instruction is not reviewed yet"), st.Command("agentium task show "+t.Name)+", then "+
			st.Command("task edit "+t.Name+" --reviewed"))
	}
	fmt.Fprintf(out, "Result: %s\n", st.Status(result.Summary()))
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
