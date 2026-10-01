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
// Only Go has a profile so far. The golden test in internal/run (TestGoProfileGolden) pins a Go project's behavior.
package buildtool

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Profile is one build tool's special cases. Every field is optional except Name.
type Profile struct {
	Name string
	// Detect are files at the repository root that mark a project of this tool; discovery asks TestCommand only when
	// one of them is present.
	Detect []string
	// TestCommand proposes the default test command; has reports whether a file exists at the repository root (a
	// wrapper script such as mvnw, say). An empty result proposes nothing.
	TestCommand func(has func(name string) bool) string
	// Runners are the command words that run this tool (matched as whole words in a verification command), and Configs
	// the files at the repository root that configure it: grading reports an agent's change to them.
	Runners []string
	Configs []string
	// TestPatterns are regular-expression alternatives matching an agent's command that runs tests (whole words).
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
	// AgentEnv returns variables to set in the agent's environment, replacing any of the same name the allowlist kept:
	// what the tool needs to build and test offline in the sandbox. allowed is the allowlisted environment, environ the
	// parent's whole environment, home the user's home folder.
	AgentEnv func(allowed, environ []string, home string) []string
	// UserCaches are the user's own caches of this tool, which the agent may not read: they hold what earlier builds
	// compiled, the hidden tests of validations and gradings included. Only absolute paths count.
	UserCaches func(environ []string, home string) []string
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
	return []Profile{goProfile()}
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
		Runners:      []string{"go"},
		Configs:      []string{"go.mod"},
		TestPatterns: []string{"go test"},
		EnvNames: []string{"GOPATH", "GOROOT", "GOBIN", "GOCACHE", "GOMODCACHE", "GOENV", "GOFLAGS", "GOTOOLCHAIN", "GOPROXY",
			"GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOINSECURE", "GOWORK", "GO111MODULE", "GOTMPDIR", "GOEXPERIMENT",
			"GODEBUG", "GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GOOS", "GOARCH", "GOAMD64", "GOARM64"},
		EnvPrefixes: []string{"CGO_"},
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
func goAgentEnv(allowed, environ []string, home string) []string {
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

// CommandEnv is the environment of the commands Agentium runs itself (setup, validation, grading): every profile's
// caches under cache, then TMPDIR and every profile's temporary-folder variables set to tmp, then XDG_CACHE_HOME under
// cache (tools that follow it, Jest's included). Both folders are in the data folder, which agents may not read.
func CommandEnv(cache, tmp string) []string {
	var env []string
	profiles := Profiles()
	for _, p := range profiles {
		for _, c := range p.CommandCaches {
			env = append(env, c.Value(cache))
		}
	}
	env = append(env, "TMPDIR="+tmp)
	for _, p := range profiles {
		for _, name := range p.TempVars {
			env = append(env, name+"="+tmp)
		}
	}
	return append(env, "XDG_CACHE_HOME="+filepath.Join(cache, "xdg"))
}

// AgentCacheEnv points every profile's agent caches into the run's own build cache.
func AgentCacheEnv(buildCache string) []string {
	var env []string
	for _, p := range Profiles() {
		for _, c := range p.AgentCaches {
			env = append(env, c.Value(buildCache))
		}
	}
	return env
}

// AgentCacheNames are the variables AgentCacheEnv sets.
func AgentCacheNames() []string {
	var names []string
	for _, p := range Profiles() {
		for _, c := range p.AgentCaches {
			names = append(names, c.Name)
		}
	}
	return names
}

// AgentEnv is every profile's AgentEnv, in table order.
func AgentEnv(allowed, environ []string, home string) []string {
	var env []string
	for _, p := range Profiles() {
		if p.AgentEnv != nil {
			env = append(env, p.AgentEnv(allowed, environ, home)...)
		}
	}
	return env
}

// UserCaches is every profile's UserCaches, in table order.
func UserCaches(environ []string, home string) []string {
	var paths []string
	for _, p := range Profiles() {
		if p.UserCaches != nil {
			paths = append(paths, p.UserCaches(environ, home)...)
		}
	}
	return paths
}

// EnvAllowlist is every profile's EnvNames and EnvPrefixes.
func EnvAllowlist() (names, prefixes []string) {
	for _, p := range Profiles() {
		names = append(names, p.EnvNames...)
		prefixes = append(prefixes, p.EnvPrefixes...)
	}
	return names, prefixes
}

// TestCommands proposes every detected profile's test command, in table order.
func TestCommands(has func(name string) bool) []string {
	var commands []string
	for _, p := range Profiles() {
		for _, marker := range p.Detect {
			if has(marker) {
				if c := p.TestCommand(has); c != "" {
					commands = append(commands, c)
				}
				break
			}
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
