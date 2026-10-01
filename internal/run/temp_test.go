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

func TestMakeAndRemoveRunTemp(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ag-0123456789")
	if err := makeRunTemp(root); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the temp root: %v, %v", info, err)
	}
	// A root a dead run left behind, with Claude Code's files in it, is made fresh.
	stale := filepath.Join(root, "claude-501", "shell-snapshot")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := makeRunTemp(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale root's files survived")
	}
	if err := removeRunTemp(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); err == nil {
		t.Error("the temp root was left behind")
	}
	if err := removeRunTemp(root); err != nil {
		t.Errorf("removing a missing root: %v", err)
	}

	// Someone else's: a link (to a folder of the user's that must survive) or a file is refused, and left alone.
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ag-aaaaaaaaaa")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "ag-bbbbbbbbbb")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, file} {
		if err := makeRunTemp(p); err == nil || !strings.Contains(err.Error(), "not the user's own folder") {
			t.Errorf("%s: %v", p, err)
		}
		if err := removeRunTemp(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Errorf("a link's target was touched: %v", err)
	}
}

// A layout without a folder for temp roots would leave the agent on the shared temp folder: the run refuses to start.
func TestOnceNeedsATempRoot(t *testing.T) {
	data := t.TempDir()
	layout := home.Layout{Root: data, Workspaces: filepath.Join(data, "workspaces"), Records: filepath.Join(data, "records")}
	_, err := Once(context.Background(), Env{ID: "r1", Layout: layout, Now: time.Now}, Spec{})
	if err == nil || !strings.Contains(err.Error(), "temp roots") {
		t.Errorf("Once without Layout.Temp: %v", err)
	}
}
