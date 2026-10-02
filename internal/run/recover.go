package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/pricing"
)

// How a recovered run was stored (Record.Recovered).
const (
	RecoveredStopped  = "stopped"  // the run was cut short: cancelled, with what it spent
	RecoveredFinished = "finished" // the run had finished; only storing it was cut short
)

// startFile is written to a run's records when they are created, and again when its agent starts: if the Agentium
// process dies, a later one finds the run (Recover), its spend in the transcript, and its agent's process group.
const startFile = "started.json"

type start struct {
	Record       Record          `json:"record"`
	Workspace    string          `json:"workspace"`
	AgentStarted bool            `json:"agent_started"`
	PGID         int             `json:"pgid,omitempty"` // of the command running now: setup, the agent or verification
	Finished     bool            `json:"finished,omitempty"`
	Meta         json.RawMessage `json:"meta,omitempty"`
}

func (env Env) writeStart(s start) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("run start file: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(s.Record.RecordsDir, startFile), data, 0o600); err != nil {
		return fmt.Errorf("run start file: %w", err)
	}
	return nil
}

// writeFileAtomic replaces path with data so that a reader, or a process killed mid-write, sees the old file or the
// new one, never a truncated mix: it writes a temp file in the same folder, syncs it and renames it over path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close() // already closed on the late paths; the error is then irrelevant
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// corruptSuffix names a start file that could not be read, moved aside by Recover.
const corruptSuffix = ".corrupt"

func (env Env) workspaceName() string {
	if env.Workspace != "" {
		return env.Workspace
	}
	return env.ID
}

// Predicted lists the folders a run in the named workspace will use: the workspace (its checkout and, with an API key
// or token, its Claude config), its temp root (as written and resolved: /tmp is /private/tmp on macOS) and, with the
// user's login, its session folder under the user's Claude config. A run that may overlap it denies these before
// either starts, since its deny list is fixed when it starts.
func (env Env) Predicted(name string) []string {
	workspace := realPath(filepath.Join(env.Layout.Workspaces, name))
	paths := []string{workspace}
	if temp := env.Layout.RunTemp(name); temp != "" {
		paths = append(paths, temp)
		if resolved := realPath(temp); resolved != temp {
			paths = append(paths, resolved)
		}
	}
	if env.SignIn == claude.SignInLogin {
		// Resolved as the sandbox will see it, even when the Claude config folder is a symlink.
		paths = append(paths, realPath(claude.SessionFolder(claude.UserConfigDir(env.Environ, env.Home), filepath.Join(workspace, "repo"))))
	}
	return paths
}

// Orphan is a run whose Agentium process ended before storing it, recovered as a cancelled run.
type Orphan struct {
	Record Record
	Meta   json.RawMessage
	// Unreadable is set when the run's start file could not be parsed (an old truncated write, a damaged disk): it holds
	// the path the file was moved to. The run's task, arm and experiment slot are unknown, so Record holds only its ID,
	// records folder and what the transcript shows it spent; the caller must report it and not store it as a run.
	Unreadable string
}

// AliveError reports runs whose agents may still be working: their process groups exist.
type AliveError struct {
	Runs       []string
	Unreadable []string // runs with an unreadable start file that may be alive: full sentences, each with what to do
}

func (e *AliveError) Error() string {
	if len(e.Runs) == 0 {
		return strings.Join(e.Unreadable, "; ")
	}
	return strings.Join(append([]string{e.runsText()}, e.Unreadable...), "; ")
}

func (e *AliveError) runsText() string {
	return fmt.Sprintf("%s may still be running (its Agentium process ended): wait for it to finish, or stop it, then try again "+
		"(ps -o pid,command -g PGID shows what it is; after a restart the number may belong to something else)", strings.Join(e.Runs, ", "))
}

