package run

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/home"
)

// Cleanup (agentium clean) frees what the data folder keeps for reuse and no longer needs. It looks in four places:
//   - seeds: the grading seeds, <cache>/grading-seed/<project>/<key>-<base> (gradingSeed), and what a dead maker left
//     (<seed>.tmp);
//   - dependencies: a project's deps folder (<deps>/<project>, depsFolder), whole, or else its Python venvs and
//     metadata folders (<deps>/<project>/py/<key>, py-meta/<key>) one by one;
//   - the quarantine (<cache>/quarantine): what a grade's cleanup could not remove;
//   - leftovers: the workspaces, temp roots and grading copies of runs whose Agentium process died, which recovery
//     (RecoverWarn) removes; PlanClean only lists them (planLeftovers), and the caller runs the recovery.
//
// A seed, and a base's dependencies, stay while a locked, unfinished experiment uses the base, and while a task in the
// pool does unless they have not been used for CleanInput.OlderThan (CleanInput.InUse, judge); they are made again on
// their next use. Last use is the seed folder's, the base's warm-up stamp's or the deps folder's modification time:
// markUsed sets it whenever a run or a validation uses them (each validation command, too), so age means "unused
// for", not "created". Nothing used within CleanGrace goes, in use or not: validation does not take the run lock, so
// one may be using it right now.
//
// Only folders inside the data folder's cache and deps folders are ever removed, reached through real folders only
// (cleanable); each is first moved into the quarantine, whole (an atomic rename: a crash never leaves half a seed or
// half a venv where the next run would take it for a whole one), then removed there by removeTree, which never follows
// a link. What resists removal stays in the quarantine, which the next recovery or cleanup empties.

// The kinds of what cleanup removes, in the order it reports them.
const (
	CleanSeeds      = "seeds"
	CleanDeps       = "dependencies"
	CleanQuarantine = "quarantine"
	CleanLeftovers  = "leftovers"
)

// CleanKinds lists the kinds in report order.
var CleanKinds = []string{CleanSeeds, CleanDeps, CleanQuarantine, CleanLeftovers}

// Why an item goes (CleanItem.Reason of a removal).
const (
	CleanUnused      = "unused"      // no task in the pool and no locked, unfinished experiment uses its base
	CleanOld         = "old"         // in use, but not used for longer than CleanInput.OlderThan
	CleanQuarantined = "quarantined" // in the quarantine
	CleanStoppedRun  = "stopped_run" // left by a run whose Agentium process ended
)

// Why an item stays (CleanItem.Reason of a kept one).
const (
	CleanKeptTask       = "in_use_by_task"
	CleanKeptExperiment = "in_use_by_experiment"
	CleanKeptRecent     = "recently_used" // used within CleanGrace
	CleanKeptRunning    = "running"       // a run whose process group still exists
	CleanKeptUnreadable = "unreadable"    // a run whose start file cannot be read: recovery decides when it is safe
)

// CleanGrace is how recently used an item must be to stay whatever else holds: validations run without the run lock.
const CleanGrace = time.Hour

// CleanDefaultAge is CleanInput.OlderThan's default.
const CleanDefaultAge = 30 * 24 * time.Hour

// BaseUse names what uses a base: active tasks of the pool and locked, unfinished experiments.
type BaseUse struct {
	Tasks, Experiments []string
}

// CleanInput is what PlanClean decides with.
type CleanInput struct {
	Layout home.Layout
	// InUse maps a project's folder name in the cache and deps folders (its ID, home.Layout.ProjectRepo) to the bases
	// (full commit IDs) its active tasks and locked, unfinished experiments use.
	InUse map[string]map[string]BaseUse
	// Stored reports whether a run is stored (the database's runs); nil leaves leftovers out (runs are in progress).
	Stored func(id string) (bool, error)
	Now    time.Time
	// OlderThan is how long an item a task uses may go unused before it goes all the same (never one a locked,
	// unfinished experiment uses); below CleanGrace counts as it.
	OlderThan time.Duration
}

