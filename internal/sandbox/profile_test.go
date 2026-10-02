package sandbox

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/runner"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// goldenProfile is a grade's profile with fixed paths that exist on no machine, so no symlink resolution adds forms:
// the data folder's grading copy, cache and deps, a fake home with gh moved by XDG_CONFIG_HOME, the user's repository
// and an other run's temp root denied.
func goldenProfile() Profile {
	return Profile{Tag: "agentium-0123456789abcdef", Home: "/golden/home", AccountHome: "/golden/account",
		Environ: []string{"PATH=/usr/bin:/bin", "XDG_CONFIG_HOME=/golden/xdg"},
		Data:    "/golden/data", Denied: []string{"/golden/repo", "/golden/home/go/pkg/mod", "/golden/t/ag-other", "/golden/data/records"},
		Copy: "/golden/data/records/r1/verify", Cache: "/golden/data/cache/grading/r1", Temp: "/golden/t/ag-0123456789",
		Deps: "/golden/data/deps/1", Loopback: true}
}

// TestProfileGolden pins the grading profile. It is the grader's whole security boundary: a change to the golden file
// is a security change, to be read line by line and never regenerated blindly (go test ./internal/sandbox -update).
func TestProfileGolden(t *testing.T) {
	for _, c := range []struct {
		file    string
		profile func() Profile
	}{
		{"profile-loopback.sb", goldenProfile},
		{"profile-closed.sb", func() Profile {
			p := goldenProfile()
			p.Loopback, p.Cache, p.Deps, p.AccountHome, p.Environ = false, "", "", "", nil
			return p
		}},
	} {
		got, err := c.profile().Render()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", c.file)
		if *update {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs from the rendered profile (a security change: check it line by line, then -update):\n%s", path, got)
		}
	}
}

// The rules a reviewer looks for first hold whatever the golden says: deny by default with the tag, no security
// server, no network but loopback, and the denied paths denied again inside the grade's folders (deps' Gradle home).
func TestProfileRules(t *testing.T) {
	text, err := goldenProfile().Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`(deny default (with message "agentium-0123456789abcdef"))`,
		`(allow network-outbound (remote ip "localhost:*"))`,
		"(deny file-read*\n  (subpath \"/golden/data/deps/1/gradle\")",
		`(subpath "/golden/home/Library/Keychains")`, `(subpath "/golden/account/Library/Keychains")`,
		`(subpath "/Library/Keychains")`, `(subpath "/golden/home/.config/pip")`, `(subpath "/golden/xdg/gh")`,
		`(subpath "/golden/home/.m2/settings.xml")`, `(subpath "/golden/home/.gradle/gradle.properties")`,
		`(subpath "/golden/home/.cargo/credentials.toml")`,
		`(allow file-write-data (regex #"^/dev/fd/[0-9]+$"))`, `(allow ipc-posix-sem (ipc-posix-name-prefix "/mp-"))`,
		`(allow ipc-posix-shm-read-data (ipc-posix-name "apple.shm.notification_center"))`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the profile lacks %s", want)
		}
	}
	for _, banned := range []string{"SecurityServer", "securityd", "(allow default", "(allow network*", `"*:*"`, "/dev/tty",
		"(allow file-write* (subpath \"/golden/data\")", "launchservicesd", "(allow ipc-posix-shm)", "(allow ipc-posix-shm ", "(allow ipc-posix-sem)",
		`"/dev/stdout"`, `"/dev/stderr"`} {
		if strings.Contains(text, banned) {
			t.Errorf("the profile holds %s", banned)
		}
	}
	p := goldenProfile()
	p.Loopback = false
	closed, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(closed, "(allow network") {
		t.Error("a profile without loopback allows network")
	}
}

