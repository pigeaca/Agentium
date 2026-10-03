package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/mine"
	"github.com/pigeaca/agentium/internal/pool"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

const poolUsage = `Usage:
  agentium pool update [--dry-run] [--accept-mined] [--limit N]
                         one pass over the task pool; it runs no agent and costs nothing: mines the commits of
                         the default branch since the last pass (within 270 days), imports up to --limit (default
                         10) as tasks that need your review and validates them; re-validates stale tasks
                         (validated more than 30 days ago, with other build-tool versions, or flaky and untried
                         for 7 days, with --repeat 3); retires tasks whose base is 270 or more days old or whose
                         files are gone from the default branch (a flag, never a delete). Tasks a locked
                         experiment uses are kept as they are. While an experiment is running, re-validations are
                         skipped. --dry-run lists what it would do and writes nothing: the candidates with their
                         scores, why other commits were set aside, and the tasks it would validate, re-validate
                         and retire. --accept-mined accepts the tasks this pass imported without your review,
                         after the automatic checks start --accept-mined makes. Mined tasks verify, set up and
                         validate as the project's settings say (agentium init); passes read only new commits, so
                         commits a pass set aside are not read again by later ones (the guide's advanced flags
                         re-read them)
  agentium pool status   the pool's health: valid, weak, flaky, invalid, awaiting review and retired tasks, the
                         last pass and the oldest valid base
  agentium pool update|status ... --json
                         one JSON document instead of text (docs/guide.md, "Scripting and automation")
`

func runPool(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, poolUsage)
		return ExitUsage
	}
	switch args[0] {
	case "update":
		return poolUpdate(ctx, env, args[1:])
	case "status":
		return poolStatus(ctx, env, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, poolUsage)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "agentium pool: unknown subcommand %q\n\n%s", args[0], poolUsage)
		return ExitUsage
	}
}

// poolArgs is what pool update was asked for.
type poolArgs struct {
	dryRun, acceptMined bool
	limit               int
	// set holds the hidden overrides of the project's settings: --verify, --setup, --require-lock, --jobs and
	// --verify-timeout.
	set *settingFlags
	// since makes the pass a re-scan from that date (pool.Pass.Since); maxFiles and maxLines bound a candidate. All three
	// are hidden expert flags.
	since              time.Time
	maxFiles, maxLines int
}

func parsePoolUpdate(env Env, args []string) (a poolArgs, code int, ok bool) {
	fs := flag.NewFlagSet("pool update", flag.ContinueOnError)
	fs.BoolVar(&a.dryRun, "dry-run", false, "list what the pass would mine, validate, re-validate and retire; write nothing")
	fs.BoolVar(&a.acceptMined, "accept-mined", false, "accept this pass's imports without a review, after the automatic checks")
	fs.IntVar(&a.limit, "limit", pool.DefaultPolicy().Limit, "how many tasks to import at most")
	a.set = addSettingFlags(fs, settingVerifyTimeout, settingVerify, settingSetup, settingRequireLock, settingJobs, settingVerifyTimeout)
	since := fs.String("since", "", "re-read the commits from this date on (YYYY-MM-DD, UTC), whatever the last pass read")
	fs.IntVar(&a.maxFiles, "max-files", mine.DefaultMaxFiles, "skip commits that change more test and code files than this")
	fs.IntVar(&a.maxLines, "max-lines", mine.DefaultMaxLines, "skip commits that change more test and code lines than this")
	rest, code, ok := parseArgs(env, fs, args, poolUsage)
	if !ok {
		return a, code, false
	}
	usage := func(format string, args ...any) (poolArgs, int, bool) {
		fmt.Fprintf(env.Stderr, "agentium pool update: "+format+"\n", args...)
		return a, ExitUsage, false
	}
	switch {
	case len(rest) != 0:
		return usage("takes no arguments (got %q)", strings.Join(rest, " "))
	case a.limit < 1:
		return usage("--limit must be at least 1")
	case a.maxFiles < 1 || a.maxLines < 1:
		return usage("--max-files and --max-lines must be at least 1")
	case a.dryRun && a.acceptMined:
		return usage("--dry-run accepts nothing, so --accept-mined does not apply")
	}
	if err := a.set.check(); err != nil {
		return usage("%v", err)
	}
	if *since != "" {
		day, err := time.Parse(time.DateOnly, *since)
		if err != nil {
			return usage("--since %q is not a date like 2026-01-31", *since)
		}
		a.since = day
	}
	return a, ExitOK, true
}

