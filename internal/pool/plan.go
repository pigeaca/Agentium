package pool

import (
	"time"

	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// Facts are what the maintenance rules read besides the tasks: the clock, the build tools' versions now (nil: not
// detected), which locked experiments use which tasks (store.TasksInUse), the base commits' times and the default
// branch's head (Retire's).
type Facts struct {
	Now       time.Time
	Toolchain task.Toolchain
	InUse     map[string][]string
	BaseTime  func(commit string) time.Time // zero when unknown
	Head      Head
}

// Retirement is a task to retire, and why.
type Retirement struct {
	Task   store.Task
	Reason string
}

// Revalidation is a stale task, and what makes it stale. Experiments names the locked experiments that still use it:
// such a task is kept as it is (Plan.Kept), since re-validating it could change what they rely on.
type Revalidation struct {
	Task        store.Task
	Stale       Staleness
	Experiments []string
}

// Plan is what a pass's maintenance does: the tasks to retire, the stale tasks to re-validate, and the stale tasks kept
// because experiments use them.
type Plan struct {
	Retire     []Retirement
	Revalidate []Revalidation
	Kept       []Revalidation
}

// Plan applies the rules to the project's tasks (as listed: oldest first). Retired tasks are left alone; a task that
// retires now is not re-validated (it leaves the eligible set either way).
func (p Policy) Plan(tasks []store.Task, f Facts) Plan {
	var out Plan
	for _, t := range tasks {
		if t.Retired() {
			continue
		}
		var base time.Time
		if f.BaseTime != nil {
			base = f.BaseTime(t.BaseCommit)
		}
		if reason := p.Retire(t, f.Now, base, f.Head); reason != "" {
			out.Retire = append(out.Retire, Retirement{Task: t, Reason: reason})
			continue
		}
		s := p.Stale(t, f.Now, f.Toolchain)
		if len(s.Reasons) == 0 {
			continue
		}
		if users := f.InUse[t.Name]; len(users) > 0 {
			out.Kept = append(out.Kept, Revalidation{Task: t, Stale: s, Experiments: users})
			continue
		}
		out.Revalidate = append(out.Revalidate, Revalidation{Task: t, Stale: s})
	}
	return out
}
