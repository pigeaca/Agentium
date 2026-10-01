package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/home"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, startFile)
	for _, body := range []string{"old", "newer content"} {
		if err := writeFileAtomic(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); string(got) != body {
			t.Errorf("content = %q, want %q", got, body)
		}
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left: %v", entries)
	}

	// A failure at the rename (the target is a folder) leaves the target alone and removes the temp file.
	bad := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(bad, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(bad, []byte("x"), 0o600); err == nil {
		t.Fatal("rename over a folder succeeded")
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 { // started.json and sub
		t.Errorf("temp file left after a failed write: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(bad, "keep")); err != nil {
		t.Error(err)
	}
}

func TestRecoverUnreadableStartFile(t *testing.T) {
	layout, err := home.Resolve(func(key string) string {
		return map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data")}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	layout.Temp = t.TempDir()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	write := func(p, body string, mtime time.Time) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	transcript := func(ws string) string {
		return `{"type":"system","subtype":"init","cwd":"` + filepath.Join(ws, "repo") + `","claude_code_version":"2.1.281","model":"claude-sonnet-5"}
{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":1000,"cache_creation_input_tokens":20000},"content":[]}}
`
	}
	// r1 and r2: unreadable start files (empty, truncated) of runs that stopped long ago, in experiment workspaces.
	// r3: a valid file of another shape. r4: no transcript, written long ago. r5: a transcript still being written.
	for id, body := range map[string]string{"r1": "", "r2": `{"record":{"id":"r2"`} {
		dir := filepath.Join(layout.Records, id)
		ws := filepath.Join(layout.Workspaces, "e1-s"+id[1:]+"-t1")
		write(filepath.Join(dir, startFile), body, old)
		write(filepath.Join(dir, "stream.jsonl"), transcript(ws), old)
		write(filepath.Join(dir, "notes.txt"), "token sk-"+"ant-api03-abcdefghijklmnopqrstuvwxyz", old)
		write(filepath.Join(dir, "verify", "hidden_test.go"), "", old)
		write(filepath.Join(dir, "judge", "config.json"), "login", old)
		write(filepath.Join(ws, "repo", "a.txt"), "", old)
		write(filepath.Join(layout.RunTemp(filepath.Base(ws)), "x"), "", old)
	}
	write(filepath.Join(layout.Records, "r1", startFile+".tmp-123"), "partial", old) // a killed write's leftover
	stored := func(string) (bool, error) { return false, nil }
	orphans, err := Recover(context.Background(), layout, stored, "", now)
	if err != nil || len(orphans) != 2 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	for i, o := range orphans {
		dir := filepath.Join(layout.Records, o.Record.ID)
		ws := filepath.Join(layout.Workspaces, "e1-s"+o.Record.ID[1:]+"-t1")
		if o.Unreadable != filepath.Join(dir, startFile+corruptSuffix) || o.Record.Metrics.CostUSD != 0.082 || !o.Record.CostEstimated ||
			!strings.Contains(strings.Join(o.Record.Notes, ";"), "judge spend") {
			t.Errorf("orphan %d = %+v", i, o)
		}
		for _, gone := range []string{filepath.Join(dir, startFile), filepath.Join(dir, "verify"), filepath.Join(dir, "judge"), ws, layout.RunTemp(filepath.Base(ws))} {
			if _, err := os.Stat(gone); err == nil {
				t.Errorf("%s was left behind", gone)
			}
		}
		if _, err := os.Stat(o.Unreadable); err != nil {
			t.Error(err)
		}
		if data, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); strings.Contains(string(data), "sk-ant") {
			t.Errorf("records not redacted: %s", data)
		}
	}
	if stray, _ := filepath.Glob(filepath.Join(layout.Records, "r1", startFile+".tmp-*")); len(stray) != 0 {
		t.Errorf("temp files left: %v", stray)
	}
	// Later recoveries are not blocked, report nothing again and keep the folders.
	orphans, err = Recover(context.Background(), layout, stored, "", now)
	if err != nil || len(orphans) != 0 {
		t.Fatalf("second Recover = %+v, %v", orphans, err)
	}
	if _, err := os.Stat(filepath.Join(layout.Records, "r1", "stream.jsonl")); err != nil {
		t.Errorf("the transcript was removed: %v", err)
	}

	// Valid JSON of another shape is not damage: it fails loudly and is left in place.
	write(filepath.Join(layout.Records, "r3", startFile), `{"record":"text"}`, old)
	if _, err := Recover(context.Background(), layout, func(id string) (bool, error) { return id != "r3", nil }, "", now); err == nil {
		t.Error("a start file of another shape was called corrupt")
	}
	if _, err := os.Stat(filepath.Join(layout.Records, "r3", startFile)); err != nil {
		t.Error("a start file of another shape was moved")
	}
	os.RemoveAll(filepath.Join(layout.Records, "r3"))

	// No transcript, written long ago: the agent never started and nothing was spent, so the folder goes, with the
	// default workspace.
	write(filepath.Join(layout.Records, "r4", startFile), "", old)
	write(filepath.Join(layout.Workspaces, "r4", "repo", "a.txt"), "", old)
	orphans, err = Recover(context.Background(), layout, func(id string) (bool, error) { return id != "r4", nil }, "", now)
	if err != nil || len(orphans) != 0 {
		t.Fatalf("no transcript: %+v, %v", orphans, err)
	}
	for _, gone := range []string{filepath.Join(layout.Records, "r4"), filepath.Join(layout.Workspaces, "r4")} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s was left behind", gone)
		}
	}

	// A transcript without a result, written a minute ago: its agent may be working. Nothing is touched. Once it has a
	// result, or is old, it is recovered.
	ws := filepath.Join(layout.Workspaces, "e1-s5-t1")
	write(filepath.Join(layout.Records, "r5", startFile), "{", now.Add(-time.Minute))
	write(filepath.Join(layout.Records, "r5", "stream.jsonl"), transcript(ws), now.Add(-time.Minute))
	write(filepath.Join(ws, "repo", "a.txt"), "", old)
	stored5 := func(id string) (bool, error) { return id != "r5", nil }
	var aliveErr *AliveError
	if _, err := Recover(context.Background(), layout, stored5, "", now); !errors.As(err, &aliveErr) || len(aliveErr.Unreadable) != 1 || !strings.Contains(aliveErr.Unreadable[0], "r5") ||
		!strings.Contains(aliveErr.Unreadable[0], "stream.jsonl") || !strings.Contains(aliveErr.Unreadable[0], "try again after") || strings.Contains(aliveErr.Error(), "PGID") {
		t.Fatalf("a recent transcript: %v", err)
	}
	for _, kept := range []string{filepath.Join(layout.Records, "r5", startFile), filepath.Join(ws, "repo", "a.txt")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("a possibly live run was touched: %v", err)
		}
	}
	write(filepath.Join(layout.Records, "r5", "stream.jsonl"), transcript(ws)+`{"type":"result","subtype":"success","total_cost_usd":0.5,"is_error":false}`+"\n", now.Add(-time.Minute))
	if orphans, err := Recover(context.Background(), layout, stored5, "", now); err != nil || len(orphans) != 1 {
		t.Fatalf("a transcript with a result: %+v, %v", orphans, err)
	}
	if _, err := os.Stat(ws); err == nil {
		t.Error("the workspace was left behind")
	}
}

