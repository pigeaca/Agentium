# Refactor round

- Date: 2026-10-01
- Status: In Progress (2026-10-01): step 4 (faster tests) in #59: the CLI tests went from 125 s to about 30 s under `-race`. Step 1 (spending record) is implemented and reviewed (approve with notes, fixes applied); its PR is next. Step 2 (handlers into services) is implemented on its branch. Step 3 comes after Java and Rust step 3. Approved with the [next chapter](2026-10-01-next-chapter.md).
- Scope: the code findings of the 2026-10-01 review. No change in behavior.

## Why
- **Business logic in the CLI:** `internal/cli` holds about a quarter of the code, and command handlers run 120–290 lines (`experimentRun` 289, `experimentNew` 134, `printReadiness` 126, `experimentPlan` 120, `runCalibrate` 118). Every feature (judge, pairs, model A/B, run reuse) adds to them.
- **Spend in five places:** `Metrics.CostUSD`, the runs table's `cost_usd`, `Record.Judge.CostUSD`, `Result.CostUSD` and `Result.JudgeUSD`, and estimates. Each new cost source must be added up correctly everywhere.
- **Duplicated helpers:**
  - three environment allowlists (`gitx`, `runner`, `claude`);
  - `instructionFilesAbove` twice;
  - the sign-in environment built in both `Invocation` and `Judgement`;
  - Wilson and binomial helpers in `judge`, apart from `stats`.
- **Slow tests:** the CLI tests take about 120 s of every check under `-race`.

## Outcome
- **Command handlers** parse, call one service function, and print. None is over 80 lines.
- **One spending record per run** (agent, judge, estimated or not), and one function that totals it. The budget, status, `show`, `report` and the estimates use it. The cost metric stays the agent's alone.
- **Helpers live once:** environment allowlists are built in one place, with their documented differences, and the sign-in environment, instruction-file guard and interval helpers are shared.
- **The CLI tests run in under 60 s** under `-race`.

## Acceptance
1. **No behavior change:** every existing test passes unmodified (apart from moved test files), the Go golden test and report golden files are unchanged, and console samples are identical.
2. **One spend total:** a test fails if a cost source is added without the total including it, for example a table test over every spend field.
3. **Size limits:** no command handler over 80 lines, checked by a test or a harness check.
4. **Test time:** the CLI package finishes in under 60 s with `-race` on this machine, recorded before and after.

## Work
Each step is one PR with a review.
- [x] **1. Spending record** (`run`, `experiment`, `cli`, `report`): `run.Spend` (the agent's and the judge's costs, whether the agent's was estimated) from `Record.Spend` or `StoredSpend` (cost column plus record), totalled by `TotalUSD`. The budget, status, `judgePending`, `show` and the report's spend use the total; the cost metric, arms' and tasks' costs, calibration, the cost column and `EstimateRun` use `AgentUSD`. Schema unchanged; tests and goldens unchanged. A reflection test fails on any `*USD` field `Spend` leaves out. Review: approve with notes, no behavior change; fixes: pointer leaves and exact paths in the guard, calibration through `Spend`, one decode in `estimateRun`, and a deterministic `TestRunRecordsTheRunningCommand` (the setup waits for a release file; the test polls for its process group).
- [x] **2. Command handlers into services** (`experiment`, `report`, `run`, `cli`): `new`, `plan`, `run`, `report` and `calibrate`. Services take a `context.Context` and their I/O as parameters, keep no state, and wrap errors with `%w`. `experiment.Create`, `LoadReview` and `Runner.Run` (the live status line and per-run progress stay in `cli`, wired through `experiment.Observer`), `report.Load` and `Report.Write`, `run.Calibrator` and `run.Calibration`. A `UsageError` becomes exit 2 in the handler. `TestCommandHandlersStayShort` parses the package with `go/ast` and fails on any handler over 80 lines; the allowlist is empty since #62 merged: `taskMine` (175) moved to `mine.Prepare`, `Importer` and `Import`, and `taskValidate` (113) to `task.Validating` (`Batch`, `StoreValidation`, `Judged`, `Gaps`), each handler under 50 lines. Notes: `experiment` now imports `store`, `gitx`, `project`, `home`, `run`, `snapshot` and `term`, so `run` can never import `experiment` (consider a sub-package if it grows); readiness lines now appear all at once, with the same bytes; the size test limits length only, not the "one service call" shape; in `run calibrate`, `source.Commit` for the base and earlier snapshots now runs after all name checks, so a repeated `--snapshot` with a failing `source.Commit` exits 2, not 1. Tests moved with their code: `TestJudgeEdgeCases` (to `run`), `TestSpendReachesTheBudget` and `TestUsageFromStoredRecords` (to `experiment`).
- [ ] **3. Shared helpers** (`claude`, `run`, `gitx`, `runner`, `stats`, `judge`; also the three `orNone` copies in `cli`, `experiment` and `run`), after Java and Rust step 3.
- [x] **4. Faster tests** (`cli` tests): [#59](https://github.com/pigeaca/Agentium/pull/59), `t.Parallel()` on every independent test, 125 s → about 30 s.

## Boundaries
Refactors only: no new features, no new Go modules, no change to output.

## Verification
Unchanged tests and golden files, `harness.py check changed`, CI, and a reviewer per step.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
