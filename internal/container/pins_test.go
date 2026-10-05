package container

import (
	"errors"
	"strings"
	"testing"
)

// Every pin is by digest for both architectures, its reference is never a tag, and an entry without its index digest
// says so.
func TestPinTable(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Pins() {
		if seen[p.Toolchain] {
			t.Errorf("%s pinned twice", p.Toolchain)
		}
		seen[p.Toolchain] = true
		if p.Version == "" || p.Repository == "" || p.Tag == "" || p.Inside == "" || p.AptEstimate <= 0 {
			t.Errorf("%s: incomplete %+v", p.Toolchain, p)
		}
		if p.Index != "" && !digestPattern.MatchString(p.Index) {
			t.Errorf("%s: index %q", p.Toolchain, p.Index)
		}
		if p.Index == "" && !strings.Contains(p.Unconfirmed, "index digest") {
			t.Errorf("%s: no index digest, and Unconfirmed does not say so: %q", p.Toolchain, p.Unconfirmed)
		}
		for _, arch := range []string{"arm64", "amd64"} {
			plat, ok := p.Platforms[arch]
			if !ok || !digestPattern.MatchString(plat.Manifest) || !digestPattern.MatchString(plat.Config) || plat.Size < 10e6 {
				t.Errorf("%s/%s: %+v", p.Toolchain, arch, plat)
			}
			ref, err := p.Ref(arch)
			if err != nil || !digestRef.MatchString(ref) || strings.Contains(ref, ":"+p.Tag) {
				t.Errorf("%s/%s: ref %q, %v", p.Toolchain, arch, ref, err)
			}
			if r, err := NewRecipe(p, arch); err != nil || !strings.Contains(r.Dockerfile, "FROM "+ref+"\n") {
				t.Errorf("%s/%s: recipe %q, %v", p.Toolchain, arch, r.Dockerfile, err)
			}
		}
		if _, err := p.Ref("s390x"); p.Index == "" && err == nil {
			t.Errorf("%s: a reference for an architecture it has no image for", p.Toolchain)
		}
	}
	for _, tc := range []string{ToolchainGo, ToolchainJDK, ToolchainRust, ToolchainPython} {
		if !seen[tc] {
			t.Errorf("no pin for %s", tc)
		}
	}
	// The Go image's index is the one the driver's real tests run (goImage).
	if goPin, _ := PinFor(ToolchainGo); goPin.Repository+"@"+goPin.Index != goImage {
		t.Errorf("the Go pin is %s@%s, the tests' image %s", goPin.Repository, goPin.Index, goImage)
	}
	// A tag in place of a digest is refused.
	bad := Pin{Toolchain: "go", Repository: "golang", Index: "1.27"}
	if _, err := bad.Ref("arm64"); err == nil {
		t.Error("a pin by tag was accepted")
	}
	bad = Pin{Toolchain: "go", Repository: "golang", Platforms: map[string]Platform{"arm64": {Manifest: "latest"}}}
	if _, err := bad.Ref("arm64"); err == nil {
		t.Error("a platform pin by tag was accepted")
	}
}

func TestParseVersion(t *testing.T) {
	for _, tt := range []struct{ toolchain, line, want string }{
		{ToolchainGo, "go1.27.1", "1.27"},
		{ToolchainGo, "go1.28rc1", "1.28"},
		{ToolchainGo, "devel", ""},
		{ToolchainJDK, `openjdk version "22.0.2" 2024-07-16`, "22"},
		{ToolchainJDK, `openjdk version "21" 2023-09-19`, "21"},
		{ToolchainJDK, `java version "1.8.0_392"`, "8"},
		{ToolchainJDK, "Picked up JAVA_TOOL_OPTIONS", ""},
		{ToolchainRust, "rustc 1.95.0 (59807616e 2026-04-14)", "1.95"},
		{ToolchainRust, "cargo 1.95.0 (f2d3ce0bd 2026-03-21)", "1.95"},
		{ToolchainPython, "3.12.13", "3.12"},
		{ToolchainPython, "Python 3.9.6", "3.9"},
		{ToolchainPython, "uv 0.11.28", ""},
		{"node", "v22.1.0", ""},
	} {
		if got := ParseVersion(tt.toolchain, tt.line); got != tt.want {
			t.Errorf("ParseVersion(%s, %q) = %q, want %q", tt.toolchain, tt.line, got, tt.want)
		}
	}
}

// The version match (open decision 6): the image's toolchain is the host's, or the mode is refused, naming the pinned
// versions.
func TestMatch(t *testing.T) {
	host := map[string]string{"go": "go1.27.1", "java": `openjdk version "21.0.6" 2025-01-21`, "rustc": "rustc 1.95.0 (x)", "cargo": "cargo 1.95.0 (y)", "python": "3.12.13"}
	got, err := Match([]string{"gradle", "go", "maven", "cargo", "python"}, host)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name())
	}
	if strings.Join(names, ",") != "go 1.27,jdk 21,rust 1.95,python 3.12" {
		t.Errorf("pins %v", names)
	}
	// Rust's version from cargo when rustc did not answer.
	if got, err := Match([]string{"cargo"}, map[string]string{"cargo": "cargo 1.95.0"}); err != nil || len(got) != 1 {
		t.Errorf("cargo only: %v, %v", got, err)
	}
	for _, tt := range []struct {
		name     string
		profiles []string
		host     map[string]string
		want     []string
	}{
		{"this host's JDK 22", []string{"gradle"}, map[string]string{"java": `openjdk version "22.0.2" 2024-07-16`}, []string{"the host's jdk is 22", "jdk 21", "eclipse-temurin:21-jdk", "JAVA_HOME"}},
		{"Rust 1.99", []string{"cargo"}, map[string]string{"rustc": "rustc 1.99.0 (z)"}, []string{"the host's rust is 1.99", "rust 1.95"}},
		{"Go 1.28", []string{"go"}, map[string]string{"go": "go1.28.0"}, []string{"the host's go is 1.28", "go 1.27"}},
		{"Python 3.9", []string{"python"}, map[string]string{"python": "Python 3.9.6"}, []string{"the host's python is 3.9", "python 3.12"}},
		{"unknown", []string{"go"}, map[string]string{}, []string{"the host's go version is unknown", "go 1.27"}},
		{"node", []string{"node"}, host, []string{"no pinned image grades node projects"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Match(tt.profiles, tt.host)
			if !errors.Is(err, ErrNoImage) || got != nil {
				t.Fatalf("Match = %v, %v; want ErrNoImage", got, err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%q lacks %q", err, w)
				}
			}
		})
	}
	// One mismatch refuses the whole set: nothing is graded with another version.
	if got, err := Match([]string{"go", "gradle"}, map[string]string{"go": "go1.27.1", "java": `openjdk version "22"`}); err == nil || got != nil {
		t.Errorf("a partial match was accepted: %v", got)
	}
}
