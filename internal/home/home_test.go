package home

import (
	"os"
	"path/filepath"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestResolve(t *testing.T) {
	override := t.TempDir()
	layout, err := Resolve(env(map[string]string{"AGENTIUM_HOME": override, "HOME": "/ignored"}))
	if err != nil {
		t.Fatal(err)
	}
	if layout.Root != override || layout.Database != filepath.Join(override, "agentium.db") || layout.Artifacts != filepath.Join(override, "artifacts") {
		t.Errorf("override layout = %+v", layout)
	}
	layout, err = Resolve(env(map[string]string{"HOME": "/Users/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	if layout.Root != "/Users/someone/.agentium" {
		t.Errorf("default root = %q", layout.Root)
	}
	if got := layout.ProjectRepo(7); got != "/Users/someone/.agentium/projects/7/repo.git" {
		t.Errorf("project repository = %q", got)
	}
	if _, err := Resolve(env(nil)); err == nil {
		t.Error("expected an error without AGENTIUM_HOME or HOME")
	}
}

func TestEnsureIsOwnerOnly(t *testing.T) {
	layout, err := Resolve(env(map[string]string{"AGENTIUM_HOME": filepath.Join(t.TempDir(), "data")}))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{layout.Root, layout.Artifacts} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want 0700", dir, info.Mode().Perm())
		}
	}
	if err := layout.Ensure(); err != nil {
		t.Errorf("Ensure is not idempotent: %v", err)
	}
}
