package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clean lists the grade folder a stopped sandboxed validation left in the artifacts, and --yes removes it, read-only as
// a dead grade leaves it; the validation's logs beside it stay.
func TestCleanRemovesAStoppedValidationsGradeFolder(t *testing.T) {
	t.Parallel()
	f := newCleanFixture(t)
	validation := filepath.Join(f.layout.Artifacts, "tasks", "1", "20261001T120000Z")
	grade := filepath.Join(validation, "grading", "base-reference")
	writeFile(t, filepath.Join(grade, "copy", "tests"), "value_test.sh", strings.Repeat("x", 4096))
	writeFile(t, filepath.Join(validation, "logs"), "base-reference.log", "$ sh run_tests.sh\n")
	if err := os.Chmod(grade, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(grade, 0o700) })
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(grade, old, old); err != nil {
		t.Fatal(err)
	}
	expect(t, f.run(context.Background(), "clean"), ExitOK, "validations", "artifacts/tasks/1/20261001T120000Z/grading/base-reference",
		"the grade folder of a validation of task 1 that stopped")
	expect(t, f.run(context.Background(), "clean", "--yes"), ExitOK, "What went:", "artifacts/tasks/1/20261001T120000Z/grading/base-reference")
	if _, err := os.Lstat(grade); err == nil {
		t.Error("the grade folder is still there")
	}
	if _, err := os.Lstat(filepath.Join(validation, "logs", "base-reference.log")); err != nil {
		t.Errorf("the validation's log: %v", err)
	}
}
