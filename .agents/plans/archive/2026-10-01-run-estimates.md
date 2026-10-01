# Usage and cost estimates from task runs

- Date: 2026-10-01
- Status: Complete (2026-10-01), [PR #42](https://github.com/pigeaca/Agentium/pull/42).
- Scope/approval: the user asked (2026-10-01) to fix the two estimator flaws that the 16-run context A/B exposed ([hardening plan](archive/2026-09-29-experiment-hardening.md), step 6, "Follow-ups"): usage per run measured on calibration runs, and one project-wide cost per run whatever the task. Tests use the fake `claude` and fixtures; no real runs.

## Outcome and boundaries

`experiment plan`, `experiment new` (the default budget) and `experiment run` (the usage gate and the budget warning) estimate from what predicts a run best:
- **Usage per run** comes only from task runs. Calibration runs (`runs.kind = 'calibration'`, a few short turns, about 1% each) are left out of the measuring window; they still count as the latest reading of the window.
- **Cost per run** is each task's own earlier fair runs on the experiment's model (their median) when it has any, and otherwise the current fallback (the median of the project's earlier task runs on the model, or the default profile at list price). The preview says which basis each task used.

Unchanged: the worst case (every run at its cap), the budget rule (a quarter above the estimate plus the reserve for runs in flight; spending never passes the budget), the usage rule itself (the latest window three or more runs read from start to end, rise divided by runs, default 6%), the stored data and its schema, locked experiments.

## Acceptance

1. `UsagePerRun` ignores calibration runs: a window that holds only calibration runs is not a measuring window, so the estimate falls back to an older window of task runs, or to the 6% default. The latest reading still comes from every run. *Evidence:* unit tests in `internal/experiment`; a CLI test reading stored runs where a window holds only calibration runs.
2. The preview's usage line says the estimate comes from task runs ("measured over N task runs in one window", or a default until task runs measure it).
3. A task with earlier fair runs on the model is estimated from them (the median); a task without falls back as today. Only runs still linked to the task count as its own (a removed task's runs, or an experiment's runs of a task changed after the lock, count toward the project's median only; a task edited in place keeps its earlier runs). The experiment's estimated cost, its default budget and the budget warning sum the tasks' own estimates. Tier rows use the average estimate over the eligible tasks. *Evidence:* unit tests of the estimate, budget and preview rows; a CLI test with two tasks, one with its own costlier runs, showing both bases.
4. `experiment plan` says which basis each task used: tasks with their own runs and their estimate, and the fallback with the tasks that use it. *Evidence:* the CLI test, and a console sample in the PR.
5. The worst-case figure and the budget rule are unchanged. *Evidence:* existing tests keep passing unchanged where they check them.
6. Docs that describe the estimates are updated (code comments, README if it describes them, the architecture code map if responsibilities change).

## Work
- [x] Usage: mark calibration samples; `UsagePerRun` skips them; preview wording; tests.
- [x] Cost: per-task estimates in `internal/experiment/plan.go`; `estimateRun`, the preview, `experiment new` and the readiness check in `internal/cli/experiment.go`; tests.
- [x] Docs, `harness.py check changed`, reviewer, PR. The README ("runs, estimated cost, detectable effects") and the architecture code map stay accurate and are unchanged; the README's plan image leaves out the estimate lines.

## Verification and handoff
`python3 scripts/harness.py check changed`; `go test ./internal/experiment ./internal/cli`; a console sample of `experiment plan` from a fixture with the fake `claude`; an independent review against these criteria.

Results:
- `harness.py check changed`: docs, `go vet ./...` and `go test -race -count=1 ./...` pass.
- New tests: `TestUsagePerRunAndLatest` (calibration-only, older and mixed windows), `TestUsagePreviewLeavesOutCalibrationRuns`, `TestEstimateFromEachTasksOwnRuns` and `TestExperimentEstimatesEachTaskFromItsOwnRuns`. They fail on the old code: 1% instead of the default, and a $11 budget instead of $15.
- Console sample (fake `claude`, stored runs): a latest window with only four calibration runs at 1% each. The preview now says "about 7% … (measured over 5 task runs in one window)", while the old code would have said about 1%. The costly task shows "fresh-checkouts $1.98 (2 run(s))", and the other task uses the project's median of $0.80.
- Review: the independent reviewer approved. Two low findings were fixed: the wording now says a task edited in place keeps its earlier runs, and the tiers note shows the average it uses.

Limitations:
- The acceptance data folder was not used for a sample, because reading a copy of its database was denied.
- A task's own estimate counts runs that hit their cap, so it runs low for tasks that often reach the cap.
- Both arms' runs count alike.
- "Own runs" means linked runs, not runs of the task's current version: keying by version would need the digest stored with each run, which is a schema change.
- Criterion 3's sentence on linked runs was added during implementation and awaits the user's agreement.

## Metrics
- Agent: Claude Code / claude-opus-5-5 / default
- Elapsed: 35m
- Check-fix loops: 0
- User corrections: 0
- Review: approve, 2 low findings fixed
