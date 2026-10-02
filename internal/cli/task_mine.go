package cli

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/mine"
	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// Defaults of task mine and task validate --all.
const (
	defaultMineLimit = 10
	defaultJobs      = 2
)

// mineArgs is what task mine was asked for.
type mineArgs struct {
	opts    mine.Options // Since, MaxFiles, MaxLines and RequireLock
	limit   int
	jobs    int
	timeout time.Duration
	verify  stringList
	setup   stringList
	dryRun  bool
}

// parseMine reads and checks task mine's arguments; a mistake is reported and its exit code returned.
func parseMine(env Env, args []string) (a mineArgs, code int, ok bool) {
	fs := flag.NewFlagSet("task mine", flag.ContinueOnError)
	since := fs.String("since", "", "only commits from this date on (YYYY-MM-DD, UTC)")
	fs.IntVar(&a.limit, "limit", defaultMineLimit, "how many of the best candidates to import (with --dry-run: to list)")
	fs.IntVar(&a.opts.MaxFiles, "max-files", mine.DefaultMaxFiles, "skip commits that change more test and code files than this")
	fs.IntVar(&a.opts.MaxLines, "max-lines", mine.DefaultMaxLines, "skip commits that change more test and code lines than this")
	fs.BoolVar(&a.dryRun, "dry-run", false, "list the candidates and why other commits were set aside; save nothing")
	fs.BoolVar(&a.opts.RequireLock, "require-lock", false, "set aside Python commits whose base pins no dependencies (no uv.lock or pinned requirements)")
	fs.IntVar(&a.jobs, "jobs", defaultJobs, "how many imported tasks to validate at once")
	fs.DurationVar(&a.timeout, "timeout", 10*time.Minute, "time limit for each verification command")
	fs.Var(&a.verify, "verify", "a verification command for the tasks (repeatable; default: the detected test commands)")
	fs.Var(&a.setup, "setup", "a command a fresh checkout needs first (repeatable)")
	rest, code, ok := parseArgs(env, fs, args, taskUsage)
	if !ok {
		return a, code, false
	}
	usage := func(format string, args ...any) (mineArgs, int, bool) {
		fmt.Fprintf(env.Stderr, "agentium task mine: "+format+"\n", args...)
		return a, ExitUsage, false
	}
	var applied []string
	fs.Visit(func(f *flag.Flag) {
		if slices.Contains([]string{"jobs", "timeout", "verify", "setup"}, f.Name) {
			applied = append(applied, "--"+f.Name)
		}
	})
	switch {
	case len(rest) != 0:
		return usage("takes no arguments (got %q)", strings.Join(rest, " "))
	case a.limit < 1:
		return usage("--limit must be at least 1")
	case a.opts.MaxFiles < 1 || a.opts.MaxLines < 1:
		return usage("--max-files and --max-lines must be at least 1")
	case a.jobs < 1:
		return usage("--jobs must be at least 1")
	case a.dryRun && len(applied) > 0:
		return usage("--dry-run imports and validates nothing, so %s do(es) not apply", strings.Join(applied, ", "))
	}
	if *since != "" {
		day, err := time.Parse(time.DateOnly, *since)
		if err != nil {
			return usage("--since %q is not a date like 2026-01-31", *since)
		}
		a.opts.Since = day
	}
	return a, ExitOK, true
}

