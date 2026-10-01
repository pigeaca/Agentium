package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/mine"
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

func taskMine(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("task mine", flag.ContinueOnError)
	since := fs.String("since", "", "only commits from this date on (YYYY-MM-DD, UTC)")
	limit := fs.Int("limit", defaultMineLimit, "how many of the best candidates to import (with --dry-run: to list)")
	maxFiles := fs.Int("max-files", mine.DefaultMaxFiles, "skip commits that change more test and code files than this")
	maxLines := fs.Int("max-lines", mine.DefaultMaxLines, "skip commits that change more test and code lines than this")
	dryRun := fs.Bool("dry-run", false, "list the candidates and why other commits were set aside; save nothing")
	jobs := fs.Int("jobs", defaultJobs, "how many imported tasks to validate at once")
	timeout := fs.Duration("timeout", 10*time.Minute, "time limit for each verification command")
	var verify, setup stringList
	fs.Var(&verify, "verify", "a verification command for the tasks (repeatable; default: the detected test commands)")
	fs.Var(&setup, "setup", "a command a fresh checkout needs first (repeatable)")
	rest, code, ok := parseArgs(env, fs, args, taskUsage)
	if !ok {
		return code
	}
	usage := func(format string, args ...any) int {
		fmt.Fprintf(env.Stderr, "agentium task mine: "+format+"\n", args...)
		return ExitUsage
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
	case *limit < 1:
		return usage("--limit must be at least 1")
	case *maxFiles < 1 || *maxLines < 1:
		return usage("--max-files and --max-lines must be at least 1")
	case *jobs < 1:
		return usage("--jobs must be at least 1")
	case *dryRun && len(applied) > 0:
		return usage("--dry-run imports and validates nothing, so %s do(es) not apply", strings.Join(applied, ", "))
	}
	opts := mine.Options{MaxFiles: *maxFiles, MaxLines: *maxLines, Exclude: map[string]bool{}}
	if *since != "" {
		day, err := time.Parse(time.DateOnly, *since)
		if err != nil {
			return usage("--since %q is not a date like 2026-01-31", *since)
		}
		opts.Since = day
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	if !*dryRun && len(verify) == 0 && len(w.defaultVerify()) == 0 {
		return fail(env, errors.New("no test commands were detected for this project: pass --verify CMD"))
	}
	existing, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	names := map[string]bool{}
	for _, t := range existing {
		names[t.Name] = true
		if t.SolutionCommit != "" {
			opts.Exclude[t.SolutionCommit] = true
		}
	}
	opts.Languages, opts.TestCommand = testLanguages(w.root)

	st := env.style()
	res, err := mine.Scan(ctx, w.root, opts)
	if err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "%s: %d commit(s) read, %d candidate(s)\n", st.Heading(fmt.Sprintf("Mined %s at %s", res.Ref, shortCommit(res.Head))),
		res.Scanned, len(res.Candidates))
	if res.Shallow {
		fmt.Fprintln(env.Stdout, note(st, "this is a shallow clone: older history is missing (git fetch --unshallow to mine it)"))
	}
	if len(opts.Languages) > 0 {
		fmt.Fprintln(env.Stdout, note(st, "only commits with "+strings.Join(opts.Languages, " or ")+" tests count, as "+opts.TestCommand+" runs them"))
	}
	top := res.Candidates[:min(*limit, len(res.Candidates))]
	if *dryRun {
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
	if len(top) == 0 {
		fmt.Fprintf(env.Stdout, "Nothing to import: %s shows why the commits were set aside\n", st.Command("agentium task mine --dry-run"))
		return ExitOK
	}

	_, live := liveEnv(env) // nothing prints while it shows
	var rows []batchRow
	var imported []store.Task
	for i, c := range top {
		live.Step(fmt.Sprintf("importing %d of %d: %s", i+1, len(top), shortCommit(c.Hash)))
		t, err := w.mineTask(ctx, c, store.Task{ProjectID: w.project.ID, Verify: verify, Setup: setup, CreatedAt: env.Now()}, names)
		if ctx.Err() != nil {
			live.Stop()
			fmt.Fprintf(env.Stdout, "Interrupted: %d task(s) imported, not validated; %s validates them\n", len(imported),
				st.Command("agentium task validate --all --status unvalidated"))
			return ExitError
		}
		if err != nil {
			rows = append(rows, batchRow{name: taskName(c.Subject, c.Hash), commit: c.Hash, problem: "not imported: " + err.Error()})
			continue
		}
		imported = append(imported, t)
	}
	live.Stop()
	fmt.Fprintf(env.Stdout, "%s, %d at a time\n", st.Heading(fmt.Sprintf("Imported %d of %d candidate(s); validating them", len(imported), len(top))), *jobs)
	results, err := validateBatch(ctx, env, w, imported, validateOptions{arms: []task.Arm{{Name: "base"}}, repeat: 1, timeout: *timeout}, *jobs)
	if err != nil {
		return fail(env, err)
	}
	rows = append(batchRows(results), rows...)
	if err := printBatchTable(ctx, env, w, rows); err != nil {
		return fail(env, err)
	}
	valid := 0
	for _, r := range results {
		if r.validated && validationOf(r.task).Status == task.StatusValid {
			valid++
		}
	}
	fmt.Fprintf(env.Stdout, "%d of %d imported task(s) are valid.\n", valid, len(imported))
	fmt.Fprintf(env.Stdout, "%s: %s, then %s; mining again skips these commits (%s removes a task you do not want)\n",
		st.Warn("Review each mined instruction for solution leaks"), st.Command("agentium task show NAME"),
		st.Command("task edit NAME --reviewed"), st.Command("task rm NAME"))
	if ctx.Err() != nil {
		return interrupted(env, results)
	}
	return ExitOK
}

// mineTask imports candidate c as a task, through the path task import --commit takes (commitTask, completeTask),
// with c's instruction and a name not in names, which it then adds there.
func (w *workspace) mineTask(ctx context.Context, c mine.Candidate, t store.Task, names map[string]bool) (store.Task, error) {
	t, err := w.commitTask(ctx, c.Hash, c.Instruction(), t)
	if err != nil {
		return t, err
	}
	base := t.Name
	for n := 2; names[t.Name]; n++ {
		t.Name = fmt.Sprintf("%s-%d", base, n)
	}
	if t, err = w.completeTask(ctx, t, judgeNever); err != nil {
		return t, err
	}
	for n := 2; ; n++ {
		saved, err := w.db.SaveTask(ctx, t)
		if errors.Is(err, store.ErrExists) { // made by another process since the list was read
			names[t.Name], t.Name = true, fmt.Sprintf("%s-%d", base, n)
			continue
		}
		if err != nil {
			return t, err
		}
		names[saved.Name] = true
		return saved, nil
	}
}

// testLanguages names the languages of the tests the project's detected build tools run (mine.Options.Languages),
// and their test commands.
func testLanguages(root string) (languages []string, commands string) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	var tests []string
	for _, p := range buildtool.Detected(has) {
		languages = append(languages, p.Languages...)
		if c := p.TestCommand(has); c != "" {
			tests = append(tests, c)
		}
	}
	return languages, strings.Join(tests, ", ")
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
		table.Row(strconv.Itoa(i+1), shortCommit(c.Hash), strconv.Itoa(c.Score), cut(c.Subject, maxSubject), strconv.Itoa(len(c.Tests)),
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

// batchStatuses are the values task validate --all --status takes.
var batchStatuses = []string{"unvalidated", task.StatusValid, task.StatusInvalid, task.StatusFlaky, task.StatusUnchecked}

// validateAll validates the project's tasks (those with status, when given) jobs at a time and prints one table.
func validateAll(ctx context.Context, env Env, w *workspace, status string, o validateOptions, jobs int) int {
	tasks, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return fail(env, err)
	}
	if status != "" {
		tasks = slices.DeleteFunc(tasks, func(t store.Task) bool { return statusOf(t) != status })
	}
	st := env.style()
	if len(tasks) == 0 {
		if status != "" {
			fmt.Fprintf(env.Stdout, "No %s tasks to validate.\n", status)
		} else {
			fmt.Fprintf(env.Stdout, "No tasks yet: %s\n", st.Command("agentium task mine"))
		}
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "%s, %d at a time\n", st.Heading(fmt.Sprintf("Validating %d task(s) in %d arm(s)", len(tasks), len(o.arms))), jobs)
	results, err := validateBatch(ctx, env, w, tasks, o, jobs)
	if err != nil {
		return fail(env, err)
	}
	if err := printBatchTable(ctx, env, w, batchRows(results)); err != nil {
		return fail(env, err)
	}
	if ctx.Err() != nil {
		return interrupted(env, results)
	}
	for _, r := range results {
		if !r.validated {
			return ExitError
		}
		if s := validationOf(r.task).Status; s == task.StatusInvalid || s == task.StatusFlaky {
			return ExitError
		}
	}
	return ExitOK
}

// interrupted reports how far an interrupted batch got and returns ExitError.
func interrupted(env Env, results []batchResult) int {
	done := 0
	for _, r := range results {
		if r.validated {
			done++
		}
	}
	fmt.Fprintf(env.Stdout, "Interrupted: %d of %d task(s) validated and kept; %s finishes the rest\n", done, len(results),
		env.style().Command("agentium task validate --all --status unvalidated"))
	return ExitError
}

// statusOf is a task's status for --status: "unvalidated", or its stored validation's status.
func statusOf(t store.Task) string {
	if t.Validation == nil {
		return "unvalidated"
	}
	return validationOf(t).Status
}

// validationOf decodes a task's stored validation (the zero Validation when there is none or it is unreadable).
func validationOf(t store.Task) task.Validation {
	var v task.Validation
	if t.Validation != nil {
		_ = json.Unmarshal(t.Validation, &v) // an unreadable validation has no status, which no filter matches
	}
	return v
}

// batchResult is one task's outcome in a batch validation.
type batchResult struct {
	task      store.Task // with its new validation when validated
	validated bool       // the validation finished and is stored
	started   bool
	stopped   bool  // the interrupt stopped it
	err       error // why a started validation did not finish
}

// validateBatch validates tasks with o, jobs at a time, as task validate would one by one, but without per-stage
// output: one line per finished task, and a live line naming the tasks in progress. Each finished validation is stored
// at once (by this goroutine only: workers touch no database), so an interrupt keeps every result that finished; the
// tasks it stopped, or that had not started, keep their earlier validation. The error is for what stops the whole
// batch before it starts.
//
// Validations share nothing that needs serializing: each has its own checkouts and logs (per task ID), and they share
// the build cache of Agentium's commands, which Go keeps safe for concurrent builds. They start no agents, so they do
// not take the run lock.
func validateBatch(ctx context.Context, env Env, w *workspace, tasks []store.Task, o validateOptions, jobs int) ([]batchResult, error) {
	results := make([]batchResult, len(tasks))
	for i, t := range tasks {
		results[i].task = t
	}
	if len(tasks) == 0 {
		return results, nil
	}
	buildEnv, err := run.BuildEnv(w.layout)
	if err != nil {
		return nil, err
	}
	env, live := liveEnv(env)
	defer live.Stop()
	st := env.style()

	var mu sync.Mutex // guards current and finished, which the live line reads
	current, finished := map[string]string{}, 0
	live.Show(func() string {
		mu.Lock()
		defer mu.Unlock()
		var doing []string
		for _, t := range tasks {
			if stage, ok := current[t.Name]; ok {
				doing = append(doing, t.Name+" ("+stage+")")
			}
		}
		text := fmt.Sprintf("validated %d of %d", finished, len(tasks))
		if len(doing) > 0 {
			text += "; " + strings.Join(doing, ", ")
		}
		return text
	})

	type outcome struct {
		i       int
		result  task.Validation
		err     error
		started bool
	}
	next, done := make(chan int), make(chan outcome)
	var wg sync.WaitGroup
	for range min(jobs, len(tasks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if ctx.Err() != nil {
					done <- outcome{i: i, err: ctx.Err()}
					continue
				}
				t := tasks[i]
				started := func(arm, stage string) {
					mu.Lock()
					current[t.Name] = arm + ", " + stage
					mu.Unlock()
				}
				result, err := w.validateQuiet(ctx, t, o, buildEnv, env.Now, started)
				mu.Lock()
				delete(current, t.Name)
				mu.Unlock()
				done <- outcome{i: i, result: result, err: err, started: true}
			}
		}()
	}
	go func() {
		defer close(next)
		for i := range tasks {
			select {
			case next <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
	for out := range done {
		r := &results[out.i]
		r.started = out.started
		if out.err == nil {
			if r.task, out.err = w.storeValidation(ctx, r.task, out.result, env.Now()); out.err == nil {
				r.validated = true
			}
		}
		if !out.started {
			continue
		}
		r.err, r.stopped = out.err, out.err != nil && ctx.Err() != nil
		mu.Lock()
		finished++
		mu.Unlock()
		if r.validated {
			fmt.Fprintf(env.Stdout, "  %s  %s\n", r.task.Name, st.Status(out.result.Summary()))
		} else {
			fmt.Fprintf(env.Stdout, "  %s  %s\n", r.task.Name, st.Bad(r.problem()))
		}
	}
	return results, nil
}

// problem says why the task's validation is not in the table: "" when it is.
func (r batchResult) problem() string {
	switch {
	case r.validated:
		return ""
	case !r.started:
		return "not started (interrupted)"
	case r.stopped:
		return "interrupted"
	case r.err != nil:
		return "not validated: " + r.err.Error()
	}
	return "not validated"
}

// validateQuiet validates t as task validate NAME does, without printing: started is told each stage.
func (w *workspace) validateQuiet(ctx context.Context, t store.Task, o validateOptions, buildEnv []string, now func() time.Time,
	started func(arm, stage string)) (task.Validation, error) {
	if t.Grading == task.GradingJudge {
		result, _, err := w.judgedValidation(ctx, t, now())
		return result, err
	}
	if t.SolutionCommit != "" {
		if err := task.RefuseInlineRustTests(ctx, t.BaseCommit, t.SolutionCommit, t.Reference, "--git-dir", w.bare); err != nil {
			return task.Validation{}, err
		}
	}
	v := w.validator(t, o, buildEnv, now)
	v.Started = started
	return v.Validate(ctx, taskSpec(t), o.arms)
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
func batchRows(results []batchResult) []batchRow {
	rows := make([]batchRow, len(results))
	for i := range results {
		rows[i] = batchRow{task: &results[i].task, problem: results[i].problem()}
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
			table.Row(orNone(r.name), shortCommit(r.commit), "-", "-", st.Bad(r.problem))
			continue
		}
		t := *r.task
		status := st.Bad(r.problem)
		if r.problem == "" {
			status = taskStatus(ctx, st, fair, t)
		}
		table.Row(t.Name, shortCommit(t.SolutionCommit), strconv.Itoa(len(t.HiddenTests)), strconv.Itoa(len(t.Reference)), status)
	}
	return table.Write(env.Stdout)
}
