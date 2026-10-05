package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/store"
)

// cleanFixture is a run fixture (the project and its task "value") with cache content: a seed and warmed
// dependencies of the task's base, a seed of a base no task uses, and a quarantined folder.
type cleanFixture struct {
	runFixture
	layout           home.Layout
	project          string // the project's folder name in the cache and deps folders (its ID)
	base             string // the task's base
	usedSeed         string
	unusedSeed       string
	deps, quarantine string
}

func newCleanFixture(t *testing.T) cleanFixture {
	t.Helper()
	f := cleanFixture{runFixture: newRunFixture(t, filepath.Join(t.TempDir(), "data"))}
	var err error
	if f.layout, err = home.Resolve(func(k string) string { return f.vars[k] }); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), f.layout.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := db.Projects(context.Background())
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %v, %v", projects, err)
	}
	task, err := db.TaskByName(context.Background(), projects[0].ID, "value")
	if err != nil {
		t.Fatal(err)
	}
	f.project, f.base = strconv.FormatInt(projects[0].ID, 10), task.BaseCommit
	now := time.Now()
	seeds := filepath.Join(f.layout.Cache, "grading-seed", f.project)
	f.usedSeed = filepath.Join(seeds, "go-12345678-"+f.base)
	f.unusedSeed = filepath.Join(seeds, "go-12345678-"+strings.Repeat("e", 40))
	f.deps = filepath.Join(f.layout.Deps, f.project)
	f.quarantine = filepath.Join(f.layout.Cache, "quarantine", "r1-grading-abcdef")
	for _, dir := range []string{f.usedSeed, f.unusedSeed, filepath.Join(f.deps, "m2"), f.quarantine} {
		writeFile(t, dir, "data", strings.Repeat("x", 4096))
	}
	writeFile(t, filepath.Join(f.layout.Cache, "warm-state", f.project), "maven-v1-"+f.base, "")
	for _, p := range []string{f.usedSeed, f.unusedSeed, filepath.Join(f.layout.Cache, "warm-state", f.project, "maven-v1-"+f.base), f.deps} {
		if err := os.Chtimes(p, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestCleanDryRunWritesNothingAndYesRemovesWhatItListed(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	before := tree(t, f.data)
	dry := f.run(context.Background(), "clean")
	expect(t, dry, ExitOK, "a dry run: nothing was removed", "seeds", "dependencies", "quarantine", "leftovers", "total",
		"What would go:", "cache/grading-seed/"+f.project+"/go-12345678-"+strings.Repeat("e", 40), "no task or experiment uses its base",
		"cache/quarantine/r1-grading-abcdef", "Kept:", "in use by task value", "agentium clean --yes")
	if after := tree(t, f.data); after != before {
		t.Errorf("the dry run changed the data folder:\nbefore\n%s\nafter\n%s", before, after)
	}
	if strings.Contains(dry.stdout, "deps/"+f.project+" ") && strings.Contains(dry.stdout, "What would go:\n  dependencies") {
		t.Errorf("the task's dependencies are listed for removal:\n%s", dry.stdout)
	}

	expect(t, f.run(context.Background(), "clean", "--yes"), ExitOK, "freed", "What went:")
	for _, gone := range []string{f.unusedSeed, f.quarantine} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{filepath.Join(f.usedSeed, "data"), filepath.Join(f.deps, "m2", "data"), f.layout.Database} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s: %v", kept, err)
		}
	}
	expect(t, f.run(context.Background(), "clean"), ExitOK, "Nothing to remove.")

	// Unused for longer than --older-than, what the task uses goes as well.
	res := f.run(context.Background(), "clean", "--older-than", "1d", "--yes")
	expect(t, res, ExitOK, "in use, but not used for 2 days")
	if _, err := os.Lstat(f.usedSeed); err == nil {
		t.Error("an old seed is still there")
	}
	if _, err := os.Lstat(f.deps); err == nil {
		t.Error("old dependencies are still there")
	}
	if _, err := os.Lstat(filepath.Join(f.layout.Cache, "warm-state", f.project, "maven-v1-"+f.base)); err == nil {
		t.Error("the stamp of removed dependencies is still there: the next run would not warm them again")
	}
}