// poolPass is one pool update: the pass's steps (pool.Pass, given internal/mine and internal/task) and what each step
// did, for the report. Nothing here starts an agent or a judge: mining reads git, imports use judgeNever, and
// validation runs only the build and the tests.
type poolPass struct {
	env    Env
	w      *workspace
	a      poolArgs
	policy pool.Policy
	opts   mine.Options
	// verify and setup are the commands mined tasks get, jobs and timeout how validations run: the project's settings
	// with this call's overrides.
	verify, setup []string
	jobs          int
	timeout       time.Duration
	// noMining: no test command was detected, so the pass mines nothing (newPoolPass).
	noMining bool
	// acceptRefused: --accept-mined accepted nothing because the state file was unreadable.
	acceptRefused bool

	ref, head string
	scan      mine.RangeResult
	imp       mine.Imported
	tried     []mine.Candidate // the candidates the import tried, best first
	validated []task.BatchResult
	maint     poolMaintenance
}

// poolMaintenance is what the maintenance step planned and did.
type poolMaintenance struct {
	plan        pool.Plan
	skipped     bool                // some re-validations were skipped: an experiment is running
	revalidated []task.BatchResult  // in plan.Revalidate's order
	notRun      []bool              // per plan.Revalidate: skipped because an experiment was running
	retired     []pool.Retirement   // those stored
	reasons     map[string][]string // task name: why it was stale
}

func newPoolPass(env Env, w *workspace, a poolArgs) *poolPass {
	settings := a.set.apply(w.settings())
	p := &poolPass{env: env, w: w, a: a, policy: pool.DefaultPolicy(), setup: settings.Setup, jobs: jobsOf(settings), timeout: verifyTimeoutOf(settings)}
	p.policy.Limit = a.limit
	var commands []string
	p.opts.Languages, commands = mine.TestLanguages(w.root)
	p.opts.TestCommand = strings.Join(commands, ", ")
	p.opts.MaxFiles, p.opts.MaxLines, p.opts.MaxCommits = a.maxFiles, a.maxLines, mine.DefaultMaxCommits
	p.opts.RequireLock = settings.RequireLock
	// The project's verify setting, else the build tools' own test commands (the tests mining picks commits by: other
	// commands, linters say, fail at old commits for reasons no agent can fix), else the project's detected ones.
	if p.verify = settings.Verify; len(p.verify) == 0 {
		if p.verify = commands; len(p.verify) == 0 {
			p.verify = w.defaultVerify()
		}
	}
	// Without a test command, mined tasks would have nothing to verify with: the pass mines nothing (its watermark stays),
	// and still validates, re-validates and retires.
	p.noMining = len(p.verify) == 0
	return p
}

// noMiningNote says why a pass mines nothing.
const noMiningNote = "no test commands were detected for this project, so the pass mines nothing (mined tasks would have nothing to verify with); " +
	"it still validates, re-validates and retires. agentium init --verify CMD sets the commands mined tasks verify with"

// pass is the pool's pass over this project, with its steps.
func (p *poolPass) pass() pool.Pass[mine.Candidate] {
	w, env := p.w, p.env
	return pool.Pass[mine.Candidate]{
		File: pool.StateFile(w.bare), Limit: p.policy.Limit, Window: p.policy.RetireAge, Margin: p.policy.StaleAfter, Since: p.a.since, Now: env.Now,
		Commit: func(c mine.Candidate) string { return c.Hash },
		Patch:  func(c mine.Candidate) string { return c.Patch },
		Base:   func(c mine.Candidate) time.Time { return c.BaseDate },
		Tasks:  func(ctx context.Context) ([]store.Task, error) { return w.db.Tasks(ctx, w.project.ID) },
		Head: func(ctx context.Context) (string, error) {
			var err error
			p.ref, p.head, err = mine.DefaultBranch(ctx, w.root)
			return p.head, err
		},
		Scan: func(ctx context.Context, r pool.ScanRange) (pool.Scanned[mine.Candidate], error) {
			if p.noMining {
				return pool.Scanned[mine.Candidate]{}, nil // incomplete, without tips: the watermark stays where it is
			}
			tasks, err := w.db.Tasks(ctx, w.project.ID)
			if err != nil {
				return pool.Scanned[mine.Candidate]{}, err
			}
			p.scan, err = mine.ScanRange(ctx, mine.RangeInput{Root: w.root, Bare: w.bare, Range: r, Options: p.opts, Tasks: tasks})
			p.scan.Result.Ref = p.ref
			if err == nil && !p.a.dryRun {
				p.printScan()
			}
			return p.scan.Scanned, err
		},
		Import:   p.importCandidates,
		Validate: p.validateNew,
		Maintain: p.maintain,
	}
}

