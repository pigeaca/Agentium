package buildtool

import (
	"os"
	"path/filepath"
	"regexp"
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
		UserCaches:    cargoCaches,
		ProjectCaches: cargoProjectCaches,
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
	for _, ch := range cargoHomes { // the config in CARGO_HOME (relative paths start above it), then those above the home folder
		for _, name := range []string{"config.toml", "config"} {
			for _, t := range configTargetDirs(filepath.Join(ch, name)) {
				if !filepath.IsAbs(t) {
					t = filepath.Join(filepath.Dir(ch), t)
				}
				paths = append(paths, t)
			}
		}
	}
	paths = append(paths, configCaches(home)...)
	if xdg := env["XDG_CACHE_HOME"]; filepath.IsAbs(xdg) {
		paths = append(paths, filepath.Join(xdg, "sccache"))
	}
	return paths
}

// targetDirSetting finds every `target-dir = "…"` in a Cargo config, however it is written: under [build] (with or
// without a comment after the header), as a dotted key (build.target-dir), with quoted keys, in an inline table, with
// a trailing comment. It does not parse TOML: any line that sets a quoted value to a key ending in target-dir counts,
// wherever it is, so it can only deny too much (a target-dir in another table, a commented-out line), never too little.
// Not seen: a value in a multi-line string, and settings given on a command line (--config).
var targetDirSetting = regexp.MustCompile(`target-dir["']?\s*=\s*(?:"([^"\n]*)"|'([^'\n]*)')`)

// configTargetDirs lists the target directories a Cargo config file names, or nil when there is no such file.
func configTargetDirs(file string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, m := range targetDirSetting.FindAllStringSubmatch(string(data), -1) {
		if dir := m[1] + m[2]; dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// configCaches are the target directories named by the Cargo configs in dir and every folder above it, as Cargo reads
// them (<folder>/.cargo/config.toml, and the older .cargo/config). Relative paths start at <folder>.
func configCaches(dir string) []string {
	var paths []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		for _, name := range []string{"config.toml", "config"} {
			for _, t := range configTargetDirs(filepath.Join(d, ".cargo", name)) {
				if !filepath.IsAbs(t) {
					t = filepath.Join(d, t)
				}
				paths = append(paths, t)
			}
		}
		if filepath.Dir(d) == d {
			return paths
		}
	}
}

// cargoProjectCaches are the target directories configured for the user's repository (its own .cargo/config.toml and
// those of the folders above it): earlier builds there may have left compiled hidden tests in them.
func cargoProjectCaches(root string) []string {
	if !filepath.IsAbs(root) {
		return nil
	}
	return configCaches(root)
}
