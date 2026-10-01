# Usage and cost estimates from task runs

- Date: 2026-10-01
- Status: In Progress
- Scope/approval: the user asked (2026-10-01) to fix the two estimator flaws that the 16-run context A/B exposed ([hardening plan](archive/2026-09-29-experiment-hardening.md), step 6, "Follow-ups"): usage per run measured on calibration runs, and one project-wide cost per run whatever the task. Tests use the fake `claude` and fixtures; no real runs.

## Outcome and boundaries

`experiment plan`, `experiment new` (the default budget) and `experiment run` (the usage gate and the budget warning) estimate from what predicts a run best:
- **Usage per run** comes only from task runs. Calibration runs (`runs.kind = 'calibration'`, a few short turns, about 1% each) are left out of the measuring window; they still count as the latest reading of the window.
- **Cost per run** is each task's own earlier fair runs on the experiment's model (their median) when it has any, and otherwise the current fallback (the median of the project's earlier task runs on the model, or the default profile at list price). The preview says which basis each task used.

Unchanged: the worst case (every run at its cap), the budget rule (a quarter above the estimate plus the reserve for runs in flight; spending never passes the budget), the usage rule itself (the latest window three or more runs read from start to end, rise divided by runs, default 6%), the stored data and its schema, locked experiments.

## Acceptance

1. `UsagePerRun` ignores calibration runs: a window that holds only calibration runs is not a measuring window, so the estimate falls back to an older window of task runs, or to the 6% default. The latest reading still comes from every run. *Evidence:* unit tests in `internal/experiment`; a CLI test reading stored runs where a window holds only calibration runs.
2. The preview's usage line says the estimate comes from task runs ("measured over N task runs in one window", or a default until task runs measure it).
3. A task with earlier fair runs on the model is estimated from them (the median); a task without falls back as today. Only runs still linked to the task count as its own (a removed task's runs, or an experiment's runs of a task changed after the lock, count toward the project's median only). The experiment's estimated cost, its default budget and the budget warning sum the tasks' own estimates. Tier rows use the average estimate over the eligible tasks. *Evidence:* unit tests of the estimate, budget and preview rows; a CLI test with two tasks, one with its own costlier runs, showing both bases.
4. `experiment plan` says which basis each task used: tasks with their own runs and their estimate, and the fallback with the tasks that use it. *Evidence:* the CLI test, and a console sample in the PR.
5. The worst-case figure and the budget rule are unchanged. *Evidence:* existing tests keep passing unchanged where they check them.
6. Docs that describe the estimates are updated (code comments, README if it describes them, the architecture code map if responsibilities change).

## Work
- [x] Usage: mark calibration samples; `UsagePerRun` skips them; preview wording; tests.
- [x] Cost: per-task estimates in `internal/experiment/plan.go`; `estimateRun`, the preview, `experiment new` and the readiness check in `internal/cli/experiment.go`; tests.
- [ ] Docs, `harness.py check changed`, reviewer, PR.

## Verification and handoff
`python3 scripts/harness.py check changed`; `go test ./internal/experiment ./internal/cli`; a console sample of `experiment plan` from a fixture with the fake `claude`; an independent review against these criteria.

## Metrics
- Agent: Claude Code / claude-opus-5-5 / default
- Elapsed:
- Check-fix loops:
- User corrections: 0
- Review:
