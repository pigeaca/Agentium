package buildtool

import "path/filepath"

// cargoProfile holds Cargo's special cases.
//
// Offline: setup runs `cargo fetch` with CARGO_HOME in the deps folder; the agent gets that CARGO_HOME, which it only
// reads (a fetched build needs no writes there), and CARGO_NET_OFFLINE=true. Its builds go to target/ in its checkout.
// Proved in a real session on tokio-rs/bytes: `cargo test --offline` passed with no denials.
//
// Leaks: rustc wrappers such as sccache cache compiled crates, hidden tests included, in a folder agents could read,
// so Agentium's own commands run with every wrapper variable cleared, and the agent's environment never carries the
// user's wrapper either (it would write there). The user's sccache folders and CARGO_TARGET_DIR are denied to agents.
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
		AgentEnv: func(c AgentContext) []string {
			env := []string{"RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER="}
			if c.Deps != "" {
				env = append(env, "CARGO_HOME="+filepath.Join(c.Deps, "cargo"), "CARGO_NET_OFFLINE=true")
			}
			return env
		},
		Warm: func(_ func(string) bool, deps string) []WarmStep {
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
	for _, v := range []string{env["SCCACHE_DIR"], env["CARGO_TARGET_DIR"]} {
		if filepath.IsAbs(v) {
			paths = append(paths, v)
		}
	}
	if xdg := env["XDG_CACHE_HOME"]; filepath.IsAbs(xdg) {
		paths = append(paths, filepath.Join(xdg, "sccache"))
	}
	return paths
}
