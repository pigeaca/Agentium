package codex

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/pricing"
	"github.com/pigeaca/agentium/internal/sandbox"
)

// Profile is the name of the permission profile every run defines and uses (-c default_permissions).
const Profile = "agentium"

// LastMessage is the file in a run's records where Codex writes its final message (-o).
const LastMessage = "last-message.txt"

// The run's own folders under Invocation.State: Codex's run-local HOME (so the user's ~/.agents skills do not load),
// its SQLite state (sqlite_home: threads, logs, memories, goals) and its log folder (log_dir).
const (
	stateHome   = "home"
	stateSQLite = "sqlite"
	stateLogs   = "log"
)

// GradleRefusal is why a Codex run may not start on a project that builds with Gradle (the plan's decision 5): Gradle's
// file-lock service needs a local socket, and Codex's only way to allow one, its experimental network proxy, also lets
// DNS out to any resolver.
var GradleRefusal = errors.New("this project builds with Gradle, whose file-lock service needs a local socket; Codex can allow one only through its network proxy, which also lets DNS out to any host, so Codex runs refuse Gradle projects")

// ToolsRefusal refuses a run whose build tools Codex cannot be given safely (GradleRefusal), whatever the project's
// local-binding opt-in.
func ToolsRefusal(tools []string) error {
	if buildtool.LocalBinding(buildtool.Select(tools)) {
		return GradleRefusal
	}
	return nil
}

// Effort is the effort a run passes: the one given, else the model's default (Codex records the effort only when it is
// passed, so it always is). A model Agentium does not know needs one given.
func Effort(model, effort string) (string, error) {
	if effort != "" {
		return effort, nil
	}
	if m, ok := LookupModel(model); ok {
		return m.DefaultEffort, nil
	}
	return "", fmt.Errorf("Codex model %s has no default effort Agentium knows: give one, as --model %s:EFFORT", model, model)
}

// Allowance is what the cost cap holds back for one more request (the plan's decision 7: one full-context request,
// conservative): the model's whole context window as uncached input, plus its largest output. A request in flight when
// the run is stopped is never recorded, so the cap stops a run while one more such request still fits under it.
func Allowance(model string) (usd float64, ok bool) {
	m, known := LookupModel(model)
	rates, priced := pricing.OpenAILookup(model)
	if !known || !priced {
		return 0, false
	}
	cost, ok := rates.Cost(pricing.OpenAIUsage{Input: m.ContextWindow, Output: m.MaxOutput})
	return cost, ok
}

// CapRefusal refuses a cap that cannot be kept: an unpriced model (no request could be priced), or a cap no larger than
// the allowance (the run would be stopped before its first request).
func CapRefusal(model string, capUSD float64) error {
	if capUSD <= 0 {
		return nil
	}
	allowance, ok := Allowance(model)
	if !ok {
		return fmt.Errorf("Codex model %s has no list price in Agentium's table (%s), so its cost cap cannot be kept: Codex reports no cost", model, pricing.OpenAIDate)
	}
	if capUSD <= allowance {
		return fmt.Errorf("a cost cap of $%.2f is too small for Codex on %s: Agentium stops a run while one more full-context request (up to $%.2f) still fits under the cap", capUSD, model, allowance)
	}
	return nil
}

// checkout is the run's whole checkout: Repo, else Dir.
func checkout(inv agent.Invocation) string {
	if inv.Repo != "" {
		return inv.Repo
	}
	return inv.Dir
}

