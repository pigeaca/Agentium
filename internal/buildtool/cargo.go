package buildtool

import (
	"errors"
	"fmt"
	"io"
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
// explicitly: `target` under the working folder for Agentium's own commands, <checkout>/target for the agent; and
// CARGO_BUILD_BUILD_DIR beside it, since `[build] build-dir` (cargo 1.95) puts test executables outside the target
// directory, and the variable beats the config.
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
		// Relative to the command's folder. CARGO_BUILD_BUILD_DIR as well: `[build] build-dir` puts test executables
		// outside the target directory, and the variable beats the config.
		CommandVars: func(string) []string { return []string{"CARGO_TARGET_DIR=target", "CARGO_BUILD_BUILD_DIR=target"} },
		AgentEnv: func(c AgentContext) []string {
			env := []string{"RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER="}
			if c.Repo != "" {
				env = append(env, "CARGO_TARGET_DIR="+filepath.Join(c.Repo, "target"), "CARGO_BUILD_BUILD_DIR="+filepath.Join(c.Repo, "target"))
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
// (SCCACHE_DIR and the defaults), target and build directories (from the environment and from every Cargo config), and
// Cargo's registry, git sources, credentials and, under its cache home, build/ (where `build.build-dir` puts test
// executables by default template). ~/.cargo/bin stays readable: the agent runs cargo and rustc through it.
func cargoCaches(environ []string, home string) []string {
	env := vars(environ)
	var paths []string
	for _, ch := range cargoHomes(environ, home) {
		paths = append(paths, filepath.Join(ch, "registry"), filepath.Join(ch, "git"), filepath.Join(ch, "credentials.toml"),
			filepath.Join(ch, "credentials"), filepath.Join(ch, "build"))
	}
	paths = append(paths, filepath.Join(home, "Library", "Caches", "Mozilla.sccache"), filepath.Join(home, ".cache", "sccache"))
	for _, v := range []string{env["SCCACHE_DIR"], env["CARGO_TARGET_DIR"], env["CARGO_BUILD_TARGET_DIR"], env["CARGO_BUILD_BUILD_DIR"]} {
		if filepath.IsAbs(v) {
			paths = append(paths, v)
		}
	}
	paths = append(paths, configDirs(userConfigFiles(environ, home))...)
	if xdg := env["XDG_CACHE_HOME"]; filepath.IsAbs(xdg) {
		paths = append(paths, filepath.Join(xdg, "sccache"))
	}
	return paths
}

// cargoHomes are the default Cargo home and CARGO_HOME: an earlier build may have used either.
func cargoHomes(environ []string, home string) []string {
	homes := []string{filepath.Join(home, ".cargo")}
	if ch := userHome(environ, "CARGO_HOME", homes[0]); ch != homes[0] {
		homes = append(homes, ch)
	}
	return homes
}

// buildDirSetting finds every `target-dir = "…"` and `build-dir = "…"` in a Cargo config, however it is written: under
// [build] (with or without a comment after the header), as a dotted key (build.target-dir), with quoted keys, in an
// inline table, with a trailing comment. It does not parse TOML: any line that sets a quoted value to a key ending in
// target-dir or build-dir counts, wherever it is, so it can only deny too much (one in another table, a commented-out
// line), never too little. Not seen: a value in a multi-line string, and settings given on a command line (--config).
var buildDirSetting = regexp.MustCompile(`(?:target|build)-dir["']?\s*=\s*(?:"([^"\n]*)"|'([^'\n]*)')`)

// maxConfigBytes bounds what is read of a Cargo config: a real one is a few lines. A larger file, or one that is not a
// regular file (a FIFO would block, /dev/zero never ends), is refused, because skipping it would deny too little.
const maxConfigBytes = 1 << 20

// configFile is a Cargo config; relative paths in it start at base.
type configFile struct{ path, base string }

// readConfig reads a config file within bounds; a missing file is nil, nil.
func readConfig(path string) ([]byte, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", path)
	case info.Size() > maxConfigBytes:
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxConfigBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err == nil && len(data) > maxConfigBytes {
		err = fmt.Errorf("%s grew past %d bytes", path, maxConfigBytes)
	}
	return data, err
}

// configDirs lists the target and build directories the config files name. A file that cannot be read is skipped here;
// CheckConfigs refuses the run before it gets this far.
func configDirs(files []configFile) []string {
	var dirs []string
	for _, f := range files {
		data, _ := readConfig(f.path)
		for _, m := range buildDirSetting.FindAllStringSubmatch(string(data), -1) {
			if d := m[1] + m[2]; d != "" {
				if !filepath.IsAbs(d) {
					d = filepath.Join(f.base, d)
				}
				dirs = append(dirs, d)
			}
		}
	}
	return dirs
}

// chainConfigFiles are the Cargo configs in dir and every folder above it, as Cargo reads them
// (<folder>/.cargo/config.toml, and the older .cargo/config); relative paths start at <folder>.
func chainConfigFiles(dir string) []configFile {
	var files []configFile
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		for _, name := range []string{"config.toml", "config"} {
			files = append(files, configFile{filepath.Join(d, ".cargo", name), d})
		}
		if filepath.Dir(d) == d {
			return files
		}
	}
}

// userConfigFiles are the configs in each Cargo home (relative paths start above it) and those above the home folder.
func userConfigFiles(environ []string, home string) []configFile {
	var files []configFile
	for _, ch := range cargoHomes(environ, home) {
		for _, name := range []string{"config.toml", "config"} {
			files = append(files, configFile{filepath.Join(ch, name), filepath.Dir(ch)})
		}
	}
	return append(files, chainConfigFiles(home)...)
}

// cargoProjectCaches are the target and build directories configured for the user's repository (its own
// .cargo/config.toml and those of the folders above it): earlier builds there may have left compiled hidden tests.
func cargoProjectCaches(root string) []string {
	if !filepath.IsAbs(root) {
		return nil
	}
	return configDirs(chainConfigFiles(root))
}

// CheckConfigs refuses a run on a Cargo project when a Cargo config that decides where builds put their output cannot
// be read within bounds (not a regular file, or over 1 MiB): without reading it Agentium cannot tell which folders to
// deny, and denying less would be unsafe, so it fails closed. root is the user's repository.
func CheckConfigs(environ []string, home, root string) error {
	files := userConfigFiles(environ, home)
	if filepath.IsAbs(root) {
		files = append(files, chainConfigFiles(root)...)
	}
	for _, f := range files {
		if _, err := readConfig(f.path); err != nil {
			return fmt.Errorf("Cargo config %s cannot be read safely (%w): Agentium cannot tell which build folders it names, so it will not run agents until it is fixed", f.path, err)
		}
	}
	return nil
}
