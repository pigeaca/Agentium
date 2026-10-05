package buildtool

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/pigeaca/agentium/internal/container"
)

// The container grader's deps volume (container plan, step 3) mirrors the host's deps folder under /deps: m2 and
// mvnw-home (Maven), gradle-ro/modules-2 and gradle (Gradle's read-only cache and home), cargo (CARGO_HOME) and
// py/<key>/venv (Python), plus gomod, Go's module cache, which on the host is the user's own. So the host's warm-up
// steps, given /deps for the deps folder, are the container's too.
const (
	containerDeps   = container.DepsDir
	containerGoMod  = "gomod"
	containerHome   = container.CacheDir + "/home"
	containerPyRoot = "py"
)

// ContainerTop are the volume's top folders. The first seed makes them, owned by the grade's user, so that warm-ups
// (which run as it) can write into them: the volume's own root stays root's.
func ContainerTop() []string {
	return []string{containerGoMod, "m2", "mvnw-home", gradleRO, "gradle", "cargo", containerPyRoot}
}

// ContainerSeeds are what seeds a project's deps volume from the host, each a folder under a trusted root (the deps
// folder, the user's module cache) that no link may lead out of (container.Seed): only the platform-independent caches
// that step 0 proved (Go's module downloads, Maven's repository and wrapper, Gradle's modules-2, wrapper distributions and
// Spotless's p2-data, Cargo's registry), never what a build compiled, and never a Python venv (macOS builds). deps is
// the project's deps folder on the host; repo the checkout of the base (its go.sum names the Go modules); environ and
// home find the user's Go module cache. The entries are folders and Agentium's own Gradle settings.
func ContainerSeeds(selected []Profile, deps, repo string, environ []string, home string) ([]container.Seed, []container.SeedEntry) {
	var seeds []container.Seed
	entries := []container.SeedEntry{}
	for _, top := range ContainerTop() {
		entries = append(entries, container.SeedEntry{Name: top, Dir: true})
	}
	for _, p := range selected {
		switch p.Name {
		case "go":
			seeds = append(seeds, goModuleSeeds(repo, goModCache(environ, home))...)
		case "maven":
			seeds = append(seeds, container.Seed{Root: deps, Path: "m2", To: "m2"},
				container.Seed{Root: deps, Path: "mvnw-home", To: "mvnw-home"})
		case "gradle":
			seeds = append(seeds, container.Seed{Root: deps, Path: gradleRO + "/modules-2", To: gradleRO + "/modules-2"},
				container.Seed{Root: deps, Path: "gradle/wrapper/dists", To: "gradle/wrapper/dists"},
				container.Seed{Root: deps, Path: "gradle/caches/p2-data", To: "gradle/caches/p2-data"})
			entries = append(entries,
				container.SeedEntry{Name: "gradle/caches/modules-2", Link: "../../" + gradleRO + "/modules-2"},
				container.SeedEntry{Name: "gradle/gradle.properties", Body: []byte(gradleHomeProps + "org.gradle.cache.cleanup=false\n")},
				container.SeedEntry{Name: "gradle/init.d/agentium-no-cleanup.gradle", Body: []byte(noCleanupScript)},
				container.SeedEntry{Name: "gradle/" + resolveAllScriptName, Body: []byte(resolveAllScript)})
		case "cargo":
			seeds = append(seeds, container.Seed{Root: deps, Path: "cargo/registry", To: "cargo/registry"},
				container.Seed{Root: deps, Path: "cargo/git", To: "cargo/git"})
		}
	}
	return seeds, entries
}

// goModCache is the user's Go module cache: GOMODCACHE (the environment's, then `go env -w`'s), else the first GOPATH's
// pkg/mod, else ~/go/pkg/mod.
func goModCache(environ []string, home string) string {
	env := vars(environ)
	file := goEnvFile(env, home)
	for _, v := range []string{env["GOMODCACHE"], file["GOMODCACHE"]} {
		if filepath.IsAbs(v) {
			return v
		}
	}
	for _, v := range []string{env["GOPATH"], file["GOPATH"]} {
		if first := filepath.SplitList(v); len(first) > 0 && filepath.IsAbs(first[0]) {
			return filepath.Join(first[0], "pkg", "mod")
		}
	}
	return filepath.Join(home, "go", "pkg", "mod")
}