// check refuses an invocation Codex cannot run safely.
func check(inv agent.Invocation) error {
	switch inv.SignIn {
	case SignInLogin:
		if inv.Secret != "" {
			return errors.New("Codex's ChatGPT sign-in takes no secret")
		}
	case SignInAPIKey:
		if inv.Secret == "" {
			return errors.New("Codex's API key sign-in needs the key")
		}
	default:
		return fmt.Errorf("unknown Codex sign-in mode %q", inv.SignIn)
	}
	if inv.CLI == "" || inv.Dir == "" || inv.Prompt == "" || inv.Model == "" || inv.Home == "" || inv.ConfigDir == "" ||
		inv.State == "" || inv.Records == "" || inv.TempRoot == "" {
		return errors.New("a Codex run needs the CLI, a folder, a prompt, a model, the home folder, its Codex home, state and records folders, and a temp root")
	}
	for _, p := range append([]string{inv.Dir, inv.Home, inv.ConfigDir, inv.State, inv.Records, inv.TempRoot, inv.TokenFile, inv.BuildCache,
		inv.Deps, inv.JavaHome, inv.Venv, inv.ProjectMetadata, inv.AccountHome, inv.Repo, inv.Marker}, inv.Deny...) {
		if p != "" && !filepath.IsAbs(p) {
			return fmt.Errorf("path %q is not absolute", p)
		}
	}
	if inv.Repo != "" {
		if rel, err := filepath.Rel(inv.Repo, inv.Dir); err != nil || !filepath.IsLocal(rel) && rel != "." {
			return fmt.Errorf("the run's folder %q is not inside its checkout %q", inv.Dir, inv.Repo)
		}
	}
	// The metadata folder is on the agent's PYTHONPATH: only inside the deps folder, which the profile keeps read-only,
	// can the agent not plant code there.
	if inv.ProjectMetadata != "" {
		if rel, err := filepath.Rel(inv.Deps, inv.ProjectMetadata); inv.Deps == "" || err != nil || !filepath.IsLocal(rel) || rel == "." {
			return fmt.Errorf("the project's metadata %q is not inside the deps folder", inv.ProjectMetadata)
		}
	}
	if err := ToolsRefusal(inv.Tools); err != nil {
		return err
	}
	return CapRefusal(inv.Model, inv.BudgetUSD)
}

// Command is the run's `codex exec` (see the package documentation): its arguments, environment, prompt on stdin, the
// run-local folders, the login's lock and the cost cap's watcher.
func (Adapter) Command(inv agent.Invocation, environ []string) (agent.Command, error) {
	if err := check(inv); err != nil {
		return agent.Command{}, err
	}
	effort, err := Effort(inv.Model, inv.Effort)
	if err != nil {
		return agent.Command{}, err
	}
	overrides, err := configOverrides(inv, environ, effort)
	if err != nil {
		return agent.Command{}, err
	}
	args := []string{"exec", "--json", "-m", inv.Model, "--ignore-user-config"}
	for _, o := range overrides {
		args = append(args, "-c", o)
	}
	args = append(args, "--disable", "unbounded_connection_retries", // a lost network ends the turn (turn.failed), never waits for ever
		"--ignore-rules",                                                  // no execpolicy rules, the project's or a Codex home's: ProjectConfigRefusal refuses a checkout's too
		"-C", inv.Dir, "-o", filepath.Join(inv.Records, LastMessage), "-") // "-": the prompt is on stdin
	cmd := agent.Command{Args: args, Env: environment(inv, environ), Stdin: inv.Prompt,
		Dirs: []string{inv.State, filepath.Join(inv.State, stateHome), filepath.Join(inv.State, stateSQLite), filepath.Join(inv.State, stateLogs),
			zshDotDir(inv.TempRoot)}}
	if inv.SignIn == SignInAPIKey {
		cmd.Dirs = append(cmd.Dirs, inv.ConfigDir) // a fresh Codex home of the run's own
	} else {
		// One ChatGPT login for every run: token refreshes write it back, and two at once could sign Agentium out (the
		// plan's decision 6), so login-mode runs take turns. The lock lies beside the home, not in it.
		cmd.Exclusive = loginLock(inv.ConfigDir)
	}
	if inv.BudgetUSD > 0 {
		rates, _ := pricing.OpenAILookup(inv.Model) // CapRefusal: priced
		allowance, _ := Allowance(inv.Model)
		cmd.Watch = watcher{transcript: filepath.Join(inv.Records, agent.Transcript), codexHome: inv.ConfigDir, rates: rates,
			capUSD: inv.BudgetUSD, allowanceUSD: allowance}.watch
	}
	return cmd, nil
}

