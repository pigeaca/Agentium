package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
)

// These tests run real sandbox-exec processes under the grading profile, on a fake layout (tempProfile), and check
// each deny the isolation plan names: a hostile build script cannot write through links into the deps, the data folder
// or another /tmp entry, cannot read credentials (the keychain among them), other runs' records or the shared cache,
// and cannot reach the network. Probes only open, list or stat; no real secret is ever read.

// needSandbox skips unless sandbox-exec can apply a profile here (macOS, not nested in another sandbox).
func needSandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the macOS sandbox")
	}
	if testing.Short() {
		t.Skip("starts sandboxed processes")
	}
	if _, err := os.Stat(Exec); err != nil {
		t.Skipf("%s: %v", Exec, err)
	}
	if err := exec.Command(Exec, "-p", "(version 1)(allow default)", "/usr/bin/true").Run(); err != nil {
		t.Skipf("sandbox-exec cannot apply a profile here (nested in a sandbox?): %v", err)
	}
}

// grade is a written profile over a fake layout, and a way to run commands under it.
type grade struct {
	t       *testing.T
	dir     string
	profile Profile
	file    string
}

func newGrade(t *testing.T, change func(*Profile)) grade {
	t.Helper()
	dir := t.TempDir()
	p := tempProfile(t, dir)
	if change != nil {
		change(&p)
	}
	tag, err := NewTag()
	if err != nil {
		t.Fatal(err)
	}
	p.Tag = tag
	file := filepath.Join(dir, "data", "records", "r1", "sandbox.sb")
	if _, err := p.WriteFile(file); err != nil {
		t.Fatal(err)
	}
	return grade{t: t, dir: dir, profile: p, file: file}
}

// run runs args under the profile, from the grading copy, and returns the exit code and output.
func (g grade) run(environ []string, args ...string) (int, string) {
	g.t.Helper()
	var out bytes.Buffer
	if environ == nil {
		environ = []string{"PATH=/usr/bin:/bin", "HOME=" + g.profile.Home}
	}
	spec, err := Wrap(runner.Spec{Dir: g.profile.Copy, Args: args, Environ: environ, Timeout: time.Minute, Output: &out}, g.file)
	if err != nil {
		g.t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), spec)
	if err != nil {
		g.t.Fatal(err)
	}
	return result.ExitCode, out.String()
}

// sh runs a shell command under the profile.
func (g grade) sh(command string) (int, string) {
	g.t.Helper()
	return g.run(nil, "/bin/sh", "-c", command)
}

func (g grade) path(name string) string { return filepath.Join(g.dir, name) }

// The profile lets a grade do its work: run a shell and tools, read the system and the deps, and write the grading
// copy, its cache and its temp root; and every command really ran sandboxed (the canary passes).
func TestSandboxAllowsTheGradesWork(t *testing.T) {
	needSandbox(t)
	g := newGrade(t, nil)
	if err := Canary(context.Background(), g.file, g.profile); err != nil {
		t.Fatalf("the canary: %v", err)
	}
	for _, command := range []string{
		"echo built > out.txt && cat main.txt out.txt",
		"mkdir -p sub/dir && touch sub/dir/f && rm -r sub",
		"echo c > " + strconv.Quote(filepath.Join(g.profile.Cache, "entry")),
		"echo t > " + strconv.Quote(filepath.Join(g.profile.Temp, "tmpfile")),
		"cat " + strconv.Quote(g.path("data/deps/1/mod/lib.txt")),
		"ls /usr/bin >/dev/null && cat /etc/hosts >/dev/null && echo x >/dev/null",
		"cat " + strconv.Quote(g.path("home/.local/share/uv/python/ok")),
		"id -un >/dev/null && uname -a >/dev/null && pwd",
	} {
		if code, out := g.sh(command); code != 0 {
			t.Errorf("%s: exit %d: %s", command, code, out)
		}
	}
}

