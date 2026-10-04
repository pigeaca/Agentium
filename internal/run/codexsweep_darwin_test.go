//go:build darwin && cgo

package run

import (
	"os"
	"path/filepath"
	"testing"
)

// The sweep by sandbox tells a run's processes by its checkout and workspace folder: on this machine nothing else (the
// system's own sandboxed agents, which may write /tmp, among them) matches, whether the workspace lies in the user's
// temp folder, /tmp or the home folder. A sweep by the temp root alone matched cfprefsd and sharingd.
func TestCodexSweepMatchesNothingElse(t *testing.T) {
	home, err := os.UserHomeDir()
	must(t, err)
	probe, err := os.MkdirTemp(home, ".agentium-sweep-probe-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(probe) })
	short := shortTemp(t)
	t.Cleanup(func() { os.RemoveAll(short) })
	for _, base := range []string{t.TempDir(), short, probe} {
		ws := filepath.Join(base, "ws")
		must(t, os.MkdirAll(filepath.Join(ws, "repo"), 0o700))
		found, err := sandboxedIn(filepath.Join(ws, "repo"), ws)
		if err != nil || len(found) != 0 {
			t.Errorf("a workspace in %s: the sweep would stop %v (%v)", base, found, err)
		}
	}
}