func TestCleanRespectsTheRunLock(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	release, err := f.layout.LockRuns()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	expect(t, f.run(context.Background(), "clean", "--yes"), ExitError, "another Agentium process is running agents", "run agentium clean when it is done")
	if _, err := os.Lstat(f.unusedSeed); err != nil {
		t.Errorf("clean removed %s while runs were in progress", f.unusedSeed)
	}
	// A dry run still shows what would go, leaving out what stopped runs left (a live run looks like one).
	expect(t, f.run(context.Background(), "clean"), ExitOK, "a dry run", "runs are in progress: what stopped runs left is not listed")
	doc := checkJSON(t, f.runFixture, f.run(context.Background(), "clean", "--yes", "--json"), ExitError, []string{"clean"})
	if msg, _ := doc.get("error", "message").(string); !strings.Contains(msg, "another Agentium process") {
		t.Errorf("error message %q", msg)
	}
}

func TestCleanKeepsTheBaseOfALockedExperimentsRetiredTask(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	db, err := store.Open(context.Background(), f.layout.Database)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(f.project, 10, 64)
	task, err := db.TaskByName(context.Background(), id, "value")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RetireTask(context.Background(), task.ID, "old", time.Now()); err != nil {
		t.Fatal(err)
	}
	e, err := db.SaveExperiment(context.Background(), store.Experiment{ProjectID: id, Name: "ab", Template: "context-ab", Design: []byte(`{}`), CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	lock, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "value", "base": f.base}}})
	if err := db.LockExperiment(context.Background(), e.ID, lock); err != nil {
		t.Fatal(err)
	}
	// Locked and unfinished in any status but done: running, stopped, over its budget or paused for usage. Its bases
	// stay whatever their age (--older-than does not apply to them).
	for _, status := range []string{store.StatusRunning, store.StatusStopped, store.StatusBudget, store.StatusUsage} {
		if err := db.SetExperimentStatus(context.Background(), e.ID, status, ""); err != nil {
			t.Fatal(err)
		}
		expect(t, f.run(context.Background(), "clean", "--yes", "--older-than", "1d"), ExitOK, "in use by experiment ab")
		if _, err := os.Lstat(f.usedSeed); err != nil {
			t.Errorf("status %s: the experiment's seed was removed: %v", status, err)
		}
	}
	db.Close()

	// Once the experiment is done, nothing uses the retired task's base.
	db, err = store.Open(context.Background(), f.layout.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetExperimentStatus(context.Background(), e.ID, store.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	db.Close()
	expect(t, f.run(context.Background(), "clean", "--yes"), ExitOK)
	if _, err := os.Lstat(f.usedSeed); err == nil {
		t.Error("the seed of a base nothing uses is still there")
	}
}

func TestCleanRemovesWhatAStoppedRunLeft(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	dir := filepath.Join(f.layout.Records, "20261003-dead")
	workspace := filepath.Join(f.layout.Workspaces, "20261003-dead")
	writeFile(t, filepath.Join(workspace, "repo"), "file", strings.Repeat("y", 8192))
	start, _ := json.Marshal(map[string]any{"record": map[string]any{"id": "20261003-dead", "records": dir}, "workspace": workspace, "agent_started": false})
	writeFile(t, dir, "started.json", string(start))
	expect(t, f.run(context.Background(), "clean"), ExitOK, "records/20261003-dead", "a run that stopped before its agent started")
	if _, err := os.Lstat(workspace); err != nil {
		t.Fatal("the dry run removed the workspace")
	}
	expect(t, f.run(context.Background(), "clean", "--yes"), ExitOK, "What went:")
	for _, gone := range []string{workspace, dir} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
}

// An orphaned agent may still be running: clean --yes refuses as every run's start does, and removes nothing.
func TestCleanRefusesWhileAStoppedRunsAgentMayStillRun(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	dir := filepath.Join(f.layout.Records, "20261003-alive")
	start, _ := json.Marshal(map[string]any{"record": map[string]any{"id": "20261003-alive", "records": dir},
		"workspace": filepath.Join(f.layout.Workspaces, "20261003-alive"), "agent_started": true, "pgid": syscall.Getpgrp()})
	writeFile(t, dir, "started.json", string(start))
	expect(t, f.run(context.Background(), "clean"), ExitOK, "records/20261003-alive", "--yes refuses until it ends")
	expect(t, f.run(context.Background(), "clean", "--yes"), ExitError, "may still be running")
	// (Recovery itself empties the quarantine first, as at every run's start; seeds and dependencies stay.)
	for _, kept := range []string{f.unusedSeed, f.deps} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was removed although clean refused", kept)
		}
	}
}