// importCandidates imports up to limit candidates, best first, through task import --commit's path (no judge).
func (p *poolPass) importCandidates(ctx context.Context, candidates []mine.Candidate, limit int) (pool.Imported, error) {
	w, env := p.w, p.env
	tasks, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return pool.Imported{}, err
	}
	names := map[string]bool{}
	for _, t := range tasks {
		names[t.Name] = true
	}
	_, live := liveEnv(env) // nothing prints while it shows
	p.imp = mine.Import(ctx, mine.ImportInput{Importer: w.importer(names), Candidates: candidates, Limit: limit,
		NewTask: func() store.Task {
			return store.Task{ProjectID: w.project.ID, Verify: p.verify, Setup: append([]string{}, p.setup...), CreatedAt: env.Now()}
		},
		Progress: func(imported int, c mine.Candidate) {
			live.Step(fmt.Sprintf("importing %d of %d: %s", imported+1, limit, experiment.ShortCommit(c.Hash)))
		}})
	live.Stop()
	p.tried = candidates[:p.imp.Tried]
	st := env.style()
	fmt.Fprintf(env.Stdout, "%s (verify: %s)\n", st.Heading(fmt.Sprintf("Imported %d of %d candidate(s) tried", len(p.imp.Tasks), p.imp.Tried)),
		strings.Join(p.verify, "; "))
	for _, f := range p.imp.Failed {
		fmt.Fprintf(env.Stdout, "  not imported: %s (%s): %v\n", cut(f.Candidate.Subject, maxSubject), experiment.ShortCommit(f.Candidate.Hash), f.Err)
	}
	return pool.Imported{Tasks: p.imp.Tasks, Tried: p.imp.Tried, Interrupted: p.imp.Interrupted}, nil
}

// validateNew validates the mined tasks without a validation (this pass's imports, and those a killed pass left) in the
// base context.
// While an experiment is running, they are validated one at a time, so its runs are slowed as little as possible (the
// background pass waits instead; a foreground one was asked for now).
func (p *poolPass) validateNew(ctx context.Context, tasks []store.Task) error {
	jobs, st := p.jobs, p.env.style()
	if p.w.layout.RunsBusy() && jobs > 1 {
		jobs = 1
		fmt.Fprintln(p.env.Stdout, note(st, "an experiment is running: the new tasks are validated one at a time, not "+strconv.Itoa(p.jobs)))
	}
	fmt.Fprintf(p.env.Stdout, "%s, %d at a time\n", st.Heading(fmt.Sprintf("Validating %d mined task(s)", len(tasks))), jobs)
	results, err := validateBatch(ctx, p.env, p.w, tasks, task.ValidateOptions{Arms: []task.Arm{{Name: "base"}}, Repeat: 1, Timeout: p.timeout,
		Grader: defaultGrader(p.env)}, jobs)
	p.validated = results
	return err
}

