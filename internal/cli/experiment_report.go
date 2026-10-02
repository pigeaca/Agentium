package cli

import (
	"context"
	"flag"

	"github.com/pigeaca/agentium/internal/report"
)

func experimentReport(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("experiment report", flag.ContinueOnError)
	var o report.Output
	fs.BoolVar(&o.JSON, "json", false, "write JSON (the lock, every run and the results) instead of Markdown")
	fs.BoolVar(&o.Markdown, "markdown", false, "write Markdown even on a terminal")
	fs.StringVar(&o.File, "out", "", "write to this file instead of the terminal")
	rest, code, ok := parseArgs(env, fs, args, experimentUsage)
	if !ok {
		return code
	}
	if _, ok := oneName(env, "experiment report", rest, experimentUsage); !ok {
		return ExitUsage
	}
	w, err := openProject(ctx, env)
	if err != nil {
		return fail(env, err)
	}
	defer w.Close()
	rep, err := report.Load(ctx, w.service(), rest[0], env.Getenv("HOME"), env.Stderr)
	if err != nil {
		return fail(env, err)
	}
	o.Terminal, o.Style = env.Terminal, env.style()
	if err := rep.Write(env.Stdout, rest[0], o); err != nil {
		return fail(env, err)
	}
	return ExitOK
}
