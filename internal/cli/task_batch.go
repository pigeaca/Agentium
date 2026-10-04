package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/mine"
	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// scanNote says which tests mining picks commits by: those of the languages the project's build tools test, run by
// their own test command or, for a tool that proposes none (Python: discovery reads the files), by the verify
// commands; without either, the note names no runner. "" when every commit counts.
func scanNote(opts mine.Options, verify []string) string {
	if len(opts.Languages) == 0 {
		return ""
	}
	msg, runs := "only commits with "+strings.Join(opts.Languages, " or ")+" tests count", opts.TestCommand
	if runs == "" {
		runs = strings.Join(verify, ", ")
	}
	if runs != "" {
		msg += ", as " + runs + " runs them"
	}
	return msg
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

// setAside is a row of the table of commits set aside: why, and how many.
type setAside struct {
	reason string
	count  int
}

// rejections counts the commits a scan set aside, per reason, in the order the scan checks them.
func rejections(res mine.Result) []setAside {
	counts := res.Counts()
	var rows []setAside
	for _, r := range mine.ReasonOrder() {
		if counts[r] > 0 {
			rows = append(rows, setAside{string(r), counts[r]})
		}
	}
	return rows
}

// printSetAside prints the commits set aside, per reason.
func printSetAside(env Env, rows []setAside) error {
	total := 0
	for _, r := range rows {
		total += r.count
	}
	if total == 0 {
		return nil
	}
	st := env.style()
	fmt.Fprintln(env.Stdout, st.Heading(fmt.Sprintf("Set aside: %d commit(s)", total)))
	table := term.NewTable(st, term.Left(""), term.Right(""))
	table.Indent = "  "
	for _, r := range rows {
		if r.count > 0 {
			table.Row(r.reason, strconv.Itoa(r.count))
		}
	}
	return table.Write(env.Stdout)
}

// cut shortens text to at most n cells, the unit the table pads by, ending with an ellipsis when it cut.
func cut(text string, n int) string { return term.Truncate(text, n, "…") }

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
			fmt.Fprintf(env.Stdout, "No tasks yet: %s\n", st.Command("agentium pool update"))
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
	// Once per command: sandbox-exec and a log that shows its denials (each task's preparation checks sandbox-exec only).
	if err := run.SandboxUsable(ctx, o.Grader); err != nil {
		return nil, err
	}
	buildEnv, err := run.BuildEnv(w.layout)
	if err != nil {
		return nil, err
	}
	v := w.validating(env, buildEnv, o.Timeout, o.Grader)
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
	tools, err := w.buildTools(ctx)
	if err != nil {
		return nil, err
	}
	found, err := pool.DetectToolchain(ctx, tools, pool.HostVersions(w.layout.Root, environ))
	if err != nil {
		return nil, err
	}
	w.toolchain = found
	return found, nil
}

// buildTools are the build tools detected at the repository's root and in every module a task runs in: validations
// record the versions of the tools they use, and container grading needs their images.
func (w *workspace) buildTools(ctx context.Context) ([]string, error) {
	modules, err := w.taskModules(ctx)
	if err != nil {
		return nil, err
	}
	var tools []string
	for _, module := range modules {
		for _, name := range buildtool.DetectIn(moduleDir(w.root, module)) {
			if !slices.Contains(tools, name) {
				tools = append(tools, name)
			}
		}
	}
	return tools, nil
}

// batchRow is a row of the table pool update and task validate --all end with: a task, or a commit that did not
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
