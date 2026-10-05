package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

const cleanUsage = `Usage: agentium clean [--older-than DURATION] [--yes] [--json]

Shows what Agentium keeps for reuse and no longer needs, and how much space removing it frees; --yes removes it:
  - grading seeds and offline dependencies of base commits that no task in the pool and no locked, unfinished
    experiment uses, or that nothing has used for --older-than (they are made again on their next use);
  - the quarantine: what a grade's cleanup could not remove;
  - what runs stopped by a dead Agentium left (workspaces, temp and grading folders), as recovery removes it;
  - the grading folders of sandboxed validations that stopped (never one a validation is grading in, or any
    process uses: nothing is stopped);
  - with your Claude Code login, the session folders runs of this data folder left in Claude Code's projects folder
    (~/.claude/projects, or under CLAUDE_CONFIG_DIR); nothing else there is touched: not your own projects, not a
    folder that holds a session file, not one whose run's workspace is still here or whose run this data folder
    never stored;
  - and it lists, as kept, processes a Codex run's commands may have left that Agentium did not see descend from the
    run's agent (in the run's sandbox, or using its folders): it never stops one of those, --yes or not; stop one that
    is yours to stop yourself (the line says how).
Nothing used in the last hour goes, and nothing a locked, unfinished experiment uses. Your repositories, reports and
snapshots are never touched. Without --yes it writes nothing; with --yes it first runs the recovery every run starts
with, which stores runs a dead Agentium left as cancelled, redacts their records, removes the records folder of one
that stopped before its agent started, and, for a dead Codex run, stops the processes Agentium saw descend from its
agent that are still running (each only while it is exactly the process recorded: its ID and start time).

Flags:
  --older-than DURATION  what a task uses goes too once unused this long: 30d (the default), 12h, 90m; at least 1h
  --yes                  remove what it lists; it takes the run lock, so not while runs are in progress
  --json                 print one JSON document (docs/guide.md, "Scripting and automation")
`

func runClean(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	olderThan := fs.String("older-than", "30d", "what is in use goes too once unused this long")
	yes := fs.Bool("yes", false, "remove what it lists")
	rest, code, ok := parseArgs(env, fs, args, cleanUsage)
	if !ok {
		return code
	}
	if len(rest) > 0 {
		fmt.Fprintf(env.Stderr, "agentium clean: takes no arguments, got %q\n\n%s", rest[0], cleanUsage)
		return ExitUsage
	}
	age, err := parseAge(*olderThan)
	if err != nil {
		fmt.Fprintf(env.Stderr, "agentium clean: --older-than %q: %v\n", *olderThan, err)
		return ExitUsage
	}
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
		return fail(env, err)
	}
	res, err := clean(ctx, env, layout, age, *yes)
	if err != nil {
		if !env.JSON {
			for _, n := range res.notes { // runs recovery stored before it refused
				fmt.Fprintln(env.Stdout, note(env.style(), n))
			}
		}
		return fail(env, err)
	}
	code = ExitOK
	if res.failed() > 0 {
		code = ExitError
	}
	if env.JSON {
		return env.emitCode(cleanDocument(env, layout, res), code)
	}
	printClean(env, layout, res)
	return code
}

// parseAge reads --older-than: a Go duration (12h, 90m) or a number of days (30d), at least run.CleanGrace.
func parseAge(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || math.IsNaN(n) || n <= 0 || n > 100*365 {
			return 0, errors.New("give a duration such as 30d, 12h or 90m")
		}
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, errors.New("give a duration such as 30d, 12h or 90m")
		}
	}
	if d < run.CleanGrace {
		return 0, errors.New("must be at least 1h: what was used in the last hour may belong to a validation in progress")
	}
	return d, nil
}

// cleanResult is what clean found, and with --yes what it did.
type cleanResult struct {
	dryRun bool
	age    time.Duration
	exists bool // the data folder exists
	plan   run.CleanPlan
	// errs holds, with --yes, each removal's error (nil: removed), in plan.Remove's order.
	errs             []error
	freed            int64
	leftoversChecked bool              // false while runs are in progress (a dry run then leaves leftovers out)
	names            map[string]string // project folder names (IDs) to project names
	notes, warnings  []string
}