// CleanItem is one thing cleanup removes, or keeps and says why.
type CleanItem struct {
	Kind string
	// Path is what goes: a seed, a deps folder, a venv's folder, a quarantined folder; for leftovers, the run's records
	// folder, which names the run (parts lists what recovery removes).
	Path     string
	Project  string // the project's folder name (its ID); "" for the quarantine
	Base     string // the base commit; "" when not one base's
	Bytes    int64
	LastUsed time.Time // zero when unknown
	Reason   string
	Detail   string // for people: what uses it, or how long it has gone unused

	parts []cleanPart // leftovers: the folders recovery removes, and their sizes
	// lock is the file locked while the item is removed: its seed's, or its project's warm-up lock.
	lock string
	// stamps is, for a project's whole deps folder, its warm-up state folder: its stamps go first, so that a crash
	// leaves dependencies unstamped (warmed again), never stamps of dependencies that are gone.
	stamps string
	// recheck reads the item's last use again, under its lock: an item used since the plan stays.
	recheck func() time.Time
}

type cleanPart struct {
	path  string
	bytes int64
}

// Gone is how much of a leftover item is gone now: the sizes of its parts that no longer exist (after recovery).
func (it CleanItem) Gone() int64 {
	var n int64
	for _, p := range it.parts {
		if _, err := os.Lstat(p.path); errors.Is(err, fs.ErrNotExist) {
			n += p.bytes
		}
	}
	return n
}

// CleanPlan is what cleanup would remove, and what it keeps.
type CleanPlan struct {
	Remove []CleanItem
	Keep   []CleanItem
}

// Names in the cache that cleanup reads.
const (
	warmStateFolder = "warm-state"
	warmLockName    = "lock"
)

// commitSuffix matches a name ending in -<full commit ID>: a seed (<key>-<base>) or a warm-up stamp (<tools>-<version>-<base>).
var commitSuffix = regexp.MustCompile(`-([0-9a-f]{40}|[0-9a-f]{64})$`)

// PlanClean decides what cleanup removes and keeps; it writes nothing. A folder that does not exist (no cache yet) has
// nothing to clean.
func PlanClean(ctx context.Context, in CleanInput) (CleanPlan, error) {
	in.OlderThan = max(in.OlderThan, CleanGrace)
	c := planner{in: in}
	for _, step := range []func(context.Context) error{c.seeds, c.deps, c.quarantine} {
		if err := step(ctx); err != nil {
			return CleanPlan{}, err
		}
	}
	if in.Stored != nil {
		if err := c.leftovers(ctx); err != nil {
			return CleanPlan{}, err
		}
	}
	order := func(a, b CleanItem) int {
		return cmp.Or(cmp.Compare(slices.Index(CleanKinds, a.Kind), slices.Index(CleanKinds, b.Kind)), strings.Compare(a.Path, b.Path))
	}
	slices.SortFunc(c.plan.Remove, order)
	slices.SortFunc(c.plan.Keep, order)
	return c.plan, nil
}

type planner struct {
	in   CleanInput
	plan CleanPlan
}

// verdict is what becomes of a seed or a base's dependencies last used at last.
type verdict struct {
	gone           bool
	reason, detail string
}

// judge decides for a project's base: kept while a task uses it and it was used within OlderThan, whatever its age
// while a locked, unfinished experiment uses it (a paused experiment's next slots must find the dependencies its
// earlier ones had: a venv resolved again could get newer versions), or when used within CleanGrace; otherwise it
// goes, as old (a task's) or unused.
func (c *planner) judge(project, base string, last time.Time) verdict {
	use, inUse := c.in.InUse[project][base]
	age := c.in.Now.Sub(last)
	switch {
	case inUse && len(use.Tasks) > 0 && age < c.in.OlderThan:
		return verdict{reason: CleanKeptTask, detail: "in use by " + names("task", use.Tasks)}
	case inUse && len(use.Experiments) > 0:
		return verdict{reason: CleanKeptExperiment, detail: "in use by " + names("experiment", use.Experiments)}
	case age < CleanGrace:
		return verdict{reason: CleanKeptRecent, detail: "used " + ago(age) + " ago"}
	case inUse:
		return verdict{gone: true, reason: CleanOld, detail: "in use, but not used for " + ago(age)}
	}
	return verdict{gone: true, reason: CleanUnused, detail: "no task or experiment uses its base"}
}

// names is "task a", "tasks a, b" or "tasks a, b and 3 more".
func names(kind string, list []string) string {
	if len(list) == 1 {
		return kind + " " + list[0]
	}
	shown := list[:min(len(list), 2)]
	text := kind + "s " + strings.Join(shown, ", ")
	if more := len(list) - len(shown); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}