// goModuleSeeds are the module cache's download files of every module version the checkout's go.sum names (and
// nothing else of the user's cache): one seed per module's @v folder, which holds files only, filtered to those
// versions. What the cache lacks is left out; the warm-up's `go mod download` then says so.
func goModuleSeeds(repo, modcache string) []container.Seed {
	f, err := os.Open(filepath.Join(repo, "go.sum"))
	if err != nil {
		return nil
	}
	defer f.Close()
	versions := map[string]map[string]bool{} // escaped module path to escaped versions
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		mod, okm := escapeModule(fields[0])
		ver, okv := escapeModule(strings.TrimSuffix(fields[1], "/go.mod"))
		if !okm || !okv || mod == "" || ver == "" {
			continue
		}
		if versions[mod] == nil {
			versions[mod] = map[string]bool{}
		}
		versions[mod][ver] = true
	}
	mods := make([]string, 0, len(versions))
	for m := range versions {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	var seeds []container.Seed
	for _, mod := range mods {
		to := containerGoMod + "/cache/download/" + mod + "/@v"
		want := map[string]bool{to + "/list": true}
		for ver := range versions[mod] {
			for _, ext := range []string{".info", ".mod", ".zip", ".ziphash"} {
				want[to+"/"+ver+ext] = true
			}
		}
		seeds = append(seeds, container.Seed{Root: modcache, Path: "cache/download/" + mod + "/@v", To: to,
			Skip: func(name string) bool { return !want[name] }})
	}
	return seeds
}

// escapeModule is Go's case-encoding of a module path or version for the module cache ("Azure" is "!azure"); false for
// a path that cannot be a module's (it would leave the cache).
func escapeModule(s string) (string, bool) {
	if s == "" || strings.HasPrefix(s, "/") || strings.Contains(s, "\\") || strings.ContainsRune(s, 0) {
		return "", false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), true
}

// ContainerWarm is a deps warm-up in a container: its steps, run in the checkout of the base (a trusted commit) in the
// grading image, with the network and the deps volume writable; Venv is the Python venv it makes, under /deps; Notes
// say what it cannot warm.
type ContainerWarm struct {
	Steps []WarmStep
	Venv  string
	Notes []string
}

// ContainerWarmSteps are the warm-up's steps for a checkout at dir (on the host: only read here to choose them):
//   - Go: `go mod download`, offline, which unpacks the seeded downloads into /deps/gomod;
//   - Python: always here (the host's venvs are macOS builds), a uv-locked project only so far: `uv sync --frozen`
//     without the project itself, into /deps/py/<key>/venv, keyed by its pyproject.toml and uv.lock;
//   - Gradle: only when the host's warm-up failed (hostFailed; on this machine, for one, a base whose Gradle the host's
//     JDK cannot run), the host's own steps with /deps for the deps folder.
//
// Maven and Cargo need nothing beyond their seeds.
func ContainerWarmSteps(selected []Profile, dir string, hostFailed bool) ContainerWarm {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	common := []string{"HOME=" + containerHome, "TMPDIR=" + container.TmpDir}
	var w ContainerWarm
	for _, p := range selected {
		switch p.Name {
		case "go":
			if has("go.mod") {
				w.Steps = append(w.Steps, WarmStep{Command: "go mod download", Env: append(append([]string{}, common...),
					"GOMODCACHE="+path.Join(containerDeps, containerGoMod), "GOPROXY=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local",
					"GOCACHE="+container.CacheDir+"/go-build", "GOPATH="+container.CacheDir+"/gopath")})
			}
		case "python":
			if !has("uv.lock") || !has("pyproject.toml") {
				w.Notes = append(w.Notes, "Python's dependencies are warmed in a container for uv-locked projects only so far (no uv.lock here): grades may not import them")
				continue
			}
			key, err := pythonContainerKey(dir)
			if err != nil {
				w.Notes = append(w.Notes, "Python's dependencies were not warmed: "+err.Error())
				continue
			}
			w.Venv = path.Join(containerDeps, containerPyRoot, key, "venv")
			w.Steps = append(w.Steps, WarmStep{Command: "uv sync --frozen --no-install-project --no-install-workspace --no-install-local",
				Env: append(append([]string{}, common...), "UV_PROJECT_ENVIRONMENT="+w.Venv, "UV_CACHE_DIR="+container.CacheDir+"/uv",
					"UV_PYTHON_DOWNLOADS=never", "UV_PYTHON_PREFERENCE=only-system", "UV_LINK_MODE=copy", "UV_NO_PROGRESS=1")})
		case "gradle":
			if hostFailed && p.Warm != nil {
				for _, step := range p.Warm(dir, containerDeps, has) {
					step.Env = append(append([]string{}, common...), step.Env...)
					w.Steps = append(w.Steps, step)
				}
			}
		}
	}
	return w
}

// pythonContainerKey keys a project's container venv by what decides its content: pyproject.toml and uv.lock.
func pythonContainerKey(dir string) (string, error) {
	h := sha256.New()
	for _, name := range []string{"pyproject.toml", "uv.lock"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		h.Write([]byte(name + "\x00"))
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