// What the grade may not read: the data folder outside its own folders (its own records, other runs' records and
// workspaces, the shared cache, the database, the deps' Gradle home), the user's repository, and the credential stores.
// What it may not write: anything else, the deps and the data folder above all, and through links planted in the
// grading copy or hard links into it.
func TestSandboxDeniesAHostileBuild(t *testing.T) {
	needSandbox(t)
	g := newGrade(t, nil)
	other := filepath.Join("/tmp", fmt.Sprintf("agentium-sandbox-other-%d", os.Getpid())) // another run's temp root
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(other) })
	for name, target := range map[string]string{"to-deps": "data/deps/1", "to-shared": "data/cache/shared", "to-ssh": "home/.ssh",
		"to-records": "data/records/r2", "to-data": "data"} {
		if err := os.Symlink(g.path(target), filepath.Join(g.profile.Copy, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(other, filepath.Join(g.profile.Copy, "to-tmp")); err != nil {
		t.Fatal(err)
	}
	unreadable := []string{"data/records/r1/transcript.jsonl", "data/records/r2/verify/hidden_test.x", "data/workspaces/r2/repo/f",
		"data/cache/shared/entry", "data/agentium.db", "data/deps/1/gradle/caches/x.bin", "repo/solution.txt",
		"home/.ssh/id_test", "home/.aws/credentials", "home/.config/gh/hosts.yml", "home/.config/pip/pip.conf", "home/.netrc",
		"home/Library/Keychains/k"}
	for _, name := range unreadable {
		if code, _ := g.sh("cat " + strconv.Quote(g.path(name)) + " >/dev/null"); code == 0 {
			t.Errorf("the grade reads %s", name)
		}
	}
	for _, name := range []string{"data", "data/records", "data/cache", "home/.ssh", "home/Library/Keychains", "repo"} {
		if code, _ := g.sh("ls " + strconv.Quote(g.path(name)) + " >/dev/null"); code == 0 {
			t.Errorf("the grade lists %s", name)
		}
	}
	for _, link := range []string{"to-shared/entry", "to-ssh/id_test", "to-records/verify/hidden_test.x"} {
		if code, _ := g.sh("cat " + link + " >/dev/null"); code == 0 {
			t.Errorf("the grade reads through the planted link %s", link)
		}
	}
	writes := map[string]string{
		"the deps":                    "echo x >> " + strconv.Quote(g.path("data/deps/1/mod/lib.txt")),
		"a new file in the deps":      "touch " + strconv.Quote(g.path("data/deps/1/mod/new")),
		"the shared cache":            "echo x >> " + strconv.Quote(g.path("data/cache/shared/entry")),
		"the data folder":             "touch " + strconv.Quote(g.path("data/planted")),
		"its own records":             "echo x >> " + strconv.Quote(g.path("data/records/r1/transcript.jsonl")),
		"the profile file":            "echo '(allow default)' >> " + strconv.Quote(g.file),
		"the user's repository":       "touch " + strconv.Quote(g.path("repo/planted")),
		"the home folder":             "touch " + strconv.Quote(g.path("home/planted")),
		"another /tmp entry":          "touch " + strconv.Quote(filepath.Join(other, "planted")),
		"the user's temp folder":      "touch \"$(getconf DARWIN_USER_TEMP_DIR)/agentium-sandbox-planted\"",
		"the deps through a link":     "echo x >> to-deps/mod/lib.txt",
		"the deps' folder via a link": "touch to-deps/mod/planted",
		"the cache through a link":    "echo x >> to-shared/entry",
		"the data through a link":     "touch to-data/planted",
		"/tmp through a link":         "touch to-tmp/planted",
		"the deps by a hard link":     "ln " + strconv.Quote(g.path("data/deps/1/mod/lib.txt")) + " hard && echo x >> hard",
	}
	for what, command := range writes {
		if code, _ := g.sh(command); code == 0 {
			t.Errorf("the grade writes %s: %s", what, command)
		}
	}
	if data, err := os.ReadFile(g.path("data/deps/1/mod/lib.txt")); err != nil || string(data) != "dependency" {
		t.Errorf("the deps changed: %q %v", data, err)
	}
	for _, p := range []string{g.path("data/planted"), g.path("data/deps/1/mod/new"), g.path("data/deps/1/mod/planted"),
		g.path("repo/planted"), g.path("home/planted"), filepath.Join(other, "planted")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	if tmp, err := exec.Command("getconf", "DARWIN_USER_TEMP_DIR").Output(); err == nil {
		planted := filepath.Join(strings.TrimSpace(string(tmp)), "agentium-sandbox-planted")
		if _, err := os.Lstat(planted); err == nil {
			os.Remove(planted)
			t.Errorf("%s was written", planted)
		}
	}
	// The grade cannot signal a process outside its sandbox: this test's own.
	if code, _ := g.sh("kill -0 " + strconv.Itoa(os.Getpid())); code == 0 {
		t.Error("the grade can signal a process outside the sandbox")
	}
}

// No network beyond loopback: a TCP connection to an outside address (TEST-NET-1 and -2, which route nowhere) is refused by
// the sandbox at once, with Loopback a server on localhost works, and
// without it even that is denied. The wildcard-bind limit (Profile.Loopback) is not tested: it is a known gap.
func TestSandboxNetwork(t *testing.T) {
	needSandbox(t)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probe := func(g grade, mode, arg string) (int, string) {
		return g.run([]string{"PATH=/usr/bin:/bin", helperVar + "=" + mode}, bin, arg)
	}
	open := newGrade(t, nil)
	for _, addr := range []string{"192.0.2.1:443", "198.51.100.1:53"} {
		start := time.Now()
		code, out := probe(open, "dial", addr)
		if code == 0 {
			t.Errorf("the grade connects to %s", addr)
		} else if !strings.Contains(out, "not permitted") || time.Since(start) > 10*time.Second {
			t.Errorf("connecting to %s fails, but not by the sandbox (%v): %s", addr, time.Since(start), out)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0"} {
		if code, out := probe(open, "serve", addr); code != 0 {
			t.Errorf("with loopback, a server on %s: exit %d: %s", addr, code, out)
		}
	}
	closed := newGrade(t, func(p *Profile) { p.Loopback = false })
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		if code, _ := probe(closed, "serve", addr); code == 0 {
			t.Errorf("without loopback, a server on %s works", addr)
		}
	}
	if code, _ := probe(closed, "dial", "192.0.2.1:443"); code == 0 {
		t.Error("without loopback, the grade connects out")
	}
}

// The keychain stays shut on this Mac's own account, without reading any of it: under the profile, with the real
// home folder, the login keychain's folder cannot be listed, `security show-keychain-info` on it fails (lock settings
// only, never items), and `security list-keychains` no longer names it (the security server cannot even be looked
// up). Outside, the same probes succeed, so the test sees a difference this machine can show. Only exit statuses and
// whether a listing names the login keychain are looked at.
func TestSandboxDeniesTheKeychain(t *testing.T) {
	needSandbox(t)
	account, err := user.Current()
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		t.Skipf("the account's home folder: %v", err)
	}
	keychains := filepath.Join(account.HomeDir, "Library", "Keychains")
	login := filepath.Join(keychains, "login.keychain-db")
	run := func(args ...string) (int, bool) {
		var out bytes.Buffer
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + account.HomeDir}
		cmd.Stdout = &out
		err := cmd.Run()
		lists := strings.Contains(out.String(), "/Library/Keychains/login.keychain")
		out.Reset() // never kept or logged
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, lists
		case errors.As(err, &exit):
			return exit.ExitCode(), lists
		}
		return -1, lists
	}
	if code, _ := run("/bin/ls", keychains); code != 0 {
		t.Skipf("this account's keychain folder cannot be listed outside a sandbox (exit %d)", code)
	}
	if code, _ := run("/usr/bin/security", "show-keychain-info", login); code != 0 {
		t.Skipf("this account's login keychain cannot be opened by path outside a sandbox (exit %d)", code)
	}
	if _, lists := run("/usr/bin/security", "list-keychains", "-d", "user"); !lists {
		t.Skip("this account's search list has no login keychain")
	}
	g := newGrade(t, func(p *Profile) { p.Home = account.HomeDir })
	sandboxed := func(args ...string) (int, bool) {
		return run(append([]string{Exec, "-f", g.file}, args...)...)
	}
	if code, _ := sandboxed("/bin/ls", keychains); code == 0 {
		t.Error("the grade lists the login keychain's folder")
	}
	if code, _ := sandboxed("/usr/bin/security", "show-keychain-info", login); code == 0 {
		t.Error("the grade opens the login keychain by its path")
	}
	if _, lists := sandboxed("/usr/bin/security", "list-keychains", "-d", "user"); lists {
		t.Error("the grade's search list names the login keychain")
	}
	// HOME redirected: the account's own keychain folder stays denied through AccountHome.
	moved := newGrade(t, func(p *Profile) { p.AccountHome = account.HomeDir })
	if code, _ := run(Exec, "-f", moved.file, "/bin/ls", keychains); code == 0 {
		t.Error("with HOME redirected, the grade lists the account's keychain folder")
	}
}