// Recover finds the runs in the records folder that stored does not know. Call it only while holding the data
// folder's run lock (home.Layout.LockRuns), when no other process can be running agents:
//   - a run that finished (its runner was killed before storing it) is returned as it finished;
//   - a run whose agent never started spent nothing: its records and workspace are removed;
//   - a started run becomes a cancelled Record with what its transcript shows it spent (priced from its requests when
//     Claude Code wrote no result), redacted with secret;
//   - a run whose current command's process group (setup, the agent, verification) still exists is left alone and
//     reported in an *AliveError, along with the runs that were recovered.
//
// The workspaces, temp roots, grading copies and grade folders (gradingFolder: a grade's cache clone) of recovered runs
// are removed, and a stored run's leftover grade folder too, after what their grades left running is stopped; what
// resists removal is moved into the quarantine with a warning (cleanGrade, RecoverWarn). A run's temp root is found from its
// workspace's name (home.Layout.RunTemp), so start files written before runs had one are read as they were.
//
// A stored run's records keep nothing to recover, but a pair's comparison (Env.JudgePair) that Agentium died in leaves
// its folder there, with a config folder that may hold the sign-in: with the run lock held none is running, so it goes.
func Recover(ctx context.Context, layout home.Layout, stored func(id string) (bool, error), secret string, now time.Time) ([]Orphan, error) {
	return RecoverWarn(ctx, layout, stored, secret, now, nil)
}