// removed counts, with --yes, the items of a kind that went and their sizes.
func (r cleanResult) removed(kind string) (n int, bytes int64) {
	for i, it := range r.plan.Remove {
		if it.Kind == kind && i < len(r.errs) && r.errs[i] == nil {
			n++
			bytes += it.Bytes
		}
	}
	return n, bytes
}

// failed counts removals that failed for a reason other than being in use (run.ErrCleanUsed, run.ErrCleanBusy).
func (r cleanResult) failed() int {
	n := 0
	for _, err := range r.errs {
		if err != nil && !errors.Is(err, run.ErrCleanUsed) && !errors.Is(err, run.ErrCleanBusy) {
			n++
		}
	}
	return n
}

// clean plans the cleanup and, with yes, carries it out holding the run lock: the leftovers through recovery (which
// also stores the runs it finds), the rest through run.RemoveClean. Without yes it writes nothing: it takes no lock,
// opens the database read-only and, while runs are in progress, does not look for leftovers (a live run between two
// commands looks like a stopped one).
func clean(ctx context.Context, env Env, layout home.Layout, age time.Duration, yes bool) (cleanResult, error) {
	res := cleanResult{dryRun: !yes, age: age, names: map[string]string{}}
	if info, err := os.Lstat(layout.Root); err != nil || !info.IsDir() {
		return res, nil // no data folder: nothing to clean
	}
	res.exists = true
	busy := false
	if yes {
		release, err := layout.LockRuns()
		if errors.Is(err, home.ErrBusy) {
			return res, fmt.Errorf("%w: run agentium clean when it is done", err)
		}
		if err != nil {
			return res, err
		}
		defer release()
	} else {
		busy = layout.RunsBusy()
	}
	db, err := openForClean(ctx, layout, yes)
	if err != nil {
		return res, err
	}
	if db != nil {
		defer db.Close()
	}
	inUse, err := basesInUse(ctx, db, res.names)
	if err != nil {
		return res, err
	}
	in := run.CleanInput{Layout: layout, InUse: inUse, Now: env.Now(), OlderThan: age, ClaudeConfig: loginConfig(env),
		KnownWorkspace: knownWorkspace(ctx, db)}
	if busy {
		res.notes = append(res.notes, "runs are in progress: what stopped runs left is not listed")
	} else {
		res.leftoversChecked = true
		in.Stored = func(id string) (bool, error) {
			if db == nil {
				return false, nil
			}
			return db.HasRun(ctx, id)
		}
	}
	if res.plan, err = run.PlanClean(ctx, in); err != nil {
		return res, err
	}
	res.notes = append(res.notes, res.plan.Notes...)
	if !yes {
		return res, nil
	}
	res.errs = make([]error, len(res.plan.Remove))
	var others []int
	leftovers := slices.ContainsFunc(res.plan.Keep, func(it run.CleanItem) bool { return it.Kind == run.CleanLeftovers })
	for i, it := range res.plan.Remove {
		if it.Kind == run.CleanLeftovers {
			leftovers = true
		} else {
			others = append(others, i)
		}
	}
	if leftovers {
		// As at every run's start (startRuns): a run that may still be running, or a recovery that fails, stops the
		// whole cleanup before anything else goes.
		if err := recoverForClean(ctx, env, layout, db, &res); err != nil {
			return res, err
		}
		for i, it := range res.plan.Remove {
			if it.Kind != run.CleanLeftovers {
				continue
			}
			gone := it.Gone()
			res.freed += gone
			if gone < it.Bytes {
				res.errs[i] = errors.New("recovery left part of it (see the warnings)")
			}
		}
	}
	items := make([]run.CleanItem, len(others))
	for j, i := range others {
		items[j] = res.plan.Remove[i]
	}
	for j, err := range run.RemoveClean(ctx, layout, items) {
		res.errs[others[j]] = err
		if err == nil {
			res.freed += items[j].Bytes
		}
	}
	return res, nil
}

// loginConfig is the user's own Claude Code config folder, where runs keep their sessions, when runs sign in with the
// login (signInMode), as runs find it (runEnvWith: CLAUDE_CONFIG_DIR, else ~/.claude); "" with another sign-in, or
// when it is not an absolute path.
func loginConfig(env Env) string {
	if mode, _ := signInMode(env); mode != claude.SignInLogin {
		return ""
	}
	var environ []string
	if env.Environ != nil {
		environ = env.Environ()
	}
	if config := claude.UserConfigDir(environ, env.Getenv("HOME")); filepath.IsAbs(config) {
		return config
	}
	return ""
}

