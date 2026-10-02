package watch

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"

	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// PanelSize is how many tasks a drift chart's panel holds (the statistics note, §4).
const PanelSize = 8

// PanelTask is one of a panel's fixed tasks: its id and name, and the fingerprint of its definition when the panel
// was drawn, so an edit closes the chart.
type PanelTask struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// TaskFingerprint is a digest of what defines a task's runs: its commits, instruction, hidden tests, reference files,
// setup, verification and grading. A new validation result is not an edit; adopted verification or setup commands are.
func TaskFingerprint(t store.Task) string {
	// A nil list and an empty one are the same task; values of these types always encode.
	encoded, _ := json.Marshal([]any{t.BaseCommit, t.SolutionCommit, t.Instruction, list(t.HiddenTests), list(t.Reference), list(t.Setup),
		list(t.Verify), cmp.Or(t.Grading, task.GradingTests)})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func list(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// DrawPanel draws PanelSize tasks from eligible (the pool's valid, reviewed, non-flaky tasks with enough life left; the
// caller selects them) by seed. The same tasks and seed draw the same panel, in name order, whatever order eligible
// is in. Fewer than PanelSize tasks is an error: no chart starts.
func DrawPanel(eligible []store.Task, seed uint64) ([]PanelTask, error) {
	if len(eligible) < PanelSize {
		return nil, fmt.Errorf("a drift panel needs %d eligible tasks, the project has %d", PanelSize, len(eligible))
	}
	sorted := slices.SortedFunc(slices.Values(eligible), func(a, b store.Task) int { return cmp.Compare(a.Name, b.Name) })
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	rng.Shuffle(len(sorted), func(i, j int) { sorted[i], sorted[j] = sorted[j], sorted[i] })
	panel := make([]PanelTask, PanelSize)
	for i, t := range sorted[:PanelSize] {
		panel[i] = PanelTask{ID: t.ID, Name: t.Name, Fingerprint: TaskFingerprint(t)}
	}
	slices.SortFunc(panel, func(a, b PanelTask) int { return cmp.Compare(a.Name, b.Name) })
	return panel, nil
}

// PanelTasks decodes a stored panel's tasks.
func PanelTasks(p store.DriftPanel) ([]PanelTask, error) {
	var tasks []PanelTask
	if err := json.Unmarshal(p.Tasks, &tasks); err != nil {
		return nil, fmt.Errorf("drift panel %d: decode its tasks: %w", p.ID, err)
	}
	return tasks, nil
}

// PanelChange says why a panel no longer measures the same thing, given the project's tasks now: a task was removed,
// retired, edited or is no longer valid. Empty: the chart goes on.
func PanelChange(panel []PanelTask, tasks []store.Task) string {
	byID := make(map[int64]store.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	for _, p := range panel {
		t, ok := byID[p.ID]
		switch {
		case !ok:
			return fmt.Sprintf("task %s was removed", p.Name)
		case t.Retired():
			return fmt.Sprintf("task %s retired (%s)", p.Name, t.RetiredReason)
		case TaskFingerprint(t) != p.Fingerprint:
			return fmt.Sprintf("task %s was edited", p.Name)
		case validationStatus(t) != task.StatusValid:
			return fmt.Sprintf("task %s is no longer valid", p.Name)
		}
	}
	return ""
}

func validationStatus(t store.Task) string {
	var v struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(t.Validation, &v) != nil {
		return ""
	}
	return v.Status
}

// Chart is what a new drift chart is keyed on: the project, model and effort, and the context snapshot pinned for it.
type Chart struct {
	ProjectID    int64
	Model        string
	Effort       string
	Snapshot     string // the snapshot's commit
	SnapshotName string
}

// StartChart draws a panel by seed and stores it as the chart's. A project has one open chart per model and effort
// (store.ErrExists otherwise).
func (s Service) StartChart(ctx context.Context, c Chart, eligible []store.Task, seed uint64) (store.DriftPanel, error) {
	panel, err := DrawPanel(eligible, seed)
	if err != nil {
		return store.DriftPanel{}, err
	}
	encoded, err := json.Marshal(panel)
	if err != nil {
		return store.DriftPanel{}, fmt.Errorf("encode the drift panel: %w", err)
	}
	return s.DB.SaveDriftPanel(ctx, store.DriftPanel{ProjectID: c.ProjectID, Model: c.Model, Effort: c.Effort, Snapshot: c.Snapshot,
		SnapshotName: c.SnapshotName, Seed: int64(seed), Tasks: encoded, CreatedAt: s.Now()})
}

// CloseChartIfChanged closes an open chart whose panel no longer holds (PanelChange) and returns the reason; empty
// when it goes on. The chart's open check ends without a point; a new chart starts a new panel.
func (s Service) CloseChartIfChanged(ctx context.Context, p store.DriftPanel, tasks []store.Task) (string, error) {
	panel, err := PanelTasks(p)
	if err != nil {
		return "", err
	}
	reason := PanelChange(panel, tasks)
	if reason == "" {
		return "", nil
	}
	if _, err := s.DB.CloseDriftPanel(ctx, p.ID, reason, s.Now()); err != nil {
		return "", err
	}
	return reason, nil
}
