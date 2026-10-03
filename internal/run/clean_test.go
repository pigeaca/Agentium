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
	"time"

	"github.com/pigeaca/agentium/internal/home"
)

// Full commit IDs for the fixtures' bases.
var (
	baseA = strings.Repeat("a", 40)
	baseB = strings.Repeat("b", 40)
	baseC = strings.Repeat("c", 40)
	baseD = strings.Repeat("d", 64)
)

// cleanLayout is a data folder in a temp folder, with no temp roots (/tmp is not the test's).
func cleanLayout(t *testing.T) home.Layout {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { // read-only venvs would fail TempDir's own removal
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	return home.Layout{Root: root, Database: filepath.Join(root, "agentium.db"), Artifacts: filepath.Join(root, "artifacts"),
		Workspaces: filepath.Join(root, "workspaces"), Records: filepath.Join(root, "records"), Cache: filepath.Join(root, "cache"),
		Deps: filepath.Join(root, "deps")}
}

// fill makes dir with a file of n bytes in it.
func fill(t *testing.T, dir string, n int) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "data"), make([]byte, n), 0o600))
}

// usedAgo sets path's modification time to d before now.
func usedAgo(t *testing.T, path string, now time.Time, d time.Duration) {
	t.Helper()
	must(t, os.Chtimes(path, now.Add(-d), now.Add(-d)))
}

func seedPath(l home.Layout, project, key, base string) string {
	return filepath.Join(l.Cache, seedsFolder, project, key+"-"+base)
}

// writeStamp writes a base's warm-up stamp in the project's state folder, naming a venv and metadata key when set.
func writeStamp(t *testing.T, l home.Layout, project, base, venv, meta string) string {
	t.Helper()
	state := filepath.Join(l.Cache, warmStateFolder, project)
	must(t, os.MkdirAll(state, 0o700))
	content := map[string]string{}
	if venv != "" {
		content["venv"] = filepath.Join("/elsewhere/deps", project, "py", venv, "venv") // the data folder moved since
	}
	if meta != "" {
		content["metadata"] = filepath.Join(l.Deps, project, "py-meta", meta)
	}
	data, _ := json.Marshal(content)
	path := filepath.Join(state, "python-v3-"+base)
	must(t, os.WriteFile(path, data, 0o600))
	return path
}

