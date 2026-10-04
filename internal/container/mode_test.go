package container_test

import (
	"testing"

	"github.com/pigeaca/agentium/internal/container"
	"github.com/pigeaca/agentium/internal/task"
)

// The container's mode is the task's grader mode. An external test: task imports buildtool, which imports container.
func TestModeIsTasks(t *testing.T) {
	if container.Mode != task.GraderContainer {
		t.Errorf("Mode %q, task.GraderContainer %q", container.Mode, task.GraderContainer)
	}
}