// plan reads what the maintenance rules need and applies them: nothing is written.
func (p *poolPass) plan(ctx context.Context) (pool.Plan, error) {
	w, env := p.w, p.env
	tasks, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return pool.Plan{}, err
	}
	inUse, err := w.db.TasksInUse(ctx, w.project.ID)
	if err != nil {
		return pool.Plan{}, err
	}
	tools, err := w.hostToolchain(ctx, env)
	if err != nil {
		return pool.Plan{}, err
	}
	var bases, named []string
	for _, t := range tasks {
		if !t.Retired() {
			bases = append(bases, t.BaseCommit)
			named = append(named, t.HiddenTests...)
			named = append(named, t.Reference...)
		}
	}
	baseTimes, err := mine.CommitTimes(ctx, bases, "--git-dir", w.bare)
	if err != nil {
		return pool.Plan{}, err
	}
	atHead, err := mine.TreeHas(ctx, p.head, slices.Compact(slices.Sorted(slices.Values(named))), "-C", w.root)
	if err != nil {
		return pool.Plan{}, err
	}
	// What each active task's solution commit has, and whether the head contains it, read up front: the rules are pure.
	inSolution, contained := map[string]map[string]bool{}, map[string]bool{}
	for _, t := range tasks {
		if t.Retired() || t.SolutionCommit == "" {
			continue
		}
		if contained[t.SolutionCommit], err = mine.Contains(ctx, w.root, p.head, t.SolutionCommit); err != nil {
			return pool.Plan{}, err
		}
		if !contained[t.SolutionCommit] {
			continue
		}
		files := append(slices.Clone(t.HiddenTests), t.Reference...)
		has, err := mine.TreeHas(ctx, t.SolutionCommit, files, "--git-dir", w.bare)
		if ctx.Err() != nil {
			return pool.Plan{}, fmt.Errorf("pool: %w", ctx.Err())
		}
		if err == nil { // unreadable: the file rule does not apply to this task, rather than failing every pass
			inSolution[t.SolutionCommit] = has
		}
	}
	return p.policy.Plan(tasks, pool.Facts{Now: env.Now(), Toolchain: tools, InUse: inUse,
		BaseTime: func(commit string) time.Time { return baseTimes[commit] },
		Head: pool.Head{
			Has:      func(path string) bool { return atHead[path] },
			Contains: func(commit string) bool { return contained[commit] },
			InCommit: func(commit, path string) bool { return inSolution[commit][path] },
		}}), nil
}

// maintain re-validates the stale tasks no locked experiment uses (each with the arms and repeats of its last
// validation, keeping its weak-tests result), none while an experiment is running, and retires the dead ones.
func (p *poolPass) maintain(ctx context.Context) error {
	env, w := p.env, p.w
	plan, err := p.plan(ctx)
	if err != nil {
		return err
	}
	p.maint = poolMaintenance{plan: plan, reasons: map[string][]string{}}
	for _, r := range append(slices.Clone(plan.Revalidate), plan.Kept...) {
		p.maint.reasons[r.Task.Name] = r.Stale.Reasons
	}
	for _, k := range plan.Kept {
		fmt.Fprintf(env.Stdout, "%s: kept for experiment %s, which uses it (%s)\n", k.Task.Name, strings.Join(k.Experiments, ", "),
			strings.Join(k.Stale.Reasons, "; "))
	}
	if len(plan.Revalidate) > 0 {
		if p.maint.revalidated, p.maint.notRun, err = p.revalidate(ctx, plan.Revalidate); err != nil {
			return err
		}
		p.maint.skipped = slices.Contains(p.maint.notRun, true)
	}
	for _, r := range plan.Retire {
		retired, err := w.db.RetireTask(context.WithoutCancel(ctx), r.Task.ID, r.Reason, env.Now())
		if err != nil {
			return err
		}
		if retired {
			p.maint.retired = append(p.maint.retired, r)
			fmt.Fprintf(env.Stdout, "Retired %s: %s\n", r.Task.Name, r.Reason)
		}
	}
	return nil
}

