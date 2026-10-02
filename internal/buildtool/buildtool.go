// Package buildtool is the table of build-tool profiles: what Agentium knows about each language's build tool, in one
// place. A profile says how discovery recognizes a project and proposes its test command, which files configure its test
// runner (grading reports changes to them), which environment variables agents keep, where the caches of Agentium's own
// commands go, which of the user's caches agents may not read, and what the agent's environment needs for its builds to
// work offline in the sandbox.
//
// Several entries guard the isolation of runs, so a change to them is a security change:
//   - CommandCaches and TempVars keep what Agentium's own commands (setup, validation, grading) compile, hidden tests
//     included, in the data folder, which agents may not read;
//   - UserCaches are the user's caches agents are denied: earlier builds left compiled tests there;
//   - EnvNames and EnvPrefixes widen the agent's environment allowlist, which never passes credentials
//     (runner.IsCredential is applied after it).
//
// Profiles are selected per repository: Select returns the ones marked Always (Go's, whose special cases applied to
// every project before profiles existed, so a Go project's runs stay exactly as they were) and those Detect finds at
// the repository's root. Everything but UserCaches follows the selection: the allowlist, the agent's environment and
// caches, the environment of Agentium's own commands, the sandbox's settings, and the hooks that warm and stop a run's
// tools. An Always profile the repository does not have, while it has another's (a Python project with no go.mod), is
// selected without its agent side (see Profile.Always). UserCaches stay global, whatever the project: a user's cache of any tool may hold hidden tests that an earlier
// build compiled, and an agent in any repository could read it.
//
// Dependencies offline (Cargo, Maven, Gradle and Python; the recipes were proved in real sessions, see the Java and Rust
// plan and the Python and TypeScript plan):
// the agent's sandbox has no network and writes only its checkout and its run's own build cache. Dependencies come
// from a deps folder in the data folder (home.Layout.Deps) that agents may read and not write. Only a run's setup
// writes it, in a checkout of the task's base, where its own hidden tests do not exist (Profile.Warm). A later task's
// base does hold earlier tasks' hidden tests, though, as ordinary tests, and a warm-up compiles them: what a build tool
// records of its compiles in the deps folder (Gradle's caches) is denied to agents (DepsDenied).
// Each tool keeps its own subfolder of the run cache; Go's cache stays at its root.
//
// The golden test in internal/run (TestGoProfileGolden) pins a Go project's behavior.
package buildtool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Profile is one build tool's special cases. Every field is optional except Name. A profile's environment, cache and
// hook entries apply to a repository only when Select chooses it, except UserCaches, which apply to all.
type Profile struct {
	Name string
	// Always selects the profile for every repository, whatever Detect finds (Go's, for compatibility): its caches for
	// Agentium's own commands, test patterns and languages apply everywhere. Its agent side (EnvNames, EnvPrefixes,
	// AgentEnv, AgentCaches) applies only where it is detected, or where no profile is (an unknown layout, such as a Go
	// module in a subfolder, keeps Go's settings as before profiles): a Python project's agent gets no GOFLAGS or GOCACHE.
	Always bool
	// implicit marks an Always profile Select chose though the repository has another profile and not this one: its
	// agent side is off (agentSide).
	implicit bool
	// Detect are files at the repository root that mark a project of this tool; discovery asks TestCommand only when
	// one of them is present.
	Detect []string
	// TestCommand proposes the default test command; has reports whether a file exists at the repository root (a
	// wrapper script such as mvnw, say). An empty result proposes nothing.
	TestCommand func(has func(name string) bool) string
	// Languages are the languages this tool's tests are written in, as internal/mine names them ("go", "java",
	// "kotlin", ...): task mine keeps only commits whose tests the project's tools run.
	Languages []string
	// Runners are the command words that run this tool (matched as whole words in a verification command), and Configs
	// the files at the repository root that configure it: grading reports an agent's change to them.
	Runners []string
	Configs []string
	// TestPatterns are regular-expression alternatives matching an agent's command that runs tests. Every profile's
	// alternatives are joined, with other runners', into one `\b(...)\b` group, so each must be valid inside it: no
	// anchors, and no unbalanced groups.
	TestPatterns []string
	// EnvNames and EnvPrefixes are the variables the agent's environment keeps for this tool.
	EnvNames    []string
	EnvPrefixes []string
	// CommandCaches point this tool's caches, for the commands Agentium runs itself, into the data folder (or clear a
	// variable, so nothing is cached outside it). TempVars name the tool's own temporary folder, set to Agentium's.
	CommandCaches []CacheVar
	TempVars      []string
	// AgentCaches point this tool's caches into the run's own build cache, which the sandbox lets the agent write: in
	// the agent's environment and in the task's setup, so setup warms the agent's builds. The variables are dropped from
	// the user's environment when the run has such a cache.
	AgentCaches []CacheVar
	// CommandVars are variables this tool needs, beyond CommandCaches, in the environment of Agentium's own commands
	// (a Maven repository is an argument, not a variable of its own): cache is the data folder's cache root.
	CommandVars func(cache string) []string
	// AgentEnv returns variables to set in the agent's environment, replacing any of the same name the allowlist kept:
	// what the tool needs to build and test offline in the sandbox.
	AgentEnv func(c AgentContext) []string
	// LocalBinding asks the sandbox's allowLocalBinding: Gradle's file-lock service binds a local UDP socket and fails
	// without it. It grants any local port to bind, inbound connections, and outbound connections to localhost, so a
	// run needs the user's opt-in (claude.LocalBindingRefusal).
	LocalBinding bool
	// Warm returns the commands that fetch this tool's dependencies into deps, run by a run's setup in a throwaway
	// checkout of the base commit (dir), where no hidden test exists (see the package documentation).
	Warm func(dir, deps string, has func(name string) bool) []WarmStep
	// WarmRecipe is whatever else shapes a warm-up besides its steps (the scripts PrepareDeps writes). With the steps
	// it makes WarmVersion, so changing the recipe re-warms bases that earlier recipes stamped as warmed.
	WarmRecipe string
	// PrepareDeps makes the deps folder's own settings and layout before a warm-up writes it, under the warm-up lock
	// (Gradle: no cache cleanup, which would delete files under agents that read them, and what agents read kept
	// outside the Gradle home they are denied).
	PrepareDeps func(deps string) error
	// PrepareRun makes the run's own folders for the tool (inside buildCache) before the agent starts: the agent
	// cannot write deps, so what must be writable is copied or created here.
	PrepareRun func(ctx context.Context, deps, buildCache string) error
	// StopRun ends what the tool left running when the run's agent ended; it must not run anything from the checkout,
	// which the agent may have changed.
	StopRun func(ctx context.Context, buildCache string, host Host) error
	// PrepareCommands makes what Agentium's own commands need under the data folder's cache root (see CommandCaches).
	PrepareCommands func(cache string) error
	// WarmFunc warms what fixed commands (Warm) cannot: Python finds an interpreter on the host and builds a venv per
	// set of dependency inputs and the base's metadata-only .dist-info, whose paths the agent's environment then names
	// (Warmed.Venv, Warmed.Metadata). It runs where Warm's steps
	// do (a throwaway checkout of the base, with network, under the warm-up lock). A failure it can explain is
	// Warmed.Failed, a note that leaves the base unstamped; errors are for cancellation and I/O.
	WarmFunc func(ctx context.Context, in WarmInput) (Warmed, error)
	// CheckoutEnv returns the variables Agentium's own commands need in a checkout of the project once its tools are
	// warmed (Python: the venv): c.Repo is the checkout (the run's, for the task's setup; the grading copy), and
	// c.BuildCache the data folder's cache root. Later entries replace earlier ones of the same name.
	CheckoutEnv func(c AgentContext) []string
	// CheckoutCaches are the folders CheckoutEnv gives Agentium's own commands in the checkout repo alone (Python's
	// hypothesis database), under the data folder's cache root: removed when the checkout is (RemoveCheckoutCaches).
	CheckoutCaches func(cache, repo string) []string
	// CheckoutDrop names the variables of the user's environment that Agentium's own commands in a checkout (setup,
	// grading, validation) must not inherit, as the agent does not (CheckoutEnviron): they would change what the tests
	// run with between the agent and grading.
	CheckoutDrop func(name string) bool
	// UserCaches are the user's own caches of this tool, which the agent may not read: they hold what earlier builds
	// compiled, the hidden tests of validations and gradings included. Only absolute paths count.
	UserCaches func(environ []string, home string) []string
	// ProjectCaches are folders configured in the user's repository (or above it) that earlier builds there may have
	// filled with compiled hidden tests (a Cargo target-dir): denied to agents like UserCaches, for every project.
	ProjectCaches func(environ []string, home string, roots []string) ([]string, error)
}