// experimentSlotTry is an experiment slot's workspace name (experiment.experimentWorkspace): e<id>-s<slot>-t<try>.
var experimentSlotTry = regexp.MustCompile(`^e([0-9]+)-s([0-9]+)-t([0-9]+)$`)

// knownWorkspace reports whether the store knows the run that used a workspace: a run's ID is a stored run; an
// experiment slot's try t is known when the experiment has at least t stored runs in that slot (its tries count its
// stored runs, cancelled ones included, so the t-th was stored). Experiment IDs are small numbers every data folder
// has: the experiment alone proves nothing. Without a database it knows none.
func knownWorkspace(ctx context.Context, db *store.Store) func(string) (bool, error) {
	slots := map[int64]map[int]int{} // an experiment's stored runs per slot, read once
	return func(name string) (bool, error) {
		if db == nil {
			return false, nil
		}
		m := experimentSlotTry.FindStringSubmatch(name)
		if m == nil {
			return db.HasRun(ctx, name)
		}
		id, err1 := strconv.ParseInt(m[1], 10, 64)
		slot, err2 := strconv.Atoi(m[2])
		try, err3 := strconv.Atoi(m[3])
		if err := errors.Join(err1, err2, err3); err != nil {
			return false, err
		}
		counts, ok := slots[id]
		if !ok {
			runs, err := db.ExperimentRuns(ctx, id)
			if err != nil {
				return false, err
			}
			counts = map[int]int{}
			for _, r := range runs {
				counts[r.Slot]++
			}
			slots[id] = counts
		}
		return try >= 1 && counts[slot] >= try, nil
	}
}

// openForClean opens the database: read-only for a dry run, which writes nothing, and normally with --yes, which may
// store recovered runs. A data folder without one has nothing in use; db is then nil.
func openForClean(ctx context.Context, layout home.Layout, yes bool) (*store.Store, error) {
	if _, err := os.Stat(layout.Database); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if yes {
		return store.Open(ctx, layout.Database)
	}
	return store.OpenReadOnly(ctx, layout.Database)
}

