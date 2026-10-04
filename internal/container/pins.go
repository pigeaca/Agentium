package container

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The toolchains a grading image holds (Pin.Toolchain).
const (
	ToolchainGo     = "go"
	ToolchainJDK    = "jdk"
	ToolchainRust   = "rust"
	ToolchainPython = "python"
)

// Platform is one architecture's image under a pin: its manifest's digest, its config's digest (the image ID the
// classic image store gives it), and the sum of its compressed layers (what a pull downloads at most).
type Platform struct {
	Manifest string // sha256:...
	Config   string // sha256:...
	Size     int64  // bytes, compressed
}

// Pin is one official base image, pinned by digest: the grading image is built locally from it (Recipe), adding git
// and less (decision 10). Changing a pin is a dependency change and needs approval (the supply-chain rule).
type Pin struct {
	Toolchain string
	// Version is what the version match compares with the host's (Match): Go's, Rust's and Python's major.minor, the
	// JDK's major.
	Version    string
	Repository string // as docker names it: golang, eclipse-temurin, rust, ghcr.io/astral-sh/uv
	Tag        string // the tag the digests were read from, for people only: Agentium never pulls or runs by tag
	// Index is the multi-arch index digest, or "" while it is not confirmed: the image is then pinned per
	// architecture, by its manifest's digest (Ref).
	Index     string
	Platforms map[string]Platform // by docker's architecture name: arm64, amd64
	Inside    string              // what the image holds, as far as it is confirmed
	// AptEstimate is about what the local build downloads through apt (its package lists, and git and less with what
	// they need), in bytes: an estimate, since only apt knows it once it runs.
	AptEstimate int64
	// Unconfirmed says what is still to be confirmed about the pin; "" when nothing is.
	Unconfirmed string
}

// pins is the pin table. The digests were read on 2026-10-04 with `docker manifest inspect -v` (manifests only, no
// layers); the Go and JDK images are the ones step 0 pulled (docs/research/2026-10-04-container-spike.md). Sizes are
// the compressed layers per architecture.
func pins() []Pin {
	return []Pin{
		{
			Toolchain: ToolchainGo, Version: "1.27", Repository: "golang", Tag: "1.27",
			Index: "sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190",
			Platforms: map[string]Platform{
				"arm64": {Manifest: "sha256:9fef6c185fa8277ea98a09c2067351103b002cce3a421771dceb241c25a22529", Config: "sha256:84349ebd3bf9b6ffc6163a249e24a1223608a0fe2b77ef7cbe90f468e3fcb71f", Size: 308550375},
				"amd64": {Manifest: "sha256:7bffdb405cd12940d2980daa49a86ef575ed4525a17ee7d0c9562547357ab46a", Config: "sha256:ecb923204467d17aa269ee9b123c80892bcd202eae287617a8baeb6afbd42a5e", Size: 316244813},
			},
			Inside: "Go 1.27.1, Debian 13 (trixie), gcc and git", AptEstimate: 15e6,
		},
		{
			Toolchain: ToolchainJDK, Version: "21", Repository: "eclipse-temurin", Tag: "21-jdk",
			Index: "sha256:3e3c176ffed168beb42c607be9bc1639b466cf00261a0fb04425562c9d0c5c2b",
			Platforms: map[string]Platform{
				"arm64": {Manifest: "sha256:681c5a2969ee6bcd535dac7d582ddbbc1ea81ee5d6187a426458ca796b345687", Config: "sha256:05e5dc64672d5a2233e7a3c7b527b43d6e7d0bb8231d21582c1729282404d259", Size: 224546198},
				"amd64": {Manifest: "sha256:442a743d9272be15c9872915eab0f7a1b6bb45b7c16c613acf172e2d7483061d", Config: "sha256:a4ab626836123d37d64448578f511211694b118a22936d328d870f8ac0f70825", Size: 226989503},
			},
			Inside: "Temurin 21.0.12.1, Ubuntu 26.04; no git", AptEstimate: 50e6,
		},
		{
			Toolchain: ToolchainRust, Version: "1.95", Repository: "rust", Tag: "1.95-slim",
			Platforms: map[string]Platform{
				"arm64": {Manifest: "sha256:039d198bab66a591902cafa2133bf7dce22dc51606fb200d7eac51e112d3c5da", Config: "sha256:97c9565543c4cafa69fbdefa703ada7b3929895b2f91192925b8aece495f0074", Size: 279315772},
				"amd64": {Manifest: "sha256:28846ec5a6bcfcddb93f403ba7071bd579787852b2f2ac3839965620e8bd9456", Config: "sha256:d02c49aa12bacc2cb79ca068d3ac4af21af3e68f6ba1e99d6f5a85ccd0684c59", Size: 320420856},
			},
			Inside: "Rust 1.95, Debian 13 (trixie) slim; no git", AptEstimate: 40e6,
			Unconfirmed: "the index digest (the docker CLI cannot read it without a pull; pinned per architecture meanwhile), and the patch version, which the build's check reads",
		},
		{
			Toolchain: ToolchainPython, Version: "3.12", Repository: "ghcr.io/astral-sh/uv", Tag: "0.11.28-python3.12-trixie-slim",
			Platforms: map[string]Platform{
				"arm64": {Manifest: "sha256:4a60515ef92706e953723fbe582d365638329a6398b62c68d0fd806c0b988741", Config: "sha256:0799a55d9b15a45482d6758777d260ec5c6b83fc0eb19bc9517c8ee203f1b06f", Size: 69225681},
				"amd64": {Manifest: "sha256:ff97bc064e32b33679dca53996f95a3544fd51d228cc41a009f5e19c192c26ca", Config: "sha256:927399947d7a721b1c8ff4fd1301d406291f8ef2ad45f4d5d89b1a50f900d019", Size: 70665771},
			},
			Inside: "uv 0.11.28, Python 3.12, Debian 13 (trixie) slim; no less", AptEstimate: 40e6,
			Unconfirmed: "the index digest (the docker CLI cannot read it without a pull; pinned per architecture meanwhile), and the Python patch and uv versions, which the build's check reads",
		},
	}
}