// RecoverWarn is Recover, and tells warn (when set) what it could not clean up but did not let stop it: a grading copy
// or grade folder that resisted removal was moved into the quarantine (removeOrQuarantine), or could not even be moved;
// processes a grade left that could not be stopped; quarantined folders that still cannot be removed. What a grade
// wrote never stops recovery, so it never blocks later runs.
func RecoverWarn(ctx context.Context, layout home.Layout, stored func(id string) (bool, error), secret string, now time.Time, warn func(string)) ([]Orphan, error) {
	if warn == nil {
		warn = func(string) {}
	}
	if w := emptyQuarantine(quarantine(layout)); w != "" {
		warn(w)
	}
	entries, err := os.ReadDir(layout.Records)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("run records: %w", err)
	}
	var orphans []Orphan
	alive := &AliveError{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return orphans, err
		}
		if !e.IsDir() {
			continue
		}
		known, err := stored(e.Name())
		if err != nil {
			return orphans, err
		}
		if known {
			if err := os.RemoveAll(filepath.Join(layout.Records, e.Name(), pairJudgeFolder)); err != nil {
				return orphans, fmt.Errorf("remove the pair judge folder of %s: %w", e.Name(), err)
			}
			// A grade's folder a failed removal left (the run is stored anyway).
			cleanGrade(layout, filepath.Join(layout.Records, e.Name()), warn)
			continue
		}
		dir := filepath.Join(layout.Records, e.Name())
		// A write killed before its rename leaves a temp file behind (the write is atomic: started.json itself is whole).
		// No folder fsync is needed: rename is atomic under SIGKILL, and after a power loss the old file, or no file at
		// all for the first write, are both states Recover handles.
		if stray, _ := filepath.Glob(filepath.Join(dir, startFile+".tmp-*")); len(stray) > 0 {
			for _, f := range stray {
				_ = os.Remove(f)
			}
		}
		data, err := os.ReadFile(filepath.Join(dir, startFile))
		if errors.Is(err, os.ErrNotExist) { // before the start file: nothing was prepared yet
			if _, err := os.Stat(filepath.Join(dir, startFile+corruptSuffix)); err == nil {
				continue // set aside by an earlier recovery and reported then: keep it for the user
			}
			if err := os.RemoveAll(dir); err != nil {
				return orphans, fmt.Errorf("remove %s: %w", dir, err)
			}
			continue
		}
		if err != nil {
			return orphans, fmt.Errorf("run %s: %w", e.Name(), err)
		}
		var s start
		if parseErr := json.Unmarshal(data, &s); parseErr != nil {
			var syntaxErr *json.SyntaxError
			if !errors.As(parseErr, &syntaxErr) && !errors.Is(parseErr, io.ErrUnexpectedEOF) {
				// Valid JSON of another shape (a newer version's file, say) is not damage: fail loudly, touch nothing.
				return orphans, fmt.Errorf("run %s: start file: %w", e.Name(), parseErr)
			}
			orphan, aliveNote, err := recoverUnreadable(layout, dir, e.Name(), data, parseErr, secret, now, warn)
			if err != nil {
				return orphans, err
			}
			if aliveNote != "" {
				alive.Unreadable = append(alive.Unreadable, aliveNote)
				continue
			}
			if orphan != nil {
				orphans = append(orphans, *orphan)
			}
			continue
		}
		if s.PGID > 0 && !s.Finished && groupExists(s.PGID) {
			alive.Runs = append(alive.Runs, fmt.Sprintf("the agent or a command of run %s (process group %d)", e.Name(), s.PGID))
			continue
		}
		workspace := realPath(s.Workspace)
		if !within(workspace, realPath(layout.Workspaces)) || workspace == realPath(layout.Workspaces) {
			return orphans, fmt.Errorf("run %s: its start file names a workspace outside %s", e.Name(), layout.Workspaces)
		}
		if err := os.RemoveAll(workspace); err != nil {
			return orphans, fmt.Errorf("remove %s: %w", workspace, err)
		}
		if err := removeRunTemp(layout.RunTemp(filepath.Base(s.Workspace))); err != nil {
			return orphans, fmt.Errorf("run %s: %w", e.Name(), err)
		}
		// The grading copy and the grade's own folder (its cache clone, temp root and profile).
		cleanGrade(layout, dir, warn)
		if s.Finished && s.Record.Outcome != "" {
			// Its judge may have been cut short: the judge's folder (a config folder with the sign-in, for an API key or
			// a token) goes, and the records are redacted again, as for a stopped run.
			for _, sub := range []string{"judge", pairJudgeFolder} {
				if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil {
					return orphans, fmt.Errorf("remove the %s folder of %s: %w", sub, e.Name(), err)
				}
			}
			if err := (Env{Secret: secret}).redactRecords(dir); err != nil {
				return orphans, err
			}
			rec := s.Record
			rec.Recovered = RecoveredFinished
			rec.Notes = append(rec.Notes, "stored on recovery: Agentium stopped after the run finished, before storing it")
			orphans = append(orphans, Orphan{Record: rec, Meta: s.Meta})
			continue
		}
		if !s.AgentStarted || s.Finished { // nothing spent, or a run that ended before its agent (executeRun drops those)
			if err := os.RemoveAll(dir); err != nil {
				return orphans, fmt.Errorf("remove %s: %w", dir, err)
			}
			continue
		}
		rec := s.Record
		rec.RecordsDir, rec.Outcome, rec.Passed = dir, claude.OutcomeCancelled, nil
		rec.Finished = now.UTC()
		transcript := filepath.Join(dir, "stream.jsonl")
		if info, err := os.Stat(transcript); err == nil {
			rec.Finished = info.ModTime().UTC()
		}
		rec.Metrics, _ = parseFile(transcript) // what can be read is kept; a missing transcript spent nothing visible
		rec.Recovered = RecoveredStopped
		rec.Notes = append(rec.Notes, fmt.Sprintf("Agentium stopped during this run; recovered on %s", now.UTC().Format("2006-01-02 15:04")))
		if !rec.Metrics.SawResult && rec.Metrics.EstimatedCostUSD > 0 {
			rec.Metrics.CostUSD = rec.Metrics.EstimatedCostUSD
			rec.CostEstimated = true
			rec.Notes = append(rec.Notes, fmt.Sprintf("Claude Code reported no cost: estimated from the transcript's requests at the list prices of %s", pricing.Date))
		}
		rec.IsolatedCostUSD = isolatedCost(rec)
		if rec.Metrics.UnpricedRequests > 0 {
			rec.Notes = append(rec.Notes, fmt.Sprintf("%d request(s) on models without a list price are not in the cost", rec.Metrics.UnpricedRequests))
		}
		if err := (Env{Secret: secret}).redactRecords(dir); err != nil {
			return orphans, err
		}
		orphans = append(orphans, Orphan{Record: rec, Meta: s.Meta})
	}
	slices.SortFunc(orphans, func(a, b Orphan) int { return strings.Compare(a.Record.ID, b.Record.ID) })
	if len(alive.Runs) > 0 || len(alive.Unreadable) > 0 {
		return orphans, alive
	}
	return orphans, nil
}