// ago is a duration for people: minutes, hours or days.
func ago(d time.Duration) string {
	switch {
	case d < 0:
		return "0 min"
	case d < 2*time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// add files the item under the plan's removals or the kept list.
func (c *planner) add(it CleanItem, v verdict) {
	it.Reason, it.Detail = v.reason, v.detail
	if v.gone {
		c.plan.Remove = append(c.plan.Remove, it)
	} else {
		c.plan.Keep = append(c.plan.Keep, it)
	}
}

// realDirs lists the entries of dir that are real folders (never links); a missing dir has none.
func realDirs(dir string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("clean: %w", err)
	}
	return slices.DeleteFunc(entries, func(e fs.DirEntry) bool { return !e.IsDir() }), nil
}

// modTime is p's modification time (of p itself, never a link's target); zero when it cannot be read.
func modTime(p string) time.Time {
	info, err := os.Lstat(p)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// seeds plans the grading seeds: each by its project and base; a dead maker's <seed>.tmp is unused.
func (c *planner) seeds(ctx context.Context) error {
	if c.in.Layout.Cache == "" {
		return nil
	}
	root := filepath.Join(c.in.Layout.Cache, seedsFolder)
	if !realFolder(root) {
		return nil
	}
	projects, err := realDirs(root)
	if err != nil {
		return err
	}
	for _, p := range projects {
		dir := filepath.Join(root, p.Name())
		entries, err := realDirs(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := filepath.Join(dir, e.Name())
			seed, tmp := strings.CutSuffix(e.Name(), ".tmp")
			m := commitSuffix.FindStringSubmatch(seed)
			if m == nil {
				continue // not a seed: left alone
			}
			it := CleanItem{Kind: CleanSeeds, Path: path, Project: p.Name(), Base: m[1], Bytes: treeSize(path), LastUsed: modTime(path),
				lock: filepath.Join(dir, seed) + ".lock"}
			it.recheck = func() time.Time { return modTime(path) }
			v := c.judge(p.Name(), m[1], it.LastUsed)
			if tmp {
				v = verdict{gone: true, reason: CleanUnused, detail: "left by a seed's maker that stopped"}
				if age := c.in.Now.Sub(it.LastUsed); age < CleanGrace {
					v = verdict{reason: CleanKeptRecent, detail: "being made (" + ago(age) + " ago)"}
				}
			}
			c.add(it, v)
		}
	}
	return nil
}

// stamp is a base's warm-up stamp (Env.stampPath) and what it names in the deps folder.
type stamp struct {
	base string
	used time.Time
	// venv and meta are the keys of the venv's and the metadata's folders (py/<key>, py-meta/<key>) it names.
	venv, meta string
}

// readStamps reads a project's warm-up stamps in its state folder (Env.warmState).
func readStamps(state string) ([]stamp, error) {
	entries, err := os.ReadDir(state)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("clean: %w", err)
	}
	var stamps []stamp
	for _, e := range entries {
		m := commitSuffix.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue // the lock, a .steps marker, metadata tries, a write in progress
		}
		path := filepath.Join(state, e.Name())
		s := stamp{base: m[1], used: modTime(path)}
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			var w buildtool.Warmed
			if json.Unmarshal(data, &w) == nil {
				// By shape, not by prefix: the data folder may have moved since the stamp was written.
				if w.Venv != "" && filepath.Base(filepath.Dir(filepath.Dir(w.Venv))) == "py" {
					s.venv = filepath.Base(filepath.Dir(w.Venv))
				}
				if w.Metadata != "" && filepath.Base(filepath.Dir(w.Metadata)) == "py-meta" {
					s.meta = filepath.Base(w.Metadata)
				}
			}
		}
		stamps = append(stamps, s)
	}
	return stamps, nil
}

// projectLast is a deps folder's last use: the newest of its stamps' and of the folder's own modification time, which
// every run's setup and validation of the project sets (markUsed), warmed or not: a failed or waited-out warm-up
// writes no stamp, yet what is there is used.
func projectLast(dir string, stamps []stamp) time.Time {
	last := modTime(dir)
	for _, s := range stamps {
		if s.used.After(last) {
			last = s.used
		}
	}
	return last
}

