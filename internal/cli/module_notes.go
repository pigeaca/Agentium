package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/snapshot"
	"github.com/pigeaca/agentium/internal/store"
)

// An arm applies its snapshot as a session in each task's module loads context (snapshot.PlanOverlayIn): a snapshot
// taken for another module, or for the root, lacks the module's own .claude files, so the arm runs without them. That
// is the snapshot's content, not an error, but it may not be what a context A/B meant: start, experiment new and run
// once say so.

// moduleLabel names a module in a sentence: "the module svc/billing", or "the repository's root".
func moduleLabel(module string) string {
	if module == "" {
		return "the repository's root"
	}
	return "the module " + module
}

// snapshotModuleNote says that snapshot name, taken for snapModule, is applied to tasks in module (who names them).
func snapshotModuleNote(name, snapModule, module, who string) string {
	return fmt.Sprintf("snapshot %s was taken for %s, not for %s, which %s in: its arm loads only its own files there, without the module's "+
		"own .claude files it lacks (agentium context snapshot NAME saves the context of the project's module)", name, moduleLabel(snapModule),
		moduleLabel(module), who)
}

// snapshotModule is the module snapshot name was taken for (snapshot.Manifest.Module; "" for the root and for older
// snapshots).
func (w *workspace) snapshotModule(ctx context.Context, name string) (string, error) {
	snap, err := w.db.SnapshotByName(ctx, w.project.ID, name)
	if err != nil {
		return "", err
	}
	var m snapshot.Manifest
	if err := json.Unmarshal(snap.Manifest, &m); err != nil {
		return "", fmt.Errorf("snapshot %s: %w", name, err)
	}
	return m.Module, nil
}

// armModuleNotes are the notes for a design's snapshot arms applied to its tasks in other modules: one per snapshot and
// module, naming how many of the tasks run there.
func (w *workspace) armModuleNotes(ctx context.Context, d experiment.Design) ([]string, error) {
	modules := map[string]int{}
	for _, name := range d.Tasks {
		t, err := w.db.TaskByName(ctx, w.project.ID, name)
		if err != nil {
			return nil, err
		}
		modules[t.Module]++
	}
	var notes, seen []string
	for _, arm := range d.Arms {
		if arm.Snapshot == "" || slices.Contains(seen, arm.Context) {
			continue
		}
		seen = append(seen, arm.Context)
		snapModule, err := w.snapshotModule(ctx, arm.Context)
		if err != nil {
			return nil, err
		}
		for _, module := range slices.Sorted(maps.Keys(modules)) {
			if module != snapModule {
				notes = append(notes, snapshotModuleNote(arm.Context, snapModule, module, fmt.Sprintf("%d of its task(s) run", modules[module])))
			}
		}
	}
	return notes, nil
}

// taskModuleNote is run once's note for its task's arm (none without a snapshot, or in the snapshot's own module).
func (w *workspace) taskModuleNote(ctx context.Context, snapshotName string, t store.Task) (string, error) {
	if snapshotName == "" {
		return "", nil
	}
	snapModule, err := w.snapshotModule(ctx, snapshotName)
	if err != nil || snapModule == t.Module {
		return "", err
	}
	return snapshotModuleNote(snapshotName, snapModule, t.Module, "task "+t.Name+" runs"), nil
}
