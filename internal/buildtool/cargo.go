package buildtool

import (
	"os"
	"path/filepath"
	"strings"
)

// cargoProfile holds Cargo's special cases.
//
// Offline: setup runs `cargo fetch` with CARGO_HOME in the deps folder; the agent gets that CARGO_HOME, which it only
// reads (a fetched build needs no writes there), and CARGO_NET_OFFLINE=true. Its builds go to target/ in its checkout.
// Proved in a real session on tokio-rs/bytes: `cargo test --offline` passed with no denials.
//
// Leaks: rustc wrappers such as sccache cache compiled crates, hidden tests included, in a folder agents could read,
// so Agentium's own commands run with every wrapper variable cleared, and the agent's environment never carries the
// user's wrapper either (it would write there). The user's sccache folders and target directories are denied to agents.
//
// Target directories: `[build] target-dir` in a Cargo config, or CARGO_BUILD_TARGET_DIR, could send a build's output
// (grading's compiled hidden tests) to a folder agents can read, so CARGO_TARGET_DIR, which beats both, is set
// explicitly: `target` under the working folder for Agentium's own commands, <checkout>/target for the agent.
func cargoProfile() Profile {
	wrappers := []string{"RUSTC_WRAPPER", "RUSTC_WORKSPACE_WRAPPER", "CARGO_BUILD_RUSTC_WRAPPER", "CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER"}
	var cleared []CacheVar
	for _, name := range wrappers {
		cleared = append(cleared, CacheVar{Name: name, Clear: true})
	}
	return Profile{
		Name:   "cargo",
		Detect: []string{"Cargo.toml"},
		TestCommand: func(func(string) bool) string {
			return "cargo test"
		},
		Languages: []string{"rust"},
		Runners:   []string{"cargo"},
		Configs: []string{"Cargo.toml", "Cargo.lock", "rust-toolchain", "rust-toolchain.toml", ".cargo/config.toml", ".cargo/config",
			"build.rs", ".config/nextest.toml"},
		TestPatterns:  []string{`cargo (test|nextest|t)`},
		EnvNames:      []string{"RUSTUP_TOOLCHAIN", "CARGO_BUILD_JOBS", "RUST_BACKTRACE"},
		CommandCaches: cleared,
		CommandVars:   func(string) []string { return []string{"CARGO_TARGET_DIR=target"} }, // relative to the command's folder
		AgentEnv: func(c AgentContext) []string {
			env := []string{"RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER="}
			if c.Repo != "" {
				env = append(env, "CARGO_TARGET_DIR="+filepath.Join(c.Repo, "target"))
			}
			if c.Deps != "" {
				env = append(env, "CARGO_HOME="+filepath.Join(c.Deps, "cargo"), "CARGO_NET_OFFLINE=true")
			}
			return env
		},
		Warm: func(_, deps string, _ func(string) bool) []WarmStep {
			return []WarmStep{{Command: "cargo fetch", Env: []string{"CARGO_HOME=" + filepath.Join(deps, "cargo")}}}
		},
		UserCaches: cargoCaches,
	}
}

// cargoCaches are what the user's builds leave that may hold compiled hidden tests or credentials: sccache's folders
// (SCCACHE_DIR and the defaults), a CARGO_TARGET_DIR, and Cargo's registry, git sources and credentials. ~/.cargo/bin
// stays readable: the agent runs cargo and rustc through it.
func cargoCaches(environ []string, home string) []string {
	env := vars(environ)
	cargoHomes := []string{filepath.Join(home, ".cargo")} // and CARGO_HOME: an earlier build may have used either
	if ch := userHome(environ, "CARGO_HOME", cargoHomes[0]); ch != cargoHomes[0] {
		cargoHomes = append(cargoHomes, ch)
	}
	var paths []string
	for _, ch := range cargoHomes {
		paths = append(paths, filepath.Join(ch, "registry"), filepath.Join(ch, "git"), filepath.Join(ch, "credentials.toml"), filepath.Join(ch, "credentials"))
	}
	paths = append(paths, filepath.Join(home, "Library", "Caches", "Mozilla.sccache"), filepath.Join(home, ".cache", "sccache"))
	for _, v := range []string{env["SCCACHE_DIR"], env["CARGO_TARGET_DIR"], env["CARGO_BUILD_TARGET_DIR"]} {
		if filepath.IsAbs(v) {
			paths = append(paths, v)
		}
	}
	for _, ch := range cargoHomes { // `[build] target-dir` in the user's config; relative paths start above the config's folder
		for _, name := range []string{"config.toml", "config"} {
			if dir := configTargetDir(filepath.Join(ch, name)); dir != "" {
				if !filepath.IsAbs(dir) {
					dir = filepath.Join(filepath.Dir(ch), dir)
				}
				paths = append(paths, dir)
			}
		}
	}
	if xdg := env["XDG_CACHE_HOME"]; filepath.IsAbs(xdg) {
		paths = append(paths, filepath.Join(xdg, "sccache"))
	}
	return paths
}

// configTargetDir reads `target-dir` from the [build] table of a Cargo config file, or "" when there is none. It reads
// the plain `key = "value"` form, which is how the setting is written; an inline table or a dotted key is not seen.
func configTargetDir(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	inBuild := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "["):
			inBuild = line == "[build]"
		case inBuild:
			if key, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(key) == "target-dir" {
				v, _, _ = strings.Cut(strings.TrimSpace(v), " #")
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	return ""
}