// basesInUse maps each project's folder name (its ID) to the bases its active tasks and its locked, unfinished
// experiments use (any status but done: such an experiment runs its tasks as its lock fixed them, retired or not), and
// fills names with the projects' names.
func basesInUse(ctx context.Context, db *store.Store, names map[string]string) (map[string]map[string]run.BaseUse, error) {
	inUse := map[string]map[string]run.BaseUse{}
	if db == nil {
		return inUse, nil
	}
	projects, err := db.Projects(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		key := strconv.FormatInt(p.ID, 10)
		names[key] = p.Name
		bases := map[string]run.BaseUse{}
		tasks, err := db.Tasks(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			if !t.Retired() && t.BaseCommit != "" {
				u := bases[t.BaseCommit]
				u.Tasks = append(u.Tasks, t.Name)
				bases[t.BaseCommit] = u
			}
		}
		experiments, err := db.Experiments(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range experiments {
			if e.Lock == nil || e.Status == store.StatusDone {
				continue
			}
			var lock experiment.Lock
			if json.Unmarshal(e.Lock, &lock) != nil {
				continue // an unreadable lock cannot be resumed: it uses nothing
			}
			for _, t := range lock.Tasks {
				if u := bases[t.Base]; t.Base != "" && !slices.Contains(u.Experiments, e.Name) {
					u.Experiments = append(u.Experiments, e.Name)
					bases[t.Base] = u
				}
			}
		}
		inUse[key] = bases
	}
	return inUse, nil
}

// recoverForClean runs the recovery every run starts with (startRuns), with the run lock held by clean: it removes
// what dead runs left and stores those whose agent started, as cancelled with what they spent. A run whose agent may
// still be running is an error, as in startRuns (*run.AliveError): the caller then removes nothing else.
func recoverForClean(ctx context.Context, env Env, layout home.Layout, db *store.Store, res *cleanResult) error {
	if db == nil {
		return errors.New("the data folder has no database to store recovered runs in")
	}
	secrets, err := recoverySecrets(env)
	if err != nil {
		return err
	}
	warn := func(msg string) { res.warnings = append(res.warnings, msg) }
	orphans, recErr := run.RecoverWarn(ctx, layout, func(id string) (bool, error) { return db.HasRun(ctx, id) }, secrets, env.Now(), warn)
	w := &workspace{db: db, layout: layout}
	for _, o := range orphans {
		if o.Unreadable != "" {
			warn(fmt.Sprintf("run %s left behind by a stopped Agentium has an unreadable start file, so it is not stored; its transcript shows $%.2f spent; the file was moved to %s",
				o.Record.ID, o.Record.Spend().AgentUSD, o.Unreadable))
			continue
		}
		var meta runMeta
		if len(o.Meta) > 0 {
			if err := json.Unmarshal(o.Meta, &meta); err != nil {
				return errors.Join(recErr, fmt.Errorf("run %s: %w", o.Record.ID, err))
			}
		}
		if meta.ProjectID == 0 {
			warn(fmt.Sprintf("run %s was recovered, but its project is unknown: the next run of its project stores it", o.Record.ID))
			continue
		}
		if meta.Kind == "" {
			meta.Kind = "task"
		}
		if err := saveRun(ctx, w, o.Record, meta); err != nil {
			return errors.Join(recErr, err)
		}
		res.notes = append(res.notes, fmt.Sprintf("recovered run %s (task %s, arm %s), left behind by a stopped Agentium: cancelled, $%.2f",
			o.Record.ID, o.Record.Task, o.Record.Arm, o.Record.Spend().AgentUSD))
	}
	return recErr
}

// sessionsWhere is where a sessions item is, as cleanup reports it: never the folder's name, which holds the user's
// home path.
const sessionsWhere = "in Claude Code's projects folder"

// cleanRel is path as cleanup reports it: relative to the data folder (a leftover is its run's records folder).
func cleanRel(layout home.Layout, path string) string {
	if rel, err := filepath.Rel(layout.Root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(path)
}

// itemPath is an item's path as cleanup reports it (cleanRel), and for a sessions item where it is, in words.
func itemPath(layout home.Layout, it run.CleanItem) string {
	if it.Kind == run.CleanSessions {
		return sessionsWhere
	}
	return cleanRel(layout, it.Path)
}

// itemRow is one row of the lists of what goes and what stays.
type itemRow struct {
	kind, path string
	bytes      int64
	why        string
	problem    bool // why is why it was not removed
}

// itemRows are the rows of items, one each, but sessions items: those are counted in one row per reason (or per
// problem, under --yes), so that no folder's name is printed (sessionsWhere). problem is item i's error under --yes ("":
// removed, or a dry run).
func itemRows(layout home.Layout, items []run.CleanItem, problem func(i int) string) []itemRow {
	var rows []itemRow
	type group struct{ row, n int }
	groups := map[[2]string]*group{}
	var order [][2]string
	for i, it := range items {
		p := problem(i)
		if it.Kind != run.CleanSessions {
			row := itemRow{kind: kindLabel(it.Kind), path: itemPath(layout, it), bytes: it.Bytes, why: it.Detail}
			if p != "" {
				row.why, row.problem = "not removed: "+p, true
			}
			rows = append(rows, row)
			continue
		}
		key := [2]string{it.Reason, p}
		g, ok := groups[key]
		if !ok {
			g = &group{row: len(rows)}
			groups[key] = g
			order = append(order, key)
			rows = append(rows, itemRow{kind: kindLabel(it.Kind), path: sessionsWhere, problem: p != ""})
		}
		g.n++
		rows[g.row].bytes += it.Bytes
	}
	for _, key := range order {
		g := groups[key]
		rows[g.row].why = sessionsWhy(key[0], key[1], g.n)
	}
	return rows
}

// sessionsWhy says what a row of n sessions items, of one reason or one problem, holds.
func sessionsWhy(reason, problem string, n int) string {
	folders, hold := fmt.Sprintf("%d folders", n), "hold a session file of their own"
	if n == 1 {
		folders, hold = "1 folder", "holds a session file of its own"
	}
	switch {
	case problem != "":
		return folders + " not removed: " + problem
	case reason == run.CleanRunSession:
		return folders + " that runs of this data folder left"
	case reason == run.CleanKeptWorkspace:
		return folders + " whose run's workspace is still here (a run in progress, one kept with --keep, or a stopped run's leftovers)"
	case reason == run.CleanKeptSessionFile:
		return folders + " named like this data folder's runs' that " + hold + ", which no run leaves: left alone"
	case reason == run.CleanKeptRecent:
		return folders + " used in the last hour"
	case reason == run.CleanKeptUnknownRun:
		return folders + " named like this data folder's runs' that it has no run of (perhaps another data folder's): left alone"
	}
	return folders
}

// kindLabel names a kind in a row of items.
func kindLabel(kind string) string {
	switch kind {
	case run.CleanSeeds:
		return "seed"
	case run.CleanLeftovers:
		return "leftovers"
	}
	return kind
}

// formatBytes is a size for people, in decimal units (MB = 10^6 bytes).
func formatBytes(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 1000*1000:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	case n < 1000*1000*1000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.2f GB", float64(n)/1e9)
}

// kindTotals counts a plan's items and sizes per kind.
type kindTotals struct {
	remove, keep           int
	removeBytes, keepBytes int64
}

func totals(plan run.CleanPlan) map[string]kindTotals {
	t := map[string]kindTotals{}
	for _, it := range plan.Remove {
		k := t[it.Kind]
		k.remove++
		k.removeBytes += it.Bytes
		t[it.Kind] = k
	}
	for _, it := range plan.Keep {
		k := t[it.Kind]
		k.keep++
		k.keepBytes += it.Bytes
		t[it.Kind] = k
	}
	return t
}

func printClean(env Env, layout home.Layout, res cleanResult) {
	out, st := env.Stdout, env.style()
	if !res.exists {
		fmt.Fprintf(out, "Nothing to clean: there is no data folder at %s.\n", layout.Root)
		return
	}
	t := totals(res.plan)
	var removeBytes int64
	for _, it := range res.plan.Remove {
		removeBytes += it.Bytes
	}
	if res.dryRun {
		fmt.Fprintln(out, st.Heading(fmt.Sprintf("Cleaning %s would free %s (a dry run: nothing was removed)", layout.Root, formatBytes(removeBytes))))
	} else {
		fmt.Fprintln(out, st.Heading(fmt.Sprintf("Cleaned %s: %s freed", layout.Root, formatBytes(res.freed))))
	}
	verb := map[bool]string{true: "remove", false: "removed"}[res.dryRun]
	kept := map[bool]string{true: "keep", false: "kept"}[res.dryRun]
	table := term.NewTable(st, term.Left("kind"), term.Right(verb), term.Right("size"), term.Right(kept), term.Right("size"))
	table.Indent = "  "
	var all kindTotals
	for _, kind := range run.CleanKinds {
		k := t[kind]
		removed, removedBytes := k.remove, k.removeBytes
		if !res.dryRun {
			removed, removedBytes = res.removed(kind)
		}
		table.Row(kind, strconv.Itoa(removed), formatBytes(removedBytes), strconv.Itoa(k.keep), formatBytes(k.keepBytes))
		all.remove, all.removeBytes, all.keep, all.keepBytes = all.remove+removed, all.removeBytes+removedBytes, all.keep+k.keep, all.keepBytes+k.keepBytes
	}
	table.Row(st.Heading("total"), strconv.Itoa(all.remove), formatBytes(all.removeBytes), strconv.Itoa(all.keep), formatBytes(all.keepBytes))
	_ = table.Write(out)
	if len(res.plan.Remove) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, st.Heading(map[bool]string{true: "What would go:", false: "What went:"}[res.dryRun]))
		items := term.NewTable(st, term.Left(""), term.Left(""), term.Right(""), term.Left(""))
		items.Indent = "  "
		problem := func(i int) string {
			if !res.dryRun && res.errs[i] != nil {
				return res.errs[i].Error()
			}
			return ""
		}
		for _, r := range itemRows(layout, res.plan.Remove, problem) {
			why := r.why
			if r.problem {
				why = st.Warn(why)
			}
			items.Row(r.kind, r.path, formatBytes(r.bytes), why)
		}
		_ = items.Write(out)
	}
	if len(res.plan.Keep) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, st.Heading("Kept:"))
		items := term.NewTable(st, term.Left(""), term.Left(""), term.Right(""), term.Left(""))
		items.Indent = "  "
		for _, r := range itemRows(layout, res.plan.Keep, func(int) string { return "" }) {
			items.Row(r.kind, r.path, formatBytes(r.bytes), r.why)
		}
		_ = items.Write(out)
	}
	for _, n := range res.notes {
		fmt.Fprintln(out, note(st, n))
	}
	for _, w := range res.warnings {
		fmt.Fprintln(out, warning(st, w))
	}
	switch {
	case res.dryRun && len(res.plan.Remove) > 0:
		fmt.Fprintf(out, "\nRun %s to remove it. What a task uses goes too once unused for %s (--older-than); what a locked, unfinished experiment uses stays.\n", st.Command("agentium clean --yes"), ago(res.age))
	case res.dryRun:
		fmt.Fprintln(out, "\nNothing to remove.")
	}
}

