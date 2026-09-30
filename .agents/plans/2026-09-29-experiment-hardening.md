# Experiment hardening, then a 20-run context A/B

- Date: 2026-09-29
- Status: In Progress (step 6 waits for the [console plan](2026-09-30-clearer-console.md))
- Scope: after Phase 1 closed, the user asked for the next step "that way" (2026-09-29): first harden experiments against what step 7 found, then run a context A/B of 12 tasks × 1 run per arm (24 runs, about two usage windows) instead of the Quick tier's 72. The paid A/B needs its own approval of budget and timing.

## Why

Step 7 ([Phase 1 plan](archive/2026-09-28-phase1-context-ab-cli.md)) showed that real experiments fail on logistics, not statistics:
- **Usage limits.** With a subscription login, each run used about 5–6.7% of the five-hour window. External scripts had to pause the runs.
- **Unfair tasks.** Three of six tasks passed validation and review but could not be solved from their instructions. Only paid runs revealed it.
- **Hand-computed noise.** The noise estimates were worked out by hand.
- **Floors.** The study's floors (§5.6: 3 runs per task per arm) make any 1-run design exploratory, whatever its size.

## Acceptance

Each item maps to evidence. Changing any of them needs the user's agreement.

1. **Rate-limit awareness.**
   - Each run records the plan's usage readings from Claude Code's stream: five-hour and seven-day utilization, reset times and status.
   - `experiment run` takes `--usage-limit PCT` (default 85). No new pair starts when the latest reading, plus the pair's expected use, would pass the limit.
   - Runs in flight finish and are never cancelled. The experiment then stops as "paused: usage", says when the window resets, and resumes with `experiment run`.
   - `--wait` sleeps until the reset instead, then continues.
   - Expected use per run comes from the latest five-hour window that three or more runs read from start to end: the rise from their lowest first reading to their highest last reading, divided by those runs (default 6%). *Changed on 2026-09-30 from "the median rise per run", pending the user's agreement:* readings arrive in 1% steps and overlapping runs share a rise, so a per-run median runs low. This estimate errs high instead, because the user's own use in the window counts too.
   - `experiment plan` shows the use per run, the windows the experiment needs, and whether the current window fits.
   - An experiment with an API key never pauses, and the preview says so. *Corrected on 2026-09-30, pending the user's agreement:* a `claude setup-token` token is a subscription, so its runs report readings and pause like a login.
   - Limitation: only the five-hour window is gated. The seven-day window is recorded but not gated.
   - Added by the user on 2026-09-29: each run records which model each subagent type used. An experiment stops when a type's model changes from earlier runs, such as a role's `sonnet` alias moving to a newer model.

   *Evidence:* end-to-end tests with the fake `claude` (and a faked wait) covering a pause between pairs, a wait and resume, the preview, and a stream without readings, and a subagent changing model.
2. **Task fairness check.** For each task, list what the hidden tests require that neither the instruction nor the base code states.
   - Go: exact string literals that the tests compare against, and identifiers (functions, methods, fields, types) that the tests use but the base does not define.
   - Other languages: string literals only.

   `task show`, `task validate` and `experiment plan` show the list, and `task list` shows its count. Marking a task reviewed with a non-empty list needs `--accept-gaps`.

   *Evidence:* hermetic fixtures rebuilding step 7's three unfair cases (an exact error message, a new struct field, a note text) are flagged; with those stated in the instruction, nothing is flagged.
3. **Noise in reports, and one-run verdicts.**
   - Reports show σ, τ for an A/B, and w when success varies, each with a range.
   - An A/A compares them with the planner's defaults.
   - The cost floor counts tasks with at least one counted run in both arms, instead of three. This applies only if a seeded simulation of 8–12 tasks × 1 run (σ = 0.19, τ = 0.10–0.25) shows:
     - A/A false differences at 6% or below;
     - 95% intervals covering the true effect in at least 93% of cases.

     Otherwise the floors stay, and the A/B's verdicts stay exploratory.
   - The success floor does not change.
   - The method version becomes `phase1-v2`, and locked experiments keep their method.

   *Evidence:* the simulation test, and updated golden reports.
4. **Small fixes.**
   - A run stopped before it starts is reported as such, not as "none found".
   - Go's module-cache stat warning and the zsh glob failures are fixed, or documented with a reason.

   *Evidence:* tests, or the plan's record. The three tasks already spawned can deliver these.