// revalidate re-validates the tasks in groups of the same arms and repeats, keeping plan order in the results, --jobs
// tasks at a time. Before each such chunk it checks for running experiments: while one runs, it starts no more (they
// would slow its runs), and notRun marks the tasks left for the next pass.
func (p *poolPass) revalidate(ctx context.Context, stale []pool.Revalidation) (results []task.BatchResult, notRun []bool, err error) {
	env, st := p.env, p.env.style()
	results, notRun = make([]task.BatchResult, len(stale)), make([]bool, len(stale))
	var order []int // plan indexes, grouped by arms and repeats
	grouped := make([]bool, len(stale))
	for i := range stale {
		for j := i; j < len(stale); j++ {
			if !grouped[j] && slices.Equal(stale[j].Stale.Arms, stale[i].Stale.Arms) && stale[j].Stale.Repeat == stale[i].Stale.Repeat {
				order, grouped[j] = append(order, j), true
			}
		}
	}
	started := 0
	for k := 0; k < len(order); {
		if p.w.layout.RunsBusy() {
			for _, j := range order[k:] {
				notRun[j] = true
			}
			left := len(order) - k
			if started == 0 {
				fmt.Fprintln(env.Stdout, warning(st, fmt.Sprintf("an experiment is running: %d stale task(s) are not re-validated now (they would slow its runs); "+
					"the next pass re-validates them", left)))
			} else {
				fmt.Fprintln(env.Stdout, warning(st, fmt.Sprintf("an experiment started: %d stale task(s) are left for the next pass", left)))
			}
			break
		}
		if started == 0 {
			fmt.Fprintf(env.Stdout, "%s, %d at a time\n", st.Heading(fmt.Sprintf("Re-validating %d stale task(s)", len(stale))), p.jobs)
			for _, r := range stale {
				fmt.Fprintf(env.Stdout, "  %s: %s\n", r.Task.Name, strings.Join(r.Stale.Reasons, "; "))
			}
		}
		// A chunk: up to --jobs tasks of one group, validated again as they were last: the same contexts, repeats and
		// grader mode (one this Agentium no longer grades in gets the default).
		graderOf := func(i int) string {
			if mode := task.ValidationOf(stale[i].Task).Grader; task.KnownGrader(mode) {
				return task.GraderOf(mode)
			}
			return defaultGrader(env)
		}
		first, firstGrader := stale[order[k]].Stale, graderOf(order[k])
		chunk := []int{}
		for k < len(order) && len(chunk) < p.jobs && slices.Equal(stale[order[k]].Stale.Arms, first.Arms) && stale[order[k]].Stale.Repeat == first.Repeat &&
			graderOf(order[k]) == firstGrader {
			chunk, k = append(chunk, order[k]), k+1
		}
		tasks := make([]store.Task, len(chunk))
		for c, j := range chunk {
			tasks[c] = stale[j].Task
		}
		o := task.ValidateOptions{Arms: first.Arms, Repeat: first.Repeat, Timeout: p.timeout, KeepWeakTests: true, Grader: firstGrader}
		got, err := validateBatchWith(ctx, env, p.w, tasks, o, p.jobs, true)
		if err != nil {
			return nil, nil, err
		}
		for c, j := range chunk {
			results[j] = got[c]
		}
		started += len(chunk)
		if ctx.Err() != nil {
			break
		}
	}
	return results, notRun, nil
}

// acceptMined marks this pass's imports reviewed when start --accept-mined's checks find nothing (heldBack): only
// valid, test-graded tasks the pass itself imported (never recovered or hand-made ones). An unreadable state file costs
// only this shortcut. It returns the accepted names and the reasons it held others back.
func (p *poolPass) acceptMined(ctx context.Context, res pool.PassResult) (accepted []string, held map[string]string, err error) {
	held = map[string]string{}
	if res.Unreadable != "" {
		p.acceptRefused = true
		fmt.Fprintln(p.env.Stdout, warning(p.env.style(), "the pool's state file was unreadable ("+res.Unreadable+"), so --accept-mined accepts nothing this time"))
		return nil, held, nil
	}
	w := p.w
	fair := task.NewFairness("--git-dir", w.bare)
	for _, imported := range res.Imported {
		t, err := w.db.TaskByName(ctx, w.project.ID, imported.Name)
		if errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return accepted, held, err
		}
		if !t.CreatedAt.Equal(imported.CreatedAt) || !t.NeedsReview || t.Grading == task.GradingJudge || t.Retired() || t.Validation == nil ||
			task.ValidationOf(t).Status != task.StatusValid {
			continue
		}
		reason, err := heldBack(ctx, fair, t)
		if err != nil {
			return accepted, held, err
		}
		if reason != "" {
			held[t.Name] = reason
			continue
		}
		t.NeedsReview = false
		if err := w.db.UpdateTask(ctx, t, p.env.Now()); err != nil {
			return accepted, held, err
		}
		accepted = append(accepted, t.Name)
	}
	return accepted, held, nil
}

