package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// The services here validate stored tasks: one at a time (Validating.Validator and StoreValidation) or a batch jobs at
// a time (Validating.Batch). They keep no state; I/O goes to the writers and callbacks given.

// BatchStatuses are the values task validate --all --status takes.
func BatchStatuses() []string {
	return []string{"unvalidated", StatusValid, StatusInvalid, StatusFlaky, StatusUnchecked}
}

// ValidateOptions are the validation settings task validate takes from its flags, for one task or many.
type ValidateOptions struct {
	Arms     []Arm
	Repeat   int
	Weak     bool
	MaxHunks int
	Timeout  time.Duration
	Keep     bool
}

// Validating is what validating tasks needs: the project's database and Agentium's repository, where checkouts and logs
// go, and the environment commands run in.
type Validating struct {
	DB        *store.Store
	Bare      string // Agentium's bare repository for the project
	Artifacts string // the data folder's artifacts folder
	Env       []string
	Cache     string // the data folder's cache root: Validator.Cache
	Now       func() time.Time
	// ReferenceDiff returns the reference diff a judge-graded task's judge would read (judge.ReferenceDiff, which this
	// package cannot import).
	ReferenceDiff func(ctx context.Context, base, solution string, reference []string) (string, error)
	// Toolchain is recorded in each validation (Validator.Toolchain); nil records none.
	Toolchain Toolchain
	// SkipInUse makes StoreValidation store nothing for a task that a locked experiment able to run still uses: it
	// returns store.ErrTaskInUse instead (store.SetTaskValidationIdle). The task pool's re-validations set it; task
	// validate, which the user asks for, does not.
	SkipInUse bool
}

// ErrTaskChanged means a validation was not stored: the task's commands changed, or it was removed, while it ran.
var ErrTaskChanged = errors.New("not stored: the task changed during validation")

// ValidationOf decodes a task's stored validation (the zero Validation when there is none or it is unreadable).
func ValidationOf(t store.Task) Validation {
	var v Validation
	if t.Validation != nil {
		_ = json.Unmarshal(t.Validation, &v) // an unreadable validation has no status, which no filter matches
	}
	return v
}

// StatusOf is a task's status for --status: "unvalidated", or its stored validation's status.
func StatusOf(t store.Task) string {
	if t.Validation == nil {
		return "unvalidated"
	}
	return ValidationOf(t).Status
}

// SpecOf is what validation needs of t.
func SpecOf(t store.Task) Spec {
	return Spec{Base: t.BaseCommit, Solution: t.SolutionCommit, HiddenTests: t.HiddenTests, Reference: t.Reference,
		Setup: t.Setup, Verify: t.Verify}
}

// Gaps lists what the task's hidden tests require that the instruction and the base do not state. f caches searches, so
// give one to every command.
func Gaps(ctx context.Context, f *Fairness, t store.Task) ([]Gap, error) {
	if t.SolutionCommit == "" || len(t.HiddenTests) == 0 {
		return nil, nil
	}
	return f.Gaps(ctx, FairnessInput{Base: t.BaseCommit, Solution: t.SolutionCommit, Instruction: t.Instruction,
		HiddenTests: t.HiddenTests, Reference: t.Reference})
}

// Arms is the base's own context followed by the named snapshots (validated as distinct and not "base").
func (v Validating) Arms(ctx context.Context, projectID int64, snapshots []string) ([]Arm, error) {
	arms := []Arm{{Name: "base"}}
	for _, name := range snapshots {
		snap, err := v.DB.SnapshotByName(ctx, projectID, name)
		if err != nil {
			return nil, err
		}
		arms = append(arms, Arm{Name: name, Snapshot: snap.CommitID})
	}
	return arms, nil
}

