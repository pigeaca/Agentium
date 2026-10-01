package run

import (
	"context"
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
	transcript := `{"type":"system","subtype":"init","claude_code_version":"2.1.281","model":"claude-sonnet-5"}
{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":1000,"cache_creation_input_tokens":20000},"content":[]}}
`
	for id, body := range map[string]string{"r1": "", "r2": `{"record":{"id":"r2"`} {
		dir := filepath.Join(layout.Records, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, startFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stored := func(string) (bool, error) { return false, nil }
	orphans, err := Recover(context.Background(), layout, stored, "", now)
	if err != nil || len(orphans) != 2 {
		t.Fatalf("Recover = %+v, %v", orphans, err)
	}
	for _, o := range orphans {
		dir := filepath.Join(layout.Records, o.Record.ID)
		if o.Unreadable != filepath.Join(dir, startFile+corruptSuffix) || o.Record.Metrics.CostUSD != 0.082 || !o.Record.CostEstimated ||
			!strings.Contains(strings.Join(o.Record.Notes, ";"), "judge spend") {
			t.Errorf("orphan = %+v", o)
		}
		if _, err := os.Stat(filepath.Join(dir, startFile)); err == nil {
			t.Error("the unreadable file was not moved aside")
		}
		if _, err := os.Stat(o.Unreadable); err != nil {
			t.Error(err)
		}
	}
	// Later recoveries are not blocked, report nothing again and keep the folders.
	orphans, err = Recover(context.Background(), layout, stored, "", now)
	if err != nil || len(orphans) != 0 {
		t.Fatalf("second Recover = %+v, %v", orphans, err)
	}
	if _, err := os.Stat(filepath.Join(layout.Records, "r1", "stream.jsonl")); err != nil {
		t.Errorf("the transcript was removed: %v", err)
	}
}