// CacheVar is an environment variable that points a cache at a folder (Dir, relative to the cache root; "" is the root
// itself), or with Clear, an empty value that turns the cache off.
type CacheVar struct {
	Name  string
	Dir   string
	Clear bool
}

// Value is the variable's entry for a cache root.
func (c CacheVar) Value(root string) string {
	if c.Clear {
		return c.Name + "="
	}
	return c.Name + "=" + filepath.Join(root, c.Dir)
}

// Profiles is the table, in the order discovery proposes test commands. It returns a fresh copy each call.
func Profiles() []Profile {
	return []Profile{goProfile(), mavenProfile(), gradleProfile(), cargoProfile(), pythonProfile(), nodeProfile()}
}

// AgentContext is what a profile's AgentEnv may use.
type AgentContext struct {
	Allowed, Environ []string // the allowlisted environment and the parent's whole one
	Home             string   // the user's home folder
	Repo             string   // the run's checkout, where its agent works; "" when unknown
	BuildCache       string   // the run's own build cache; "" without one
	Deps             string   // the deps folder agents read; "" without one
	JavaHome         string   // a JDK resolved on the host (ResolveJavaHome); "" when none was found
	Venv             string   // the Python venv the run's warm-up chose (Warmed.Venv); "" without one
	Metadata         string   // the base's Python metadata folder (Warmed.Metadata), on PYTHONPATH after Repo; "" without one
	// ImportRoot is where the project's code imports from, relative to Repo ("src", or "" for Repo itself), decided once
	// from the base commit (ImportRoot), so the agent, setup, grading and validation agree whatever the agent changes.
	ImportRoot string
}

