package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/store"
)

const initUsage = `Usage: agentium init [path]

Registers the git repository containing path (default: the current folder) and reports what Agentium found. It only
reads the repository; data goes to ~/.agentium (or AGENTIUM_HOME), which must be outside the repository.
`

// runInit registers a repository: discovery is read-only, and the result goes to the data folder only.
func runInit(ctx context.Context, env Env, args []string) int {
	paths, code, ok := parseArgs(env, flag.NewFlagSet("init", flag.ContinueOnError), args, initUsage)
	if !ok {
		return code
	}
	if len(paths) > 1 {
		fmt.Fprintf(env.Stderr, "agentium init: expected at most one path, got %d\n\n%s", len(paths), initUsage)
		return ExitUsage
	}
	dir := env.Dir
	if len(paths) == 1 {
		dir = paths[0]
	}
	if !filepath.IsAbs(dir) {
		if env.Dir == "" {
			return fail(env, errors.New("the current folder cannot be read: pass an absolute path"))
		}
		dir = filepath.Join(env.Dir, dir)
	}
	info, err := project.Discover(ctx, dir, project.Env{Getenv: env.Getenv, LookPath: env.LookPath})
	if err != nil {
		return fail(env, err)
	}
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
		return fail(env, err)
	}
	if err := layout.CheckOutside(info.Root); err != nil { // before anything is created
		return fail(env, err)
	}
	if err := layout.Ensure(); err != nil {
		return fail(env, err)
	}
	db, err := store.Open(ctx, layout.Database)
	if err != nil {
		return fail(env, err)
	}
	defer db.Close()
	discovery, err := info.JSON()
	if err != nil {
		return fail(env, fmt.Errorf("encode discovery: %w", err))
	}
	saved, err := db.SaveProject(ctx, info.Root, filepath.Base(info.Root), discovery, env.Now())
	if err != nil {
		return fail(env, err)
	}
	src, err := source.WorkingTree(ctx, info.Root)
	if err != nil {
		return fail(env, err)
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return fail(env, err)
	}
	printInit(env, saved, info, layout, resolved)
	return ExitOK
}

func printInit(env Env, saved store.Project, info project.Info, layout home.Layout, resolved claudectx.Context) {
	w, st := env.Stdout, env.style()
	fmt.Fprintln(w, st.Heading(fmt.Sprintf("Registered %s (project %d)", saved.Name, saved.ID)))
	fmt.Fprintf(w, "  repository   %s @ %s\n", info.Root, shortCommit(info.Head))
	claude := st.Bad("not found")
	if info.Claude.Path != "" {
		claude = strings.TrimSpace(info.Claude.Path + " " + info.Claude.Version)
	}
	fmt.Fprintf(w, "  Claude Code  %s\n", claude)
	fmt.Fprintf(w, "  sign-in      %s\n", describeSignIn(info.Claude.SignIn))
	fmt.Fprintf(w, "  tests        %s\n", orNone(strings.Join(info.TestCommands, "; ")))
	startup := 0
	for _, e := range resolved.Entries {
		if e.StartupBytes > 0 {
			startup++
		}
	}
	fmt.Fprintf(w, "  context      about %d tokens at session start (estimated) from %d file(s); %d on demand; details: %s\n",
		claudectx.EstimateTokens(resolved.StartupBytes()), startup, len(resolved.Entries)-startup, st.Command("agentium context show"))
	fmt.Fprintf(w, "  data         %s (your repository was not modified)\n", layout.Root)
	for _, text := range info.Warnings {
		fmt.Fprintln(w, warning(st, text))
	}
}

func describeSignIn(mode string) string {
	switch mode {
	case project.SignInAPIKey:
		return "API key from ANTHROPIC_API_KEY (each run gets a fresh config folder)"
	case project.SignInTokenFile:
		return "token file from `claude setup-token` (each run gets a fresh config folder)"
	default:
		return "your Claude login, restricted to project settings (checked on the first run)"
	}
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func sizeLabel(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
}

func orNone(value string) string {
	if value == "" {
		return "none found"
	}
	return value
}
