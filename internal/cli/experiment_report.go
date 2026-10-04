package cli

import (
	"context"
	"flag"
	"io"
	"strings"

	"github.com/pigeaca/agentium/internal/report"
	"github.com/pigeaca/agentium/internal/term"
)

func experimentReport(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("experiment report", flag.ContinueOnError)
	var o report.Output
	fs.BoolVar(&o.JSON, "json", false, "write JSON (the lock, every run and the results) instead of Markdown")
	fs.BoolVar(&o.Markdown, "markdown", false, "write Markdown even on a terminal")
	fs.StringVar(&o.File, "out", "", "write to this file instead of the terminal")
	details := fs.Bool("details", false, "on a terminal, show every number (intervals, levels, noise) instead of the answer in plain words")
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
	// On a terminal that shows the designed console, the report is the answer in plain words, unless --details asks
	// for every number; everything else (a pipe, --markdown, --out, --json, NO_COLOR, TERM=dumb, a narrow terminal)
	// is written as before.
	if caps := term.DetectCapabilities(env.Terminal, env.Getenv, envSize(env)); !*details && !o.JSON && !o.Markdown && o.File == "" &&
		!env.JSON && !env.Plain && caps.Designed() {
		if _, err := io.WriteString(env.Stdout, strings.Join(reportView(rep, caps.Shapes(), caps.Width), "\n")+"\n"); err != nil {
			return fail(env, err)
		}
		return ExitOK
	}
	if err := rep.Write(env.Stdout, rest[0], o); err != nil {
		return fail(env, err)
	}
	return ExitOK
}