// The rename is the commit point: when cleanup fails the start file stays, so the next start retries it.
func TestRecoverUnreadableRetriesAfterAFailedCleanup(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("file permissions do not bind root")
	}
	layout, err := home.Resolve(func(key string) string {
		return map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data")}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	dir := filepath.Join(layout.Records, "r1")
	notes := filepath.Join(dir, "notes.txt")
	for p, body := range map[string]string{filepath.Join(dir, startFile): "{", filepath.Join(dir, "stream.jsonl"): `{"type":"system","subtype":"init"}` + "\n",
		notes: "token sk-" + "ant-api03-abcdefghijklmnopqrstuvwxyz", filepath.Join(dir, "judge", "config.json"): "login"} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, old, old)
	}
	stored := func(string) (bool, error) { return false, nil }
	if err := os.Chmod(notes, 0); err != nil { // redaction cannot read it
		t.Fatal(err)
	}
	if _, err := Recover(context.Background(), layout, stored, "", now); err == nil {
		t.Fatal("a failed redaction was not reported")
	}
	if _, err := os.Stat(filepath.Join(dir, startFile)); err != nil {
		t.Fatalf("the start file was moved before cleanup finished: %v", err)
	}
	if err := os.Chmod(notes, 0o600); err != nil {
		t.Fatal(err)
	}
	orphans, err := Recover(context.Background(), layout, stored, "", now)
	if err != nil || len(orphans) != 1 {
		t.Fatalf("retry = %+v, %v", orphans, err)
	}
	if data, _ := os.ReadFile(notes); strings.Contains(string(data), "sk-ant") {
		t.Errorf("not redacted: %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "judge")); err == nil {
		t.Error("judge folder left")
	}
	if _, err := os.Stat(filepath.Join(dir, startFile+corruptSuffix)); err != nil {
		t.Error(err)
	}
}

