package buildtool

import "path/filepath"

// nodeProfile holds only the user's npm and pnpm caches, denied to agents in every project (the coordinator's default
// in the Python and TypeScript plan): a user's own `npm pack`, `npm install` of a local folder or pnpm store may hold
// their project's files, tests included. It detects nothing, so it is never selected; the TypeScript profile (that
// plan's step 4) takes these over with its runners, environment and warm-up.
func nodeProfile() Profile {
	return Profile{Name: "node", UserCaches: nodeCaches}
}

// nodeCaches are npm's cache (~/.npm, or npm_config_cache) and pnpm's content-addressed store, at its defaults (macOS,
// Linux, XDG_DATA_HOME, PNPM_HOME). Only the store: PNPM_HOME also holds the pnpm executable and global binaries.
func nodeCaches(environ []string, home string) []string {
	env := vars(environ)
	paths := []string{filepath.Join(home, ".npm"), filepath.Join(home, "Library", "pnpm", "store"),
		filepath.Join(home, ".local", "share", "pnpm", "store")}
	for _, v := range []string{env["npm_config_cache"], env["NPM_CONFIG_CACHE"]} {
		if filepath.IsAbs(v) {
			paths = append(paths, v)
		}
	}
	if v := env["XDG_DATA_HOME"]; filepath.IsAbs(v) {
		paths = append(paths, filepath.Join(v, "pnpm", "store"))
	}
	if v := env["PNPM_HOME"]; filepath.IsAbs(v) {
		paths = append(paths, filepath.Join(v, "store"))
	}
	return paths
}
