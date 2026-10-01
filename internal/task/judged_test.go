package task

import (
	"strings"
	"testing"
	"time"
)

func TestValidateJudged(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,2 @@\n ctx\n--- removed SQL comment\n+new\n"
	v := ValidateJudged("Fix it.", []string{"a.go", "README.md"}, diff, now)
	if v.Status != StatusValid || v.Judge == nil || v.Judge.ChangedLines != 2 || strings.Join(v.Judge.CodeFiles, ",") != "a.go" || v.Summary() != "valid" {
		t.Errorf("valid task: %+v %+v", v, v.Judge)
	}
	v = ValidateJudged("  \n", []string{"README.md", "docs/guide.md"}, "", now)
	if v.Status != StatusInvalid || len(v.Judge.Problems) != 2 || !strings.HasPrefix(v.Summary(), "invalid: the instruction is empty") ||
		!strings.Contains(v.Summary(), "the reference changes no code") {
		t.Errorf("invalid task: %s", v.Summary())
	}
	if v := ValidateJudged("Fix it.", []string{"a.go"}, "diff --git a/a.go b/a.go\nold mode 100644\nnew mode 100755\n", now); !strings.Contains(v.Summary(), "changes no lines") {
		t.Errorf("mode-only change: %s", v.Summary())
	}
}