// WarmStep is a command that fetches dependencies, with the variables it needs added to the environment.
type WarmStep struct {
	Command string
	Env     []string
}

// DetectIn names the profiles of the repository checked out at dir, from the regular files at its root.
func DetectIn(dir string) []string {
	return DetectedNames(func(name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && info.Mode().IsRegular()
	})
}

// Select returns the profiles for a repository: the Always ones and those named (see DetectedNames), in table order.
// An Always profile not named while others are is marked implicit: selected without its agent side (Profile.Always).
func Select(names []string) []Profile {
	var out []Profile
	for _, p := range Profiles() {
		named := slices.Contains(names, p.Name)
		if p.Always || named {
			p.implicit = p.Always && !named && len(names) > 0
			out = append(out, p)
		}
	}
	return out
}

// agentSide reports whether the profile's agent variables (allowlist, AgentEnv, AgentCaches) apply.
func (p Profile) agentSide() bool {
	return !p.implicit
}

// DetectedNames names the profiles whose Detect files has reports: what a run keeps (Select) for its repository.
func DetectedNames(has func(name string) bool) []string {
	var names []string
	for _, p := range Detected(has) {
		names = append(names, p.Name)
	}
	return names
}

// goProfile holds Go's special cases.
//
// Offline: the agent reads the module cache (GOMODCACHE) and never writes it; the task's setup has network and warms
// it. The agent builds into the run's own GOCACHE, and with -buildvcs=false (see goAgentEnv).
func goProfile() Profile {
	return Profile{
		Name:         "go",
		Detect:       []string{"go.mod"},
		TestCommand:  func(func(string) bool) string { return "go test ./..." },
		Languages:    []string{"go"},
		Runners:      []string{"go"},
		Configs:      []string{"go.mod"},
		TestPatterns: []string{"go test"},
		EnvNames: []string{"GOPATH", "GOROOT", "GOBIN", "GOCACHE", "GOMODCACHE", "GOENV", "GOFLAGS", "GOTOOLCHAIN", "GOPROXY",
			"GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOINSECURE", "GOWORK", "GO111MODULE", "GOTMPDIR", "GOEXPERIMENT",
			"GODEBUG", "GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GOOS", "GOARCH", "GOAMD64", "GOARM64"},
		EnvPrefixes: []string{"CGO_"},
		Always:      true,
		// GOCACHEPROG is cleared so hidden tests go to no cache program (one set with `go env -w` still applies).
		CommandCaches: []CacheVar{{Name: "GOCACHE", Dir: "go-build"}, {Name: "GOCACHEPROG", Clear: true}},
		TempVars:      []string{"GOTMPDIR"},
		AgentCaches:   []CacheVar{{Name: "GOCACHE"}},
		AgentEnv:      goAgentEnv,
		UserCaches:    goCaches,
	}
}

