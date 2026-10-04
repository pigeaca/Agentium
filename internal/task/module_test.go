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

// The build tools' caches follow the module's build files: a Maven module's folder gives Maven's cache variable at the
// checkout whose root has no pom.xml.
func TestValidatorEnvFollowsTheModulesBuildFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "svc", "pom.xml"), []byte("<project/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	inModule := strings.Join(Validator{Cache: cache, Module: "svc"}.envFor(dir), "\n")
	if !strings.Contains(inModule, "MAVEN_USER_HOME="+filepath.Join(cache, "maven")) {
		t.Errorf("the module's Maven cache is not in the environment:\n%s", inModule)
	}
	if atRoot := strings.Join(Validator{Cache: cache}.envFor(dir), "\n"); strings.Contains(atRoot, "MAVEN_USER_HOME") {
		t.Errorf("the root has no pom.xml, yet:\n%s", atRoot)
	}
}

// The checkout's module folder is checked before a command runs: a module that is missing, a file, or a link (to
// another folder, or through a linked parent) is an error, never a run in some other folder.
func TestValidatorRefusesAModuleThatIsNotAFolder(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "real", "svc"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644))
	must(os.Symlink(filepath.Join(dir, "real", "svc"), filepath.Join(dir, "link")))
	must(os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "parent")))
	marker := filepath.Join(t.TempDir(), "ran")
	for _, module := range []string{"missing", "file", "link", "parent/svc", "real/missing"} {
		v := Validator{Timeout: 10 * time.Second, Module: module}
		var log bytes.Buffer
		if _, ok, err := v.run(context.Background(), &log, dir, []string{"touch " + marker}); err == nil || ok {
			t.Errorf("module %q: ok %v, err %v: want an error", module, ok, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a command ran in a folder that is not the module")
	}
}