5. **Ten fair tasks** in the acceptance data folder: the six from step 7, plus four or more from Agentium's history, ideally some that agents fail sometimes. *Changed from twelve by the user on 2026-09-30:* the arms are snapshots of the docs as of 2026-09-29, and on bases from after Phase 1 closed they break the repository's docs check. Each task:
   - has an issue-style instruction;
   - passes the fairness check;
   - passes an independent review of instruction against hidden tests;
   - is valid in both A/B arms, which pass each task base's docs check.

   *Evidence:* the task list, validation logs and the recorded review.
6. **The 20-run A/B (paid; separate approval).**
   - Arms `full` against `minimal`, as in step 7's A/B, unless the user picks others at approval.
   - 10 tasks × 1 run per arm, with both arms calibrated first.
   - Run with `--usage-limit 85 --wait`.
   - Estimated $16–21 at list prices, over about two windows.

   *Evidence:* the report, the costs compared with the preview, and the usage readings.

## Work
Each step is one PR with green CI and a review, except step 5 (data, no code) and step 6 (paid runs).

- [x] **1. Rate-limit awareness** (`internal/claude` readings, `internal/run` records, `internal/experiment` pause and wait, `internal/cli` flags and preview). Done in #25, which the independent reviewer approved on its second pass after four findings were fixed: pairs split on retry, API-key experiments pausing, the model check comparing across arms, and the estimator. Limitation: only the five-hour window is gated.
- [x] **2. Task fairness check** (`internal/task`, using `go/parser` from the standard library). Done by an `implementer` (Sonnet 5.5) and approved by the reviewer after two fix rounds:
  - a gap is now only a text that the solution's own code produces;
  - every way of marking a task reviewed is gated;
  - `git grep` replaces reading every file;
  - name searches are case-sensitive.

  On its own commit the check went from 59 gaps to 5 real ones, at about 0.5 s per task. Limitations: accepted gaps are not remembered; multi-line strings, and expected texts that a test builds itself with `fmt.Sprintf`, are missed.
- [x] **3. Noise in reports, and one-run verdicts** (`internal/stats`, `internal/experiment`, `internal/report`). The method change applies only if the simulation passes. Done by an `implementer` (Opus); the reviewer approved it after one round of fixes.
  - **Simulation gate passed,** so `phase1-v2` counts a task for cost with one run per arm. Over 8–12 tasks × 1 run: false differences 4.7–5.7% under normal noise and 4.6–5.5% under skewed noise; coverage 94.3–95.4%.
  - **Separate robustness check:** a Python run by the reviewer on Phase 0's residuals gave 4.4–4.9%.
  - **Unchanged:** the success floor, and the verdicts of `phase1-v1` experiments.
  - **Limitations:**
    - the chi-square ranges assume normal noise;
    - with unequal repeats, the τ formula uses the arithmetic mean of runs per cell, which slightly overstates τ;
    - the largest false-difference rate sits under one standard error from the 6% limit.
  - **For step 6:** a 12 × 1 A/B cannot measure σ or w (only τ, under the assumed σ = 0.19). Updating the planner defaults would need a one-run A/A, or a few repeated tasks, alongside.
- [x] **4. Small fixes**, done by an `implementer` (Sonnet 5.5) after one review round:
  - A run interrupted before its agent starts is reported as "stopped before its agent started (not counted; it runs again on resume)". `Execute` marks such runs on the finish event.
  - Agents get `-buildvcs=false` appended to their `GOFLAGS`, after the user's own flags from the environment or from `go env -w`. That stops Go writing a stat-cache entry into the read-only module cache; the warning was reproduced locally with a read-only module-cache copy. Agentium's own setup and grading are unchanged.
  - zsh glob failures are kept on purpose: runs use the user's own shell, like their Claude Code sessions, and both arms get the same one. This is documented on `Environ`.
