package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/claudectx"
	"github.com/pigeaca/agentium/internal/codex"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/mine"
	"github.com/pigeaca/agentium/internal/project"
	"github.com/pigeaca/agentium/internal/source"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

const initUsage = `Usage: agentium init [path] [--verify CMD]... [--setup CMD]... [--require-lock[=false]] [--jobs N]
                     [--verify-timeout DURATION] [--module PATH] [--allow-local-binding[=false]] [--json]

Registers the git repository containing path (default: the current folder), reports what Agentium found and prints
the project's settings. It only reads the repository; data, settings included, goes to ~/.agentium (or AGENTIUM_HOME),
which must be outside the repository, so no commit can change them. Running init again keeps every setting it is not
given.

Settings (pool update, start, task import, task add and task validate use them):
  --verify CMD              a verification command for the tasks mined and imported (repeatable; default: the
                            detected build tools' test commands for mined tasks, every detected test command for
                            imported ones); --verify '' returns to the default
  --setup CMD               a command a fresh checkout of those tasks runs first (repeatable; default none;
                            --setup '' removes them)
  --require-lock            mining sets aside Python commits whose base pins no dependencies (no uv.lock or fully
                            pinned requirement files); --require-lock=false turns it off
  --jobs N                  how many tasks to validate at once (default 2): above 1 assumes the project's tests can
                            run side by side (no fixed ports, shared /tmp paths or databases)
  --verify-timeout DURATION
                            the time limit of each setup or verification command (default 10m)
  --module PATH             measure one module of a monorepo: a folder of the repository (relative to its root) that
                            holds a build file (go.mod, pom.xml, pyproject.toml, ...). Build tools are detected there
                            and setup and verification commands run in it; --module '' returns to the whole repository.
                            When the root has no build file, init lists the folders that could be modules
  --allow-local-binding     let agent runs on a Gradle project bind local ports and connect to localhost in the
                            sandbox; --allow-local-binding=false turns it off

  --json                    print one JSON document instead of text (docs/guide.md, "Scripting and automation")

Gradle's file-lock service binds a local socket, which the sandbox forbids by default. Allowing it lets the agent bind
any local port and connect to any service listening on localhost (databases, dev servers) on this machine; outbound
network to other hosts stays blocked. Without it, agent runs on a Gradle project refuse to start.
`

// initArgs is what init was asked for: the folder, and the choices to store.
type initArgs struct {
	dir string
	set *settingFlags
	// allow is the local-binding choice, when given (allowGiven).
	allow, allowGiven bool
}

// parseInit reads and checks init's arguments; a mistake is reported and its exit code returned.
func parseInit(env Env, args []string) (a initArgs, code int, ok bool) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.BoolVar(&a.allow, "allow-local-binding", false, "")
	removeFlags(fs, map[string]string{"no-allow-local-binding": "use --allow-local-binding=false"})
	a.set = addSettingFlags(fs, settingVerifyTimeout, settingVerify, settingSetup, settingRequireLock, settingJobs, settingVerifyTimeout, settingModule)
	paths, code, ok := parseArgs(env, fs, args, initUsage)
	if !ok {
		return a, code, false
	}
	if err := a.set.check(); err != nil {
		fmt.Fprintf(env.Stderr, "agentium init: %v\n", err)
		return a, ExitUsage, false
	}
	fs.Visit(func(f *flag.Flag) { a.allowGiven = a.allowGiven || f.Name == "allow-local-binding" })
	if len(paths) > 1 {
		fmt.Fprintf(env.Stderr, "agentium init: expected at most one path, got %d\n\n%s", len(paths), initUsage)
		return a, ExitUsage, false
	}
	a.dir = env.Dir
	if len(paths) == 1 {
		a.dir = paths[0]
	}
	return a, ExitOK, true
}

// storeChoices saves the choices init was given (and only those) and returns the project as stored.
func (a initArgs) storeChoices(ctx context.Context, db *store.Store, saved store.Project) (store.Project, error) {
	if a.allowGiven {
		if err := db.SetLocalBinding(ctx, saved.ID, a.allow); err != nil {
			return saved, err
		}
		saved.AllowLocalBinding = a.allow
	}
	if a.set.any() {
		settings := a.set.apply(saved.Settings)
		if err := db.SetSettings(ctx, saved.ID, settings); err != nil {
			return saved, err
		}
		saved.Settings = settings
	}
	return saved, nil
}

