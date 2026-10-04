package task

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// With a module, setup and verification commands run in its folder of the checkout, and the build tools' caches follow
// the module's build files; without one they run at the checkout's root, as ever.
func TestValidatorRunsCommandsInTheModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "svc", "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for module, want := range map[string]string{"svc": "/svc", "": filepath.Base(dir)} {
		v := Validator{Timeout: 10 * time.Second, Module: module}
		var log bytes.Buffer
		results, ok, err := v.run(context.Background(), &log, dir, []string{"pwd"})
		if err != nil || !ok || len(results) != 1 {
			t.Fatalf("module %q: %v, %v, %v", module, results, ok, err)
		}
		if got := strings.TrimSpace(strings.TrimPrefix(log.String(), "$ pwd\n")); !strings.HasSuffix(got, want) {
			t.Errorf("module %q: ran in %q, want a folder ending %q", module, got, want)
		}
	}
	if got := (Validator{Module: "svc"}).inModule(dir); got != filepath.Join(dir, "svc") {
		t.Errorf("inModule = %s", got)
	}
}