// Validator is a Validator for t, its checkouts and logs in a folder of its own under the artifacts (task ID and time),
// with no progress output.
func (v Validating) Validator(t store.Task, o ValidateOptions) Validator {
	folder := filepath.Join(v.Artifacts, "tasks", strconv.FormatInt(t.ID, 10), v.Now().UTC().Format("20060102T150405Z"))
	return Validator{Bare: v.Bare, WorkDir: filepath.Join(folder, "checkouts"), LogDir: filepath.Join(folder, "logs"),
		Timeout: o.Timeout, Keep: o.Keep, Repeats: o.Repeat, WeakTests: o.Weak, MaxHunks: o.MaxHunks, Env: v.Env, Cache: v.Cache, Now: v.Now,
		Toolchain: v.Toolchain}
}

// StoreValidation records result as t's validation, if t still has the verify and setup commands it was validated
// with (ErrTaskChanged otherwise) and, with SkipInUse, no locked experiment able to run uses it (store.ErrTaskInUse),
// and returns the task as stored now: edits made meanwhile (an instruction, the review flag) are kept, not overwritten
// from t. It stores even when ctx is cancelled: a validation that finished is kept through an interrupt.
func (v Validating) StoreValidation(ctx context.Context, t store.Task, result Validation, now time.Time) (store.Task, error) {
	ctx = context.WithoutCancel(ctx)
	encoded, err := json.Marshal(result)
	if err != nil {
		return t, fmt.Errorf("encode validation: %w", err)
	}
	write := v.DB.SetTaskValidation
	if v.SkipInUse {
		write = v.DB.SetTaskValidationIdle
	}
	stored, err := write(ctx, t.ID, t.Verify, t.Setup, encoded, now)
	if err != nil {
		return t, err
	}
	if !stored {
		return t, ErrTaskChanged
	}
	return v.DB.TaskByName(ctx, t.ProjectID, t.Name)
}

// Judged checks a judge-graded task without running anything (ValidateJudged) and returns the reference diff the judge
// would read.
func (v Validating) Judged(ctx context.Context, t store.Task, now time.Time) (Validation, string, error) {
	var diff string
	if len(JudgedFiles(t.Reference)) > 0 && t.SolutionCommit != "" {
		var err error
		if diff, err = v.ReferenceDiff(ctx, t.BaseCommit, t.SolutionCommit, t.Reference); err != nil {
			return Validation{}, "", err
		}
	}
	return ValidateJudged(t.Instruction, t.Reference, diff, now), diff, nil
}

// Quiet validates t as task validate NAME does, without printing: started is told each stage.
func (v Validating) Quiet(ctx context.Context, t store.Task, o ValidateOptions, started func(arm, stage string)) (Validation, error) {
	if t.Grading == GradingJudge {
		result, _, err := v.Judged(ctx, t, v.Now())
		return result, err
	}
	if t.SolutionCommit != "" {
		if err := RefuseInlineRustTests(ctx, t.BaseCommit, t.SolutionCommit, t.Reference, "--git-dir", v.Bare); err != nil {
			return Validation{}, err
		}
	}
	val := v.Validator(t, o)
	val.Started = started
	return val.Validate(ctx, SpecOf(t), o.Arms)
}

// BatchResult is one task's outcome in a batch validation.
type BatchResult struct {
	Task      store.Task // with its new validation when validated
	Validated bool       // the validation finished and is stored
	Started   bool
	Stopped   bool  // the interrupt stopped it
	Err       error // why a started validation did not finish
}

// Problem says why the task's validation is not in the table: "" when it is.
func (r BatchResult) Problem() string {
	switch {
	case r.Validated:
		return ""
	case !r.Started:
		return "not started (interrupted)"
	case r.Stopped:
		return "interrupted"
	case errors.Is(r.Err, ErrTaskChanged):
		return r.Err.Error()
	case errors.Is(r.Err, store.ErrTaskInUse):
		return "not stored: " + r.Err.Error()
	case r.Err != nil:
		return "not validated: " + r.Err.Error()
	}
	return "not validated"
}

