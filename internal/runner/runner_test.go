package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func output(t *testing.T) (*os.File, func() string) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, func() string {
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

// alive reports whether the process whose pid the command wrote to file still exists.
func alive(t *testing.T, file string) bool {
	t.Helper()
	var data []byte
	for i := 0; i < 50 && len(data) == 0; i++ { // the shell writes the file right after starting the child
		data, _ = os.ReadFile(file)
		time.Sleep(10 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid file %q: %v", data, err)
	}
	time.Sleep(50 * time.Millisecond) // let a SIGKILL land
	return syscall.Kill(pid, 0) == nil
}

func TestExitCodesOutputAndEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-not-real") // secret-scan: allow
	t.Setenv("GIT_DIR", "/elsewhere/.git")
	t.Setenv("AGENTIUM_HOME", "/data")
	out, read := output(t)
	dir := t.TempDir()
	result, err := Run(context.Background(), Spec{Dir: dir, Output: out, Env: []string{"EXTRA=1"},
		Command: `echo "key=${ANTHROPIC_API_KEY:-none} git=${GIT_DIR:-none} home=${AGENTIUM_HOME:-none} extra=$EXTRA"; pwd; echo oops >&2; exit 3`})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 3 || result.Passed() || result.TimedOut {
		t.Errorf("result = %+v, want exit 3", result)
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	text := read()
	for _, want := range []string{"key=none git=none home=none extra=1", realDir, "oops"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if result, err := Run(context.Background(), Spec{Command: "true", Output: out}); err != nil || !result.Passed() {
		t.Errorf("true: %+v, %v", result, err)
	}
}

func TestEnvironDropsCredentialsByName(t *testing.T) {
	kept := Environ([]string{"PATH=/bin", "HOME=/h", "GOFLAGS=-mod=readonly", "LANG=C", "TERM=xterm",
		"HF_TOKEN=x", "STRIPE_SECRET_KEY=x", "DB_PASSWORD=x", "SSH_AUTH_SOCK=/tmp/agent", "MY_SERVICE_API_KEY=x",
		"GOOGLE_APPLICATION_CREDENTIALS=/k.json", "ANTHROPIC_API_KEY=x", "GIT_DIR=/x", "AGENTIUM_HOME=/d"})
	if strings.Join(kept, " ") != "PATH=/bin HOME=/h GOFLAGS=-mod=readonly LANG=C TERM=xterm" {
		t.Errorf("kept %v", kept)
	}
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	out, _ := output(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	result, err := Run(context.Background(), Spec{Output: out, Timeout: 200 * time.Millisecond,
		Command: "sleep 30 & echo $! > " + pidFile + "; wait"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.Passed() || time.Since(start) > 5*time.Second {
		t.Errorf("result = %+v after %v, want a timeout", result, time.Since(start))
	}
	if alive(t, pidFile) {
		t.Error("the child outlived the timeout")
	}
}

func TestBackgroundChildrenDieWithTheCommand(t *testing.T) {
	out, _ := output(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	result, err := Run(context.Background(), Spec{Output: out, Command: "sleep 30 & echo $! > " + pidFile + "; exit 0"})
	if err != nil || !result.Passed() {
		t.Fatalf("%+v, %v", result, err)
	}
	if alive(t, pidFile) {
		t.Error("a background child outlived its command")
	}
}

func TestArgsEnvironAndGracefulStop(t *testing.T) {
	out, read := output(t)
	errFile, readErr := output(t)
	// Args run without a shell; Environ replaces the base environment; Stderr is separate.
	result, err := Run(context.Background(), Spec{Output: out, Stderr: errFile, Environ: []string{"ONLY=1"},
		Args: []string{"/bin/sh", "-c", `echo "only=$ONLY home=${HOME:-unset}"; echo problem >&2`}})
	if err != nil || !result.Passed() || strings.TrimSpace(read()) != "only=1 home=unset" || strings.TrimSpace(readErr()) != "problem" {
		t.Errorf("result %+v, err %v, stdout %q, stderr %q", result, err, read(), readErr())
	}
	// On timeout the group gets SIGINT first: a command that handles it can finish cleanly.
	out, read = output(t)
	result, err = Run(context.Background(), Spec{Output: out, Timeout: 2 * time.Second, Grace: 5 * time.Second,
		Command: `trap 'echo finishing; exit 0' INT; sleep 30 & wait`})
	if err != nil || !result.TimedOut || !strings.Contains(read(), "finishing") {
		t.Errorf("graceful stop: %+v, %v, output %q", result, err, read())
	}
	// A command that ignores SIGINT is killed after the grace period.
	out, _ = output(t)
	start := time.Now()
	result, err = Run(context.Background(), Spec{Output: out, Timeout: 100 * time.Millisecond, Grace: 300 * time.Millisecond,
		Command: `trap '' INT; while true; do sleep 0.05; done`})
	if err != nil || !result.TimedOut || time.Since(start) > 4*time.Second {
		t.Errorf("forced stop: %+v, %v after %v", result, err, time.Since(start))
	}
}

func TestCancelWithGraceInterruptsFirst(t *testing.T) {
	out, read := output(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(2*time.Second, cancel)
	_, err := Run(ctx, Spec{Output: out, Grace: 5 * time.Second, Command: `trap 'echo finishing; exit 0' INT; sleep 30 & wait`})
	if !errors.Is(err, context.Canceled) || !strings.Contains(read(), "finishing") {
		t.Errorf("err %v, output %q", err, read())
	}
	if _, err := Run(ctx, Spec{Output: out, Args: []string{"/no/such/binary"}}); err == nil || !strings.Contains(err.Error(), "/no/such/binary") {
		t.Errorf("errors name the program: %v", err)
	}
}

func TestCancelledContextIsAnError(t *testing.T) {
	out, _ := output(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	pidFile := filepath.Join(t.TempDir(), "pid")
	_, err := Run(ctx, Spec{Output: out, Command: "sleep 30 & echo $! > " + pidFile + "; wait"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if alive(t, pidFile) {
		t.Error("the child outlived cancellation")
	}
}

func TestStartedReportsTheProcessGroup(t *testing.T) {
	out, read := output(t)
	var started int
	result, err := Run(context.Background(), Spec{Command: "ps -o pgid= -p $$", Output: out, Started: func(pid int) { started = pid }})
	if err != nil || !result.Passed() {
		t.Fatalf("result %+v, %v", result, err)
	}
	if got := strings.TrimSpace(read()); started == 0 || got != strconv.Itoa(started) {
		t.Errorf("Started got %d; the command's process group is %q", started, got)
	}
}

// pinnedEnviron is one environment that every allowlist's pin test filters: credentials, GIT_*, AGENTIUM_*, CLAUDE_*,
// toolchain and system settings, and names that only look like their neighbours (GIT, GITHUB).
func pinnedEnviron() []string {
	return []string{"PATH=/bin", "HOME=/h", "GIT_DIR=/x", "GIT_AUTHOR_NAME=a", "AGENTIUM_HOME=/a", "AGENTIUM_X=1", "ANTHROPIC_API_KEY=k",
		"GITHUB_TOKEN=t", "SSH_AUTH_SOCK=/s", "MY_PASSWORD=p", "CLAUDE_CODE_TMPDIR=/t", "LC_ALL=C", "GOFLAGS=-mod=mod", "JAVA_HOME=/j",
		"RUSTC_WRAPPER=w", "HTTPS_PROXY=http://p", "XDG_RUNTIME_DIR=/r", "FOO=bar", "NODE_OPTIONS=x", "TERM=xterm", "EMPTY=", "GOPATH=/g",
		"MAVEN_OPTS=-X", "GRADLE_USER_HOME=/gu", "CARGO_HOME=/c", "SHELL=/bin/zsh", "TMPDIR=/tmp", "AWS_PROFILE=p", "NETRC=/n", "GIT=ok", "GITHUB=ok"}
}

// TestEnvironPinned pins what Environ keeps byte for byte: everything but credentials, GIT_* and AGENTIUM_*, in order.
// An empty result is an empty slice, not nil: Spec.Environ nil means "use the default".
func TestEnvironPinned(t *testing.T) {
	want := []string{"PATH=/bin", "HOME=/h", "CLAUDE_CODE_TMPDIR=/t", "LC_ALL=C", "GOFLAGS=-mod=mod", "JAVA_HOME=/j", "RUSTC_WRAPPER=w",
		"HTTPS_PROXY=http://p", "XDG_RUNTIME_DIR=/r", "FOO=bar", "NODE_OPTIONS=x", "TERM=xterm", "EMPTY=", "GOPATH=/g", "MAVEN_OPTS=-X",
		"GRADLE_USER_HOME=/gu", "CARGO_HOME=/c", "SHELL=/bin/zsh", "TMPDIR=/tmp", "GIT=ok", "GITHUB=ok"}
	if got := Environ(pinnedEnviron()); !slices.Equal(got, want) {
		t.Errorf("Environ = %q\nwant %q", got, want)
	}
	if got := Environ(nil); got == nil || len(got) != 0 {
		t.Errorf("Environ(nil) = %#v, want an empty non-nil slice", got)
	}
}

// A stop (Spec.Stop) ends the command as a timeout does, gently first (the command's trap runs), and says so apart
// from a timeout; a command that ends by itself first is not stopped.
func TestStopEndsTheCommandGently(t *testing.T) {
	out, read := output(t)
	stop := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, func() { close(stop) })
	start := time.Now()
	result, err := Run(context.Background(), Spec{Command: "trap 'echo interrupted; exit 1' INT; while :; do sleep 0.05; done", Output: out,
		Grace: 5 * time.Second, Timeout: time.Minute, Stop: stop})
	if err != nil || !result.Stopped || result.TimedOut || result.Passed() || time.Since(start) > 10*time.Second {
		t.Fatalf("result %+v, %v after %v", result, err, time.Since(start))
	}
	if !strings.Contains(read(), "interrupted") {
		t.Error("the command was not interrupted first")
	}
	result, err = Run(context.Background(), Spec{Command: "true", Stop: make(chan struct{})})
	if err != nil || result.Stopped || !result.Passed() {
		t.Errorf("a command that was never stopped: %+v, %v", result, err)
	}
}