// goAgentEnv sets GOFLAGS for the agent. Go stamps the main module's version from VCS and writes a stat-cache entry into
// the module cache, which the sandbox rightly denies: every `go build` would print "writing stat cache ... operation not
// permitted" and agents would spend turns on it. Only the agent's environment gets the flag, not setup or grading
// commands. The user's own GOFLAGS stay, ours appended. Go reads GOFLAGS from `go env -w` only while the environment
// leaves it unset, so setting it here would hide the user's saved flags from the agent: start from them.
func goAgentEnv(c AgentContext) []string {
	allowed, environ, home := c.Allowed, c.Environ, c.Home
	goflags, userSet := "-buildvcs=false", false
	for _, kv := range allowed {
		if v, ok := strings.CutPrefix(kv, "GOFLAGS="); ok {
			goflags, userSet = strings.TrimSpace(v+" "+goflags), true
		}
	}
	if !userSet {
		if saved := strings.TrimSpace(goEnvFile(vars(environ), home)["GOFLAGS"]); saved != "" {
			goflags = saved + " " + goflags
		}
	}
	return []string{"GOFLAGS=" + goflags}
}

// goCaches are the user's Go build caches: GOCACHE when set (in the environment or with `go env -w`), and the defaults
// under the home folder (macOS, Linux and XDG_CACHE_HOME). Each holds what earlier builds compiled, hidden tests
// included. Values Go ignores (relative, "off") are skipped.
func goCaches(environ []string, home string) []string {
	env := vars(environ)
	paths := []string{filepath.Join(home, "Library", "Caches", "go-build"), filepath.Join(home, ".cache", "go-build")}
	for _, v := range []string{env["GOCACHE"], goEnvFile(env, home)["GOCACHE"], filepath.Join(env["XDG_CACHE_HOME"], "go-build")} {
		if filepath.IsAbs(v) {
			paths = append(paths, v)
		}
	}
	return paths
}

// goEnvFile reads the settings `go env -w` keeps: $GOENV, else go/env in the user's config folder. A missing file, or
// GOENV=off, gives none.
func goEnvFile(vars map[string]string, home string) map[string]string {
	path := vars["GOENV"]
	switch {
	case path == "off":
		return nil
	case path != "":
	case runtime.GOOS == "darwin":
		path = filepath.Join(home, "Library", "Application Support", "go", "env")
	case filepath.IsAbs(vars["XDG_CONFIG_HOME"]):
		path = filepath.Join(vars["XDG_CONFIG_HOME"], "go", "env")
	default:
		path = filepath.Join(home, ".config", "go", "env")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	settings := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if name, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			settings[name] = v
		}
	}
	return settings
}

// vars maps an environment's names to their values; a later entry wins, as in a process's environment.
func vars(environ []string) map[string]string {
	m := map[string]string{}
	for _, kv := range environ {
		name, v, _ := strings.Cut(kv, "=")
		m[name] = v
	}
	return m
}