func poolUpdate(ctx context.Context, env Env, args []string) int {
	a, code, ok := parsePoolUpdate(env, args)
	if !ok {
		return code
	}
	w, err := openProjectFor(ctx, env, a.dryRun)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	if partial, err := gitx.PartialClone(ctx, w.root); err != nil {
		return fail(env, err)
	} else if partial {
		return fail(env, mine.ErrPartialClone)
	}
	p := newPoolPass(env, w, a)
	if p.noMining {
		fmt.Fprintln(env.Stdout, note(env.style(), noMiningNote))
	}
	if !a.since.IsZero() {
		fmt.Fprintln(env.Stdout, note(env.style(), p.rescanNote()))
	}
	if a.dryRun {
		return p.dryRun(ctx)
	}
	fmt.Fprintln(env.Stdout, note(env.style(), "the pool pass runs no agent and costs nothing: it mines, validates and retires"))
	res, err := p.pass().Run(ctx)
	if errors.Is(err, pool.ErrPassRunning) {
		return fail(env, fmt.Errorf("%w: it continues on its own", err))
	}
	interrupted := ctx.Err() != nil
	if err != nil && !interrupted {
		return fail(env, err)
	}
	var accepted []string
	held := map[string]string{}
	if a.acceptMined && !interrupted {
		if accepted, held, err = p.acceptMined(ctx, res); err != nil {
			return fail(env, err)
		}
	}
	health, err := poolHealth(context.WithoutCancel(ctx), w)
	if err != nil {
		return fail(env, err)
	}
	doc := p.document(ctx, res, accepted, held, health, interrupted)
	if env.JSON {
		code := ExitOK
		if interrupted {
			code = ExitError
		}
		return env.emitCode(doc, code)
	}
	p.printEnd(res, accepted, held, health)
	if interrupted {
		fmt.Fprintf(env.Stdout, "Interrupted: what finished is kept; %s goes on from there\n", env.style().Command("agentium pool update"))
		return ExitError
	}
	return ExitOK
}

// rescanNote says what a re-scan (--since) reads, and that it leaves the watermark alone.
func (p *poolPass) rescanNote() string {
	from := p.a.since.Format(time.DateOnly)
	if window := p.env.Now().Add(-p.policy.RetireAge); p.a.since.Before(window) {
		from = fmt.Sprintf("%s (the start of the pool's %d days; --since %s is earlier)", window.UTC().Format(time.DateOnly),
			int(p.policy.RetireAge/pool.Day), from)
	}
	return "a re-scan: it reads every commit from " + from + ", whatever earlier passes read; the next pass still starts where the last one ended"
}

// readSince says which commits the scan read: those since the last pass, or a re-scan's since its date.
func (p *poolPass) readSince() string {
	if p.a.since.IsZero() {
		return "since the last pass"
	}
	return "since " + p.a.since.Format(time.DateOnly)
}

// setAside counts the commits the scan set aside, per reason, then those read past (older than the window) and the
// candidates the pass itself drops (kept is how many it keeps): a base too old to import or a change already mined.
func (p *poolPass) setAside(kept int) []setAside {
	rows := rejections(p.scan.Result)
	rows = append(rows, setAside{fmt.Sprintf("outside the pool's %d days", int(p.policy.RetireAge/pool.Day)), p.scan.Old},
		setAside{"base too old for the pool, or a change mined before", len(p.scan.Scanned.Candidates) - kept})
	return rows
}

// printScan says what the scan read: the commits since the last pass, the candidates (before the pass drops those whose
// base is too old or whose change was mined already), watermark commits that are gone, and whether it read them all.
func (p *poolPass) printScan() {
	env, st, scan := p.env, p.env.style(), p.scan
	line := fmt.Sprintf("%s: %d commit(s) read %s, %d candidate(s)",
		st.Heading(fmt.Sprintf("Mined %s at %s", p.ref, experiment.ShortCommit(p.head))), scan.Result.Scanned, p.readSince(), len(scan.Scanned.Candidates))
	if scan.Old > 0 {
		line += fmt.Sprintf(" (%d older commit(s) are outside the pool's %d days)", scan.Old, int(p.policy.RetireAge/pool.Day))
	}
	fmt.Fprintln(env.Stdout, line)
	if n := len(scan.Scanned.Unknown); n > 0 {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("%d commit(s) the last pass ended at are gone from the repository (a force-push or rebase): their history was read again", n)))
	}
	if !scan.Scanned.Complete && p.a.since.IsZero() {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("read the oldest %d new commit(s); the next pass reads on", scan.Result.Scanned)))
	} else if !scan.Scanned.Complete {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("read the oldest %d commit(s) %s, the most one pass reads; a later date reads newer ones", scan.Result.Scanned, p.readSince())))
	}
	if msg := scanNote(p.opts, p.verify); msg != "" {
		fmt.Fprintln(env.Stdout, note(st, msg))
	}
}