// Pins is the pin table, in a fixed order.
func Pins() []Pin { return pins() }

// PinFor is the pinned image of a toolchain.
func PinFor(toolchain string) (Pin, bool) {
	for _, p := range pins() {
		if p.Toolchain == toolchain {
			return p, true
		}
	}
	return Pin{}, false
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Ref is the pin's reference on a daemon of arch: <repository>@<index digest>, or the architecture's manifest digest
// while the index is not confirmed. Never a tag.
func (p Pin) Ref(arch string) (string, error) {
	if p.Index != "" {
		if !digestPattern.MatchString(p.Index) {
			return "", fmt.Errorf("pin %s: index %q is not a digest", p.Toolchain, p.Index)
		}
		return p.Repository + "@" + p.Index, nil
	}
	plat, ok := p.Platforms[arch]
	if !ok {
		return "", fmt.Errorf("pin %s: no image for %s", p.Toolchain, arch)
	}
	if !digestPattern.MatchString(plat.Manifest) {
		return "", fmt.Errorf("pin %s: %s manifest %q is not a digest", p.Toolchain, arch, plat.Manifest)
	}
	return p.Repository + "@" + plat.Manifest, nil
}

// Short is the pin's reference for people: the repository, the tag it was read from, and the digest's first 12
// digits.
func (p Pin) Short(arch string) string {
	ref, err := p.Ref(arch)
	if err != nil {
		return p.Repository + ":" + p.Tag + " (no image for " + arch + ")"
	}
	_, digest, _ := strings.Cut(ref, "@sha256:")
	return p.Repository + ":" + p.Tag + "@" + digest[:12]
}

// Name is the toolchain and version, for people: "go 1.27", "jdk 21".
func (p Pin) Name() string { return p.Toolchain + " " + p.Version }

// ToolchainOf is the toolchain a build-tool profile (buildtool.Profile.Name) grades with; "" for a profile no image
// holds yet (node).
func ToolchainOf(profile string) string {
	switch profile {
	case "go":
		return ToolchainGo
	case "maven", "gradle":
		return ToolchainJDK
	case "cargo":
		return ToolchainRust
	case "python":
		return ToolchainPython
	}
	return ""
}

// ErrNoImage: no pinned image holds the host's toolchain (open decision 6): the mode is refused, never graded with
// another version.
var ErrNoImage = errors.New("no pinned grading image matches")

var (
	goVersion     = regexp.MustCompile(`\bgo(\d+)\.(\d+)`)
	javaVersion   = regexp.MustCompile(`\bversion "(\d+)(?:\.(\d+))?`)
	rustVersion   = regexp.MustCompile(`^(?:rustc|cargo) (\d+)\.(\d+)`)
	pythonVersion = regexp.MustCompile(`^(?:Python )?(\d+)\.(\d+)`)
)

// ParseVersion reads a toolchain's version, as Pin.Version holds it, from what its tool printed (the host's, as
// pool.DetectToolchain records it, or the image's): "go1.27.1" is 1.27, `openjdk version "22.0.2"` is 22 (and
// `version "1.8.0_392"` is 8), "rustc 1.95.0 (...)" is 1.95, "Python 3.12.13" or "3.12.13" is 3.12. "" when it
// cannot be read.
func ParseVersion(toolchain, line string) string {
	line = strings.TrimSpace(line)
	pair := func(re *regexp.Regexp) string {
		m := re.FindStringSubmatch(line)
		if m == nil {
			return ""
		}
		return m[1] + "." + m[2]
	}
	switch toolchain {
	case ToolchainGo:
		return pair(goVersion)
	case ToolchainRust:
		return pair(rustVersion)
	case ToolchainPython:
		return pair(pythonVersion)
	case ToolchainJDK:
		m := javaVersion.FindStringSubmatch(line)
		if m == nil {
			return ""
		}
		if m[1] == "1" && m[2] != "" { // the old scheme: 1.8 is Java 8
			return m[2]
		}
		if _, err := strconv.Atoi(m[1]); err != nil {
			return ""
		}
		return m[1]
	}
	return ""
}

// hostTools are the tools whose versions pool.DetectToolchain records for a toolchain, in the order they are read.
func hostTools(toolchain string) []string {
	switch toolchain {
	case ToolchainGo:
		return []string{"go"}
	case ToolchainJDK:
		return []string{"java"}
	case ToolchainRust:
		return []string{"rustc", "cargo"}
	case ToolchainPython:
		return []string{"python"}
	}
	return nil
}

// Match finds the pinned images that grade a project's build tools (profile names, as buildtool.DetectIn gives them)
// with the toolchains the host's agent uses (host: tool to version line, as pool.DetectToolchain records them). Each
// image's toolchain must be the host's version: a profile no image holds, a host version that cannot be read, or one
// that no pin has, is ErrNoImage, naming the pinned versions (open decision 6). The pins come once each, in the
// table's order.
func Match(profiles []string, host map[string]string) ([]Pin, error) {
	var need []string
	var bad []string
	for _, profile := range profiles {
		tc := ToolchainOf(profile)
		if tc == "" {
			bad = append(bad, fmt.Sprintf("no pinned image grades %s projects", profile))
			continue
		}
		if !slices.Contains(need, tc) {
			need = append(need, tc)
		}
	}
	var out []Pin
	for _, p := range pins() {
		if !slices.Contains(need, p.Toolchain) {
			continue
		}
		have := ""
		for _, tool := range hostTools(p.Toolchain) {
			if have = ParseVersion(p.Toolchain, host[tool]); have != "" {
				break
			}
		}
		switch {
		case have == "":
			bad = append(bad, fmt.Sprintf("the host's %s version is unknown (%s did not answer), and the pinned image holds %s %s", p.Toolchain, strings.Join(hostTools(p.Toolchain), " or "), p.Toolchain, p.Version))
		case have != p.Version:
			bad = append(bad, fmt.Sprintf("the host's %s is %s, and the pinned image holds %s %s (%s)%s", p.Toolchain, have, p.Toolchain, p.Version, p.Repository+":"+p.Tag, matchHint(p)))
		default:
			out = append(out, p)
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("%w: %s; grading with another version would compare the agent's work against another toolchain, so the mode is refused (a new pin is a dependency change that needs approval)",
			ErrNoImage, strings.Join(bad, "; "))
	}
	return out, nil
}

// matchHint says how the host can match a pin, where it can.
func matchHint(p Pin) string {
	switch p.Toolchain {
	case ToolchainJDK:
		return ": point JAVA_HOME at a JDK " + p.Version + " to match it"
	case ToolchainRust:
		return ": `rustup default " + p.Version + "` matches it"
	case ToolchainPython:
		return ": install Python " + p.Version + " (uv python find picks the newest)"
	}
	return ""
}