// groupExists reports whether a process group exists (possibly another user's: EPERM).
func groupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// unreadableAliveWindow is how recently an unreadable run must have written, with no result yet, for its agent to count
// as possibly alive. It is about the time since the last write, not the run's timeout: nobody enforces --timeout once
// Agentium is dead, so a live agent silent for longer than this is recovered (and a still-running one is not stopped),
// and in the no-transcript case a setup command running longer than this looks dead too. A "pgid" still readable in
// the truncated bytes is checked first and is exact.
const unreadableAliveWindow = 25 * time.Minute

var pgidInTruncated = regexp.MustCompile(`"pgid":\s*(\d+)[,}]`)

// recoverUnreadable handles a run whose start file cannot be parsed, losing the least. A start file that cannot be
// read must not block every later start, but the run's task, arm, experiment slot and process group are unknown, so
// it is not stored as a run. What needs nothing from the file is done: the transcript decides what is left of it.
//   - A process group still readable in the bytes and alive, or a transcript without a result written within
//     unreadableAliveWindow (with no transcript, a recently written start file), may belong to a live agent: aliveNote
//     is returned and nothing is touched.
//   - Otherwise the workspace (found from the transcript's working directory, which must be <Workspaces>/<name>/repo),
//     its temp root, the grading copy and the judge's config folder are removed and the records redacted; then, last,
//     the file is moved aside (not retried, not deleted) and the transcript's spend is returned as an Orphan with
//     Unreadable set, for the caller to report. The rename is the commit point: every step before it is safe to
//     repeat, so a failure leaves the file in place and the next start retries.
//   - With no transcript the agent never started and nothing was spent: the records folder and a workspace named
//     after the run go too. That covers `run once` workspaces only; an experiment's workspace is named after its slot
//     and found by no one, so Once removes a stale one before its checkout.
//
// The judge's per-call cost lives only in the start file and may be missing from the spend.
func recoverUnreadable(layout home.Layout, dir, id string, data []byte, parseErr error, secret string, now time.Time, warn func(string)) (orphan *Orphan, aliveNote string, err error) {
	startPath := filepath.Join(dir, startFile)
	transcript := filepath.Join(dir, "stream.jsonl")
	info, statErr := os.Stat(transcript)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, "", fmt.Errorf("run %s: %w", id, statErr)
	}
	hasTranscript := statErr == nil
	var m claude.Metrics
	if hasTranscript {
		m, _ = parseFile(transcript)
	}
	if match := pgidInTruncated.FindSubmatch(data); match != nil {
		if pgid, err := strconv.Atoi(string(match[1])); err == nil && pgid > 0 && groupExists(pgid) {
			return nil, fmt.Sprintf("run %s: its start file is unreadable, but its process group %d exists; it may still be running: "+
				"wait for it to finish, or stop it, then try again (ps -o pid,command -g %d shows what it is; after a restart the "+
				"number may belong to something else; if it is not an Agentium agent, move %s aside)", id, pgid, pgid, startPath), nil
		}
	}
	fresh, written := info, transcript
	if !hasTranscript {
		fresh, _ = os.Stat(startPath)
		written = startPath
	}
	if fresh != nil && !m.SawResult && now.Sub(fresh.ModTime()) < unreadableAliveWindow {
		return nil, fmt.Sprintf("run %s: its start file is unreadable and %s was written at %s, so it may still be running: "+
			"try again after %s, or stop it first", id, written, fresh.ModTime().Format("15:04:05"),
			fresh.ModTime().Add(unreadableAliveWindow).Format("15:04:05")), nil
	}
	workspace := ""
	switch {
	case m.CWD != "":
		// Claude Code started in <workspace>/repo. Only that shape is trusted with a removal: an absolute path whose
		// last element is repo; removeStaleWorkspace then requires a direct child of the workspaces folder.
		if cwd := filepath.Clean(m.CWD); filepath.IsAbs(cwd) && filepath.Base(cwd) == "repo" {
			workspace = filepath.Dir(cwd)
		}
	case !hasTranscript:
		workspace = filepath.Join(layout.Workspaces, id) // the agent never started; the default workspace name
	}
	if workspace != "" {
		if err := removeStaleWorkspace(layout.Workspaces, workspace, layout.RunTemp(filepath.Base(workspace))); err != nil {
			return nil, "", fmt.Errorf("run %s: %w", id, err)
		}
	}
	if !hasTranscript {
		if err := os.RemoveAll(dir); err != nil {
			return nil, "", fmt.Errorf("remove %s: %w", dir, err)
		}
		return nil, "", nil
	}
	// The hidden tests (what a grade wrote never stops recovery), then the sign-in config folders.
	cleanGrade(layout, dir, warn)
	for _, sub := range []string{"judge", pairJudgeFolder} {
		if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil {
			return nil, "", fmt.Errorf("remove the %s folder of %s: %w", sub, id, err)
		}
	}
	// Redaction rewrites stream.jsonl, which would make a half-finished cleanup look like a live run for the next
	// window: the transcript keeps its mtime, on failure too.
	redactErr := (Env{Secret: secret}).redactRecords(dir)
	if err := os.Chtimes(transcript, info.ModTime(), info.ModTime()); err != nil && redactErr == nil {
		redactErr = err
	}
	if redactErr != nil {
		return nil, "", redactErr
	}
	aside := startPath + corruptSuffix
	if err := os.Rename(startPath, aside); err != nil {
		return nil, "", fmt.Errorf("run %s: start file unreadable (%v) and not moved aside: %w", id, parseErr, err)
	}
	rec := Record{ID: id, RecordsDir: dir, Outcome: claude.OutcomeCancelled, Recovered: RecoveredStopped, Finished: info.ModTime().UTC(), Metrics: m}
	if !m.SawResult && m.EstimatedCostUSD > 0 {
		rec.Metrics.CostUSD, rec.CostEstimated = m.EstimatedCostUSD, true
	}
	rec.IsolatedCostUSD = isolatedCost(rec)
	rec.Notes = append(rec.Notes, fmt.Sprintf("start file unreadable (%v): moved to %s; judge spend, if any, is not included", parseErr, aside))
	return &Orphan{Record: rec, Unreadable: aside}, "", nil
}

// cleanGrade stops what a dead run's grade left running, then removes its grading copy and grade folder in the run's
// records dir, moving into the quarantine what resists removal. Nothing here returns an error: a grade controls what
// these folders hold, so a failure is a warning (warn), never a reason to stop recovery and block every later run.
func cleanGrade(layout home.Layout, dir string, warn func(string)) {
	verify, grade := filepath.Join(dir, "verify"), filepath.Join(dir, gradingFolder)
	if _, err := os.Lstat(verify); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(grade); errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	killed, err := stopUsing(buildtool.Profiles(), filepath.Join(grade, "cache"), grade, verify)
	for _, k := range killed {
		warn(fmt.Sprintf("run %s: its grade left a process running; it was stopped: %s", filepath.Base(dir), k))
	}
	if err != nil {
		warn(fmt.Sprintf("run %s: %v", filepath.Base(dir), err))
	}
	for _, folder := range []string{verify, grade} {
		warning, err := removeOrQuarantine(folder, quarantine(layout))
		if warning != "" {
			warn(warning)
		}
		if err != nil {
			warn(fmt.Sprintf("run %s: %v; recovery tries again next time", filepath.Base(dir), err))
		}
	}
}
