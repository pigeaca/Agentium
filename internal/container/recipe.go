package container

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// GradingPackages are what the local build adds to each pinned base through apt (decision 10): junit-pioneer's build
// runs `git describe` while it configures, and click's tests run `less`; the JDK and slim images have neither. Their
// versions are not pinned: the Debian and Ubuntu archives drop a version once an update replaces it, so a pinned build
// would fail within weeks. The build's check records the versions it got instead (Built.Packages).
const GradingPackages = "git less"

// recipeVersion names the build recipe. It is part of every grading image's hash, so a change to the recipe makes new
// tags and the old images are no longer used.
const recipeVersion = "agentium-grading-image-1"

// GradingRepository is the local repository of the grading images: never pushed, never pulled.
const GradingRepository = "agentium-grade"

// The labels on a grading image, read back by the check after the build and by every later use.
const (
	LabelRecipe    = "agentium.recipe"    // the recipe's hash (Recipe.Hash)
	LabelBase      = "agentium.base"      // the pinned base's reference
	LabelToolchain = "agentium.toolchain" // the pin's toolchain and version, "go 1.27"
)

// Recipe is the local build of one grading image: the pinned base, by digest, plus GradingPackages.
type Recipe struct {
	Pin        Pin
	Arch       string
	Base       string // the base's pinned reference (Pin.Ref)
	Dockerfile string
	Hash       string // sha256 of the Dockerfile, which names the base by digest: the content hash of base plus recipe
	Tag        string // agentium-grade:<toolchain><version>-<hash's first 12 digits>
}

// NewRecipe is the grading image's recipe for a pin on a daemon of arch.
func NewRecipe(p Pin, arch string) (Recipe, error) {
	base, err := p.Ref(arch)
	if err != nil {
		return Recipe{}, err
	}
	// Each command on its own line of one RUN, so the build's log shows which failed. No package is kept beyond the two
	// and what they need (--no-install-recommends), and apt's lists are removed, so the image holds no index that a
	// later grade could read as the archive's state.
	dockerfile := "# " + recipeVersion + ": the pinned official base, plus " + GradingPackages + " (container plan, decision 10).\n" +
		"FROM " + base + "\n" +
		"RUN set -eu; \\\n" +
		"    export DEBIAN_FRONTEND=noninteractive; \\\n" +
		"    apt-get update; \\\n" +
		"    apt-get install -y --no-install-recommends " + GradingPackages + "; \\\n" +
		"    apt-get clean; \\\n" +
		"    rm -rf /var/lib/apt/lists/*\n"
	sum := sha256.Sum256([]byte(dockerfile))
	hash := hex.EncodeToString(sum[:])
	return Recipe{Pin: p, Arch: arch, Base: base, Dockerfile: dockerfile, Hash: hash,
		Tag: GradingRepository + ":" + p.Toolchain + p.Version + "-" + hash[:12]}, nil
}

// labels are the grading image's labels.
func (r Recipe) labels() [][2]string {
	return [][2]string{{LabelRecipe, r.Hash}, {LabelBase, r.Base}, {LabelToolchain, r.Pin.Name()}}
}

// contextTar is the build's whole context: the Dockerfile alone, so nothing of the host goes into the build.
func (r Recipe) contextTar() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "Dockerfile", Mode: 0o644, Size: int64(len(r.Dockerfile)),
		ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err != nil {
		return nil, err
	}
	if _, err := tw.Write([]byte(r.Dockerfile)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildArgs is the build's docker call: the classic builder or BuildKit, whichever the client has (the image must land
// in the daemon either way, which the check confirms by its ID); never pulling the base (it is pulled by digest first)
// and never reading a context but the one on stdin.
func (r Recipe) buildArgs(iidFile string) []string {
	args := []string{"build", "--pull=false", "--force-rm", "--tag", r.Tag, "--iidfile", iidFile}
	for _, l := range r.labels() {
		args = append(args, "--label", l[0]+"="+l[1])
	}
	return append(args, "-")
}

// checkScript runs in the built image, offline and as the grade's user, and prints what the check reads: git's and
// less's versions, the packages' versions apt installed, and the toolchain's version.
func (r Recipe) checkScript() string {
	version := ""
	switch r.Pin.Toolchain {
	case ToolchainGo:
		version = "go env GOVERSION"
	case ToolchainJDK:
		version = "java -version 2>&1"
	case ToolchainRust:
		version = "rustc --version"
	case ToolchainPython:
		version = "python3 --version; uv --version"
	}
	return `printf '== git\n'; git --version
printf '== less\n'; less --version | head -n 1
printf '== packages\n'; dpkg-query -W -f '${Package}=${Version}\n' ` + GradingPackages + `
printf '== toolchain\n'; ` + version + `
printf '== end\n'
`
}

// readCheck reads checkScript's output: git and less run, and the toolchain is the pin's version.
func (r Recipe) readCheck(out string) (packages []string, toolchain string, err error) {
	s := sections(out)
	if _, ok := s["end"]; !ok {
		return nil, "", fmt.Errorf("the check did not finish")
	}
	var bad []string
	if git := s["git"]; len(git) != 1 || !strings.HasPrefix(git[0], "git version ") {
		bad = append(bad, fmt.Sprintf("git printed %q", git))
	}
	if less := s["less"]; len(less) != 1 || !strings.HasPrefix(less[0], "less ") {
		bad = append(bad, fmt.Sprintf("less printed %q", less))
	}
	for _, name := range strings.Fields(GradingPackages) {
		found := false
		for _, line := range s["packages"] {
			if pkg, v, ok := strings.Cut(line, "="); ok && pkg == name && v != "" {
				packages = append(packages, line)
				found = true
			}
		}
		if !found {
			bad = append(bad, "package "+name+" is not installed")
		}
	}
	found := ""
	for _, line := range s["toolchain"] {
		if v := ParseVersion(r.Pin.Toolchain, line); v != "" {
			found = v
			break
		}
	}
	switch {
	case found == "":
		bad = append(bad, fmt.Sprintf("no %s version in %q", r.Pin.Toolchain, s["toolchain"]))
	case found != r.Pin.Version:
		bad = append(bad, fmt.Sprintf("the image holds %s %s (%q), not the pinned %s", r.Pin.Toolchain, found, s["toolchain"], r.Pin.Version))
	default:
		var lines []string
		for _, line := range s["toolchain"] {
			if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, "warning") {
				lines = append(lines, line)
			}
		}
		toolchain = strings.Join(lines, "; ")
	}
	if len(bad) > 0 {
		return nil, "", fmt.Errorf("the built image %s fails its check: %s", r.Tag, strings.Join(bad, "; "))
	}
	return packages, toolchain, nil
}