// A profile that would not hold is refused before anything is written: a bad tag, a missing or relative folder, a
// path with a control character, an empty denied path (it would render as the current folder), or a writable folder
// that is or holds the home folder, the data folder, the deps, a credential store or a system folder, or lies inside
// the deps or a credential store.
func TestProfileRefusesWhatWouldNotHold(t *testing.T) {
	for name, change := range map[string]func(*Profile){
		"no tag":                      func(p *Profile) { p.Tag = "" },
		"a tag with a quote":          func(p *Profile) { p.Tag = `a"b` },
		"a tag with a space":          func(p *Profile) { p.Tag = "a b" },
		"no data folder":              func(p *Profile) { p.Data = "" },
		"no grading copy":             func(p *Profile) { p.Copy = "" },
		"no temp root":                func(p *Profile) { p.Temp = "" },
		"no home folder":              func(p *Profile) { p.Home = "" },
		"a relative copy":             func(p *Profile) { p.Copy = "data/verify" },
		"a relative denied path":      func(p *Profile) { p.Denied = append(p.Denied, "repo") },
		"a newline in a path":         func(p *Profile) { p.Copy = "/golden/data/records/r1/verify\n(allow default)" },
		"a newline in the env":        func(p *Profile) { p.Environ = []string{"XDG_CONFIG_HOME=/x\n(allow default)"} },
		"a temp root over the home":   func(p *Profile) { p.Temp = "/golden" },
		"a temp root that is home":    func(p *Profile) { p.Temp = "/golden/home" },
		"a copy that is the data":     func(p *Profile) { p.Copy = "/golden/data" },
		"a cache over the deps":       func(p *Profile) { p.Cache = "/golden/data/deps" },
		"a cache inside the deps":     func(p *Profile) { p.Cache = "/golden/data/deps/1/go" },
		"a temp root inside ~/.ssh":   func(p *Profile) { p.Temp = "/golden/home/.ssh/t" },
		"a copy over the keychains":   func(p *Profile) { p.Copy = "/golden/home/Library" },
		"a cache over moved gh":       func(p *Profile) { p.Cache = "/golden/xdg" },
		"the root as the temp folder": func(p *Profile) { p.Temp = "/" },
		"an empty denied path":        func(p *Profile) { p.Denied = append(p.Denied, "") },
		"/tmp as the temp root":       func(p *Profile) { p.Temp = "/tmp" },
		"/private/tmp as the temp":    func(p *Profile) { p.Temp = "/private/tmp" },
		"/private as the cache":       func(p *Profile) { p.Cache = "/private" },
		"/usr as the copy":            func(p *Profile) { p.Copy = "/usr" },
		"/Library as the cache":       func(p *Profile) { p.Cache = "/Library" },
		"/System as the cache":        func(p *Profile) { p.Cache = "/System" },
	} {
		p := goldenProfile()
		change(&p)
		if text, err := p.Render(); err == nil {
			t.Errorf("%s: rendered\n%s", name, text)
		}
	}
	// A quote or backslash in a path is escaped, not refused.
	p := goldenProfile()
	p.Denied = []string{`/golden/a "quoted\ folder`}
	text, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `(subpath "/golden/a \"quoted\\ folder")`) {
		t.Errorf("the quoted path is not escaped:\n%s", text)
	}
}

// The profile file is new, owner-only and outside every folder the grade writes: a build that could rewrite it would
// run the grade's next command unsandboxed.
func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	p := tempProfile(t, dir)
	file := filepath.Join(dir, "data", "records", "r1", "sandbox.sb")
	digest, err := p.WriteFile(file)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 || len(digest) != 64 {
		t.Fatalf("the profile file: %v, mode %v, digest %q", err, info.Mode().Perm(), digest)
	}
	if _, err := p.WriteFile(file); err == nil {
		t.Error("an existing profile file was replaced")
	}
	for _, inside := range []string{filepath.Join(p.Copy, "p.sb"), filepath.Join(p.Temp, "p.sb"), filepath.Join(p.Cache, "x", "p.sb")} {
		if _, err := p.WriteFile(inside); err == nil {
			t.Errorf("a profile file in a writable folder was written: %s", inside)
		}
	}
	if _, err := p.WriteFile("relative.sb"); err == nil {
		t.Error("a relative profile file was written")
	}
	missing := p
	missing.Cache = filepath.Join(dir, "missing")
	if _, err := missing.WriteFile(filepath.Join(dir, "other.sb")); err == nil {
		t.Error("a profile whose cache does not exist was written")
	}
}