- [x] **5. Ten fair tasks**: import, rewrite, check, review, validate. Free. Done on 2026-09-30:
  - **The tasks:** the six from step 7, plus `active-config-files` (rewritten in full, now that its hidden tests' needs are known), `temp-files-in-data` (2fbd5cd), `stats-review` (ae407ee) and `fresh-checkouts` (e64def4).
  - **Checks:**
    - all ten are valid in the base, `full` and `minimal` contexts;
    - each arm passes each task base's docs check;
    - the fairness check shows no gaps against the stored instructions;
    - an independent reviewer approved every new instruction against its hidden tests, after small fixes.
  - **Likely hard:** `fresh-checkouts` (six small fixes); `temp-files-in-data` may be too.
  - **Dropped:**
    - `experiment-errors` (9710ea5), `budget-reserve` (9111086) and `calibration-transcript` (ccd96e7): their hidden tests need too many exact texts and internal signatures to state fairly, 400 words or more.
    - `names-case-sensitive` (9b808ea) and `pairs-whole-at-limit` (0193996): valid and fair, but their bases postdate the docs snapshots.
  - **The fairness check itself:**
    - It missed the real unfair case (a new field with the same name as a field on another type, and texts built from format strings). A fix added typed-key and format checks.
    - Its first version of that fix flagged old keys, and a NUL byte broke the search. Both were found in review and fixed.
    - On the tasks above it now reports only real gaps.
- [ ] **6. The A/B**, after approval. On 2026-09-30 the user moved the [console plan](2026-09-30-clearer-console.md) ahead of it, so it runs with the new output. Then record the results, update the planner defaults if the measured noise differs, and archive this plan.

## Boundaries
- No new dependencies; `go/parser` is in the standard library.
- No real Claude Code runs before step 6's approval; tests use the fake `claude`. Calibrations and runs in step 6 count against the approved budget.
- Claude Code only. Phase 2 (console output, Codex, agent comparison) is not part of this plan.
- Usage readings are only read from the stream and stored with runs; nothing is sent anywhere.
- The method changes only through step 3's simulation gate.
- The user's repository is never written. Arm B's docs stay on a local branch, not merged.

## Verification
- Every step: `harness.py check changed`, CI, and a review.
- Steps 1–3: end-to-end tests with the fake `claude`, and the seeded simulation.
- Step 5: `task validate` in both arms, the docs check per arm, and the fairness review.
- Step 6: the report, and the preview's estimates compared with the actual spend and usage.

## Parallel ownership
Steps 1–3 run in parallel from base `0f5e237`, one PR each, and the coordinator integrates them serially. Worktrees live under `/Users/pigeaca/GolandProjects/Agentium-worktrees/`.

| Step | Owner | Branch / worktree | Editable scope | Must not touch |
|---|---|---|---|---|
| 1 | coordinator (Opus) | `claude/feat/rate-limit-awareness` / `claude-feat-rate-limit-awareness` | `internal/claude/stream.go` (readings); `internal/run`; `internal/experiment/execute.go` and a new `usage.go`; `internal/cli/experiment_run.go` (flags, pause messages); the usage lines of the preview in `internal/cli/experiment.go`; added in review: a new `internal/cli/usage.go`, `Env.Sleep` in `cli.go`, `run show` in `cli/run.go`, and the status constant in `internal/store` | `internal/experiment/plan.go`, `analyze.go`, `lock.go`; `internal/stats`; `internal/report`; `internal/task` |
| 2 | `implementer` (Sonnet, medium) | `claude/feat/task-fairness-check` / `claude-feat-task-fairness-check` | `internal/task` (a new fairness file); `internal/cli/task.go`; the store only if the gap count must persist; one check line in `experiment plan`'s "Before it runs" list (`internal/cli/experiment.go`) | `internal/experiment`, `internal/stats`, `internal/report`, `internal/claude` |
| 3 | `implementer` (Opus, high: a method change) | `claude/feat/noise-one-run-verdicts` / `claude-feat-noise-one-run-verdicts` | `internal/stats` (components, simulation); `internal/experiment/analyze.go`, `plan.go` (floors), `lock.go` (`MethodVersion`); `internal/report` and its golden files | `internal/experiment/execute.go`, `internal/cli/experiment_run.go`, `internal/claude`, `internal/task` |

Each implementer pushes its branch and returns a handoff; the coordinator runs the reviewer, opens the PR and integrates. `internal/cli/experiment.go` is shared by steps 1 and 2 (one small edit each), so the later of the two rebases.

Step 4's spawned tasks each get their own worktree and PR. Before they merge, steps 1–3 must not edit their files: the run progress line in `internal/cli/experiment_run.go`, and the sandbox environment in `internal/claude/invoke.go`.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
