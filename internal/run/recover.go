package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

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
	if err := os.WriteFile(filepath.Join(s.Record.RecordsDir, startFile), data, 0o600); err != nil {
		return fmt.Errorf("run start file: %w", err)
	}
	return nil
}

func (env Env) workspaceName() string {
	if env.Workspace != "" {
		return env.Workspace
	}
	return env.ID
}

// Predicted lists the folders a run in the named workspace will use: the workspace (its checkout and, with an API key
// or token, its Claude config) and, with the user's login, its session folder under the user's Claude config. A run
// that may overlap it denies these before either starts, since its deny list is fixed when it starts.
func (env Env) Predicted(name string) []string {
	workspace := realPath(filepath.Join(env.Layout.Workspaces, name))
	paths := []string{workspace}
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
}

// AliveError reports runs whose agents may still be working: their process groups exist.
type AliveError struct{ Runs []string }

func (e *AliveError) Error() string {
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
// The workspaces and grading copies of recovered runs are removed.
func Recover(ctx context.Context, layout home.Layout, stored func(id string) (bool, error), secret string, now time.Time) ([]Orphan, error) {
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
			continue
		}
		dir := filepath.Join(layout.Records, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, startFile))
		if errors.Is(err, os.ErrNotExist) { // before the start file: nothing was prepared yet
			if err := os.RemoveAll(dir); err != nil {
				return orphans, fmt.Errorf("remove %s: %w", dir, err)
			}
			continue
		}
		if err != nil {
			return orphans, fmt.Errorf("run %s: %w", e.Name(), err)
		}
		var s start
		if err := json.Unmarshal(data, &s); err != nil {
			return orphans, fmt.Errorf("run %s: start file: %w", e.Name(), err)
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
		if err := os.RemoveAll(filepath.Join(dir, "verify")); err != nil {
			return orphans, fmt.Errorf("remove the grading copy of %s: %w", e.Name(), err)
		}
		if s.Finished && s.Record.Outcome != "" {
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
		if rec.Metrics.UnpricedRequests > 0 {
			rec.Notes = append(rec.Notes, fmt.Sprintf("%d request(s) on models without a list price are not in the cost", rec.Metrics.UnpricedRequests))
		}
		if err := (Env{Secret: secret}).redactRecords(dir); err != nil {
			return orphans, err
		}
		orphans = append(orphans, Orphan{Record: rec, Meta: s.Meta})
	}
	slices.SortFunc(orphans, func(a, b Orphan) int { return strings.Compare(a.Record.ID, b.Record.ID) })
	if len(alive.Runs) > 0 {
		return orphans, alive
	}
	return orphans, nil
}

// groupExists reports whether a process group exists (possibly another user's: EPERM).
func groupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