// A transcript's working directory must not steer a removal outside the workspaces folder or onto the folder itself.
func TestRecoverUnreadableTouchesNothingOutsideWorkspaces(t *testing.T) {
	layout, err := home.Resolve(func(key string) string {
		return map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data")}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "precious"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(layout.Workspaces, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	guard := filepath.Join(layout.Workspaces, "guard")
	if err := os.WriteFile(guard, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwds := map[string]string{
		"outside":  filepath.Join(outside, "repo"),
		"direct":   filepath.Join(layout.Workspaces, "repo"), // its parent is the workspaces folder itself
		"dotdot":   filepath.Join(layout.Workspaces, "w", "..", "..", "x", "repo"),
		"symlink":  filepath.Join(link, "repo"),
		"relative": "workspaces/w/repo",
		"notrepo":  filepath.Join(layout.Workspaces, "w", "src"),
	}
	for id, cwd := range cwds {
		dir := filepath.Join(layout.Records, id)
		for p, body := range map[string]string{filepath.Join(dir, startFile): "{",
			filepath.Join(dir, "stream.jsonl"): `{"type":"system","subtype":"init","cwd":"` + cwd + `"}` + "\n"} {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			os.Chtimes(p, old, old)
		}
	}
	if err := os.MkdirAll(filepath.Join(layout.Workspaces, "w", "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	orphans, err := Recover(context.Background(), layout, func(string) (bool, error) { return false, nil }, "", now)
	if err != nil || len(orphans) != len(cwds) {
		t.Fatalf("Recover = %d orphans, %v", len(orphans), err)
	}
	for _, kept := range []string{filepath.Join(outside, "precious"), filepath.Join(outside, "repo"), guard, link, filepath.Join(layout.Workspaces, "w", "src")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was touched: %v", kept, err)
		}
	}
}

func TestRemoveStaleWorkspace(t *testing.T) {
	workspaces := filepath.Join(t.TempDir(), "workspaces")
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "precious"), []byte("x"), 0o600)
	stale := filepath.Join(workspaces, "e1-s2-t1")
	if err := os.MkdirAll(filepath.Join(stale, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleWorkspace(workspaces, stale, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("the stale workspace was left")
	}
	if err := removeStaleWorkspace(workspaces, stale, ""); err != nil {
		t.Errorf("a missing workspace: %v", err)
	}
	link := filepath.Join(workspaces, "linked")
	os.Symlink(outside, link)
	for _, bad := range []string{link, workspaces, filepath.Join(workspaces, "a", "b"), outside} {
		if err := removeStaleWorkspace(workspaces, bad, ""); err != nil {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Errorf("a path outside was removed: %v", err)
	}
	if _, err := os.Stat(workspaces); err != nil {
		t.Errorf("the workspaces folder was removed: %v", err)
	}
}

func TestRecoverUnreadableUsesAReadablePGID(t *testing.T) {
	layout, err := home.Resolve(func(key string) string {
		return map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data")}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	pgid := syscall.Getpgrp() // exists
	dir := filepath.Join(layout.Records, "r1")
	os.MkdirAll(dir, 0o700)
	old := time.Now().Add(-time.Hour)
	for name, body := range map[string]string{startFile: fmt.Sprintf(`{"agent_started":true,"pgid":%d,"meta":`, pgid), "stream.jsonl": `{"type":"system","subtype":"init"}` + "\n"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		os.Chtimes(p, old, old)
	}
	var aliveErr *AliveError
	_, err = Recover(context.Background(), layout, func(string) (bool, error) { return false, nil }, "", time.Now())
	if !errors.As(err, &aliveErr) || len(aliveErr.Unreadable) != 1 || !strings.Contains(aliveErr.Unreadable[0], "process group") {
		t.Fatalf("a live group in a truncated file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, startFile)); err != nil {
		t.Errorf("a live run was touched: %v", err)
	}
}