// ago is a duration as --older-than reads it: days when whole, else Go's form.
func ago(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}

// cleanDoc is clean's --json document.
type cleanDoc struct {
	header
	DryRun           bool           `json:"dry_run"`
	OlderThanSeconds int64          `json:"older_than_seconds"`
	LeftoversChecked bool           `json:"leftovers_checked"`
	Kinds            []cleanKindDoc `json:"kinds"`
	RemoveBytes      int64          `json:"remove_bytes"`
	FreedBytes       *int64         `json:"freed_bytes"` // null in a dry run
	Remove           []cleanItemDoc `json:"remove"`
	Kept             []cleanItemDoc `json:"kept"`
	Notes            []string       `json:"notes"`
	Warnings         []string       `json:"warnings"`
}

type cleanKindDoc struct {
	Kind        string `json:"kind"`
	Remove      int    `json:"remove"`
	RemoveBytes int64  `json:"remove_bytes"`
	Keep        int    `json:"keep"`
	KeepBytes   int64  `json:"keep_bytes"`
}

type cleanItemDoc struct {
	Kind     string     `json:"kind"`
	Path     string     `json:"path"`    // relative to the data folder; for sessions, where they are in words (sessionsWhere)
	Project  *string    `json:"project"` // the project's name; null for the quarantine and leftovers
	Base     *string    `json:"base"`
	Bytes    int64      `json:"bytes"`
	LastUsed *time.Time `json:"last_used"`
	Why      string     `json:"why"` // a value to branch on: run.CleanUnused, run.CleanKeptTask, ...
	Note     string     `json:"note"`
	Removed  *bool      `json:"removed"` // null in a dry run and for kept items
	Problem  *string    `json:"problem"` // why it was not removed
}

