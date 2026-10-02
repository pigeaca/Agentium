# Cheaper verdicts: sequential stopping, run reuse and drift (wave 3)

- Date: 2026-10-02
- Status: Planned (2026-10-02). Waiting on the user's answers to the [statistics note's open questions](../../docs/research/2026-10-02-wave3-statistics-note.md#9-open-questions-for-the-user): α, the primary cost metric, the reuse cut, futility, the drift panel, the defaults and the paid checks. No step starts before them.
- Scope: wave 3's feature track in the [next chapter](2026-10-01-next-chapter.md). It implements the [wave-3 statistics note](../../docs/research/2026-10-02-wave3-statistics-note.md), which fixes every statistical choice below. Changing one of them means changing the note first, with the user's agreement.

## Acceptance
1. **Exit gate**, exactly as the note's §5 defines it:
   - the long simulation passes against the production implementation;
   - a real reused-against-fresh A/A passes on one project (paid; its own approval).

   If the real A/A fails, the gate is recorded as failed, reuse stays off, and the user decides.
2. **Unchanged:** `phase1-v1` and `phase1-v2` experiments analyse, resume and report as before. `TestOneRunCostVerdictsSimulation` and the existing verdict tests pass unchanged.
3. **Each step** meets its own acceptance below, and has a recorded review (every step is high or medium risk).
4. **Docs:**
   - the README's method section and the architecture's code map describe `seq-v1`, the reuse key, the cold-start cost and the chart;
   - the note's status is updated;
   - the plan is archived with its metrics.

## Work
Steps run one after another where they touch `internal/experiment` (a hotspot). Step 6 can run beside steps 2–4.

- [ ] **1. The sequential engine: method `seq-v1`. Risk: high** (money: stop and budget logic; persistence: lock schema; concurrency: the stage barrier).
  - **`internal/stats`:** the Lan–DeMets O'Brien–Fleming-type spending, boundaries by numerical integration for actual information fractions, and the nominal levels. These move from the test-only helpers, which then call them.
  - **`internal/experiment`:**
    - designs with looks (8/12/16, or fewer when fewer tasks are eligible) and α = 4.5%;
    - a staged schedule, each stage interleaved as today;
    - an executor that settles a stage before its look and starts no run of the next stage until the decision;
    - `Decide` at each look's levels;
    - non-binding futility (conditional power below 10%, recorded in the lock);
    - early-stop and budget-stop rules (the last completed look's verdict).
  - **The lock** records the looks, α, the spending, the levels and the futility setting. A resume refuses changes.
  - **The preview** shows the maximum spend and the expected spend at no effect and at a 20% cut.
  - **The report** says "stopped at look k of 3", shows the look's interval, and notes that early stops overstate the effect.
  - *Acceptance:*
    - `TestGroupSequentialBounds` passes on production code (gsDesign values ±0.002, Brownian check).
    - The default and long simulations pass with the note's thresholds, calling the production analysis.
    - Executor tests with a fake Claude Code prove three things: no run of stage k+1 starts before look k; a look counts exactly its stage's prefix; and a stop, a usage pause or a budget stop between looks keeps the last look's verdict.
    - Resuming a `seq-v1` experiment after a crash mid-stage repeats no look and skips none.
    - A real console sample of a report stopped early.
- [ ] **2. Cold-start cost. Risk: high** (the money metric; a new stored field).
  - **`internal/claude`:** record each session's first-request cache-read tokens and write TTL, for the main session and each `parent_tool_use_id`.
  - **`internal/run`:** compute the cold-start cost with the dated price table, and store both costs.
  - **Reports** show the cold-start cost beside the raw cost. Which one is primary in `seq-v1` is the user's choice (open question 2).
  - *Acceptance:*
    - Golden stream tests: a warm first request, a cold one, subagents, and a 5-minute TTL.
    - A run that started cold is unchanged.
    - Unknown models make the value absent, not zero.
    - Old records without the field still load and report.
    - On recorded transcripts from the real experiments, the measured adjustment stays within the note's bound (first-request tokens × rate difference), and the check confirms that later requests hit only the run's own cache. Otherwise the note is corrected before step 4.
- [ ] **3. The reuse key and storage. Risk: high** (persistence; a wrong key silently biases verdicts).
  - **`internal/run`:** the `RunMethod` constant, and the invocation fingerprint with a golden test (as `TestGoProfileGolden`).
  - **The key:** the note's §1 table, including the SHA-256 of the pinned Claude Code binary and the toolchain identity.
  - **A migration:** a stored key per run, and the eligibility rules (outcomes, recovery, estimated cost, transcript, drift, warm-up notes, age at most 14 days).
  - **A query:** "the 4 most recent eligible runs per task for a key", returned in a fixed order.
  - *Acceptance:*
    - Table tests prove each key field changes the key, and harmless refactors don't.
    - The fingerprint golden test fails on a changed sandbox setting, allowlist or prompt suffix.
    - Ineligible runs are never returned; capped and timed-out runs are.
    - Migration up from today's schema keeps every run.
    - No path, secret or personal name enters the key or reports.
- [ ] **4. The reused-against-fresh A/A. Risk: high** (money; persistence of validations).
  - **A validation command or template** for a key: 16 tasks × 4 fresh runs against 4 stored runs at least 48 hours old (at most 14 days), with a seeded schedule.
  - **The pass rule** of the note's §2, and the allowance stored with the key.
  - **A failed key stays failed.** A validation is valid 30 days.
  - **The report** shows the estimate, the per-task table and the cache-read shares.
  - *Acceptance:*
    - Tests with a fake Claude Code cover pass, fail on a detected bias, fail on a wide interval, refusal to retry a failed key, refusal of stored runs younger than 48 hours, and expiry.
    - The preview states its cost.
    - **The real run** (exit gate, paid, approval first): on samber/lo or a project the user names, with Claude Code pinned through `AGENTIUM_CLAUDE`. Its report goes into `docs/examples/`.
- [ ] **5. Reuse in cost experiments. Risk: high** (money; verdicts).
  - **Design option:** reuse arm A from a validated key.
  - **The lock** records the stored run IDs and the allowance.
  - **Anchors:** 4 fresh runs of arm A on seeded tasks of the first stage, interleaved. They are kept out of the analysis and feed the key's anchor chart.
  - **Verdicts:** cost must clear ±allowance, with no equivalence. Success and time are exploratory.
  - **An anchor alarm** invalidates the key.
  - This is the step to defer if the user cuts reuse from wave 3 (open question 3).
  - *Acceptance:*
    - Tests: no reuse without a validated, unexpired key; stored runs are chosen blind to outcomes; the allowance changes verdicts as specified; success with a reused arm is exploratory; an alarm stops later reuse.
    - `TestReuseValidationAndAllowance` passes on production code.
    - A real console sample.
- [ ] **6. The drift chart. Risk: medium** (no money of its own; a missed alarm keeps a stale key alive, a false one only disables reuse).
  - **`internal/stats`:** the self-starting two-sided CUSUM (k = 0.5, h = 6) with its warm-up (no claims before 12 points).
  - **The chart's points** are stored per (project, model, effort, context, panel) and per key (anchors).
  - **Display:** a command shows a chart with the versions marked.
  - **The scheduled watch** that runs checks is wave 4 (automation A5).
  - *Acceptance:* `TestDriftChart` passes on production code, with h = 6 at most 5% false alarms within 52 checks in every scenario, and deterministic replay of stored points gives the same alarms.
- [ ] **7. Close the wave. Risk: low.**
  - Run the long simulation and record its figures in the note.
  - Get approval and run the real A/A. Then run the real `seq-v1` A/A smoke check (about $3), if approved.
  - Update the docs, then the next chapter's wave-3 results (by the coordinator).
  - Archive this plan.
  - *Acceptance:* the exit gate is recorded, passed or failed, with its evidence.

## Boundaries
- **Unchanged:** phase1-v1 and phase1-v2 analysis, `Decide`'s logic, and verdict thresholds.
- **Success:** never reused. No sequential success design in this wave.
- **Approvals:** no paid run without its own approval and estimate. No new Go modules.
- **Defaults:** a reuse default (14 days, 30 days, ±15%, 4 anchors) changes only through the note, before any validation runs.

## Verification
- **Per step:** `python3 scripts/harness.py check changed`, the step's tests, and a reviewer walking the threat checklist.
- **For the gate:** `AGENTIUM_LONG_SIM=50 go test -v -run 'TestGroupSequential|TestReuseValidation|TestDriftChart' ./internal/stats`, and the real A/A's report.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