func taskMine(ctx context.Context, env Env, args []string) int {
	a, code, ok := parseMine(env, args)
	if !ok {
		return code
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	prep, err := mine.Prepare(ctx, mine.PrepareInput{DB: w.db, ProjectID: w.project.ID, Root: w.root, Options: a.opts, Verify: a.verify,
		DefaultVerify: w.defaultVerify(), DryRun: a.dryRun})
	if err != nil {
		return fail(env, err)
	}
	st, res := env.style(), prep.Result
	printScan(env, prep)
	if a.dryRun {
		if env.JSON {
			return env.emit(env.mineDocument(prep, res.Candidates[:min(a.limit, len(res.Candidates))], true))
		}
		return printDryRun(env, prep, a.limit)
	}
	if len(res.Candidates) == 0 && env.JSON { // nothing to do is a success
		return env.emit(env.mineDocument(prep, nil, false))
	}
	if len(res.Candidates) == 0 {
		fmt.Fprintf(env.Stdout, "Nothing to import: %s shows why the commits were set aside\n", st.Command("agentium task mine --dry-run"))
		return ExitOK
	}
	// Candidates are imported best first until --limit of them succeed; those that fail stay in the table.
	_, live := liveEnv(env) // nothing prints while it shows
	imp := mine.Import(ctx, mine.ImportInput{Importer: w.importer(prep.Names), Candidates: res.Candidates, Limit: a.limit,
		NewTask: func() store.Task {
			return store.Task{ProjectID: w.project.ID, Verify: prep.Verify, Setup: a.setup, CreatedAt: env.Now()}
		},
		Progress: func(imported int, c mine.Candidate) {
			live.Step(fmt.Sprintf("importing %d of %d: %s", imported+1, a.limit, experiment.ShortCommit(c.Hash)))
		}})
	live.Stop()
	if imp.Interrupted && env.JSON {
		doc := env.mineDocument(prep, nil, false)
		doc.Tried, doc.Imported, doc.Interrupted = imp.Tried, len(imp.Tasks), true
		return env.emitCode(doc, ExitError)
	}
	if imp.Interrupted {
		fmt.Fprintf(env.Stdout, "Interrupted: %d task(s) imported, not validated; %s validates them\n", len(imp.Tasks),
			st.Command("agentium task validate --all --status unvalidated"))
		return ExitError
	}
	return finishMine(ctx, env, w, a, prep, imp)
}

// printScan reports what the scan read and the tests it picked commits by.
func printScan(env Env, prep mine.Prepared) {
	st, res := env.style(), prep.Result
	fmt.Fprintf(env.Stdout, "%s: %d commit(s) read, %d candidate(s)\n", st.Heading(fmt.Sprintf("Mined %s at %s", res.Ref, experiment.ShortCommit(res.Head))),
		res.Scanned, len(res.Candidates))
	if res.Shallow {
		fmt.Fprintln(env.Stdout, note(st, "this is a shallow clone: older history is missing (git fetch --unshallow to mine it)"))
	}
	if langs := prep.Options.Languages; len(langs) > 0 {
		// A tool that proposes no test command of its own (Python: discovery reads the files) is run by the verify
		// commands; without either, the note names none.
		msg, runs := "only commits with "+strings.Join(langs, " or ")+" tests count", prep.Options.TestCommand
		if runs == "" {
			runs = strings.Join(prep.Verify, ", ")
		}
		if runs != "" {
			msg += ", as " + runs + " runs them"
		}
		fmt.Fprintln(env.Stdout, note(st, msg))
	}
}

// printDryRun lists the best candidates and why the others were set aside; nothing is saved.
func printDryRun(env Env, prep mine.Prepared, limit int) int {
	st, res := env.style(), prep.Result
	top := res.Candidates[:min(limit, len(res.Candidates))]
	if len(prep.Verify) > 0 {
		fmt.Fprintln(env.Stdout, note(st, "mined tasks will verify with: "+strings.Join(prep.Verify, "; ")+" (--verify to change)"))
	}
	if err := printCandidates(env, top, len(res.Candidates)); err != nil {
		return fail(env, err)
	}
	if err := printRejections(env, res); err != nil {
		return fail(env, err)
	}
	if len(top) > 0 {
		fmt.Fprintf(env.Stdout, "Next: %s imports and validates them\n", st.Command(fmt.Sprintf("agentium task mine --limit %d", len(top))))
	}
	return ExitOK
}

// finishMine validates the imported tasks and prints the table and what to do next.
func finishMine(ctx context.Context, env Env, w *workspace, a mineArgs, prep mine.Prepared, imp mine.Imported) int {
	st := env.style()
	var rows []batchRow
	for _, f := range imp.Failed {
		rows = append(rows, batchRow{name: taskName(f.Candidate.Subject, f.Candidate.Hash), commit: f.Candidate.Hash, problem: "not imported: " + f.Err.Error()})
	}
	if len(imp.Tasks) == 0 && env.JSON {
		doc := env.mineDocument(prep, nil, false)
		doc.Tried, doc.Rows = imp.Tried, batchRowDocs(ctx, env, w, rows)
		return env.emitCode(doc, ExitError)
	}
	if len(imp.Tasks) == 0 {
		fmt.Fprintln(env.Stdout, st.Heading(fmt.Sprintf("Imported none of %d candidate(s)", imp.Tried)))
		if err := printBatchTable(ctx, env, w, rows); err != nil {
			return fail(env, err)
		}
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "%s (verify: %s); validating them, %d at a time\n",
		st.Heading(fmt.Sprintf("Imported %d of %d candidate(s) tried", len(imp.Tasks), imp.Tried)), strings.Join(prep.Verify, "; "), a.jobs)
	results, err := validateBatch(ctx, env, w, imp.Tasks, task.ValidateOptions{Arms: []task.Arm{{Name: "base"}}, Repeat: 1, Timeout: a.timeout}, a.jobs)
	if err != nil {
		return fail(env, err)
	}
	valid := 0
	for _, r := range results {
		if r.Validated && task.ValidationOf(r.Task).Status == task.StatusValid {
			valid++
		}
	}
	if env.JSON {
		doc := env.mineDocument(prep, nil, false)
		doc.Tried, doc.Imported, doc.Valid, doc.Interrupted = imp.Tried, len(imp.Tasks), valid, ctx.Err() != nil
		doc.Rows = batchRowDocs(ctx, env, w, append(batchRows(results), rows...))
		code := ExitOK
		if doc.Interrupted {
			code = ExitError
		}
		return env.emitCode(doc, code)
	}
	if err := printBatchTable(ctx, env, w, append(batchRows(results), rows...)); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "%d of %d imported task(s) are valid.\n", valid, len(imp.Tasks))
	if ctx.Err() != nil {
		return interrupted(env, results)
	}
	batchAdvice(env, results, a.jobs)
	if left := len(prep.Result.Candidates) - imp.Tried; valid < a.limit && left > 0 {
		fmt.Fprintf(env.Stdout, "%d candidate(s) are left: %s mines more\n", left, st.Command(fmt.Sprintf("agentium task mine --limit %d", a.limit-valid)))
	}
	fmt.Fprintf(env.Stdout, "%s: %s, then %s.\n"+
		"Keep the invalid ones: experiments use only valid tasks, and mining skips every commit that is a task (a removed task's commit is mined again).\n",
		st.Warn("Review each mined instruction for solution leaks"), st.Command("agentium task show NAME"), st.Command("task edit NAME --reviewed"))
	return ExitOK
}