// printEnd prints what --accept-mined did, the review advice and the pool's health.
func (p *poolPass) printEnd(res pool.PassResult, accepted []string, held map[string]string, health pool.Health) {
	env, st := p.env, p.env.style()
	if res.Candidates == 0 && len(res.Validated) == 0 {
		fmt.Fprintln(env.Stdout, "Nothing new to import.")
	}
	if len(accepted) > 0 {
		fmt.Fprintf(env.Stdout, "Accepted %d mined instruction(s) without your review (--accept-mined): %s\n"+
			"  Only solution headings, reference-file names and unstated test requirements were checked; a message that explains the fix is not detected.\n",
			len(accepted), strings.Join(accepted, ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(held)) {
		fmt.Fprintf(env.Stdout, "  held back from --accept-mined: %s: %s\n", name, held[name])
	}
	if len(res.Imported) > len(accepted) {
		fmt.Fprintf(env.Stdout, "%s: %s, then %s.\n", st.Warn("Review each mined instruction for solution leaks"), st.Command("agentium task show NAME"),
			st.Command("agentium task edit NAME --reviewed"))
	}
	printHealth(env, health)
}

// dryRun lists what a pass would do now, and writes nothing (pool.Pass.Preview and the maintenance plan).
func (p *poolPass) dryRun(ctx context.Context) int {
	env, st := p.env, p.env.style()
	prev, err := p.pass().Preview(ctx)
	if err != nil {
		return fail(env, err)
	}
	plan, err := p.plan(ctx)
	if err != nil {
		return fail(env, err)
	}
	health, err := poolHealth(ctx, p.w)
	if err != nil {
		return fail(env, err)
	}
	busy := len(plan.Revalidate) > 0 && p.w.layout.RunsBusy()
	top := prev.Candidates[:min(p.policy.Limit, len(prev.Candidates))]
	if env.JSON {
		doc := p.dryRunDocument(ctx, prev, top, plan, health, busy)
		return env.emit(doc)
	}
	fmt.Fprintln(env.Stdout, st.Heading("Dry run: nothing is imported, validated, re-validated, retired or written"))
	fmt.Fprintf(env.Stdout, "Would mine %s at %s: %d commit(s) %s, %d candidate(s)\n", p.ref, experiment.ShortCommit(prev.Head),
		p.scan.Result.Scanned, p.readSince(), len(prev.Candidates))
	if p.scan.Result.Shallow {
		fmt.Fprintln(env.Stdout, note(st, "this is a shallow clone: older history is missing (git fetch --unshallow to mine it)"))
	}
	if msg := scanNote(p.opts, p.verify); msg != "" && !p.noMining {
		fmt.Fprintln(env.Stdout, note(st, msg))
	}
	if len(p.verify) > 0 {
		fmt.Fprintln(env.Stdout, note(st, "mined tasks will verify with: "+strings.Join(p.verify, "; ")+" (agentium init --verify to change)"))
	}
	if len(prev.Unknown) > 0 {
		fmt.Fprintln(env.Stdout, note(st, fmt.Sprintf("%d commit(s) the last pass ended at are gone from the repository: their history would be read again", len(prev.Unknown))))
	}
	if prev.Unreadable != "" {
		fmt.Fprintln(env.Stdout, warning(st, "the pool's state file is unreadable ("+prev.Unreadable+"): a pass sets it aside and starts over"))
	}
	if err := printCandidates(env, top, len(prev.Candidates)); err != nil {
		return fail(env, err)
	}
	if err := printSetAside(env, p.setAside(len(prev.Candidates))); err != nil {
		return fail(env, err)
	}
	if len(prev.Unvalidated) > 0 {
		fmt.Fprintf(env.Stdout, "Would validate %d mined task(s) without a validation: %s\n", len(prev.Unvalidated), strings.Join(taskNames(prev.Unvalidated), ", "))
	}
	for _, r := range plan.Revalidate {
		fmt.Fprintf(env.Stdout, "Would re-validate %s: %s\n", r.Task.Name, strings.Join(r.Stale.Reasons, "; "))
	}
	if busy {
		fmt.Fprintln(env.Stdout, note(st, "an experiment is running: a pass now would skip the re-validations"))
	}
	for _, k := range plan.Kept {
		fmt.Fprintf(env.Stdout, "Would keep %s for experiment %s, which uses it (%s)\n", k.Task.Name, strings.Join(k.Experiments, ", "), strings.Join(k.Stale.Reasons, "; "))
	}
	for _, r := range plan.Retire {
		fmt.Fprintf(env.Stdout, "Would retire %s: %s\n", r.Task.Name, r.Reason)
	}
	printHealth(env, health)
	if len(top) > 0 && !health.LastPass.IsZero() { // before the first pass, printHealth names the command already
		fmt.Fprintf(env.Stdout, "Next: %s imports %d and validates them\n", st.Command("agentium pool update"), len(top))
	}
	return ExitOK
}

func taskNames(tasks []store.Task) []string {
	names := make([]string, len(tasks))
	for i, t := range tasks {
		names[i] = t.Name
	}
	return names
}

func poolStatus(ctx context.Context, env Env, args []string) int {
	rest, code, ok := parseArgs(env, flag.NewFlagSet("pool status", flag.ContinueOnError), args, poolUsage)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		fmt.Fprintf(env.Stderr, "agentium pool status: takes no arguments (got %q)\n", strings.Join(rest, " "))
		return ExitUsage
	}
	w, err := openProjectFor(ctx, env, true)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	health, err := poolHealth(ctx, w)
	if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		return env.emit(poolStatusDoc{header: env.hdr(), Health: healthDocOf(health)})
	}
	printHealth(env, health)
	return ExitOK
}