// unitLast is the last use of a venv's or metadata's folder: the newest of the stamps that name it, or with none, the
// folder's own modification time.
func unitLast(path, sub, key string, stamps []stamp) (last time.Time, named bool) {
	for _, s := range stamps {
		if (sub == "py" && s.venv == key) || (sub == "py-meta" && s.meta == key) {
			named = true
			if s.used.After(last) {
				last = s.used
			}
		}
	}
	if !named {
		return modTime(path), false
	}
	return last, true
}

// deps plans each project's deps folder. A tool's caches (Maven's repository, Gradle's home, Cargo's registry) serve
// every base of the project at once, so the folder goes whole when none of its warmed bases stays, with the project's
// stamps; otherwise only the venvs and metadata folders no kept base names go.
func (c *planner) deps(ctx context.Context) error {
	if c.in.Layout.Deps == "" || c.in.Layout.Cache == "" || !realFolder(c.in.Layout.Deps) {
		return nil
	}
	projects, err := realDirs(c.in.Layout.Deps)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		project, dir := p.Name(), filepath.Join(c.in.Layout.Deps, p.Name())
		state := filepath.Join(c.in.Layout.Cache, warmStateFolder, project)
		stamps, err := readStamps(state)
		if err != nil {
			return err
		}
		recheck := func() time.Time {
			now, _ := readStamps(state)
			return projectLast(dir, now)
		}
		whole := CleanItem{Kind: CleanDeps, Path: dir, Project: project, LastUsed: projectLast(dir, stamps), lock: filepath.Join(state, warmLockName),
			stamps: state, recheck: recheck}
		kept, best := 0, verdict{}
		var gone []verdict
		judged := func(v verdict) {
			if v.gone {
				gone = append(gone, v)
				return
			}
			kept++
			if best.reason == "" || rank(v.reason) < rank(best.reason) {
				best = v
			}
		}
		stamped := map[string]bool{}
		for _, s := range stamps {
			stamped[s.base] = true
			judged(c.judge(project, s.base, s.used))
		}
		// A base in use with no stamp (its warm-up failed, waited out or never ran) uses the folder all the same: it
		// counts with the folder's last use.
		for base := range c.in.InUse[project] {
			if !stamped[base] {
				judged(c.judge(project, base, whole.LastUsed))
			}
		}
		if age := c.in.Now.Sub(whole.LastUsed); age < CleanGrace {
			judged(verdict{reason: CleanKeptRecent, detail: "used " + ago(age) + " ago"})
		}
		if kept == 0 {
			whole.Bytes = treeSize(dir)
			v := verdict{gone: true, reason: CleanUnused}
			if len(gone) > 0 {
				v = gone[0]
			}
			if slices.ContainsFunc(gone, func(v verdict) bool { return v.reason == CleanOld }) {
				v = verdict{gone: true, reason: CleanOld, detail: "in use, but not used for " + ago(c.in.Now.Sub(whole.LastUsed))}
			} else if len(stamps) > 0 {
				v.detail = fmt.Sprintf("no task or experiment uses its %d warmed base(s)", len(stamps))
			} else {
				v.detail = "no task or experiment uses its project"
			}
			c.add(whole, v)
			continue
		}
		// The project stays; its venvs and metadata folders that no kept base names go.
		var freed int64
		for _, sub := range []string{"py", "py-meta"} {
			units, err := realDirs(filepath.Join(dir, sub))
			if err != nil {
				return err
			}
			for _, u := range units {
				path := filepath.Join(dir, sub, u.Name())
				it, v, ok := c.unit(project, sub, path, u.Name(), stamps)
				if !ok {
					continue
				}
				it.lock, it.recheck = whole.lock, func() time.Time {
					now, _ := readStamps(state)
					last, _ := unitLast(path, sub, u.Name(), now)
					return last
				}
				it.Bytes = treeSize(path)
				freed += it.Bytes
				c.add(it, v)
			}
		}
		whole.Bytes = treeSize(dir) - freed
		c.add(whole, best)
	}
	return nil
}