// importer is how mined candidates become tasks: through the path task import --commit takes (commitTask, completeTask).
func (w *workspace) importer(names map[string]bool) mine.Importer {
	return mine.Importer{DB: w.db, ProjectID: w.project.ID, Names: names,
		Commit: func(ctx context.Context, c mine.Candidate, t store.Task) (store.Task, error) {
			return w.commitTask(ctx, c.Hash, c.Instruction(), t)
		},
		Complete: func(ctx context.Context, t store.Task) (store.Task, error) { return w.completeTask(ctx, t, judgeNever) }}
}

// maxSubject is how many characters of a subject the candidates table shows.
const maxSubject = 60

// printCandidates lists the best candidates with their score and its parts.
func printCandidates(env Env, top []mine.Candidate, total int) error {
	st := env.style()
	if len(top) == 0 {
		fmt.Fprintln(env.Stdout, "No candidates.")
		return nil
	}
	table := term.NewTable(st, term.Right("#"), term.Left("COMMIT"), term.Right("SCORE"), term.Left("SUBJECT"), term.Right("TESTS"),
		term.Right("CODE"), term.Right("LINES"))
	for i, c := range top {
		table.Row(strconv.Itoa(i+1), experiment.ShortCommit(c.Hash), strconv.Itoa(c.Score), cut(c.Subject, maxSubject), strconv.Itoa(len(c.Tests)),
			strconv.Itoa(len(c.Code)), strconv.Itoa(c.Lines))
		parts := make([]string, len(c.Reasons))
		for j, p := range c.Reasons {
			parts[j] = p.String()
		}
		table.Line(st.Note("    " + strings.Join(parts, ", ")))
	}
	if err := table.Write(env.Stdout); err != nil {
		return err
	}
	if more := total - len(top); more > 0 {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("%d more candidate(s); --limit N lists more", more)))
	}
	return nil
}

