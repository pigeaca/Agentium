package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// timedWriter keeps every write with the seconds since the first one, for scripts/readme_images/cast2svg.py.
type timedWriter struct {
	mu     sync.Mutex
	start  time.Time
	writes [][2]any
}

func (w *timedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.start.IsZero() {
		w.start = now
	}
	w.writes = append(w.writes, [2]any{float64(now.Sub(w.start).Milliseconds()) / 1000, string(p)})
	return len(p), nil
}

// TestRecordLiveRun is the recorder behind the README's status-line animation, not a check: it runs only when
// AGENTIUM_RECORD_LIVE names an output file. It runs "experiment run" on the experiment fixture (the stand-in agent
// answers for Claude Code, at its real speed) as on a terminal, and writes the output as JSON [[seconds, text], ...],
// the input of cast2svg.py. The usage file makes the status line show a usage reading.
func TestRecordLiveRun(t *testing.T) {
	out := os.Getenv("AGENTIUM_RECORD_LIVE")
	if out == "" {
		t.Skip("set AGENTIUM_RECORD_LIVE=FILE to record")
	}
	f, ctrl := experimentFixture(t)
	ctx := context.Background()
	usage := fmt.Sprintf("0.32 0.0 %d\n", time.Now().Add(3*time.Hour).Unix())
	if err := os.WriteFile(filepath.Join(ctrl, "usage"), []byte(usage), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "3", "--seed", "5", "--budget", "22"), ExitOK)
	f.vars["TERM"] = "xterm"
	w := &timedWriter{}
	var stderr bytes.Buffer
	code := Run(ctx, Env{Args: []string{"experiment", "run", "lean-ab"}, Stdout: w, Stderr: &stderr, Dir: f.repo, Terminal: true,
		Getenv:   func(key string) string { return f.vars[key] },
		Environ:  func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home} },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now,
		Backoff: func(int) time.Duration { return 10 * time.Millisecond },
		Sleep:   func(context.Context, time.Duration) error { return nil }})
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	data, err := json.Marshal(w.writes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
