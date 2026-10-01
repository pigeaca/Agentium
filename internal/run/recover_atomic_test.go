package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if _, err := Recover(context.Background(), layout, stored5, "", now); !errors.As(err, &aliveErr) || len(aliveErr.Runs) != 1 || !strings.Contains(aliveErr.Runs[0], "r5") {
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
