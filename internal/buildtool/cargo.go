package buildtool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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
	paths = append(paths, resolveBuildDir(env["CARGO_BUILD_BUILD_DIR"], "", nil, cargoHomes(environ, home))...)
	for _, v := range []string{env["SCCACHE_DIR"], env["CARGO_TARGET_DIR"], env["CARGO_BUILD_TARGET_DIR"]} {
		if filepath.IsAbs(v) {
			paths = append(paths, v)
		}
	}
	// Best effort here (no error path); ProjectCaches reads the same files and fails closed, and a run needs it first.
	userDirs, _ := configDirs(userConfigFiles(environ, home), nil, cargoHomes(environ, home))
	paths = append(paths, userDirs...)
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

// readConfig reads a config file within bounds; a missing file is nil, nil. The file is opened without blocking and
// checked once opened (a FIFO swapped in after a Stat would block a plain open forever, and a link could change under
// a Stat before the Open): it must be a regular file under the limit.
func readConfig(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", path)
	case info.Size() > maxConfigBytes:
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxConfigBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err == nil && len(data) > maxConfigBytes {
		err = fmt.Errorf("%s grew past %d bytes", path, maxConfigBytes)
	}
	return data, err
}

// resolveBuildDir turns a build-dir or target-dir value into the folder to deny. Cargo expands {workspace-root} (each
// of roots: the user's repository and its worktrees) and {cargo-cache-home} (each Cargo home); {workspace-path-hash}
// and any other template names a folder that does not exist beforehand, so the value is cut at the first `{` that is
// left and the folder before it is denied (the parent of every such folder, which holds them all). Relative values start
// at base.
func resolveBuildDir(value, base string, roots, homes []string) []string {
	candidates := []string{value}
	expand := func(name string, with []string) {
		var next []string
		for _, c := range candidates {
			if !strings.Contains(c, name) || len(with) == 0 {
				next = append(next, c) // unknown here: cut at it below
				continue
			}
			for _, w := range with {
				next = append(next, strings.ReplaceAll(c, name, w))
			}
		}
		candidates = next
	}
	expand("{workspace-root}", roots)
	expand("{cargo-cache-home}", homes)
	var out []string
	for _, c := range candidates {
		if i := strings.Index(c, "{"); i >= 0 {
			c = c[:i]
		}
		if c == "" {
			continue
		}
		if !filepath.IsAbs(c) {
			if base == "" {
				continue // a relative environment value means nothing without a folder to start from
			}
			c = filepath.Join(base, c)
		}
		out = append(out, filepath.Clean(c))
	}
	return out
}

// configDirs lists the target and build directories the config files name, with templates resolved (resolveBuildDir). A
// file that cannot be read within bounds is an error, not skipped: skipping would deny too little.
func configDirs(files []configFile, roots, homes []string) ([]string, error) {
	var dirs []string
	var errs []error
	for _, f := range files {
		data, err := readConfig(f.path)
		if err != nil {
			errs = append(errs, fmt.Errorf("Cargo config %s cannot be read safely (%w): make it a regular file under 1 MiB, or move it away; Agentium cannot tell which build folders it names, so it will not run agents until then", f.path, err))
			continue
		}
		for _, m := range buildDirSetting.FindAllStringSubmatch(string(data), -1) {
			dirs = append(dirs, resolveBuildDir(m[1]+m[2], f.base, roots, homes)...)
		}
	}
	return dirs, errors.Join(errs...)
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

// cargoProjectCaches are the target and build directories that the user's Cargo configs (the Cargo homes', those above
// the home folder, and those of the repository and the folders above it) and CARGO_BUILD_BUILD_DIR name for the user's
// repository and its worktrees (roots): earlier builds there may have left compiled hidden tests. It fails closed on a
// config it cannot read within bounds.
func cargoProjectCaches(environ []string, home string, roots []string) ([]string, error) {
	files := userConfigFiles(environ, home)
	var abs []string
	for _, root := range roots {
		if filepath.IsAbs(root) {
			abs = append(abs, root)
			files = append(files, chainConfigFiles(root)...)
		}
	}
	homes := cargoHomes(environ, home)
	dirs, err := configDirs(files, abs, homes)
	if v := vars(environ)["CARGO_BUILD_BUILD_DIR"]; v != "" { // a templated value too, not skipped
		dirs = append(dirs, resolveBuildDir(v, "", abs, homes)...)
	}
	return dirs, err
}