// runInit registers a repository: discovery is read-only, and the result goes to the data folder only.
func runInit(ctx context.Context, env Env, args []string) int {
	a, code, ok := parseInit(env, args)
	if !ok {
		return code
	}
	dir := a.dir
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
	if a.set.given(settingModule) { // checked before anything is created
		if a.set.module, err = project.ValidateModule(ctx, info.Root, a.set.module); err != nil {
			fmt.Fprintf(env.Stderr, "agentium init: %v\n", err)
			return ExitUsage
		}
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
	// The module in force: the one given, else the one stored; discovery then describes its folder (WithModule).
	module := a.set.module
	if !a.set.given(settingModule) {
		if stored, err := db.ProjectByRoot(ctx, info.Root); err == nil {
			module = stored.Settings.Module
		} else if !errors.Is(err, store.ErrNotFound) {
			return fail(env, err)
		}
		if _, err := project.ValidateModule(ctx, info.Root, module); err != nil { // a folder removed or changed since
			info.Warnings = append(info.Warnings, fmt.Sprintf("The stored module no longer works (%v): agentium init --module PATH picks another.", err))
		}
	}
	info = info.WithModule(module)
	discovery, err := info.JSON()
	if err != nil {
		return fail(env, fmt.Errorf("encode discovery: %w", err))
	}
	saved, err := db.SaveProject(ctx, info.Root, filepath.Base(info.Root), discovery, env.Now())
	if err != nil {
		return fail(env, err)
	}
	if saved, err = a.storeChoices(ctx, db, saved); err != nil {
		return fail(env, err)
	}
	src, err := source.WorkingTree(ctx, info.Root)
	if err != nil {
		return fail(env, err)
	}
	// The context a session in the module loads, where the agent of the module's tasks starts.
	resolved, err := claudectx.ResolveIn(src, saved.Settings.Module)
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
	Codex        codexDoc        `json:"codex"`
	SignIn       string          `json:"sign_in"` // api-key | token-file | login (presence only, never the secret)
	TestCommands []string        `json:"test_commands"`
	Context      contextSizeDoc  `json:"context"`
	LocalBinding localBindingDoc `json:"local_binding"`
	Settings     settingsDoc     `json:"settings"`
	// Modules lists the candidate modules when the repository's root has no build file (absent otherwise);
	// ModulesTotal counts them all, Modules holding at most project.MaxModules.
	Modules      []moduleDoc `json:"modules,omitempty"`
	ModulesTotal int         `json:"modules_total,omitempty"`
	Warnings     []string    `json:"warnings"`
}

// moduleDoc is a candidate module: its folder (relative to the root) and the build tools its files name.
type moduleDoc struct {
	Path  string   `json:"path"`
	Tools []string `json:"tools"`
}

// settingsDoc is the project's settings as commands use them: the stored verify and setup commands (empty: not set),
// the verify commands mined tasks get now, the effective jobs and verify timeout, and which settings are at their
// built-in default (verify, setup, require_lock, jobs, verify_timeout).
type settingsDoc struct {
	Verify               []string `json:"verify"`
	MinedVerify          []string `json:"mined_verify"`
	Setup                []string `json:"setup"`
	RequireLock          bool     `json:"require_lock"`
	Jobs                 int      `json:"jobs"`
	VerifyTimeoutSeconds float64  `json:"verify_timeout_seconds"`
	AllowLocalBinding    bool     `json:"allow_local_binding"`
	Defaults             []string `json:"defaults"`
	// Module is the monorepo module the project measures; absent for the repository's root.
	Module string `json:"module,omitempty"`
}

// projectSettings is what init shows of a project's settings: each one's value as commands use it, and whether it is
// the built-in default.
type projectSettings struct {
	stored       store.Settings
	minedVerify  []string // the verify commands mined tasks get: the setting, else the build tools', else the detected ones
	importVerify []string // those imported tasks get: the setting, else every detected test command
}

func settingsOf(saved store.Project, info project.Info) projectSettings {
	p := projectSettings{stored: saved.Settings, minedVerify: saved.Settings.Verify, importVerify: saved.Settings.Verify}
	if len(p.minedVerify) == 0 {
		// The build tools of the module's folder (the root's without a module), as mining detects them; info's test
		// commands are that folder's too (WithModule).
		if _, p.minedVerify = mine.TestLanguages(moduleDir(info.Root, saved.Settings.Module)); len(p.minedVerify) == 0 {
			p.minedVerify = info.TestCommands
		}
		p.importVerify = info.TestCommands
	}
	return p
}

// defaults names the settings left at their built-in default.
func (p projectSettings) defaults() []string {
	s := p.stored
	var names []string
	for _, d := range []struct {
		name  string
		unset bool
	}{{"verify", len(s.Verify) == 0}, {"setup", len(s.Setup) == 0}, {"require_lock", !s.RequireLock}, {"jobs", s.Jobs == 0},
		{"verify_timeout", s.VerifyTimeout == 0}} {
		if d.unset {
			names = append(names, d.name)
		}
	}
	return names
}

func (p projectSettings) document(allowLocalBinding bool) settingsDoc {
	return settingsDoc{Verify: list(p.stored.Verify), MinedVerify: list(p.minedVerify), Setup: list(p.stored.Setup), RequireLock: p.stored.RequireLock,
		Jobs: jobsOf(p.stored), VerifyTimeoutSeconds: verifyTimeoutOf(p.stored).Seconds(), AllowLocalBinding: allowLocalBinding, Defaults: list(p.defaults()),
		Module: p.stored.Module}
}

// print shows the settings under a heading that says how to change them.
func (p projectSettings) print(env Env, saved store.Project, gradle bool) {
	w, st, s := env.Stdout, env.style(), p.stored
	fmt.Fprintf(w, "%s (in the data folder; %s changes one, the others stay)\n", st.Heading("Settings"), st.Command("agentium init --FLAG VALUE"))
	label := func(set bool, text string) string {
		if set {
			return text
		}
		return text + st.Note(" (default)")
	}
	none := func(commands []string) string {
		if len(commands) == 0 {
			return "none"
		}
		return strings.Join(commands, "; ")
	}
	var verify string
	switch {
	case len(s.Verify) > 0:
		verify = strings.Join(s.Verify, "; ")
	case len(p.minedVerify) == 0 && len(p.importVerify) == 0:
		verify = "not set, and no test command was detected: " + st.Command("agentium init --verify CMD") + " sets one"
	default:
		verify = "not set: mined tasks verify with " + none(p.minedVerify)
		if !slices.Equal(p.minedVerify, p.importVerify) {
			verify += ", imported ones with " + none(p.importVerify)
		}
	}
	var rows [][2]string
	if s.Module != "" { // a project measured whole needs no word about modules
		rows = append(rows, [2]string{"module", s.Module + st.Note(" (new tasks belong to this folder: they are mined there, and their agents start, build and verify there; --module '' returns to the whole repository)")})
	}
	rows = append(rows, [][2]string{
		{"verify", verify},
		{"setup", label(len(s.Setup) > 0, none(s.Setup))},
		{"require lock", label(s.RequireLock, onOff(s.RequireLock))},
		{"jobs", label(s.Jobs > 0, strconv.Itoa(jobsOf(s)))},
		{"verify timeout", label(s.VerifyTimeout > 0, durationLabel(verifyTimeoutOf(s)))},
	}...)
	if gradle || saved.AllowLocalBinding { // a project without Gradle needs no word about it
		rows = append(rows, [2]string{"local ports", localBindingLine(saved.AllowLocalBinding, gradle)})
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %-15s %s\n", r[0], r[1])
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// durationLabel writes d without zero units: 10m, 1h30m, 45s.
func durationLabel(d time.Duration) string {
	text := d.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

type projectDoc struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// codexDoc is the optional Codex CLI: found, its version, and the sign-in a Codex run would use (presence only).
type codexDoc struct {
	Found   bool   `json:"found"`
	Version string `json:"version"`
	SignIn  string `json:"sign_in"` // api-key | login
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
		Codex:        codexDoc{Found: info.Codex.Path != "", Version: info.Codex.Version, SignIn: info.Codex.SignIn},
		TestCommands: list(info.TestCommands), Context: contextSize(resolved),
		LocalBinding: localBindingDoc{Allowed: saved.AllowLocalBinding, Gradle: slices.Contains(buildtool.DetectIn(moduleDir(info.Root, saved.Settings.Module)), "gradle")},
		Settings:     settingsOf(saved, info).document(saved.AllowLocalBinding), Modules: moduleDocs(info.Modules), ModulesTotal: info.ModulesTotal,
		Warnings: list(info.Warnings)}
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
	fmt.Fprintf(w, "  Codex        %s\n", describeCodex(st, info.Codex, layout))
	fmt.Fprintf(w, "  tests        %s\n", term.OrNone(strings.Join(info.TestCommands, "; ")))
	startup := 0
	for _, e := range resolved.Entries {
		if e.StartupBytes > 0 {
			startup++
		}
	}
	fmt.Fprintf(w, "  context      about %d tokens at session start (estimated) from %d file(s); %d on demand; details: %s\n",
		claudectx.EstimateTokens(resolved.StartupBytes()), startup, len(resolved.Entries)-startup, st.Command("agentium context show"))
	fmt.Fprintf(w, "  data         %s (your repository was not modified)\n", layout.Root)
	settingsOf(saved, info).print(env, saved, slices.Contains(buildtool.DetectIn(moduleDir(info.Root, saved.Settings.Module)), "gradle"))
	printModules(env, saved.Settings.Module, info)
	for _, text := range info.Warnings {
		fmt.Fprintln(w, warning(st, text))
	}
}

func moduleDocs(modules []project.Module) []moduleDoc {
	var docs []moduleDoc
	for _, m := range modules {
		docs = append(docs, moduleDoc{Path: m.Path, Tools: list(m.Tools)})
	}
	return docs
}

// printModules lists the candidate modules of a repository whose root has no build file, marking the chosen one.
func printModules(env Env, chosen string, info project.Info) {
	if len(info.Modules) == 0 {
		return
	}
	w, st := env.Stdout, env.style()
	fmt.Fprintf(w, "%s (the root has no build file; %s measures one)\n", st.Heading("Modules"), st.Command("agentium init --module PATH"))
	width := 0
	for _, m := range info.Modules {
		width = max(width, len(m.Path))
	}
	for _, m := range info.Modules {
		mark := ""
		if m.Path == chosen {
			mark = "  " + st.Note("(chosen)")
		}
		fmt.Fprintf(w, "  %-*s  %s%s\n", width, m.Path, strings.Join(m.Tools, ", "), mark)
	}
	if more := info.ModulesTotal - len(info.Modules); more > 0 {
		fmt.Fprintf(w, "  and %d more\n", more)
	}
}

// localBindingLine says what the project's setting means for agent runs.
func localBindingLine(allowed, gradle bool) string {
	switch {
	case allowed:
		return "agents may bind local ports and connect to localhost (--allow-local-binding=false turns it off)"
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

// describeCodex says whether the optional Codex CLI was found, its version, and how a Codex run would sign in.
func describeCodex(st term.Style, c project.CodexInfo, layout home.Layout) string {
	if c.Path == "" {
		return st.Note("not found (optional: runs with --agent codex need it)")
	}
	found := strings.TrimSpace(c.Path + " " + c.Version)
	if c.SignIn == codex.SignInAPIKey {
		return found + st.Note(" (optional; sign-in: the API key in CODEX_API_KEY or OPENAI_API_KEY, given to Codex alone)")
	}
	return found + st.Note(fmt.Sprintf(" (optional; sign-in: the ChatGPT login in %s, checked when a run starts: CODEX_HOME=%s codex login)", layout.CodexHome(), layout.CodexHome()))
}

func sizeLabel(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
}