// tempProfile is a profile over a real fake layout in dir: the data folder with this run's grading copy, cache (with a
// denied folder inside it, which must stay denied there) and records, another run's records and workspace, a shared cache, the deps (with a Gradle home and a module), a
// database; a fake home with credential stores; the user's repository; and a temp root. Every folder exists.
func tempProfile(t *testing.T, dir string) Profile {
	t.Helper()
	files := map[string]string{
		"data/records/r1/verify/main.txt":      "copy",
		"data/records/r1/transcript.jsonl":     "own record",
		"data/records/r2/verify/hidden_test.x": "another run's hidden test",
		"data/workspaces/r2/repo/f":            "another run's workspace",
		"data/cache/grading/r1/.keep":          "",
		"data/cache/grading/r1/private/entry":  "denied in the cache",
		"data/cache/shared/entry":              "shared cache",
		"data/deps/1/mod/lib.txt":              "dependency",
		"data/deps/1/gradle/caches/x.bin":      "compiled hidden tests",
		"data/agentium.db":                     "database",
		"home/.ssh/id_test":                    "key",
		"home/.aws/credentials":                "aws",
		"home/.config/gh/hosts.yml":            "gh",
		"home/.config/pip/pip.conf":            "pip",
		"home/.netrc":                          "netrc",
		"home/Library/Keychains/k":             "keychain",
		"home/.local/share/uv/python/ok":       "interpreter",
		"repo/solution.txt":                    "the user's repository",
		"t/.keep":                              "",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Profile{Tag: "agentium-test", Home: filepath.Join(dir, "home"), Environ: []string{"PATH=/usr/bin:/bin"},
		Data: filepath.Join(dir, "data"), Denied: []string{filepath.Join(dir, "repo"), filepath.Join(dir, "data", "cache", "grading", "r1", "private")},
		Copy: filepath.Join(dir, "data", "records", "r1", "verify"), Cache: filepath.Join(dir, "data", "cache", "grading", "r1"),
		Temp: filepath.Join(dir, "t"), Deps: filepath.Join(dir, "data", "deps", "1"), Loopback: true}
}

// CheckFile accepts the file only with the digest WriteFile returned: another digest, or a changed file, wraps
// ErrUnavailable.
func TestCheckFile(t *testing.T) {
	dir := t.TempDir()
	p := tempProfile(t, dir)
	file := filepath.Join(dir, "p.sb")
	digest, err := p.WriteFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(file, digest); err != nil {
		t.Fatalf("the file as written: %v", err)
	}
	other := strings.Repeat("0", 64)
	if err := CheckFile(file, other); !errors.Is(err, ErrUnavailable) {
		t.Errorf("another digest: %v, want ErrUnavailable", err)
	}
	if err := os.WriteFile(file, []byte("; changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(file, digest); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a changed file: %v, want ErrUnavailable", err)
	}
	if err := CheckFile(filepath.Join(dir, "missing.sb"), digest); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a missing file: %v, want ErrUnavailable", err)
	}
}

// Wrap runs a shell command or arguments through sandbox-exec by its absolute path, with the profile file and `--`
// before the command, and keeps the rest of the spec.
func TestWrapArgv(t *testing.T) {
	started := func(int) {}
	for _, c := range []struct {
		spec runner.Spec
		want []string
	}{
		{runner.Spec{Dir: "/w", Command: "go test ./...", Environ: []string{"A=1"}, Started: started},
			[]string{"/usr/bin/sandbox-exec", "-f", "/p/grade.sb", "--", "/bin/sh", "-c", "go test ./..."}},
		{runner.Spec{Dir: "/w", Args: []string{"-x", "y"}}, []string{"/usr/bin/sandbox-exec", "-f", "/p/grade.sb", "--", "-x", "y"}},
	} {
		got, err := Wrap(c.spec, "/p/grade.sb")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Args, c.want) || got.Command != "" || got.Dir != c.spec.Dir || !slices.Equal(got.Environ, c.spec.Environ) ||
			(c.spec.Started != nil) != (got.Started != nil) {
			t.Errorf("Wrap(%+v) = %+v, want args %q", c.spec, got, c.want)
		}
	}
	if _, err := Wrap(runner.Spec{Command: "true"}, "grade.sb"); err == nil {
		t.Error("a relative profile file was wrapped")
	}
	if _, err := Wrap(runner.Spec{}, "/p/grade.sb"); err == nil {
		t.Error("a spec without a command was wrapped")
	}
}