// The canary turns a sandbox that does not hold into ErrUnavailable, never a pass: a missing profile file, one the
// system refuses, and one that allows everything (no deny reaches the data folder).
func TestCanaryRefusesASandboxThatDoesNotHold(t *testing.T) {
	needSandbox(t)
	g := newGrade(t, nil)
	write := func(name, text string) string {
		path := g.path(name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, file := range map[string]string{
		"a missing file":     g.path("missing.sb"),
		"a broken profile":   write("broken.sb", "(version 1)\n(allow nothing-at-all\n"),
		"allow everything":   write("open.sb", "(version 1)\n(allow default)\n"),
		"no write to temp":   write("nowrite.sb", "(version 1)\n(allow default)\n(deny file-write*)\n"),
		"reads but no write": write("readable.sb", "(version 1)\n(allow default)\n(deny file-write* (subpath "+quote(g.profile.Data)+"))\n"),
	} {
		err := Canary(context.Background(), file, g.profile)
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: the canary returns %v, want ErrUnavailable", name, err)
		}
	}
	for _, p := range []string{filepath.Join(g.profile.Data, ".agentium-canary-"+g.profile.Tag), filepath.Join(g.profile.Temp, ".agentium-canary-"+g.profile.Tag)} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("the canary left %s", p)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Canary(ctx, g.file, g.profile); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled canary returns %v", err)
	}
}

