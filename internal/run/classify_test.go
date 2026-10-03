package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pigeaca/agentium/internal/sandbox"
	"github.com/pigeaca/agentium/internal/task"
)

// classify writes the grade's own folder in a denial's target as task.GradeFolder, in whichever form the kernel gave it
// (on macOS a temp folder's real form is under /private), so a denial a task's reference logged in its validation's
// grade folder matches the same denial in a run's: harmless, not flagged. Noise is left out; other flagged denials
// count, their targets rewritten the same way.
func TestClassifyWritesTheGradeFolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "grading")
	must(t, os.MkdirAll(root, 0o700))
	real := sandbox.RealForm(root)
	p := sandbox.Profile{Home: t.TempDir(), Data: "/data"}
	socket := sandbox.Denial{Process: "python3", Operation: "network-outbound", Target: real + "/copy/s.sock", Repeats: 2}
	other := sandbox.Denial{Process: "python3", Operation: "network-bind", Target: root + "/tmp/t.sock", Repeats: 1}
	noise := sandbox.Denial{Process: "sh", Operation: "file-write-data", Target: "/dev/dtracehelper", Repeats: 1}
	read := sandbox.Denial{Process: "cat", Operation: "file-read-data", Target: real + "/../profile.sb", Repeats: 1}

	var report task.SandboxGrade
	classify(&report, p, []sandbox.Denial{socket, other, noise, read}, root,
		[]task.DenialKey{{Operation: "network-outbound", Target: task.GradeFolder + "/copy/s.sock"}})
	if report.Harmless != 2 || report.FlaggedCount != 1 || len(report.Flagged) != 1 || report.DenialCount != 4 {
		t.Fatalf("report %+v", report)
	}
	if got := report.Flagged[0].Target; got != task.GradeFolder+"/tmp/t.sock" {
		t.Errorf("the flagged target %q", got)
	}
	if got := report.Denials[0].Target; got != task.GradeFolder+"/copy/s.sock" {
		t.Errorf("the kept denial's target %q (real form %s)", got, real)
	}
	if got := report.Denials[2].Target; got != real+"/../profile.sb" && got != task.GradeFolder+"/../profile.sb" {
		t.Errorf("a target outside the folder %q", got)
	}
	if inGrade(root+"ing/x", root) != root+"ing/x" {
		t.Error("a sibling folder sharing the prefix was rewritten")
	}
}
