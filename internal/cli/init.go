package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/store"
)

// runInit registers a repository: discovery is read-only, and the result goes to the data folder only.
func runInit(ctx context.Context, env Env, args []string) int {
	if len(args) > 1 {
		fmt.Fprintf(env.Stderr, "agentium init: expected at most one path, got %d\n", len(args))
		return ExitUsage
	}
	dir := env.Dir
	if len(args) == 1 {
		dir = args[0]
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(env.Dir, dir)
		}
	}
	info, err := project.Discover(ctx, dir, project.Env{Getenv: env.Getenv, LookPath: env.LookPath})
	if err != nil {
		return fail(env, err)
	}
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
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
