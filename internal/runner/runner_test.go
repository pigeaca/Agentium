package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
