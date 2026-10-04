package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNameReadsAnAbsentAgentAsClaudeCode(t *testing.T) {
	for recorded, want := range map[string]string{"": ClaudeCode, ClaudeCode: ClaudeCode, "codex": "codex"} {
		if got := Name(recorded); got != want {
			t.Errorf("Name(%q) = %q, want %q", recorded, got, want)
		}
	}
}

// shellAgent is an adapter whose "agent" is a shell script: Command gives its arguments and environment as set.
type shellAgent struct {
	args, env []string
	err       error
}

func (s shellAgent) Name() string { return "shell" }
func (s shellAgent) Command(Invocation, []string) ([]string, []string, error) {
	return s.args, s.env, s.err
}
func (shellAgent) DeniedPaths(Invocation, []string) []string       { return nil }
func (shellAgent) Parse(io.Reader) (Metrics, error)                { return Metrics{}, nil }
func (shellAgent) Classify(Metrics, bool, []string) string         { return OutcomeOK }
func (shellAgent) Check(Metrics, Expect) []string                  { return nil }
func (shellAgent) Version(context.Context, string) (string, error) { return "1", nil }

func files(t *testing.T) (out, errOut *os.File) {
	t.Helper()
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	errOut, err = os.Create(filepath.Join(dir, "err"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close(); errOut.Close() })
	return out, errOut
}

// Run starts the invocation's CLI with the adapter's arguments and exactly its environment, in the invocation's folder,
// and reports the process it started.
func TestRunStartsTheAdaptersCommand(t *testing.T) {
	work := t.TempDir()
	out, errOut := files(t)
	var pid int
	a := shellAgent{args: []string{"-c", `pwd -P; printf '%s|%s\n' "$GIVEN" "$HOME"`}, env: []string{"GIVEN=yes"}}
	inv := Invocation{CLI: "/bin/sh", Dir: work, Timeout: time.Minute, Grace: time.Second, Started: func(p int) { pid = p }}
	result, err := Run(context.Background(), a, inv, []string{"HOME=/parent"}, out, errOut)
	if err != nil || !result.Passed() {
		t.Fatalf("%+v, %v", result, err)
	}
	got, _ := os.ReadFile(out.Name())
	real, _ := filepath.EvalSymlinks(work)
	if want := real + "\nyes|\n"; string(got) != want {
		t.Errorf("output %q, want %q: the folder, and the adapter's environment alone", got, want)
	}
	if pid <= 0 {
		t.Error("Started was not called")
	}
}

// A command the adapter refuses never starts; the timeout stops one that runs too long.
func TestRunRefusalAndTimeout(t *testing.T) {
	out, errOut := files(t)
	refused := errors.New("refused")
	if _, err := Run(context.Background(), shellAgent{err: refused}, Invocation{CLI: "/bin/sh", Dir: t.TempDir()}, nil, out, errOut); !errors.Is(err, refused) {
		t.Errorf("a refused command: %v", err)
	}
	a := shellAgent{args: []string{"-c", "sleep 30"}}
	start := time.Now()
	result, err := Run(context.Background(), a, Invocation{CLI: "/bin/sh", Dir: t.TempDir(), Timeout: 200 * time.Millisecond, Grace: 100 * time.Millisecond}, nil, out, errOut)
	if err != nil || !result.TimedOut || time.Since(start) > 10*time.Second {
		t.Errorf("timeout: %+v, %v after %v", result, err, time.Since(start))
	}
}
