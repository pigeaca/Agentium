package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pigeaca/agentium/internal/home"
)

// containerMarker is the file in a run's records folder that names its grade's container, written before the container
// is created (MarkContainer; container grading, step 4): if Agentium dies, recovery knows to remove the run's
// containers and volumes by their labels (RecoverContainers). A run without one never had a container, so recovery
// calls no Docker for it.
const containerMarker = "container"

// MarkContainer records, in a run's records folder, that its grade is about to create the named container.
func MarkContainer(recordsDir, name string) error {
	if name == "" || strings.ContainsAny(name, "/\n") {
		return fmt.Errorf("container marker: name %q", name)
	}
	if err := writeFileAtomic(filepath.Join(recordsDir, containerMarker), []byte(name+"\n"), 0o600); err != nil {
		return fmt.Errorf("container marker: %w", err)
	}
	return nil
}

// UnmarkContainer removes the marker once the run's containers are gone.
func UnmarkContainer(recordsDir string) error {
	if err := os.Remove(filepath.Join(recordsDir, containerMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("container marker: %w", err)
	}
	return nil
}

// RecoverContainers removes the containers and volumes that dead runs' grades left, by their labels: for each run with
// a container marker whose Agentium process is gone (stored, or its process group no longer exists), remove (the
// driver's RemoveRun for this data folder) is called with the run's ID, and the marker goes once it succeeded. A run
// that may still be running is left alone, as is one whose start file cannot be read (Recover decides about it;
// --rm and the deadline remove a started container, and clean a created one). A failure is a warning, and the marker
// stays, so the next recovery tries again: Docker being down never blocks a run. remove is called only when there is a
// marker. Call it before Recover, which may remove a dead run's records, marker included.
func RecoverContainers(ctx context.Context, layout home.Layout, remove func(ctx context.Context, run string) error, warn func(string)) error {
	if warn == nil {
		warn = func(string) {}
	}
	markers, err := filepath.Glob(filepath.Join(layout.Records, "*", containerMarker))
	if err != nil {
		return fmt.Errorf("container markers: %w", err)
	}
	for _, marker := range markers {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := filepath.Dir(marker)
		id := filepath.Base(dir)
		if info, err := os.Lstat(marker); err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, startFile))
		if err == nil {
			var s start
			if json.Unmarshal(data, &s) != nil {
				continue // unreadable: Recover decides whether the run may be alive
			}
			if s.PGID > 0 && !s.Finished && groupExists(s.PGID) {
				continue // it may still be running
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if remove == nil {
			warn(fmt.Sprintf("run %s: its grade's container could not be removed: Docker is not available; recovery tries again next time", id))
			continue
		}
		if err := remove(ctx, id); err != nil {
			warn(fmt.Sprintf("run %s: its grade's container could not be removed: %v; recovery tries again next time", id, err))
			continue
		}
		if err := UnmarkContainer(dir); err != nil {
			warn(fmt.Sprintf("run %s: %v", id, err))
		}
	}
	return nil
}