// CommandEnv is the environment of the commands Agentium runs itself (setup, validation, grading) for every project:
// the Always profiles' caches under cache, then TMPDIR and their temporary-folder variables set to tmp, then
// XDG_CACHE_HOME under cache (tools that follow it, Jest's included). Both folders are in the data folder, which
// agents may not read. CommandEnvFor adds a repository's other tools.
func CommandEnv(cache, tmp string) []string {
	var env []string
	var always []Profile
	for _, p := range Profiles() {
		if p.Always {
			always = append(always, p)
		}
	}
	for _, p := range always {
		for _, c := range p.CommandCaches {
			env = append(env, c.Value(cache))
		}
	}
	env = append(env, "TMPDIR="+tmp)
	for _, p := range always {
		for _, name := range p.TempVars {
			env = append(env, name+"="+tmp)
		}
	}
	return append(env, "XDG_CACHE_HOME="+filepath.Join(cache, "xdg"))
}

// CommandEnvFor is what the selected profiles that are not Always add to CommandEnv: their caches (or cleared
// variables) and variables, under the data folder's cache root. Later entries replace earlier ones of the same name.
func CommandEnvFor(selected []Profile, cache string) []string {
	var env []string
	for _, p := range selected {
		if p.Always {
			continue
		}
		for _, c := range p.CommandCaches {
			env = append(env, c.Value(cache))
		}
		if p.CommandVars != nil {
			env = append(env, p.CommandVars(cache)...)
		}
	}
	return env
}

// AgentCacheEnv points the selected profiles' agent caches into the run's own build cache (none of an implicit one).
func AgentCacheEnv(selected []Profile, buildCache string) []string {
	var env []string
	for _, p := range selected {
		if !p.agentSide() {
			continue
		}
		for _, c := range p.AgentCaches {
			env = append(env, c.Value(buildCache))
		}
	}
	return env
}

// AgentCacheNames are the variables AgentCacheEnv sets.
func AgentCacheNames(selected []Profile) []string {
	var names []string
	for _, p := range selected {
		if !p.agentSide() {
			continue
		}
		for _, c := range p.AgentCaches {
			names = append(names, c.Name)
		}
	}
	return names
}

// AgentEnv is the selected profiles' AgentEnv, in table order (none of an implicit one: see Profile.Always).
func AgentEnv(selected []Profile, c AgentContext) []string {
	var env []string
	for _, p := range selected {
		if p.AgentEnv != nil && p.agentSide() {
			env = append(env, p.AgentEnv(c)...)
		}
	}
	return env
}

// CheckoutEnv is the selected profiles' CheckoutEnv, in table order: what Agentium's own commands in a checkout need
// from the warmed tools (see Profile.CheckoutEnv).
func CheckoutEnv(selected []Profile, c AgentContext) []string {
	var env []string
	for _, p := range selected {
		if p.CheckoutEnv != nil {
			env = append(env, p.CheckoutEnv(c)...)
		}
	}
	return env
}

// RemoveCheckoutCaches removes the selected profiles' CheckoutCaches of the checkout repo, once the checkout is gone.
func RemoveCheckoutCaches(selected []Profile, cache, repo string) error {
	var errs []error
	for _, p := range selected {
		if p.CheckoutCaches == nil {
			continue
		}
		for _, dir := range p.CheckoutCaches(cache, repo) {
			errs = append(errs, os.RemoveAll(dir))
		}
	}
	return errors.Join(errs...)
}

// CheckoutEnviron is environ without what the selected profiles' CheckoutDrop names: the base environment of Agentium's
// own commands in a checkout.
func CheckoutEnviron(selected []Profile, environ []string) []string {
	var drops []func(string) bool
	for _, p := range selected {
		if p.CheckoutDrop != nil {
			drops = append(drops, p.CheckoutDrop)
		}
	}
	if len(drops) == 0 {
		return environ
	}
	out := []string{}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(drops, func(drop func(string) bool) bool { return drop(name) }) {
			out = append(out, kv)
		}
	}
	return out
}