// poolHealth counts the project's tasks (pool.HealthOf), with the base commits' times from Agentium's repository and
// the last pass from the pool's state file (an unreadable one has none).
func poolHealth(ctx context.Context, w *workspace) (pool.Health, error) {
	tasks, err := w.db.Tasks(ctx, w.project.ID)
	if err != nil {
		return pool.Health{}, err
	}
	bases := make([]string, len(tasks))
	for i, t := range tasks {
		bases[i] = t.BaseCommit
	}
	times, err := mine.CommitTimes(ctx, bases, "--git-dir", w.bare)
	if err != nil {
		return pool.Health{}, err
	}
	st, err := pool.Load(pool.StateFile(w.bare))
	if err != nil {
		return pool.Health{}, err
	}
	return pool.HealthOf(tasks, func(commit string) time.Time { return times[commit] }, st.LastPass), nil
}

// printHealth prints the pool's counts, its last pass and its oldest valid base.
func printHealth(env Env, h pool.Health) {
	st := env.style()
	fmt.Fprintln(env.Stdout, st.Heading(fmt.Sprintf("Task pool: %d task(s)", h.Total)))
	table := term.NewTable(st, term.Left(""), term.Right(""), term.Left(""))
	table.Indent = "  "
	weak := ""
	if h.Weak > 0 {
		weak = fmt.Sprintf("%d with weak tests", h.Weak)
	}
	table.Row("valid", strconv.Itoa(h.Valid), weak)
	table.Row("flaky", strconv.Itoa(h.Flaky), "")
	table.Row("invalid", strconv.Itoa(h.Invalid), "")
	if h.Unchecked > 0 {
		table.Row("unchecked", strconv.Itoa(h.Unchecked), "no hidden tests to check")
	}
	table.Row("not validated", strconv.Itoa(h.Unvalidated), "")
	table.Row("awaiting review", strconv.Itoa(h.AwaitingReview), "")
	table.Row("retired", strconv.Itoa(h.Retired), "")
	_ = table.Write(env.Stdout)
	last, oldest := "never", "none"
	if !h.LastPass.IsZero() {
		last = h.LastPass.UTC().Format("2006-01-02 15:04 UTC")
	}
	if !h.OldestValidBase.IsZero() {
		oldest = h.OldestValidBase.UTC().Format(time.DateOnly)
	}
	fmt.Fprintf(env.Stdout, "  last pass %s; oldest valid base %s\n", last, oldest)
	if h.LastPass.IsZero() {
		fmt.Fprintf(env.Stdout, "%s mines, validates and retires (no agent runs)\n", st.Command("agentium pool update"))
	}
}