// unit decides for a venv's or metadata's folder of a kept project: ok is false when it stays with the project (a
// kept base names it, or it was made within CleanGrace). A folder moved aside (.bad-<time>), one a warm-up left
// unfinished (.new-*) and one no stamp names are unused.
func (c *planner) unit(project, sub, path, key string, stamps []stamp) (CleanItem, verdict, bool) {
	it := CleanItem{Kind: CleanDeps, Path: path, Project: project}
	last, named := unitLast(path, sub, key, stamps)
	it.LastUsed = last
	if !named {
		if c.in.Now.Sub(last) < CleanGrace {
			return it, verdict{}, false
		}
		detail := "no warmed base names it"
		if strings.Contains(key, ".bad-") || strings.HasPrefix(key, ".new-") {
			detail = "set aside by a warm-up"
		}
		return it, verdict{gone: true, reason: CleanUnused, detail: detail}, true
	}
	var gone []verdict
	var bases []string
	for _, s := range stamps {
		if (sub == "py" && s.venv == key) || (sub == "py-meta" && s.meta == key) {
			v := c.judge(project, s.base, s.used)
			if !v.gone {
				return it, verdict{}, false
			}
			gone, bases = append(gone, v), append(bases, s.base)
		}
	}
	if len(bases) == 1 {
		it.Base = bases[0]
	}
	if i := slices.IndexFunc(gone, func(v verdict) bool { return v.reason == CleanOld }); i >= 0 {
		return it, gone[i], true
	}
	return it, gone[0], true
}

// rank orders the reasons to keep: a task first, then an experiment, then recent use.
func rank(reason string) int {
	return slices.Index([]string{CleanKeptTask, CleanKeptExperiment, CleanKeptRecent}, reason)
}

// quarantine plans what the quarantine holds: all of it goes.
func (c *planner) quarantine(ctx context.Context) error {
	dir := quarantine(c.in.Layout)
	if dir == "" || !realFolder(dir) {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("clean: %w", err)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue // recovery removes a link there (emptyQuarantine); cleanup removes no link
		}
		path := filepath.Join(dir, e.Name())
		c.add(CleanItem{Kind: CleanQuarantine, Path: path, Bytes: treeSize(path), LastUsed: modTime(path)},
			verdict{gone: true, reason: CleanQuarantined, detail: "could not be removed when it was put there"})
	}
	return nil
}

// realFolder reports whether p is a folder and not a link.
func realFolder(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

// treeSize is the disk space of the tree at root: each file's blocks, counted once however many links it has here,
// never following a link. What cannot be read counts as nothing.
func treeSize(root string) int64 {
	var total int64
	seen := map[[2]uint64]bool{}
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable (a read-only venv's folder is still listable): skip what cannot be read
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			total += info.Size()
			return nil
		}
		key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
		if st.Nlink > 1 {
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		total += int64(st.Blocks) * 512
		return nil
	})
	return total
}

// markUsed sets path's modification time to now, as its last use for cleanup: a seed when a run's grading takes it,
// a warm-up stamp when a run or a validation of its base uses what it names. It never follows a link, and a failure
// only makes the item look older than it is (cleanup then removes it sooner, and its next use makes it again).
func markUsed(path string) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 {
		return
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// seedTaken is prepareSeed's first look: whether seed is a finished seed, read and marked used (markUsed) under a shared
// lock on its lock file. Cleanup takes that lock exclusively, rechecks the seed's last use and only then moves it away:
// so it either sees this mark and keeps the seed, or moved the seed before this look, which then finds none and makes
// it again. What remains is the time from here to the grade's clone, which CleanGrace covers: a seed marked within it
// is never removed. A missing seed takes no lock and creates nothing.
func seedTaken(ctx context.Context, seed string) (bool, error) {
	if _, err := os.Lstat(seed); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	unlock, err := lockShared(ctx, seed+".lock")
	if err != nil {
		return false, fmt.Errorf("grading seed lock: %w", err)
	}
	defer unlock()
	ready, err := seedReady(seed)
	if ready {
		markUsed(seed)
	}
	return ready, err
}

// lockShared takes a shared lock on path (created when missing), waiting until ctx ends: it excludes home.LockFile's
// exclusive lock, not other shared ones.
func lockShared(ctx context.Context, path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Errors of RemoveClean for items it left in place.
var (
	// ErrCleanUsed: the item was used after the plan was made (a validation, which takes no run lock).
	ErrCleanUsed = errors.New("used since it was listed: kept")
	// ErrCleanBusy: a warm-up or a seed's maker holds the item's lock.
	ErrCleanBusy = errors.New("in use by a warm-up: kept; try again later")
)

// cleanLockWait is how long RemoveClean waits for an item's lock before it leaves the item (ErrCleanBusy).
const cleanLockWait = 3 * time.Second

// RemoveClean removes the plan's items of every kind but leftovers (those are recovery's: the caller runs it), each
// under its lock when it has one and only if not used since the plan, and returns each item's error (nil: removed).
// Call it holding the run lock (home.Layout.LockRuns), so that no run uses what goes; a validation may, which the lock
// and the recheck cover. It refuses any path outside the cache and deps folders (cleanable).
func RemoveClean(ctx context.Context, layout home.Layout, items []CleanItem) []error {
	errs := make([]error, len(items))
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			errs[i] = err
			continue
		}
		errs[i] = removeItem(ctx, layout, it)
	}
	return errs
}