// UserCaches is every profile's UserCaches, in table order: global, whatever the repository (see the package
// documentation).
func UserCaches(environ []string, home string) []string {
	var paths []string
	for _, p := range Profiles() {
		if p.UserCaches != nil {
			paths = append(paths, p.UserCaches(environ, home)...)
		}
	}
	return paths
}

// ProjectCaches is every profile's ProjectCaches for the user's repository and its worktrees (roots). It fails closed:
// a configuration it cannot read safely is an error, which stops a run before it starts (CheckConfigs).
func ProjectCaches(environ []string, home string, roots []string) ([]string, error) {
	var paths []string
	var errs []error
	for _, p := range Profiles() {
		if p.ProjectCaches != nil {
			found, err := p.ProjectCaches(environ, home, roots)
			paths = append(paths, found...)
			errs = append(errs, err)
		}
	}
	return paths, errors.Join(errs...)
}

// CheckConfigs reports what ProjectCaches would refuse, for callers that only check (an experiment, before it locks).
func CheckConfigs(environ []string, home string, roots []string) error {
	_, err := ProjectCaches(environ, home, roots)
	return err
}

// DepsDenied are the folders under deps agents may not read, each whole (a folder a later warm-up adds inside one is
// denied too, which a list of its entries made when the agent started would miss):
//   - the deps folder's whole Gradle home: its caches record what warm-ups compiled in each base, whose later tasks'
//     hidden tests are there (javaCompile/classAnalysis.bin names their classes), and a warm-up running another Gradle
//     version adds a caches/<version> folder at any time, while agents of other runs work. So nothing in it is listed:
//     it is denied whole, and what agents need of it lives outside it (gradleShared: gradleRO, which
//     GRADLE_RO_DEP_CACHE reads and holds only modules-2, and gradleJDKs);
//   - build-cache, a build cache's folder: warm-ups run with every build cache off, so it should not exist;
//   - Python's download caches and resolve reports (pythonPrivate): agents need only the venvs, and uv's or pip's
//     cache would hold the project's own build had anything ever installed it.
func DepsDenied(deps string) []string {
	paths := []string{filepath.Join(deps, "gradle"), filepath.Join(deps, "build-cache")}
	for _, name := range pythonPrivate {
		paths = append(paths, filepath.Join(deps, name))
	}
	return paths
}

// EnvAllowlist is the selected profiles' EnvNames and EnvPrefixes (none of an implicit one: see Profile.Always).
func EnvAllowlist(selected []Profile) (names, prefixes []string) {
	for _, p := range selected {
		if !p.agentSide() {
			continue
		}
		names = append(names, p.EnvNames...)
		prefixes = append(prefixes, p.EnvPrefixes...)
	}
	return names, prefixes
}

// LocalBinding reports whether a selected profile needs the sandbox to allow local sockets.
func LocalBinding(selected []Profile) bool {
	return slices.ContainsFunc(selected, func(p Profile) bool { return p.LocalBinding })
}

// Detected returns the profiles whose Detect files has reports, in table order.
func Detected(has func(name string) bool) []Profile {
	var found []Profile
	for _, p := range Profiles() {
		if slices.ContainsFunc(p.Detect, has) {
			found = append(found, p)
		}
	}
	return found
}

// TestCommands proposes every detected profile's test command, in table order.
func TestCommands(has func(name string) bool) []string {
	var commands []string
	for _, p := range Detected(has) {
		if c := p.TestCommand(has); c != "" {
			commands = append(commands, c)
		}
	}
	return commands
}

// RunnerConfigs maps each profile's runner words to its configuration files.
func RunnerConfigs() map[string][]string {
	m := map[string][]string{}
	for _, p := range Profiles() {
		for _, word := range p.Runners {
			m[word] = append(m[word], p.Configs...)
		}
	}
	return m
}

// TestPatterns is every profile's TestPatterns.
func TestPatterns() []string {
	var patterns []string
	for _, p := range Profiles() {
		patterns = append(patterns, p.TestPatterns...)
	}
	return patterns
}