// environment is Codex's own environment: the shared allowlist (sandbox.EnvironFor), the build tools' variables and
// caches, a run-local HOME, the run's temp root as TMPDIR (and zsh's TMPPREFIX under it, where zsh writes heredocs),
// CODEX_HOME and, with an API key, CODEX_API_KEY: the only credential, which the agent's shells never get
// (shell_environment_policy).
func environment(inv agent.Invocation, environ []string) []string {
	profiles := buildtool.SelectRun(inv.Tools, inv.AgentTools)
	allowed := sandbox.EnvironFor(environ, profiles)
	toolEnv := buildtool.AgentEnv(profiles, buildtool.AgentContext{Allowed: allowed, Environ: environ, Home: inv.Home, Repo: checkout(inv),
		BuildCache: inv.BuildCache, Deps: inv.Deps, JavaHome: inv.JavaHome, Venv: inv.Venv, Metadata: inv.ProjectMetadata, ImportRoot: inv.ImportRoot})
	replaced := map[string]bool{"HOME": true, "TMPDIR": true, "TMPPREFIX": true}
	for _, kv := range toolEnv {
		name, _, _ := strings.Cut(kv, "=")
		replaced[name] = true
	}
	if inv.BuildCache != "" {
		for _, name := range buildtool.AgentCacheNames(profiles) {
			replaced[name] = true
		}
	}
	var env []string
	for _, kv := range allowed {
		if name, _, _ := strings.Cut(kv, "="); !replaced[name] {
			env = append(env, kv)
		}
	}
	env = append(env, toolEnv...)
	if inv.BuildCache != "" {
		env = append(env, buildtool.AgentCacheEnv(profiles, inv.BuildCache)...)
	}
	env = append(env, "HOME="+filepath.Join(inv.State, stateHome), "TMPDIR="+inv.TempRoot, "TMPPREFIX="+zshPrefix(inv.TempRoot),
		"CODEX_HOME="+inv.ConfigDir)
	if inv.SignIn == SignInAPIKey {
		env = append(env, "CODEX_API_KEY="+inv.Secret)
	}
	return env
}

// zshPrefix is zsh's TMPPREFIX in the run's temp root: zsh writes heredocs to $TMPPREFIX<random>, /tmp/zsh<random> by
// default, which the profile does not let the agent write.
func zshPrefix(tempRoot string) string { return filepath.Join(tempRoot, "zsh") }

// zshDotDir is zsh's ZDOTDIR for the agent's shells: an empty folder in the run's temp root (Command.Dirs makes it).
// `zsh -c` reads $ZDOTDIR/.zshenv, ~/.zshenv by default, even without a login shell, and after Codex's environment
// filter: the user's own could add a key back, or change PATH or GOTOOLCHAIN. The run's temp root is the agent's to
// write, so what the agent puts there is its own doing; /etc/zshenv, the system's, still runs (macOS ships none).
func zshDotDir(tempRoot string) string { return filepath.Join(tempRoot, "zdotdir") }

// features are Codex's features a run turns off: outward-facing tools (apps, plugins, browser and computer use, image
// generation), what keeps state across sessions (memories, goals), what runs code Agentium did not choose (hooks, tool
// suggestions, MCP dependency installs), the background daemon, the fast tier, and realtime conversation. Unbounded
// connection retries go too (also --disable): without network a run would otherwise wait for ever. So do the
// multi-agent tools (multi_agent, multi_agent_v2, and agents.enabled=false in configOverrides).
var features = []string{"apps", "plugins", "remote_plugin", "browser_use", "browser_use_external", "computer_use", "image_generation",
	"memories", "hooks", "tool_suggest", "daemon_auto_start", "fast_mode", "realtime_conversation", "skill_mcp_dependency_install", "goals",
	"unbounded_connection_retries", "multi_agent", "multi_agent_v2"}