// printRejections counts the commits set aside, per reason, in the order the scan checks them.
func printRejections(env Env, res mine.Result) error {
	if len(res.Rejected) == 0 {
		return nil
	}
	st := env.style()
	fmt.Fprintln(env.Stdout, st.Heading(fmt.Sprintf("Set aside: %d commit(s)", len(res.Rejected))))
	counts := res.Counts()
	table := term.NewTable(st, term.Left(""), term.Right(""))
	table.Indent = "  "
	for _, r := range mine.ReasonOrder() {
		if counts[r] > 0 {
			table.Row(string(r), strconv.Itoa(counts[r]))
		}
	}
	return table.Write(env.Stdout)
}

// cut shortens text to at most n characters, ending with an ellipsis when it cut.
func cut(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n-1]) + "…"
}

// validateAll validates the project's tasks (those with status, when given) jobs at a time and prints one table.
func validateAll(ctx context.Context, env Env, w *workspace, status string, o task.ValidateOptions, jobs int) int {
	tasks, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	if status != "" {
		tasks = task.FilterByStatus(tasks, status)
	}
	st := env.style()
	if len(tasks) == 0 && env.JSON { // nothing to validate is a success
		return env.emit(validateAllDoc{header: env.hdr(), Rows: []batchRowDoc{}})
	}
	if len(tasks) == 0 {
		if status != "" {
			fmt.Fprintf(env.Stdout, "No %s tasks to validate.\n", status)
		} else {
			fmt.Fprintf(env.Stdout, "No tasks yet: %s\n", st.Command("agentium task mine"))
		}
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "%s, %d at a time\n", st.Heading(fmt.Sprintf("Validating %d task(s) in %d arm(s)", len(tasks), len(o.Arms))), jobs)
	results, err := validateBatch(ctx, env, w, tasks, o, jobs)
	if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		doc := validateAllDoc{header: env.hdr(), Rows: batchRowDocs(ctx, env, w, batchRows(results)), Total: len(results), Interrupted: ctx.Err() != nil}
		for _, r := range results {
			if r.Validated && task.ValidationOf(r.Task).Status == task.StatusValid {
				doc.Valid++
			}
		}
		return env.emitCode(doc, batchExit(ctx, results))
	}
	if err := printBatchTable(ctx, env, w, batchRows(results)); err != nil {
		return fail(env, err)
	}
	if ctx.Err() != nil {
		return interrupted(env, results)
	}
	batchAdvice(env, results, jobs)
	for _, r := range results {
		if !r.Validated {
			return ExitError
		}
		if s := task.ValidationOf(r.Task).Status; s == task.StatusInvalid || s == task.StatusFlaky {
			return ExitError
		}
	}
	return ExitOK
}

// batchExit is task validate --all's exit code: a failure when interrupted, or when any task is not validated, invalid or flaky.
func batchExit(ctx context.Context, results []task.BatchResult) int {
	if ctx.Err() != nil {
		return ExitError
	}
	for _, r := range results {
		if !r.Validated || validationExit(task.ValidationOf(r.Task).Status) != ExitOK {
			return ExitError
		}
	}
	return ExitOK
}

// batchAdvice suggests validating invalid or flaky tasks again one at a time after a batch that ran several at once:
// tests that share ports, temporary paths or databases can fail only side by side.
func batchAdvice(env Env, results []task.BatchResult, jobs int) {
	if jobs < 2 {
		return
	}
	var cmds []string
	for _, s := range []string{task.StatusInvalid, task.StatusFlaky} {
		if slices.ContainsFunc(results, func(r task.BatchResult) bool { return r.Validated && task.ValidationOf(r.Task).Status == s }) {
			cmds = append(cmds, env.style().Command("agentium task validate --all --status "+s+" --jobs 1"))
		}
	}
	if len(cmds) > 0 {
		fmt.Fprintf(env.Stdout, "%s: %s\n", note(env.style(), "some tasks failed while others ran beside them; if their tests share ports, temporary paths or databases, check them alone"),
			strings.Join(cmds, " and "))
	}
}

