package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
)

func experimentReport(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("experiment report", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "write JSON (the lock, every run and the results) instead of Markdown")
	out := fs.String("out", "", "write to this file instead of the terminal")
	rest, code, ok := parseArgs(env, fs, args, experimentUsage)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		fmt.Fprint(env.Stderr, experimentUsage)
		return ExitUsage
	}
	name := rest[0]
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	stored, err := w.db.ExperimentByName(ctx, w.project.ID, name)
	if err != nil {
		return fail(env, err)
	}
	if stored.Lock == nil {
		return fail(env, fmt.Errorf("experiment %s has not run yet: agentium experiment run %s", name, name))
	}
	var lock experiment.Lock
	if err := json.Unmarshal(stored.Lock, &lock); err != nil {
		return fail(env, fmt.Errorf("experiment %s: its lock cannot be read: %w", name, err))
	}
	runs, err := w.db.ExperimentRuns(ctx, stored.ID)
	if err != nil {
		return fail(env, err)
	}
	if len(runs) == 0 {
		return fail(env, fmt.Errorf("experiment %s has no runs yet", name))
	}
	in := report.Input{Name: name, Lock: lock, Status: stored.Status, StatusNote: stored.StatusNote}
	if stored.Status == store.StatusRunning && !w.layout.RunsBusy() {
		in.Status, in.StatusNote = store.StatusStopped, "its Agentium process ended; run it again to resume"
	}
	for _, r := range runs {
		var rec run.Record
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			return fail(env, fmt.Errorf("run %s: %w", r.ID, err))
		}
		in.Runs = append(in.Runs, report.Run{ID: r.ID, Slot: r.Slot, Attempt: r.Attempt, Record: rec})
	}
	rep, err := report.Build(in)
	if err != nil {
		return fail(env, err)
	}
	var dst io.Writer = env.Stdout
	var file *os.File
	if *out != "" {
		if file, err = os.Create(*out); err != nil {
			return fail(env, fmt.Errorf("report: %w", err))
		}
		dst = file
	}
	render := rep.Markdown
	if *asJSON {
		render = rep.JSON
	}
	err = render(dst)
	if file != nil {
		err = errors.Join(err, file.Close())
	}
	if err != nil {
		return fail(env, fmt.Errorf("report: %w", err))
	}
	if file != nil {
		fmt.Fprintf(env.Stdout, "Wrote the report of %s to %s.\n", name, *out)
	}
	return ExitOK
}