func removeItem(ctx context.Context, layout home.Layout, it CleanItem) error {
	if it.Kind == CleanLeftovers {
		return errors.New("a run's leftovers are removed by recovery")
	}
	if err := cleanable(layout, it.Path); err != nil {
		return err
	}
	if it.Kind == CleanQuarantine {
		return removeTree(it.Path) // in the quarantine already
	}
	if it.lock != "" {
		if err := os.MkdirAll(filepath.Dir(it.lock), 0o700); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, cleanLockWait)
		unlock, err := home.LockFile(waitCtx, it.lock, nil)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
			return ErrCleanBusy
		case err != nil:
			return fmt.Errorf("lock: %w", err)
		}
		defer unlock()
	}
	if it.recheck != nil && it.recheck().After(it.LastUsed) {
		return ErrCleanUsed
	}
	if it.stamps != "" {
		if err := removeStamps(layout, it.stamps); err != nil {
			return err
		}
	}
	return discard(layout, it.Path)
}

// removeStamps removes everything in a project's warm-up state folder but its lock: the stamps, the .steps markers and
// the metadata tries, so that the next run of every base warms again.
func removeStamps(layout home.Layout, state string) error {
	entries, err := os.ReadDir(state)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("warm-up state: %w", err)
	}
	for _, e := range entries {
		if e.Name() == warmLockName {
			continue // a waiting warm-up holds the same file: removing it would let two in at once
		}
		p := filepath.Join(state, e.Name())
		if err := cleanable(layout, p); err != nil {
			return err
		}
		if err := removeTree(p); err != nil {
			return fmt.Errorf("warm-up state: %w", err)
		}
	}
	return nil
}

// discard moves path into the quarantine whole (one rename: a crash leaves it whole or gone, never half a seed or a
// venv in place), then removes it there. A read-only folder (a venv) is made writable first, as a folder's rename
// to another parent needs.
func discard(layout home.Layout, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() && info.Mode().Perm()&0o200 == 0 {
		if err := os.Chmod(path, info.Mode().Perm()|0o700); err != nil {
			return err
		}
	}
	moved, err := moveAside(path, quarantine(layout))
	if err != nil {
		return fmt.Errorf("move %s into the quarantine: %w", path, err)
	}
	if err := removeTree(moved); err != nil {
		return fmt.Errorf("moved into the quarantine (%s) but not removed: %w", moved, err)
	}
	return nil
}

// cleanable refuses any path cleanup must not remove: one not strictly inside the data folder's cache or deps folder,
// a folder the cache's own layout needs (a top folder of the cache), one reached through a link (each folder from the
// cache or deps folder down to path's parent must be a real folder: a link there would lead the removal elsewhere),
// and a link itself (plans list real folders and files only).
func cleanable(layout home.Layout, path string) error {
	path = filepath.Clean(path)
	for _, root := range []string{layout.Cache, layout.Deps} {
		if root == "" || !filepath.IsAbs(path) {
			continue
		}
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if root == filepath.Clean(layout.Cache) && len(parts) < 2 {
			return fmt.Errorf("refused: %s is a top folder of the cache", path)
		}
		dir := root
		for i := 0; i < len(parts); i++ {
			if !realFolder(dir) {
				return fmt.Errorf("refused: %s is not a real folder (a link, or gone), so %s is not removed", dir, path)
			}
			dir = filepath.Join(dir, parts[i])
		}
		if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refused: %s is a link, which no plan lists", path)
		}
		return nil
	}
	return fmt.Errorf("refused: %s is not inside the data folder's cache or deps folder", path)
}
