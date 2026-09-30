package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	markdown := fs.Bool("markdown", false, "write Markdown even on a terminal")
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
	in := report.Input{Name: name, Lock: lock, Status: stored.Status, StatusNote: stored.StatusNote, DataDir: w.layout.Root, Home: env.Getenv("HOME")}
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
	// Markdown is for files, pipes and pull requests; on a terminal (unless asked otherwise) the report is
	// rendered for reading there, colored as the terminal allows.
	render := rep.Markdown
	switch {
	case *asJSON:
		render = rep.JSON
	case env.Terminal && !*markdown && *out == "":
		st := env.style()
		render = func(w io.Writer) error { return rep.Terminal(w, st) }
	}
	var buf bytes.Buffer // rendered whole first, so a failure leaves no partial file
	if err := render(&buf); err != nil {
		return fail(env, fmt.Errorf("report: %w", err))
	}
	if *out == "" {
		if _, err := buf.WriteTo(env.Stdout); err != nil {
			return fail(env, fmt.Errorf("report: %w", err))
		}
		return ExitOK
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		return fail(env, fmt.Errorf("report: %w", err))
	}
	fmt.Fprintf(env.Stdout, "Wrote the report of %s to %s.\n", name, *out)
	return ExitOK
}
