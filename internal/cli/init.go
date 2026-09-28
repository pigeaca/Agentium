package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/project"
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
	printInit(env, saved, info, layout)
	return ExitOK
}

func printInit(env Env, saved store.Project, info project.Info, layout home.Layout) {
	w := env.Stdout
	fmt.Fprintf(w, "Registered %s (project %d)\n", saved.Name, saved.ID)
	fmt.Fprintf(w, "  repository   %s @ %s\n", info.Root, shortCommit(info.Head))
	claude := "not found"
	if info.Claude.Path != "" {
		claude = strings.TrimSpace(info.Claude.Path + " " + info.Claude.Version)
	}
	fmt.Fprintf(w, "  Claude Code  %s\n", claude)
	fmt.Fprintf(w, "  sign-in      %s\n", describeSignIn(info.Claude.SignIn))
	fmt.Fprintf(w, "  tests        %s\n", orNone(strings.Join(info.TestCommands, "; ")))
	var files []string
	for _, file := range info.Instructions {
		files = append(files, fmt.Sprintf("%s (%s)", file.Path, sizeLabel(file.Bytes)))
	}
	fmt.Fprintf(w, "  context      %s; %d skill(s), %d rule file(s)\n", orNone(strings.Join(files, ", ")), info.Skills, info.Rules)
	fmt.Fprintf(w, "  data         %s (your repository was not modified)\n", layout.Root)
	for _, warning := range info.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warning)
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