// configOverrides are the run's settings, every one a -c override (exec has no flag for a configuration file, and
// --ignore-user-config skips CODEX_HOME's): see the package documentation. Codex's defaults that Agentium keeps (the
// plan's decisions 2 and 4): it tells the model its deny list, and its bundled skills load.
func configOverrides(inv agent.Invocation, environ []string, effort string) ([]string, error) {
	login := "chatgpt"
	if inv.SignIn == SignInAPIKey {
		login = "api"
	}
	filesystem, err := permissions(inv, environ)
	if err != nil {
		return nil, err
	}
	var featureTable []string
	for _, f := range features {
		featureTable = append(featureTable, f+"=false")
	}
	// The project is pinned trusted by its start folder and its checkout (the plan's decision 7, re-decided 2026-10-04):
	// Codex 0.160 trusts all or nothing, and an untrusted checkout loads neither its AGENTS.md nor its .codex/config.toml.
	// Trusted, both load, as in the user's own sessions; the overrides here outrank the project's config, and a project
	// config that sets what they protect is refused before the run (ProjectConfigRefusal). Pinned, not left unset:
	// unset, exec saves the trust in CODEX_HOME's config.toml. Only the table form works (the dotted form
	// -c projects."<path>".trust_level is ignored).
	var projects []string
	for _, p := range dedupe(append(sandbox.Forms(inv.Dir), sandbox.Forms(checkout(inv))...)) {
		projects = append(projects, tomlString(p)+"={trust_level=\"trusted\"}")
	}
	return []string{
		// The model again, as a setting: a session flag outranks a trusted project's model (-m alone is not shown to).
		"model=" + tomlString(inv.Model),
		"model_reasoning_effort=" + tomlString(effort),
		`approval_policy="never"`,
		"default_permissions=" + tomlString(Profile),
		"forced_login_method=" + tomlString(login),
		"check_for_update_on_startup=false",
		`web_search="disabled"`,
		`history.persistence="none"`,
		"sqlite_home=" + tomlString(filepath.Join(inv.State, stateSQLite)),
		"log_dir=" + tomlString(filepath.Join(inv.State, stateLogs)),
		"analytics.enabled=false",
		"feedback.enabled=false",
		// The plan's decision 3: no login shell, so the user's .zprofile and .zlogin never run in the agent's shells (and
		// ZDOTDIR below keeps out their .zshenv, which every zsh reads).
		"allow_login_shell=false",
		"features={" + strings.Join(featureTable, ",") + "}",
		// No subagents: every subagent thread can have a request in flight at once, and the cap's allowance holds back
		// one request. In 0.160.0 the session's multi-agent version is the features' override (multi_agent_v2 on: V2;
		// agents.enabled false: Disabled) before the model catalog's (V2 for gpt-6.1-sol), and Disabled offers no
		// multi-agent tools at all (the source at rust-v0.160.0: Config::multi_agent_version_for_model,
		// tools/spec_plan.rs). Codex's own limit (features.multi_agent_v2.max_concurrent_threads_per_session, which
		// counts the root, default 4) would keep the tools and need an allowance per thread. Claude Code's subagents
		// stay on: a fairness difference the plan records.
		"agents.enabled=false",
		// The agent's shells: the user's HOME (Codex's own is run-local), the run's temp root, zsh's heredocs in it, an
		// empty ZDOTDIR (zshDotDir); the
		// default excludes (*KEY*, *SECRET*, *TOKEN*) on, which 0.160.0 turns off when read from TOML, and Codex's and
		// OpenAI's variables (CODEX_HOME, CODEX_API_KEY) dropped.
		"shell_environment_policy.set={" + tomlTable([][2]string{{"HOME", inv.Home}, {"TMPDIR", inv.TempRoot}, {"TMPPREFIX", zshPrefix(inv.TempRoot)},
			{"ZDOTDIR", zshDotDir(inv.TempRoot)}}) + "}",
		"shell_environment_policy.ignore_default_excludes=false",
		`shell_environment_policy.exclude=["CODEX_*","OPENAI_*"]`,
		"permissions." + Profile + ".filesystem={" + tomlTable(filesystem) + "}",
		"permissions." + Profile + ".network={enabled=false}",
		"projects={" + strings.Join(projects, ",") + "}",
	}, nil
}

// permissions is the profile's filesystem table, in order: the whole disk readable (":root"), then what the agent may
// write (the checkout, the run's build cache and temp root, each in every form), then each project layer's .codex and
// .agents kept read-only, then every denied path (DeniedPaths), which Codex denies for reading and writing. A path both
// allowed and denied is refused: the run could not work.
func permissions(inv agent.Invocation, environ []string) ([][2]string, error) {
	entries := [][2]string{{":root", "read"}}
	access := map[string]string{}
	for _, dir := range []string{checkout(inv), inv.BuildCache, inv.TempRoot, inv.Marker} {
		if dir == "" {
			continue
		}
		for _, form := range sandbox.Forms(dir) {
			if access[form] == "" {
				access[form] = "write"
				entries = append(entries, [2]string{form, "write"})
			}
		}
	}
	// Every project layer's .codex and .agents, from the checkout's root down to the start folder, existing or not, stay
	// read-only: Codex keeps only the top-level ones of a writable root read-only, so an agent started in a module could
	// otherwise create svc/.codex/config.toml (a project layer Codex would load on a later rebuild of its config).
	for _, layer := range layers(inv) {
		for _, name := range []string{".codex", ".agents"} {
			for _, form := range sandbox.Forms(filepath.Join(layer, name)) {
				if access[form] == "" {
					access[form] = "read"
					entries = append(entries, [2]string{form, "read"})
				}
			}
		}
	}
	for _, p := range deniedPaths(inv, environ) {
		switch access[p] {
		case "write", "read":
			return nil, fmt.Errorf("the run's folder %s is also a path the agent may not read", p)
		case "":
			access[p] = "deny"
			entries = append(entries, [2]string{p, "deny"})
		}
	}
	return entries, nil
}