func TestCleanJSON(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	dry := jsonRun(t, f.runFixture, ExitOK, "clean")
	assertKeys(t, dry.doc, "command,dry_run,freed_bytes,kept,kinds,leftovers_checked,notes,older_than_seconds,remove,remove_bytes,schema,warnings")
	if dry.get("dry_run") != true || dry.get("freed_bytes") != nil || dry.get("older_than_seconds") != float64(30*24*3600) {
		t.Errorf("dry run document: %v", dry.doc)
	}
	remove := dry.get("remove").([]any)
	if len(remove) != 2 {
		t.Fatalf("remove %v", remove)
	}
	assertKeys(t, remove[0], "base,bytes,kind,last_used,note,path,problem,project,removed,why")
	assertKeys(t, dry.get("kinds").([]any)[0], "keep,keep_bytes,kind,remove,remove_bytes")
	seed := remove[0].(map[string]any)
	if seed["kind"] != "seeds" || seed["why"] != "unused" || seed["removed"] != nil || seed["path"] != "cache/grading-seed/"+f.project+"/go-12345678-"+strings.Repeat("e", 40) {
		t.Errorf("seed item %v", seed)
	}
	kept := dry.get("kept").([]any)
	reasons := map[string]bool{}
	for _, k := range kept {
		reasons[k.(map[string]any)["why"].(string)] = true
	}
	if !reasons["in_use_by_task"] {
		t.Errorf("kept %v", kept)
	}

	res := f.run(context.Background(), "clean", "--yes", "--json")
	done := checkJSON(t, f.runFixture, res, ExitOK, []string{"clean"})
	if done.get("dry_run") != false || done.get("freed_bytes") == nil || done.get("freed_bytes").(float64) <= 0 {
		t.Errorf("--yes document: %v", done.doc)
	}
	for _, it := range done.get("remove").([]any) {
		if it.(map[string]any)["removed"] != true {
			t.Errorf("not removed: %v", it)
		}
	}
	// Under --yes the kinds count what went: a seed whose lock a warm-up holds stays, and is not counted.
	g := newCleanFixture(t)
	unlock, err := home.LockFile(context.Background(), g.unusedSeed+".lock", nil)
	if err != nil {
		t.Fatal(err)
	}
	busy := checkJSON(t, g.runFixture, g.run(context.Background(), "clean", "--yes", "--json"), ExitOK, []string{"clean"})
	unlock()
	seeds := busy.get("kinds").([]any)[0].(map[string]any)
	if seeds["kind"] != "seeds" || seeds["remove"] != float64(0) || seeds["remove_bytes"] != float64(0) {
		t.Errorf("--yes kinds: %v", seeds)
	}
	item := busy.get("remove").([]any)[0].(map[string]any)
	if item["removed"] != false || item["problem"] == nil {
		t.Errorf("the busy seed's item: %v", item)
	}

	// Usage errors are documents too.
	bad := f.run(context.Background(), "clean", "--older-than", "10m", "--json")
	doc := checkJSON(t, f.runFixture, bad, ExitUsage, []string{"clean"})
	if msg, _ := doc.get("error", "message").(string); !strings.Contains(msg, "at least 1h") {
		t.Errorf("error message %q", msg)
	}
}

func TestCleanWithoutADataFolderOrCache(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "none")
	f := runFixtureAt(t.TempDir(), missing, t.TempDir())
	expect(t, f.run(context.Background(), "clean"), ExitOK, "Nothing to clean")
	expect(t, f.run(context.Background(), "clean", "--yes"), ExitOK, "Nothing to clean")
	if _, err := os.Lstat(missing); err == nil {
		t.Error("clean created the data folder")
	}
	// A data folder without a cache: nothing to remove.
	g := newRunFixture(t, filepath.Join(t.TempDir(), "data"))
	expect(t, g.run(context.Background(), "clean", "--yes"), ExitOK, "0 B freed")
	expect(t, g.run(context.Background(), "clean"), ExitOK, "Nothing to remove.")
}

func TestCleanUsageErrors(t *testing.T) {
	t.Parallel()
	f := runFixtureAt(t.TempDir(), filepath.Join(t.TempDir(), "data"), t.TempDir())
	for _, args := range [][]string{{"clean", "now"}, {"clean", "--older-than", "soon"}, {"clean", "--older-than", "30m"}, {"clean", "--older-than", "-2d"}} {
		expect(t, f.run(context.Background(), args...), ExitUsage)
	}
	for _, age := range []string{"2h", "7d", "1.5d"} {
		if _, err := parseAge(age); err != nil {
			t.Errorf("%s: %v", age, err)
		}
	}
	expect(t, f.run(context.Background(), "clean", "-h"), ExitOK, "Usage: agentium clean")
}