func cleanDocument(env Env, layout home.Layout, res cleanResult) cleanDoc {
	doc := cleanDoc{header: hdr("clean"), DryRun: res.dryRun, OlderThanSeconds: int64(res.age / time.Second), LeftoversChecked: res.leftoversChecked,
		Kinds: []cleanKindDoc{}, Remove: []cleanItemDoc{}, Kept: []cleanItemDoc{}, Notes: env.redactAll(list(res.notes)), Warnings: env.redactAll(list(res.warnings))}
	t := totals(res.plan)
	for _, kind := range run.CleanKinds {
		k := t[kind]
		if !res.dryRun { // what went, as the console says; the items' "removed" tell the rest
			k.remove, k.removeBytes = res.removed(kind)
		}
		doc.Kinds = append(doc.Kinds, cleanKindDoc{Kind: kind, Remove: k.remove, RemoveBytes: k.removeBytes, Keep: k.keep, KeepBytes: k.keepBytes})
		doc.RemoveBytes += k.removeBytes
	}
	item := func(it run.CleanItem) cleanItemDoc {
		d := cleanItemDoc{Kind: it.Kind, Path: itemPath(layout, it), Bytes: it.Bytes, Why: it.Reason, Note: env.redact(it.Detail)}
		if name, ok := res.names[it.Project]; ok {
			d.Project = &name
		}
		if it.Base != "" {
			base := it.Base
			d.Base = &base
		}
		if !it.LastUsed.IsZero() {
			last := it.LastUsed.UTC().Truncate(time.Second)
			d.LastUsed = &last
		}
		return d
	}
	for i, it := range res.plan.Remove {
		d := item(it)
		if !res.dryRun {
			removed := res.errs[i] == nil
			d.Removed = &removed
			if !removed {
				problem := env.redact(res.errs[i].Error())
				d.Problem = &problem
			}
		}
		doc.Remove = append(doc.Remove, d)
	}
	for _, it := range res.plan.Keep {
		doc.Kept = append(doc.Kept, item(it))
	}
	if !res.dryRun {
		freed := res.freed
		doc.FreedBytes = &freed
	}
	return doc
}