// layers are the folders whose .codex Codex loads as project layers: the checkout's root, then each folder down to
// where the agent starts.
func layers(inv agent.Invocation) []string {
	root := checkout(inv)
	out := []string{root}
	rel, err := filepath.Rel(root, inv.Dir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return out
	}
	dir := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		out = append(out, dir)
	}
	return out
}

// DeniedPaths is every path the run's agent may not read or write (see deniedPaths).
func (Adapter) DeniedPaths(inv agent.Invocation, environ []string) []string {
	return deniedPaths(inv, environ)
}

// deniedPaths is the shared deny list (sandbox.AgentDenied: inv.Deny, the credential stores, the user's build caches,
// the deps folder's denied parts), each path in every form, with Codex's and Claude Code's own data as the agent's:
//   - Codex's: the user's own ~/.codex (and CODEX_HOME, when the user's environment sets one), ~/.agents (the user's
//     skills), the run's Codex home (the shared login home in login mode, which holds the sign-in; the run's own with
//     an API key) and its state folder;
//   - Agentium's: ~/.agentium, which covers the shared login home and every run's rollouts, unless the run's checkout
//     lies in it (it is the data folder: then inv.Deny holds its parts, and the Codex home is denied by name), and the
//     folder of Claude Code's token file;
//   - Claude Code's, whole: ~/.claude, the user's CLAUDE_CONFIG_DIR and ~/.claude.json;
//   - with a temp root of the run's own, the temp and log folders every Claude Code session of the user shares
//     (sandbox.SharedTempDirs, SharedLogDirs).
func deniedPaths(inv agent.Invocation, environ []string) []string {
	home := inv.Home
	own := []string{filepath.Join(home, ".codex")}
	if dir := lookup(environ, "CODEX_HOME"); filepath.IsAbs(dir) {
		own = append(own, dir)
	}
	own = append(own, filepath.Join(home, ".agents"))
	if data := filepath.Join(home, ".agentium"); !within(checkout(inv), data) {
		own = append(own, data)
	}
	own = append(own, inv.ConfigDir, inv.State)
	tokenFile := lookup(environ, "AGENTIUM_CLAUDE_TOKEN_FILE")
	if !filepath.IsAbs(tokenFile) {
		tokenFile = filepath.Join(home, ".config", "agentium", "claude-oauth-token")
	}
	own = append(own, filepath.Dir(tokenFile), filepath.Join(home, ".claude"))
	if dir := lookup(environ, "CLAUDE_CONFIG_DIR"); filepath.IsAbs(dir) {
		own = append(own, dir)
	}
	own = append(own, filepath.Join(home, ".claude.json"))
	var shared []string
	if inv.TempRoot != "" {
		shared = append(shared, sandbox.SharedTempDirs(environ, inv.UID)...)
	}
	shared = append(shared, sandbox.SharedLogDirs(home, "")...)
	return sandbox.AgentDenied{Deny: inv.Deny, AgentData: own, SecretFile: inv.TokenFile, Home: home, AccountHome: inv.AccountHome,
		Environ: environ, Deps: inv.Deps, Shared: shared}.Reads()
}

// within reports whether p is root or lies under it, as written and with links resolved.
func within(p, root string) bool {
	for _, a := range sandbox.Forms(p) {
		for _, b := range sandbox.Forms(root) {
			if rel, err := filepath.Rel(b, a); err == nil && (rel == "." || filepath.IsLocal(rel)) {
				return true
			}
		}
	}
	return false
}

func lookup(environ []string, name string) string {
	value := ""
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			value = v // the last one wins, as for a process
		}
	}
	return value
}

func dedupe(list []string) []string {
	var out []string
	for _, x := range list {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// tomlTable is the body of a TOML inline table: each key and value a basic string.
func tomlTable(entries [][2]string) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = tomlString(e[0]) + "=" + tomlString(e[1])
	}
	return strings.Join(parts, ",")
}

// tomlString is s as a TOML basic string: Codex parses each -c value as TOML, and takes one that fails to parse as a
// literal string instead, so every value Agentium builds from a path is quoted and escaped here.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\u` + strconv.FormatInt(int64(0x10000+r), 16)[1:])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
