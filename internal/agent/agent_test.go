package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/home"
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
func (s shellAgent) Command(Invocation, []string) (Command, error) {
	return Command{Args: s.args, Env: s.env}, s.err
}
func (shellAgent) DeniedPaths(Invocation, []string) []string       { return nil }
func (shellAgent) Gather(string, string) error                     { return nil }
func (shellAgent) Parse(string) (Metrics, error)                   { return Metrics{}, nil }
func (shellAgent) Classify(Metrics, Stop, []string) string         { return OutcomeOK }
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
	if err != nil || !result.TimedOut || result.Stop != StopTimeout || time.Since(start) > 10*time.Second {
		t.Errorf("timeout: %+v, %v after %v", result, err, time.Since(start))
	}
}

// watchAgent is a shell agent whose command also has a prompt on stdin, folders, a lock and a watcher.
type watchAgent struct {
	shellAgent
	cmd Command
}

func (w watchAgent) Command(Invocation, []string) (Command, error) { return w.cmd, nil }

// Run gives the agent its standard input, makes its folders owner-only, holds its lock while it runs, and stops it
// gently when its watcher says so, with the watcher's reason; a watcher that never says so leaves the run alone.
func TestRunWatchesTheAgent(t *testing.T) {
	work, lock := t.TempDir(), filepath.Join(t.TempDir(), "agent.lock")
	out, errOut := files(t)
	dir := filepath.Join(work, "state", "home")
	cmd := Command{Args: []string{"-c", `cat; trap 'echo stopped; exit 1' INT; while :; do sleep 0.05; done`}, Stdin: "the prompt\n", Dirs: []string{dir},
		Exclusive: lock, Watch: func(ctx context.Context) Stop {
			select {
			case <-time.After(300 * time.Millisecond):
				return StopCap
			case <-ctx.Done():
				return StopNone
			}
		}}
	result, err := Run(context.Background(), watchAgent{cmd: cmd}, Invocation{CLI: "/bin/sh", Dir: work, Timeout: time.Minute, Grace: 5 * time.Second}, nil, out, errOut)
	if err != nil || result.Stop != StopCap || !result.Stopped || result.TimedOut {
		t.Fatalf("%+v, %v", result, err)
	}
	if got, _ := os.ReadFile(out.Name()); string(got) != "the prompt\nstopped\n" {
		t.Errorf("output %q: the prompt on stdin, then a gentle stop", got)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the agent's folder: %v", err)
	}
	// The lock is free again; while another holds it, a run waits until cancelled.
	unlock, err := home.LockFile(context.Background(), lock, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	cmd.Watch = nil
	if _, err := Run(ctx, watchAgent{cmd: cmd}, Invocation{CLI: "/bin/sh", Dir: work}, nil, out, errOut); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a run while the lock was held: %v", err)
	}
	unlock()
	cmd.Args, cmd.Watch = []string{"-c", "cat >/dev/null"}, func(ctx context.Context) Stop { <-ctx.Done(); return StopNone }
	if result, err := Run(context.Background(), watchAgent{cmd: cmd}, Invocation{CLI: "/bin/sh", Dir: work}, nil, out, errOut); err != nil || result.Stop != StopNone || !result.Passed() {
		t.Errorf("a run its watcher let be: %+v, %v", result, err)
	}
}