// BatchOutput is where a batch reports: a line per finished task to Out, and the live line naming the tasks in
// progress through Show.
type BatchOutput struct {
	Out   io.Writer
	Style term.Style
	Show  func(text func() string) // required: Batch calls it before the first task starts
	// RunsBusy says an experiment is running on this machine, whose runs these validations slow.
	RunsBusy bool
}

// Batch validates tasks with o, jobs at a time, as task validate would one by one, but without per-stage output: one
// line per finished task, and a live line naming the tasks in progress. Each finished validation is stored at once (by
// this goroutine only: workers touch no database), so an interrupt keeps every result that finished; the tasks it
// stopped, or that had not started, keep their earlier validation.
//
// Validations share nothing that needs serializing: each has its own checkouts and logs (per task ID), and they share
// the build cache of Agentium's commands, which Go keeps safe for concurrent builds. They start no agents, so they do
// not take the run lock.
func (v Validating) Batch(ctx context.Context, out BatchOutput, tasks []store.Task, o ValidateOptions, jobs int) []BatchResult {
	results := make([]BatchResult, len(tasks))
	for i, t := range tasks {
		results[i].Task = t
	}
	if len(tasks) == 0 {
		return results
	}
	st := out.Style
	if out.RunsBusy {
		fmt.Fprintln(out.Out, st.Note("note: an experiment is running: these validations build and test on the same machine, and will slow its runs"))
	}
	if len(o.Arms) > 1 || o.Repeat > 1 {
		var judged []string
		for _, t := range tasks {
			if t.Grading == GradingJudge {
				judged = append(judged, t.Name)
			}
		}
		if len(judged) > 0 {
			fmt.Fprintln(out.Out, st.Note("note: --snapshot and --repeat do not apply to judge-graded tasks, for which nothing runs: "+strings.Join(judged, ", ")))
		}
	}
	var mu sync.Mutex // guards current and finished, which the live line reads
	current, finished := map[string]string{}, 0
	out.Show(func() string {
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
	done := v.startWorkers(ctx, tasks, o, jobs, &mu, current)
	for oc := range done {
		r := &results[oc.i]
		r.Started = oc.started
		if oc.err == nil {
			if r.Task, oc.err = v.StoreValidation(ctx, r.Task, oc.result, v.Now()); oc.err == nil {
				r.Validated = true
			}
		}
		if !oc.started {
			continue
		}
		r.Err, r.Stopped = oc.err, oc.err != nil && ctx.Err() != nil && !errors.Is(oc.err, ErrTaskChanged) && !errors.Is(oc.err, store.ErrTaskInUse)
		mu.Lock()
		finished++
		mu.Unlock()
		if r.Validated {
			fmt.Fprintf(out.Out, "  %s  %s\n", r.Task.Name, st.Status(oc.result.Summary()))
		} else {
			fmt.Fprintf(out.Out, "  %s  %s\n", r.Task.Name, st.Bad(r.Problem()))
		}
	}
	return results
}

// outcome is what a worker reports for task i.
type outcome struct {
	i       int
	result  Validation
	err     error
	started bool
}

// startWorkers starts jobs workers that validate the tasks, a feeder that hands out their indexes until ctx is
// cancelled, and returns the channel of outcomes, closed when every worker is done. mu guards current.
func (v Validating) startWorkers(ctx context.Context, tasks []store.Task, o ValidateOptions, jobs int, mu *sync.Mutex, current map[string]string) <-chan outcome {
	next, done := make(chan int), make(chan outcome)
	var wg sync.WaitGroup
	for range max(1, min(jobs, len(tasks))) { // at least one worker, or no task would start and the feeder would wait forever
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
				result, err := v.Quiet(ctx, t, o, started)
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
	return done
}

// FilterByStatus keeps the tasks whose StatusOf is status.
func FilterByStatus(tasks []store.Task, status string) []store.Task {
	return slices.DeleteFunc(tasks, func(t store.Task) bool { return StatusOf(t) != status })
}
