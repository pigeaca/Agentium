package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/buildtool"
)

// The shared deny list keeps its order (agents' settings list it as it comes): Agentium's paths, the agent's own data,
// the token's folder, the credential stores, the build tools' caches and the deps folder's denied parts, then the shared
// folders, which are also denied for writing with Agentium's paths and the deps folder.
func TestAgentDeniedOrderAndParts(t *testing.T) {
	home := "/golden/home"
	environ := []string{"HOME=" + home, "GH_CONFIG_DIR=/golden/gh", "GOCACHE=/golden/gocache"}
	d := AgentDenied{Deny: []string{"/golden/data/projects", "/golden/repo"}, AgentData: []string{"/golden/home/.agent/history"},
		SecretFile: "/golden/secrets/token", Home: home, AccountHome: "/golden/account", Environ: environ, Deps: "/golden/data/deps/1",
		Shared: []string{"/golden/shared/a", "/golden/shared/b"}}

	reads := d.Reads()
	head := append(append(append([]string{}, d.Deny...), d.AgentData...), "/golden/secrets")
	head = append(head, WithForms(CredentialPaths(home, "/golden/account"))...)
	head = append(head, "/golden/gh")
	if len(reads) < len(head)+len(d.Shared) || !slices.Equal(reads[:len(head)], head) {
		t.Fatalf("reads start %q\nwant %q", reads[:min(len(reads), len(head))], head)
	}
	if tail := reads[len(reads)-len(d.Shared):]; !slices.Equal(tail, d.Shared) {
		t.Errorf("reads end %q, want the shared folders %q", tail, d.Shared)
	}
	middle := reads[len(head) : len(reads)-len(d.Shared)]
	for _, p := range WithForms(append(buildtool.UserCaches(environ, home), buildtool.DepsDenied(d.Deps)...)) {
		if !slices.Contains(middle, p) {
			t.Errorf("reads lack %s (the user's build caches and the deps folder's denied parts)", p)
		}
	}
	if !slices.Contains(middle, "/golden/gocache") {
		t.Errorf("the user's GOCACHE is not denied: %q", middle)
	}
	if slices.Contains(reads, d.Deps) {
		t.Error("the deps folder itself is readable to the agent: only its denied parts are listed")
	}

	if got, want := d.Writes(), []string{"/golden/data/projects", "/golden/repo", "/golden/data/deps/1", "/golden/shared/a", "/golden/shared/b"}; !slices.Equal(got, want) {
		t.Errorf("writes %q, want %q", got, want)
	}
	if got, want := d.Credentials(), append(CredentialPaths(home, "/golden/account"), "/golden/secrets"); !slices.Equal(got, want) {
		t.Errorf("credentials %q, want %q", got, want)
	}

	// Without a token file, a deps folder or shared folders, those parts are simply absent.
	bare := AgentDenied{Deny: []string{"/golden/repo"}, Home: home}
	if slices.Contains(bare.Reads(), "/golden/secrets") || !slices.Equal(bare.Writes(), []string{"/golden/repo"}) ||
		!slices.Equal(bare.Credentials(), CredentialPaths(home, "")) {
		t.Errorf("a bare list: reads %q, writes %q", bare.Reads(), bare.Writes())
	}
}

func TestSharedTempDirs(t *testing.T) {
	both := func(names ...string) []string {
		var out []string
		for _, n := range names {
			out = append(out, filepath.Join("/tmp", n), filepath.Join("/private/tmp", n))
		}
		return out
	}
	base := both("claude-501", "claude", "cc-socks", "cc-socks-501", "cc-daemon-501")
	if got := SharedTempDirs(nil, 501); !slices.Equal(got, base) {
		t.Errorf("default: %q", got)
	}
	got := SharedTempDirs([]string{"CLAUDE_CODE_TMPDIR=/golden/cc", "XDG_RUNTIME_DIR=/golden/run"}, 501)
	if want := append(slices.Clone(base), "/golden/cc/claude-501", "/golden/cc/cc-socks", "/golden/run/cc-socks"); !slices.Equal(got, want) {
		t.Errorf("the user's own folders: %q\nwant %q", got, want)
	}
	// Relative values are ignored: they would name folders in wherever the agent stands.
	if got := SharedTempDirs([]string{"CLAUDE_CODE_TMPDIR=rel", "XDG_RUNTIME_DIR=rel"}, 501); !slices.Equal(got, base) {
		t.Errorf("relative values: %q", got)
	}
}

func TestSharedLogDirs(t *testing.T) {
	base := []string{"/h/.npm/_logs", "/h/.claude/debug"}
	for _, c := range []struct {
		loginConfig string
		want        []string
	}{
		{"", base},
		{"/h/.claude", base},
		{"/h/.claude/", base},
		{"/h/.claude-work", append(slices.Clone(base), "/h/.claude-work/debug")},
	} {
		if got := SharedLogDirs("/h", c.loginConfig); !slices.Equal(got, c.want) {
			t.Errorf("SharedLogDirs(/h, %q) = %q, want %q", c.loginConfig, got, c.want)
		}
	}
}

// Every path of the shared deny list is listed in its resolved form too, the form the macOS sandbox matches: here the
// home folder, Agentium's data, the deps folder and the shared folders are all reached through a link.
func TestAgentDeniedListsResolvedForms(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	d := AgentDenied{Deny: []string{filepath.Join(link, "data")}, AgentData: []string{filepath.Join(link, "agent-history")},
		SecretFile: filepath.Join(link, "secrets", "token"), Home: link, Environ: []string{"HOME=" + link},
		Deps: filepath.Join(link, "deps"), Shared: []string{filepath.Join(link, "shared")}}
	reads, writes := d.Reads(), d.Writes()
	for _, rel := range []string{"data", "agent-history", "secrets", ".ssh", "Library/Keychains", ".aws", ".cache/go-build", "shared"} {
		for _, p := range []string{filepath.Join(link, rel), filepath.Join(resolved, rel)} {
			if !slices.Contains(reads, p) {
				t.Errorf("reads lack %s", p)
			}
		}
	}
	for _, p := range buildtool.DepsDenied(d.Deps) {
		if !slices.Contains(reads, filepath.Join(resolved, strings.TrimPrefix(p, link))) {
			t.Errorf("reads lack the resolved form of %s", p)
		}
	}
	for _, rel := range []string{"data", "deps", "shared"} {
		for _, p := range []string{filepath.Join(link, rel), filepath.Join(resolved, rel)} {
			if !slices.Contains(writes, p) {
				t.Errorf("writes lack %s", p)
			}
		}
	}
}
