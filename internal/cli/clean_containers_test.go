package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/container"
	"github.com/pigeaca/agentium/internal/run"
)

// Clean lists the data folder's containers and volumes, and with --yes removes only what is idle: never a running
// container, a volume a container mounts, or anything made in the last hour; a removal Docker refuses because the item
// was used since is a kept item, not a failure. A removed deps volume's state goes with it.
func TestCleanContainersAndVolumes(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	old, now := time.Now().Add(-48*time.Hour), time.Now()
	deps := "agentium-deps-" + dataID(f.layout) + "-" + f.project + "-84349ebd3bf9"
	stale := "agentium-deps-" + dataID(f.layout) + "-" + f.project + "-000000000000"
	d := &fakeDocker{items: []container.Item{
		{Kind: "container", Name: "agentium-x-r1-grade", State: "created", Run: "r1", Created: old},
		{Kind: "container", Name: "agentium-x-r2-grade", State: "running", Run: "r2", Created: old},
		{Kind: "container", Name: "agentium-x-seed-ab-seed", State: "created", Run: "seed-ab", Created: now},
		{Kind: "volume", Name: strings.Repeat("a", 64), Run: "r1", Users: 1, UsedBy: []string{"agentium-x-r1-grade"}, Created: old, Size: 12e6},
		{Kind: "volume", Name: deps, Deps: f.project, Users: 1, UsedBy: []string{"agentium-x-r2-grade"}, Created: old, Size: 1.5e9},
		{Kind: "volume", Name: stale, Deps: f.project, Users: 0, Created: old.Add(-40 * 24 * time.Hour), Size: 2e9},
	}, local: []container.LocalImage{{Kind: "grading", Ref: "agentium-grade:go1.27-x", Size: 920e6}}}
	*f.docker = func(context.Context, []string) (DockerClient, error) { return d, nil }
	// The stale volume's state, which goes with it.
	state := filepath.Join(f.layout.Cache, "container-deps", stale)
	writeFile(t, state, "used", "")
	must(t, os.Chtimes(filepath.Join(state, "used"), old.Add(-40*24*time.Hour), old.Add(-40*24*time.Hour)))

	dry := f.run(context.Background(), "clean")
	expect(t, dry, ExitOK, "containers", "volumes", "agentium-x-r1-grade", "a created container that no command runs",
		"agentium-x-r2-grade", "its own deadline removes it", "made within the last hour", strings.Repeat("a", 64), stale, "unused for",
		deps, "in use by a container", "Docker holds 1 of the images container grading uses (920.0 MB); clean never removes them: agentium images remove --bases lists them all")
	if len(d.removed) > 0 {
		t.Fatalf("a dry run removed %v", d.removed)
	}
	d.idleErr = map[string]error{strings.Repeat("a", 64): container.ErrInUse}
	res := f.run(context.Background(), "clean", "--yes")
	expect(t, res, ExitOK, "used since it was listed: kept")
	want := []string{"agentium-x-r1-grade", strings.Repeat("a", 64), stale}
	if !slices.Equal(d.removed, want) {
		t.Errorf("removed %v, want %v (never a running container or a volume a container mounts)", d.removed, want)
	}
	if _, err := os.Stat(filepath.Join(state, "used")); !os.IsNotExist(err) {
		t.Errorf("the removed deps volume's state is left: %v", err)
	}
	if d.closed == 0 {
		t.Error("Docker was not closed")
	}
	// The JSON document names them by kind, and by name in place of a path.
	doc := checkJSON(t, f.runFixture, f.run(context.Background(), "clean", "--json"), ExitOK, []string{"clean"})
	var kinds []string
	for _, k := range doc.get("kinds").([]any) {
		kinds = append(kinds, k.(map[string]any)["kind"].(string))
	}
	if !slices.Equal(kinds, run.CleanKinds) {
		t.Errorf("kinds %v", kinds)
	}
	data, _ := json.Marshal(doc.get("kept"))
	if !strings.Contains(string(data), `"path":"agentium-x-r2-grade"`) || !strings.Contains(string(data), `"why":"running"`) {
		t.Errorf("kept %s", data)
	}
}

// Without Docker, clean works as before; recovery warns about a dead run whose grade had a container, and keeps its
// marker for the next time. With Docker, recovery removes that run's containers by label before anything else.
func TestCleanRecoversContainersOfDeadRuns(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	// A run whose Agentium died while it graded: its agent had started, so recovery keeps its records.
	dir := filepath.Join(f.layout.Records, "r-dead")
	must(t, os.MkdirAll(dir, 0o700))
	start, _ := json.Marshal(map[string]any{"record": map[string]any{"id": "r-dead", "records_dir": dir}, "workspace": filepath.Join(f.layout.Workspaces, "r-dead"),
		"agent_started": true})
	must(t, os.WriteFile(filepath.Join(dir, "started.json"), start, 0o600))
	must(t, run.MarkContainer(dir, "agentium-x-r-dead-grade"))
	expect(t, f.run(context.Background(), "clean", "--yes"), ExitOK, "run r-dead: its grade's container could not be removed: Docker is not available")
	if _, err := os.Stat(filepath.Join(dir, "container")); err != nil {
		t.Fatalf("the marker went without a removal: %v", err)
	}
	d := &fakeDocker{}
	*f.docker = func(context.Context, []string) (DockerClient, error) { return d, nil }
	res := f.run(context.Background(), "clean", "--yes")
	expect(t, res, ExitOK)
	if !slices.Equal(d.runs, []string{"r-dead"}) {
		t.Errorf("removed runs %v", d.runs)
	}
	if strings.Contains(res.stdout, "could not be removed") {
		t.Errorf("warned:\n%s", res.stdout)
	}
}
