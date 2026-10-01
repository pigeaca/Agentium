package run

import (
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
)

func TestJudgeEdgeCases(t *testing.T) {
	t.Parallel()
	grep := []claude.ToolCall{{Name: "Grep", Input: map[string]any{"pattern": "codeword"}, Result: "AGENTS.md:3:Calibration codeword: AGENTIUM-ABC"}}
	if _, _, instructions := judgeChecks(grep, "CODEWORD=AGENTIUM-ABC", "AGENTIUM-ABC", "CLAUDE.md"); instructions != checkUnverified {
		t.Errorf("a codeword found with a tool counts as loaded: %s", instructions)
	}
	truncated := []claude.ToolCall{{Name: "Bash", Input: map[string]any{"command": "seq 1 40000"}, Result: "<persisted-output> saved to: "}}
	if _, large, _ := judgeChecks(truncated, "", "x", ""); large != checkUnverified {
		t.Errorf("a saved-output notice without a path: %s", large)
	}
}
