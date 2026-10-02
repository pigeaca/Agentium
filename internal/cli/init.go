package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

const initUsage = `Usage: agentium init [path] [--json]

Registers the git repository containing path (default: the current folder) and reports what Agentium found. It only
reads the repository; data goes to ~/.agentium (or AGENTIUM_HOME), which must be outside the repository.

  --json                    print one JSON document instead of text (docs/guide.md, "Scripting and automation")
  --allow-local-binding     let agent runs on a Gradle project bind local ports and connect to localhost in the sandbox
  --no-allow-local-binding  turn that off again

Gradle's file-lock service binds a local socket, which the sandbox forbids by default. Allowing it lets the agent bind
any local port and connect to any service listening on localhost (databases, dev servers) on this machine; outbound
network to other hosts stays blocked. Without it, agent runs on a Gradle project refuse to start. Running init again
without either flag keeps the stored choice.
`

// runInit registers a repository: discovery is read-only, and the result goes to the data folder only.
func runInit(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	allow, deny := fs.Bool("allow-local-binding", false, ""), fs.Bool("no-allow-local-binding", false, "")
	paths, code, ok := parseArgs(env, fs, args, initUsage)
	if !ok {
		return code
	}
	if *allow && *deny {
		fmt.Fprintf(env.Stderr, "agentium init: --allow-local-binding and --no-allow-local-binding exclude each other\n\n%s", initUsage)
		return ExitUsage
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
	env.noteRoot(info.Root)
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
	if *allow || *deny {
		if err := db.SetLocalBinding(ctx, saved.ID, *allow); err != nil {
			return fail(env, err)
		}
		saved.AllowLocalBinding = *allow
	}
	src, err := source.WorkingTree(ctx, info.Root)
	if err != nil {
		return fail(env, err)
	}
	resolved, err := claudectx.Resolve(src)
	if err != nil {
		return fail(env, err)
	}
	if env.JSON {
		doc := initDocument(saved, info, resolved)
		doc.Warnings = env.redactAll(doc.Warnings)
		return env.emit(doc)
	}
	printInit(env, saved, info, layout, resolved)
	return ExitOK
}

// initDoc is init's --json document. It names no path: not the repository's, the data folder's or Claude Code's.
type initDoc struct {
	header
	Project      projectDoc      `json:"project"`
	Head         string          `json:"head"`
	ClaudeCode   claudeCodeDoc   `json:"claude_code"`
	SignIn       string          `json:"sign_in"` // api-key | token-file | login (presence only, never the secret)
	TestCommands []string        `json:"test_commands"`
	Context      contextSizeDoc  `json:"context"`
	LocalBinding localBindingDoc `json:"local_binding"`
	Warnings     []string        `json:"warnings"`
}

type projectDoc struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type claudeCodeDoc struct {
	Found   bool   `json:"found"`
	Version string `json:"version"`
}

// contextSizeDoc is what a context loads: tokens are estimated (claudectx.EstimateTokens).
type contextSizeDoc struct {
	StartupTokens int `json:"startup_tokens_estimated"`
	StartupFiles  int `json:"startup_files"`
	OnDemandFiles int `json:"on_demand_files"`
}

type localBindingDoc struct {
	Allowed bool `json:"allowed"`
	Gradle  bool `json:"gradle_project"`
}

func contextSize(resolved claudectx.Context) contextSizeDoc {
	startup := 0
	for _, e := range resolved.Entries {
		if e.StartupBytes > 0 {
			startup++
		}
	}
	return contextSizeDoc{StartupTokens: claudectx.EstimateTokens(resolved.StartupBytes()), StartupFiles: startup, OnDemandFiles: len(resolved.Entries) - startup}
}

func initDocument(saved store.Project, info project.Info, resolved claudectx.Context) initDoc {
	return initDoc{header: hdr("init"), Project: projectDoc{ID: saved.ID, Name: saved.Name}, Head: info.Head,
		ClaudeCode: claudeCodeDoc{Found: info.Claude.Path != "", Version: info.Claude.Version}, SignIn: info.Claude.SignIn,
		TestCommands: list(info.TestCommands), Context: contextSize(resolved),
		LocalBinding: localBindingDoc{Allowed: saved.AllowLocalBinding, Gradle: slices.Contains(buildtool.DetectIn(info.Root), "gradle")},
		Warnings:     list(info.Warnings)}
}

func printInit(env Env, saved store.Project, info project.Info, layout home.Layout, resolved claudectx.Context) {
	w, st := env.Stdout, env.style()
	fmt.Fprintln(w, st.Heading(fmt.Sprintf("Registered %s (project %d)", saved.Name, saved.ID)))
	fmt.Fprintf(w, "  repository   %s @ %s\n", info.Root, experiment.ShortCommit(info.Head))
	claude := st.Bad("not found")
	if info.Claude.Path != "" {
		claude = strings.TrimSpace(info.Claude.Path + " " + info.Claude.Version)
	}
	fmt.Fprintf(w, "  Claude Code  %s\n", claude)
	fmt.Fprintf(w, "  sign-in      %s\n", describeSignIn(info.Claude.SignIn))
	fmt.Fprintf(w, "  tests        %s\n", term.OrNone(strings.Join(info.TestCommands, "; ")))
	startup := 0
	for _, e := range resolved.Entries {
		if e.StartupBytes > 0 {
			startup++
		}
	}
	fmt.Fprintf(w, "  context      about %d tokens at session start (estimated) from %d file(s); %d on demand; details: %s\n",
		claudectx.EstimateTokens(resolved.StartupBytes()), startup, len(resolved.Entries)-startup, st.Command("agentium context show"))
	if gradle := slices.Contains(buildtool.DetectIn(info.Root), "gradle"); gradle || saved.AllowLocalBinding {
		fmt.Fprintf(w, "  local ports  %s\n", localBindingLine(saved.AllowLocalBinding, gradle))
	}
	fmt.Fprintf(w, "  data         %s (your repository was not modified)\n", layout.Root)
	for _, text := range info.Warnings {
		fmt.Fprintln(w, warning(st, text))
	}
}

// localBindingLine says what the project's setting means for agent runs.
func localBindingLine(allowed, gradle bool) string {
	switch {
	case allowed:
		return "agents may bind local ports and connect to localhost (--no-allow-local-binding turns it off)"
	case gradle:
		return "off: agent runs on this Gradle project refuse to start until you allow it with `agentium init --allow-local-binding` (it lets the agent bind any local port and reach localhost services)"
	}
	return "off"
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

func sizeLabel(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
}