// interrupted reports how far an interrupted batch got and returns ExitError.
func interrupted(env Env, results []task.BatchResult) int {
	done := 0
	for _, r := range results {
		if r.Validated {
			done++
		}
	}
	fmt.Fprintf(env.Stdout, "Interrupted: %d of %d task(s) validated and kept; %s finishes the rest\n", done, len(results),
		env.style().Command("agentium task validate --all --status unvalidated"))
	return ExitError
}

// validateBatch validates tasks with o, jobs at a time (task.Validating.Batch), with a live line naming those in progress.
// The error is for what stops the whole batch before it starts.
func validateBatch(ctx context.Context, env Env, w *workspace, tasks []store.Task, o task.ValidateOptions, jobs int) ([]task.BatchResult, error) {
	return validateBatchWith(ctx, env, w, tasks, o, jobs, false)
}

// validateBatchWith is validateBatch; skipInUse stores nothing for a task that a locked experiment able to run uses
// (task.Validating.SkipInUse: the task pool's re-validations).
func validateBatchWith(ctx context.Context, env Env, w *workspace, tasks []store.Task, o task.ValidateOptions, jobs int, skipInUse bool) ([]task.BatchResult, error) {
	if len(tasks) == 0 {
		return []task.BatchResult{}, nil
	}
	buildEnv, err := run.BuildEnv(w.layout)
	if err != nil {
		return nil, err
	}
	v := w.validating(env, buildEnv)
	if v.Toolchain, err = w.hostToolchain(ctx, env); err != nil {
		return nil, err
	}
	v.SkipInUse = skipInUse
	env, live := liveEnv(env)
	defer live.Stop()
	out := task.BatchOutput{Out: env.Stdout, Style: env.style(), Show: live.Show, RunsBusy: w.layout.RunsBusy()}
	return v.Batch(ctx, out, tasks, o, jobs), nil
}

// hostToolchain is the versions of the project's build tools on this host (pool.DetectToolchain, for the build tools
// detected at the repository's root), asked once per command: every validation records them, and the task pool
// re-validates a task when they change. A tool that cannot be asked is left out.
func (w *workspace) hostToolchain(ctx context.Context, env Env) (task.Toolchain, error) {
	if w.toolchain != nil {
		return w.toolchain, nil
	}
	var environ []string
	if env.Environ != nil {
		environ = env.Environ()
	}
	found, err := pool.DetectToolchain(ctx, buildtool.DetectIn(w.root), pool.HostVersions(w.layout.Root, environ))
	if err != nil {
		return nil, err
	}
	w.toolchain = found
	return found, nil
}

// batchRow is a row of the table task mine and task validate --all end with: a task, or a commit that did not
// become one.
type batchRow struct {
	task    *store.Task
	name    string // without a task
	commit  string // without a task
	problem string // replaces the task's status: why it was not imported or validated
}

// batchRows makes the table's rows of a batch's results.
func batchRows(results []task.BatchResult) []batchRow {
	rows := make([]batchRow, len(results))
	for i := range results {
		rows[i] = batchRow{task: &results[i].Task, problem: results[i].Problem()}
	}
	return rows
}

// printBatchTable prints the rows: name, commit, hidden tests, reference files, and the status with its reason. It
// runs after an interrupt too (the unstated-requirement checks read git briefly), so it ignores ctx's cancellation.
func printBatchTable(ctx context.Context, env Env, w *workspace, rows []batchRow) error {
	ctx = context.WithoutCancel(ctx)
	st := env.style()
	fair := task.NewFairness("--git-dir", w.bare)
	table := term.NewTable(st, term.Left("NAME"), term.Left("COMMIT"), term.Right("TESTS"), term.Right("FILES"), term.Left("STATUS"))
	for _, r := range rows {
		if r.task == nil {
			table.Row(term.OrNone(r.name), experiment.ShortCommit(r.commit), "-", "-", st.Bad(r.problem))
			continue
		}
		t := *r.task
		status := st.Bad(r.problem)
		if r.problem == "" {
			status = taskStatus(ctx, st, fair, t)
		}
		table.Row(t.Name, experiment.ShortCommit(t.SolutionCommit), strconv.Itoa(len(t.HiddenTests)), strconv.Itoa(len(t.Reference)), status)
	}
	return table.Write(env.Stdout)
}