// A real toolchain builds and tests under the profile, with its caches in the grade's own folders: Go, when it is
// installed, with an httptest server on loopback. (Maven, Gradle, Cargo and pytest were checked by the isolation
// plan's step 0 spike.)
func TestSandboxRunsGoTests(t *testing.T) {
	needSandbox(t)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	g := newGrade(t, nil)
	files := map[string]string{
		"go.mod":    "module example.com/m\n\ngo 1.21\n",
		"m.go":      "package m\n\nfunc Add(a, b int) int { return a + b }\n",
		"m_test.go": "package m\n\nimport (\n\t\"net/http\"\n\t\"net/http/httptest\"\n\t\"testing\"\n)\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n\nfunc TestServer(t *testing.T) {\n\ts := httptest.NewServer(http.NotFoundHandler())\n\tdefer s.Close()\n\tr, err := http.Get(s.URL)\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tr.Body.Close()\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(g.profile.Copy, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	environ := []string{"PATH=" + filepath.Dir(goBin) + ":/usr/bin:/bin", "HOME=" + g.profile.Home, "TMPDIR=" + g.profile.Temp,
		"GOCACHE=" + filepath.Join(g.profile.Cache, "go-build"), "GOPATH=" + filepath.Join(g.profile.Cache, "gopath"),
		"GOPROXY=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "CGO_ENABLED=0"}
	if code, out := g.run(environ, goBin, "test", "-count=1", "./..."); code != 0 {
		t.Errorf("go test under the profile: exit %d:\n%s", code, out)
	}
}

// A grade's denials reach the unified log under its tag (LogPredicate), which is how grading will tell a sandbox
// failure from a test failure; and they show that the security server cannot even be looked up, the narrowing the
// file denies alone do not prove. The keychain probe searches for a made-up service name: nothing real is asked for.
func TestSandboxDenialsAreLoggedWithTheTag(t *testing.T) {
	needSandbox(t)
	if _, err := os.Stat("/usr/bin/log"); err != nil {
		t.Skipf("/usr/bin/log: %v", err)
	}
	g := newGrade(t, nil)
	since := time.Now().Add(-2 * time.Second).Format("2006-01-02 15:04:05")
	g.sh("/usr/bin/security find-generic-password -s agentium-made-up-" + g.profile.Tag + " >/dev/null 2>&1")
	g.sh("cat " + strconv.Quote(g.path("home/.ssh/id_test")) + " >/dev/null 2>&1")
	want := []string{"deny(1) mach-lookup com.apple.SecurityServer", "deny(1) file-read-data " + RealForm(g.path("home/.ssh/id_test"))}
	var logged string
	for attempt := 0; attempt < 6; attempt++ { // the log is written asynchronously
		out, err := exec.Command("/usr/bin/log", "show", "--start", since, "--style", "compact", "--predicate", LogPredicate(g.profile.Tag)).Output()
		if err != nil {
			t.Skipf("log show: %v", err)
		}
		logged = string(out)
		if strings.Contains(logged, want[0]) && strings.Contains(logged, want[1]) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, w := range want {
		if !strings.Contains(logged, w) {
			t.Errorf("the log has no %q under the tag %s", w, g.profile.Tag)
		}
	}
}

// The JVM starts, compiles and runs under the profile, without the Mach services the profile drops, when a JDK is
// installed: javac and java with the temp folder pointed at the grade's temp root (the grader's recipe sets it, see
// the isolation plan), and a loopback server.
func TestSandboxRunsJava(t *testing.T) {
	needSandbox(t)
	out, err := exec.Command("/usr/libexec/java_home").Output()
	if err != nil {
		t.Skip("no JDK")
	}
	javaHome := strings.TrimSpace(string(out))
	g := newGrade(t, nil)
	source := "import java.net.*;\n\npublic class Probe {\n  public static void main(String[] args) throws Exception {\n" +
		"    try (ServerSocket s = new ServerSocket(0, 1, InetAddress.getByName(\"::1\"));\n" +
		"         Socket c = new Socket(InetAddress.getByName(\"::1\"), s.getLocalPort())) {\n" +
		"      System.out.println(\"ok \" + System.getProperty(\"java.io.tmpdir\"));\n    }\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(g.profile.Copy, "Probe.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := []string{"PATH=/usr/bin:/bin", "HOME=" + g.profile.Home, "TMPDIR=" + g.profile.Temp, "JAVA_HOME=" + javaHome,
		"JAVA_TOOL_OPTIONS=-Djava.io.tmpdir=" + g.profile.Temp}
	if code, out := g.run(environ, filepath.Join(javaHome, "bin", "javac"), "Probe.java"); code != 0 {
		t.Fatalf("javac under the profile: exit %d:\n%s", code, out)
	}
	if code, out := g.run(environ, filepath.Join(javaHome, "bin", "java"), "-cp", ".", "Probe"); code != 0 || !strings.Contains(out, "ok "+g.profile.Temp) {
		t.Errorf("java under the profile: exit %d:\n%s", code, out)
	}
}
