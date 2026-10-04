package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/pigeaca/agentium/internal/home"
)

// Recovery removes the containers of dead runs that had one (a marker), by label, and keeps the marker when that
// fails; a live run, an unreadable one, and a run without a marker are never touched, and without a marker Docker is
// never called.
func TestRecoverContainers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	layout := home.Layout{Root: root, Records: filepath.Join(root, "records"), Workspaces: filepath.Join(root, "workspaces")}
	run := func(id string, s *start, marked bool) string {
		dir := filepath.Join(layout.Records, id)
		must(t, os.MkdirAll(dir, 0o700))
		if s != nil {
			data, err := json.Marshal(s)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(dir, startFile), data, 0o600))
		}
		if marked {
			must(t, MarkContainer(dir, "agentium-test-"+id+"-grade"))
		}
		return dir
	}
	dead := run("r-dead", &start{AgentStarted: true, PGID: 999999}, true)
	stored := run("r-stored", nil, true)
	live := run("r-live", &start{AgentStarted: true, PGID: syscall.Getpgrp()}, true)
	broken := run("r-broken", nil, true)
	must(t, os.WriteFile(filepath.Join(broken, startFile), []byte("{trunc"), 0o600))
	run("r-host", &start{AgentStarted: true}, false)
	var removed []string
	fail := map[string]bool{}
	remove := func(_ context.Context, id string) error {
		removed = append(removed, id)
		if fail[id] {
			return errors.New("docker is down")
		}
		return nil
	}
	var warnings []string
	warn := func(s string) { warnings = append(warnings, s) }
	fail["r-stored"] = true
	must(t, RecoverContainers(ctx, layout, remove, warn))
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"r-dead", "r-stored"}) {
		t.Errorf("removed %v", removed)
	}
	marked := func(dir string) bool { _, err := os.Stat(filepath.Join(dir, containerMarker)); return err == nil }
	if marked(dead) || !marked(stored) || !marked(live) || !marked(broken) {
		t.Errorf("markers: dead %v, stored %v, live %v, broken %v", marked(dead), marked(stored), marked(live), marked(broken))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "r-stored") || !strings.Contains(warnings[0], "tries again") {
		t.Errorf("warnings %q", warnings)
	}
	// Next time, what failed is tried again.
	removed, warnings, fail = nil, nil, map[string]bool{}
	must(t, RecoverContainers(ctx, layout, remove, warn))
	if !slices.Equal(removed, []string{"r-stored"}) || marked(stored) {
		t.Errorf("again: removed %v", removed)
	}
	// Without Docker, a marked dead run is a warning, and keeps its marker.
	run("r-later", nil, true)
	warnings = nil
	must(t, RecoverContainers(ctx, layout, nil, warn))
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Docker is not available") || !marked(filepath.Join(layout.Records, "r-later")) {
		t.Errorf("no docker: %q", warnings)
	}
	// Without any marker, remove is never called.
	empty := home.Layout{Records: filepath.Join(t.TempDir(), "records")}
	must(t, RecoverContainers(ctx, empty, func(context.Context, string) error { t.Error("called"); return nil }, warn))
	if err := MarkContainer(dead, "a/b"); err == nil {
		t.Error("a name with a slash")
	}
}
