package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// Load builds the report of a locked experiment from what is stored. home is the user's home folder, which the report
// leaves out of paths; warn gets a line for the runs whose context use could not be recovered (the report holds all
// the same). A cancelled ctx fails it: a recovery cut short may have read less than there is.
func Load(ctx context.Context, p experiment.Project, name, home string, warn io.Writer) (Report, error) {
	stored, err := p.DB.ExperimentByName(ctx, p.ID, name)
	if err != nil {
		return Report{}, err
	}
	if stored.Lock == nil {
		return Report{}, fmt.Errorf("experiment %s has not run yet: agentium experiment run %s", name, name)
	}
	var lock experiment.Lock
	if err := json.Unmarshal(stored.Lock, &lock); err != nil {
		return Report{}, fmt.Errorf("experiment %s: its lock cannot be read: %w", name, err)
	}
	runs, err := p.DB.ExperimentRuns(ctx, stored.ID)
	if err != nil {
		return Report{}, err
	}
	if len(runs) == 0 {
		return Report{}, fmt.Errorf("experiment %s has no runs yet", name)
	}
	in := Input{Name: name, Lock: lock, Status: stored.Status, StatusNote: stored.StatusNote, DataDir: p.Layout.Root, Home: home}
	if stored.Status == store.StatusRunning && !p.Layout.RunsBusy() {
		in.Status, in.StatusNote = store.StatusStopped, "its Agentium process ended; run it again to resume"
	}
	if in.Runs, err = loadRuns(ctx, p, lock, runs, warn); err != nil {
		return Report{}, err
	}
	return Build(in)
}

// loadRuns decodes the stored runs. Runs recorded before Agentium kept their context use get it from their
// transcripts, where those remain. A run whose context use cannot be worked out is reported without it, with a warning.
func loadRuns(ctx context.Context, p experiment.Project, lock experiment.Lock, runs []store.Run, warn io.Writer) ([]Run, error) {
	bases, snapshots := map[string]string{}, map[string]string{}
	for _, t := range lock.Tasks {
		bases[t.Name] = t.Base
	}
	for _, a := range lock.Arms {
		snapshots[a.Name] = a.Snapshot
	}
	recovery := run.Recovery{Bare: p.Bare, Records: p.Layout.Records}
	unrecovered := map[string]int{} // by reason: every run of an arm shares one
	var out []Run
	for _, r := range runs {
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return nil, fmt.Errorf("run %s: %w", r.ID, err)
		}
		if rec.ContextUse == nil && bases[rec.Task] != "" {
			var err error
			if rec.ContextUse, err = recovery.Recover(ctx, rec, bases[rec.Task], snapshots[rec.Arm]); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				unrecovered[err.Error()]++
			}
		}
		out = append(out, Run{ID: r.ID, Slot: r.Slot, Attempt: r.Attempt, Record: rec})
	}
	if ctx.Err() != nil { // a cancelled recovery may have read less than there is: write no report
		return nil, ctx.Err()
	}
	for _, reason := range slices.Sorted(maps.Keys(unrecovered)) {
		fmt.Fprintf(warn, "agentium: context use not recovered for %d run(s): %s\n", unrecovered[reason], reason)
	}
	return out, nil
}

// Output is where and how a report is written.
type Output struct {
	JSON     bool       // the lock, every run and the results
	Markdown bool       // Markdown even on a terminal
	Terminal bool       // stdout is a terminal
	Style    term.Style // for the terminal rendering
	File     string     // write here instead of to stdout
}

// Write renders the report: Markdown is for files, pipes and pull requests; on a terminal (unless asked otherwise) it
// is rendered for reading there, colored as the terminal allows. It is rendered whole first, so a failure leaves no
// partial file. To a file, it then says where it wrote.
func (r Report) Write(stdout io.Writer, name string, o Output) error {
	render := r.Markdown
	switch {
	case o.JSON:
		render = r.JSON
	case o.Terminal && !o.Markdown && o.File == "":
		render = func(w io.Writer) error { return r.Terminal(w, o.Style) }
	}
	var buf bytes.Buffer
	if err := render(&buf); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	if o.File == "" {
		if _, err := buf.WriteTo(stdout); err != nil {
			return fmt.Errorf("report: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(o.File, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	fmt.Fprintf(stdout, "Wrote the report of %s to %s.\n", name, o.File)
	return nil
}