// byPath finds an item of the list by its path.
func byPath(items []CleanItem, path string) (CleanItem, bool) {
	i := slices.IndexFunc(items, func(it CleanItem) bool { return it.Path == path })
	if i < 0 {
		return CleanItem{}, false
	}
	return items[i], true
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestCleanKeepsWhatIsInUseAndRemovesTheRest(t *testing.T) {
	t.Parallel()
	l := cleanLayout(t)
	now := time.Now()
	inUseByTask := seedPath(l, "1", "go-11111111", baseA)
	unused := seedPath(l, "1", "go-11111111", baseB)
	old := seedPath(l, "1", "go-22222222", baseA)
	recent := seedPath(l, "1", "go-11111111", baseC)
	deadMaker := seedPath(l, "1", "go-33333333", baseC) + ".tmp"
	for path, age := range map[string]time.Duration{inUseByTask: 48 * time.Hour, unused: 48 * time.Hour, old: 40 * 24 * time.Hour,
		recent: 10 * time.Minute, deadMaker: 2 * time.Hour} {
		fill(t, path, 5000)
		usedAgo(t, path, now, age)
	}
	must(t, os.WriteFile(seedPath(l, "1", "go-11111111", baseB)+".lock", nil, 0o600)) // a seed's lock stays

	// Project 1 is in use: its venv k1 serves base A (a task's), k2 only base B (unused), k3 no base at all.
	p1 := filepath.Join(l.Deps, "1")
	fill(t, filepath.Join(p1, "m2"), 9000)
	for _, k := range []string{"k1", "k2", "k3"} {
		fill(t, filepath.Join(p1, "py", k, "venv"), 3000)
		must(t, os.Chmod(filepath.Join(p1, "py", k, "venv"), 0o500)) // venvs are read-only
		must(t, os.Chmod(filepath.Join(p1, "py", k), 0o500))
		usedAgo(t, filepath.Join(p1, "py", k), now, 5*24*time.Hour)
	}
	usedAgo(t, writeStamp(t, l, "1", baseA, "k1", ""), now, 48*time.Hour)
	usedAgo(t, writeStamp(t, l, "1", baseB, "k2", "m2key"), now, 48*time.Hour)
	fill(t, filepath.Join(p1, "py-meta", "m2key"), 100)
	// Project 2: no task uses any of its bases; its whole folder goes, with its stamps but not its lock.
	p2 := filepath.Join(l.Deps, "2")
	fill(t, filepath.Join(p2, "cargo"), 7000)
	stampD := writeStamp(t, l, "2", baseD, "", "")
	usedAgo(t, stampD, now, 3*24*time.Hour)
	steps := stampD + ".steps"
	must(t, os.WriteFile(steps, nil, 0o600))
	lock := filepath.Join(l.Cache, warmStateFolder, "2", warmLockName)
	must(t, os.WriteFile(lock, nil, 0o600))

	in := CleanInput{Layout: l, Now: now, OlderThan: CleanDefaultAge, InUse: map[string]map[string]BaseUse{"1": {baseA: {Tasks: []string{"value"}}}}}
	plan, err := PlanClean(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{unused: CleanUnused, old: CleanOld, deadMaker: CleanUnused, filepath.Join(p1, "py", "k2"): CleanUnused,
		filepath.Join(p1, "py", "k3"): CleanUnused, filepath.Join(p1, "py-meta", "m2key"): CleanUnused, p2: CleanUnused}
	for path, reason := range want {
		it, ok := byPath(plan.Remove, path)
		if !ok {
			t.Errorf("%s is not removed; plan %+v", path, plan.Remove)
			continue
		}
		if it.Reason != reason || it.Bytes <= 0 {
			t.Errorf("%s: reason %q, %d bytes; want %q", path, it.Reason, it.Bytes, reason)
		}
	}
	if len(plan.Remove) != len(want) {
		t.Errorf("removes %d item(s), want %d: %+v", len(plan.Remove), len(want), plan.Remove)
	}
	for path, reason := range map[string]string{inUseByTask: CleanKeptTask, recent: CleanKeptRecent, p1: CleanKeptTask} {
		if it, ok := byPath(plan.Keep, path); !ok || it.Reason != reason {
			t.Errorf("%s: kept %v, reason %q; want %q", path, ok, it.Reason, reason)
		}
	}
	if it, _ := byPath(plan.Keep, inUseByTask); it.Detail != "in use by task value" {
		t.Errorf("detail = %q", it.Detail)
	}

	errs := RemoveClean(context.Background(), l, plan.Remove)
	for i, err := range errs {
		if err != nil {
			t.Errorf("%s: %v", plan.Remove[i].Path, err)
		}
	}
	for path := range want {
		if exists(path) {
			t.Errorf("%s is still there", path)
		}
	}
	for _, kept := range []string{inUseByTask, recent, filepath.Join(p1, "py", "k1", "venv", "data"), filepath.Join(p1, "m2", "data"),
		seedPath(l, "1", "go-11111111", baseB) + ".lock", lock} {
		if !exists(kept) {
			t.Errorf("%s was removed", kept)
		}
	}
	for _, gone := range []string{stampD, steps} {
		if exists(gone) {
			t.Errorf("the stamp %s outlived its dependencies: a run would take them for warmed", gone)
		}
	}
	// What was removed went through the quarantine, and left nothing there.
	if entries, _ := os.ReadDir(quarantine(l)); len(entries) != 0 {
		t.Errorf("the quarantine holds %d entries", len(entries))
	}
}

func TestCleanKeepsALockedExperimentsBases(t *testing.T) {
	t.Parallel()
	l := cleanLayout(t)
	now := time.Now()
	seed := seedPath(l, "3", "mvn-44444444", baseA)
	fill(t, seed, 100)
	usedAgo(t, seed, now, 10*24*time.Hour)
	fill(t, filepath.Join(l.Deps, "3", "m2"), 100)
	usedAgo(t, writeStamp(t, l, "3", baseA, "", ""), now, 10*24*time.Hour)
	in := CleanInput{Layout: l, Now: now, OlderThan: CleanDefaultAge, InUse: map[string]map[string]BaseUse{"3": {baseA: {Experiments: []string{"ab"}}}}}
	plan, err := PlanClean(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Remove) != 0 {
		t.Errorf("removes %+v", plan.Remove)
	}
	for _, path := range []string{seed, filepath.Join(l.Deps, "3")} {
		if it, ok := byPath(plan.Keep, path); !ok || it.Reason != CleanKeptExperiment || it.Detail != "in use by experiment ab" {
			t.Errorf("%s: kept %v, %q %q", path, ok, it.Reason, it.Detail)
		}
	}
	// Unused for longer than --older-than, it goes all the same.
	in.OlderThan = 7 * 24 * time.Hour
	plan, err = PlanClean(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Remove) != 2 || plan.Remove[0].Reason != CleanOld || plan.Remove[1].Reason != CleanOld {
		t.Errorf("with --older-than 7d: %+v", plan.Remove)
	}
}

func TestCleanNeverFollowsALink(t *testing.T) {
	t.Parallel()
	l := cleanLayout(t)
	now := time.Now()
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep.txt")
	must(t, os.WriteFile(victim, []byte("yours"), 0o600))
	outsideSeed := filepath.Join(outside, "go-55555555-"+baseB)
	fill(t, outsideSeed, 10)

	// Links planted in candidates: to a folder and to a file outside.
	seed := seedPath(l, "1", "go-55555555", baseA)
	fill(t, seed, 10)
	must(t, os.Symlink(outside, filepath.Join(seed, "to-dir")))
	must(t, os.Symlink(victim, filepath.Join(seed, "to-file")))
	usedAgo(t, seed, now, 48*time.Hour)
	venv := filepath.Join(l.Deps, "4", "py", "k", "venv")
	fill(t, venv, 10)
	must(t, os.Symlink(outside, filepath.Join(venv, "lib")))
	usedAgo(t, filepath.Join(l.Deps, "4"), now, 48*time.Hour)
	// A link in a candidate's place, and a project folder that is a link: neither is a candidate.
	must(t, os.Symlink(outsideSeed, seedPath(l, "1", "go-55555555", baseC)))
	must(t, os.Symlink(outside, filepath.Join(l.Cache, seedsFolder, "9")))
	must(t, os.Symlink(outside, filepath.Join(l.Deps, "8")))

	plan, err := PlanClean(context.Background(), CleanInput{Layout: l, Now: now, OlderThan: CleanDefaultAge})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, it := range plan.Remove {
		paths = append(paths, it.Path)
	}
	slices.Sort(paths)
	if want := []string{seed, filepath.Join(l.Deps, "4")}; !slices.Equal(paths, want) {
		t.Errorf("removes %v, want %v", paths, want)
	}
	for i, err := range RemoveClean(context.Background(), l, plan.Remove) {
		if err != nil {
			t.Errorf("%s: %v", plan.Remove[i].Path, err)
		}
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "yours" {
		t.Errorf("the file outside: %q, %v", data, err)
	}
	if !exists(filepath.Join(outsideSeed, "data")) {
		t.Error("the folder outside lost its file")
	}
	if exists(seed) || exists(filepath.Join(l.Deps, "4")) {
		t.Error("the candidates are still there")
	}

	// Paths cleanup refuses whatever a plan says: outside the cache and deps folders, through a link, a cache top folder.
	for _, p := range []string{outside, filepath.Join(l.Root, "records", "r1"), l.Deps, filepath.Join(l.Cache, seedsFolder),
		filepath.Join(l.Cache, seedsFolder, "9", "go-55555555-"+baseB), filepath.Join(l.Deps, "8"), filepath.Join(l.Deps, "8", "x"),
		filepath.Join(l.Deps, "..", "records")} {
		if err := cleanable(l, p); err == nil {
			t.Errorf("cleanable(%s) = nil", p)
		}
		errs := RemoveClean(context.Background(), l, []CleanItem{{Kind: CleanSeeds, Path: p}})
		if errs[0] == nil || !strings.Contains(errs[0].Error(), "refused") {
			t.Errorf("RemoveClean(%s): %v", p, errs[0])
		}
	}
	if !exists(filepath.Join(outsideSeed, "data")) {
		t.Error("a refused removal touched the folder outside")
	}
}

func TestCleanWithoutACacheHasNothingToDo(t *testing.T) {
	t.Parallel()
	l := cleanLayout(t) // nothing created inside
	plan, err := PlanClean(context.Background(), CleanInput{Layout: l, Now: time.Now(), OlderThan: CleanDefaultAge,
		Stored: func(string) (bool, error) { return false, nil }})
	if err != nil || len(plan.Remove)+len(plan.Keep) != 0 {
		t.Errorf("plan %+v, %v", plan, err)
	}
	if errs := RemoveClean(context.Background(), l, nil); len(errs) != 0 {
		t.Errorf("errors %v", errs)
	}
	plan, err = PlanClean(context.Background(), CleanInput{Layout: home.Layout{}, Now: time.Now()})
	if err != nil || len(plan.Remove) != 0 {
		t.Errorf("an empty layout: %+v, %v", plan, err)
	}
}

func TestCleanLeavesWhatWasUsedSinceThePlanOrIsLocked(t *testing.T) {
	t.Parallel()
	l := cleanLayout(t)
	now := time.Now()
	used, locked := seedPath(l, "1", "go-66666666", baseA), seedPath(l, "1", "go-66666666", baseB)
	for _, s := range []string{used, locked} {
		fill(t, s, 10)
		usedAgo(t, s, now, 48*time.Hour)
	}
	plan, err := PlanClean(context.Background(), CleanInput{Layout: l, Now: now, OlderThan: CleanDefaultAge})
	if err != nil || len(plan.Remove) != 2 {
		t.Fatalf("plan %+v, %v", plan.Remove, err)
	}
	markUsed(used) // a validation took the seed after the plan
	unlock, err := home.LockFile(context.Background(), locked+".lock", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	errs := RemoveClean(context.Background(), l, plan.Remove)
	for i, it := range plan.Remove {
		want := map[string]error{used: ErrCleanUsed, locked: ErrCleanBusy}[it.Path]
		if !errors.Is(errs[i], want) {
			t.Errorf("%s: %v, want %v", it.Path, errs[i], want)
		}
		if !exists(it.Path) {
			t.Errorf("%s was removed", it.Path)
		}
	}
}

func TestCleanEmptiesTheQuarantine(t *testing.T) {
	t.Parallel()
	l := cleanLayout(t)
	stuck := filepath.Join(quarantine(l), "r1-grading-0a0b0c")
	fill(t, filepath.Join(stuck, "ro"), 10)
	must(t, os.Chmod(filepath.Join(stuck, "ro"), 0o500)) // what made removeTree's first try fail
	must(t, os.WriteFile(filepath.Join(quarantine(l), "a-file"), []byte("x"), 0o600))
	plan, err := PlanClean(context.Background(), CleanInput{Layout: l, Now: time.Now(), OlderThan: CleanDefaultAge})
	if err != nil || len(plan.Remove) != 2 || plan.Remove[0].Reason != CleanQuarantined {
		t.Fatalf("plan %+v, %v", plan.Remove, err)
	}
	for i, err := range RemoveClean(context.Background(), l, plan.Remove) {
		if err != nil {
			t.Errorf("%s: %v", plan.Remove[i].Path, err)
		}
	}
	if entries, _ := os.ReadDir(quarantine(l)); len(entries) != 0 {
		t.Errorf("the quarantine holds %d entries", len(entries))
	}
}

func TestCleanListsWhatRecoveryRemoves(t *testing.T) {
	t.Parallel()
	l := cleanLayout(t)
	now := time.Now()
	record := func(id string, s *start) string {
		dir := filepath.Join(l.Records, id)
		must(t, os.MkdirAll(dir, 0o700))
		if s != nil {
			s.Record.ID, s.Record.RecordsDir = id, dir
			must(t, (Env{}).writeStart(*s))
		}
		return dir
	}
	// A stored run's leftover grade folder.
	stored := record("r-stored", nil)
	fill(t, filepath.Join(stored, gradingFolder, "cache"), 2000)
	// A dead run whose agent never started: its workspace and records go.
	ws := filepath.Join(l.Workspaces, "r-early")
	fill(t, filepath.Join(ws, "repo"), 3000)
	early := record("r-early", &start{Workspace: ws})
	// A dead run whose agent started: its workspace goes, its records stay for recovery to store.
	ws2 := filepath.Join(l.Workspaces, "r-cut")
	fill(t, filepath.Join(ws2, "repo"), 3000)
	cut := record("r-cut", &start{Workspace: ws2, AgentStarted: true})
	fill(t, filepath.Join(cut, "verify"), 1000)
	// A run whose process group exists (this test's), and one whose start file is unreadable: both kept.
	live := record("r-live", &start{Workspace: filepath.Join(l.Workspaces, "r-live"), AgentStarted: true, PGID: syscall.Getpgrp()})
	broken := record("r-broken", nil)
	must(t, os.WriteFile(filepath.Join(broken, startFile), []byte(`{"record": {"id"`), 0o600))

	isStored := func(id string) (bool, error) { return id == "r-stored", nil }
	plan, err := PlanClean(context.Background(), CleanInput{Layout: l, Now: now, OlderThan: CleanDefaultAge, Stored: isStored})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{stored, early, cut} {
		if it, ok := byPath(plan.Remove, dir); !ok || it.Kind != CleanLeftovers || it.Reason != CleanStoppedRun || it.Bytes <= 0 {
			t.Errorf("%s: listed %v, %+v", dir, ok, it)
		}
	}
	for dir, reason := range map[string]string{live: CleanKeptRunning, broken: CleanKeptUnreadable} {
		if it, ok := byPath(plan.Keep, dir); !ok || it.Reason != reason {
			t.Errorf("%s: kept %v, %q; want %q", dir, ok, it.Reason, reason)
		}
	}
	// Recovery removes what was listed; Gone measures it. (The live run makes Recover report it; nothing else fails.)
	if _, err := Recover(context.Background(), l, isStored, "", now); err == nil || !strings.Contains(err.Error(), "r-live") {
		t.Fatalf("recover: %v", err)
	}
	for _, dir := range []string{stored, early, cut} {
		it, _ := byPath(plan.Remove, dir)
		if got := it.Gone(); got != it.Bytes {
			t.Errorf("%s: %d of %d bytes gone", dir, got, it.Bytes)
		}
	}
	if !exists(filepath.Join(cut, startFile)) || exists(ws2) {
		t.Error("the stopped run's records should stay and its workspace go")
	}
}

func TestMarkUsedSetsTheLastUseAndNeverFollowsALink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	seed := filepath.Join(dir, "cache", "go-77777777-"+baseA)
	fill(t, seed, 1)
	usedAgo(t, seed, now, 72*time.Hour)
	if err := prepareSeed(context.Background(), nil, "", seed, nil); err != nil {
		t.Fatal(err)
	}
	if age := time.Since(modTime(seed)); age > time.Hour {
		t.Errorf("a seed taken by prepareSeed looks %s old", age)
	}
	target := filepath.Join(dir, "target")
	fill(t, target, 1)
	usedAgo(t, target, now, 72*time.Hour)
	link := filepath.Join(dir, "link")
	must(t, os.Symlink(target, link))
	markUsed(link)
	if age := time.Since(modTime(target)); age < 71*time.Hour {
		t.Errorf("markUsed followed a link: the target looks %s old", age)
	}
}