// With the login, clean lists the session folders this data folder's runs left in Claude Code's projects folder (a
// stand-in under the test's home) by count and size, never by name, and --yes removes them; the user's own projects and
// a folder that holds a session file stay. With another sign-in it lists none.
func TestCleanSessions(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	workspaces, err := filepath.EvalSymlinks(f.layout.Workspaces)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(f.home, ".claude")
	old := time.Now().Add(-48 * time.Hour)
	session := func(id string, files ...string) string {
		p := claude.SessionFolder(config, filepath.Join(workspaces, id, "repo"))
		writeFile(t, filepath.Join(p, "tool-results"), "t1.txt", strings.Repeat("o", 5000))
		for _, name := range files {
			writeFile(t, p, name, "{}\n")
		}
		for _, q := range []string{filepath.Join(p, "tool-results", "t1.txt"), filepath.Join(p, "tool-results"), p} {
			if err := os.Chtimes(q, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	// The store knows a run and an experiment slot's first try: only their folders go. A name it does not know (another
	// run ID, the slot's second try, another slot) may be another data folder's whose path encodes the same: it stays.
	ctx := context.Background()
	db, err := store.Open(ctx, f.layout.Database)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.ParseInt(f.project, 10, 64)
	e, err := db.SaveExperiment(ctx, store.Experiment{ProjectID: pid, Name: "ab", Template: "context-ab", Design: []byte(`{}`), CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []store.Run{{ID: "20261004T101500Z-0a1b2c", ProjectID: pid, TaskName: "value", Arm: "A", Outcome: "ok"},
		{ID: "20261004T101600Z-0d0e0f", ProjectID: pid, TaskName: "value", Arm: "A", Outcome: "ok", ExperimentID: e.ID, Slot: 2, Attempt: 1}} {
		if err := db.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	slot := func(s, try int) string { return fmt.Sprintf("e%d-s%d-t%d", e.ID, s, try) }
	gone, slotGone := session("20261004T101500Z-0a1b2c"), session(slot(2, 1))
	withSession := session(slot(7, 1), "5e6f.jsonl")
	unknown := []string{session("20261004T101500Z-ffffff"), session(slot(2, 2)), session(slot(5, 1))}
	own := filepath.Join(config, "projects", "-Users-someone-code-app")
	writeFile(t, own, "9a8b.jsonl", "{}\n")
	hidden := []string{filepath.Base(gone), filepath.Base(slotGone), filepath.Base(withSession), filepath.Base(workspaces), "-Users-someone"}
	unnamed := func(text string) {
		t.Helper()
		for _, h := range hidden {
			if strings.Contains(text, h) {
				t.Errorf("the output names a folder in Claude Code's projects folder (%s):\n%s", h, text)
			}
		}
	}

	dry := f.run(context.Background(), "clean")
	expect(t, dry, ExitOK, "sessions", "in Claude Code's projects folder", "2 folders that runs of this data folder left",
		"1 folder named like this data folder's runs' that holds a session file of its own, which no run leaves: left alone",
		"3 folders named like this data folder's runs' that it has no run of (perhaps another data folder's): left alone")
	unnamed(dry.stdout)
	doc := jsonRun(t, f.runFixture, ExitOK, "clean")
	var kind map[string]any
	for _, k := range doc.get("kinds").([]any) {
		if k.(map[string]any)["kind"] == "sessions" {
			kind = k.(map[string]any)
		}
	}
	if kind == nil || kind["remove"] != float64(2) || kind["keep"] != float64(4) || kind["remove_bytes"].(float64) < 10000 {
		t.Errorf("sessions kind: %v", kind)
	}
	var item map[string]any
	for _, it := range doc.get("remove").([]any) {
		if m := it.(map[string]any); m["kind"] == "sessions" && strings.Contains(m["note"].(string), "20261004T101500Z-0a1b2c") {
			item = m
		}
	}
	unknownKept := 0
	for _, it := range doc.get("kept").([]any) {
		if m := it.(map[string]any); m["kind"] == "sessions" && m["why"] == "unknown_run" {
			unknownKept++
		}
	}
	if unknownKept != 3 {
		t.Errorf("kept as unknown runs: %d, want 3", unknownKept)
	}
	if item == nil || item["path"] != "in Claude Code's projects folder" || item["why"] != "left_by_run_session" ||
		!strings.Contains(item["note"].(string), "20261004T101500Z-0a1b2c") {
		t.Errorf("sessions item: %v", item)
	}
	raw, _ := json.Marshal(doc.doc)
	unnamed(string(raw))
	if _, err := os.Lstat(gone); err != nil {
		t.Fatal("the dry run removed it")
	}

	// Another sign-in keeps its sessions in its own workspace: none are listed.
	f.vars["ANTHROPIC_API_KEY"] = "sk-ant-api03-clean-test" // secret-scan: allow
	if res := f.run(context.Background(), "clean"); strings.Contains(res.stdout, "in Claude Code's projects folder") {
		t.Errorf("listed with an API key:\n%s", res.stdout)
	}
	delete(f.vars, "ANTHROPIC_API_KEY")

	yes := f.run(context.Background(), "clean", "--yes")
	expect(t, yes, ExitOK, "What went:", "2 folders that runs of this data folder left")
	unnamed(yes.stdout)
	for _, p := range []string{gone, slotGone} {
		if _, err := os.Lstat(p); err == nil {
			t.Error("a stored run's folder is still there")
		}
	}
	for _, kept := range append([]string{filepath.Join(withSession, "5e6f.jsonl"), filepath.Join(own, "9a8b.jsonl")}, unknown...) {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s: %v", filepath.Base(kept), err)
		}
	}
}

// A dead run's session folder is recovery's to remove, with its workspace: clean plans and counts it with that run's
// leftovers, not as a session folder kept for its workspace, and --yes reports it as freed.
func TestCleanCountsADeadRunsSessionFolderWithItsLeftovers(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	workspaces, err := filepath.EvalSymlinks(f.layout.Workspaces)
	if err != nil {
		t.Fatal(err)
	}
	id := "20261004T111500Z-0a0b0c"
	dir, workspace := filepath.Join(f.layout.Records, id), filepath.Join(f.layout.Workspaces, id)
	writeFile(t, filepath.Join(workspace, "repo"), "file", "x")
	session := claude.SessionFolder(filepath.Join(f.home, ".claude"), filepath.Join(workspaces, id, "repo"))
	writeFile(t, filepath.Join(session, "tool-results"), "big.txt", strings.Repeat("o", 1<<20))
	old := time.Now().Add(-48 * time.Hour)
	for _, q := range []string{filepath.Join(session, "tool-results", "big.txt"), filepath.Join(session, "tool-results"), session} {
		if err := os.Chtimes(q, old, old); err != nil {
			t.Fatal(err)
		}
	}
	start, _ := json.Marshal(map[string]any{"record": map[string]any{"id": id, "records": dir, "sign_in": "login"},
		"workspace": workspace, "agent_started": true, "session": session})
	writeFile(t, dir, "started.json", string(start))
	kinds := func(doc jsonResult) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, k := range doc.get("kinds").([]any) {
			out[k.(map[string]any)["kind"].(string)] = k.(map[string]any)
		}
		return out
	}

	dry := jsonRun(t, f.runFixture, ExitOK, "clean")
	k := kinds(dry)
	if k["sessions"]["keep"] != float64(0) || k["sessions"]["remove"] != float64(0) || k["leftovers"]["remove"] != float64(1) ||
		k["leftovers"]["remove_bytes"].(float64) < 1<<20 {
		t.Errorf("dry run kinds: sessions %v, leftovers %v", k["sessions"], k["leftovers"])
	}
	expect(t, f.run(context.Background(), "clean"), ExitOK, "records/"+id, "a run that stopped: recovery stores it as cancelled")

	res := f.run(context.Background(), "clean", "--yes", "--json")
	done := checkJSON(t, f.runFixture, res, ExitOK, []string{"clean"})
	if freed := done.get("freed_bytes").(float64); freed < 1<<20 {
		t.Errorf("freed %v: the session folder is not counted", freed)
	}
	if k := kinds(done); k["leftovers"]["remove"] != float64(1) || k["sessions"]["keep"] != float64(0) {
		t.Errorf("--yes kinds: sessions %v, leftovers %v", k["sessions"], k["leftovers"])
	}
	for _, gone := range []string{session, workspace} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s is still there", filepath.Base(gone))
		}
	}
}
