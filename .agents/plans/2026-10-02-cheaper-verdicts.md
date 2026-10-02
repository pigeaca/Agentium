# Cheaper verdicts: sequential stopping (wave 3)

- Date: 2026-10-02
- Status: Ready (2026-10-02).
  - **The user's decisions** (source: the user, 2026-10-02), answering the [statistics note's](../../docs/research/2026-10-02-wave3-statistics-note.md) questions:
    1. α = 4.5% for `seq-v1`: approved.
    2. Actual cost stays the primary metric. The isolated-run cost is reported beside it (step 2).
    3. Sequential stopping is built now. The reuse key, the reuse A/A, reuse in experiments and the drift chart are deferred (below).
    4. The `seq-v1` smoke check (about $3) is approved once step 1 is built. The reuse A/A is not approved.
    5. Futility stops are on by default (non-binding).
    6. The drift panel is deferred.
    7. The reuse defaults are deferred, and stay open until reuse resumes.
  - **Open before step 1's gate can pass:** the arm-specific noise result in the note's §7. False differences reach 5.66% [5.53, 5.79]; phase1-v2 gives 5.79% on the same scenarios. It went back to the coordinator, and α is unchanged.
- Scope: wave 3's feature track in the [next chapter](2026-10-01-next-chapter.md). It implements the [wave-3 statistics note](../../docs/research/2026-10-02-wave3-statistics-note.md), which fixes every statistical choice below. Changing one of them means changing the note first, with the user's agreement.

## Acceptance
1. **Exit gate** (the note's §5):
   - the long simulation (`AGENTIUM_LONG_SIM=50`, `TestGroupSequentialBounds` and `TestGroupSequentialFalseVerdicts`) passes against the production `seq-v1` implementation;
   - the real `seq-v1` A/A smoke check runs on one real project (approved; about $3, at most 32 runs). Its report goes into `docs/examples/`.
2. **Unchanged:** `phase1-v1` and `phase1-v2` experiments analyse, resume and report as before. `TestOneRunCostVerdictsSimulation` and the existing verdict tests pass unchanged.
3. **Reviews:** steps 1 and 2 each have a recorded review.
4. **Docs:** the README's method section and the architecture's code map describe `seq-v1` and the isolated-run cost. The note's status is updated, and the plan is archived with its metrics.

## Work
Steps 1 and 2 touch different packages and can run in parallel (step 1: `internal/stats`, `internal/experiment`, `internal/report`, `internal/cli`; step 2: `internal/claude`, `internal/run`, the report's cost table).

- [ ] **1. The sequential engine: method `seq-v1`. Risk: high** (money: stop and budget logic; persistence: lock schema; concurrency: the stage barrier).
  - **`internal/stats`:** the Lan–DeMets O'Brien–Fleming-type spending, and boundaries by numerical integration.
    - Interim fractions are counted tasks over the planned maximum.
    - Boundaries already used stay fixed, and the final look spends the remainder.
    - The test-only helpers move here and call the production code.
  - **`internal/experiment`:**
    - designs with looks (8/12/16, or fewer when fewer tasks are eligible) and α = 4.5%;
    - a staged schedule, each stage interleaved as today;
    - an executor that settles a stage before its look and starts no run of the next stage until the decision;
    - `Decide` at each look's levels;
    - non-binding futility, on by default (conditional power below 10%, recorded in the lock);
    - early-stop and budget-stop rules (the last completed look's verdict).
  - **The lock** records the looks, α, the spending, the levels and the futility setting. A resume refuses changes.
  - **The preview and the reserve** use the maximum spend (16 tasks), and the preview also shows the expected spend at no effect and at a 20% cut.
  - **The report** says "stopped at look k of 3", shows the look's interval, and notes that early stops overstate the effect.
  - *Threats* ([checklist](../roles/reviewer.md#threat-checklist)):
    - budget and consent: the reserve stays at the maximum on every resume between stages, and the preview states the maximum spend before consent;
    - crash and recovery: a kill mid-stage resumes the stage without repeating or skipping a look;
    - concurrent runs: the stage barrier holds against the concurrency of 2 and retries; no run of the next stage starts before the look.
  - *Acceptance:*
    - `TestGroupSequentialBounds` passes on production code (gsDesign values ±0.002, Brownian check).
    - The default simulation passes, and the long one passes per the gate (open item above).
    - Executor tests with a fake Claude Code prove four things:
      - no run of stage k+1 starts before look k;
      - a look counts exactly its stage's prefix;
      - lost tasks keep earlier boundaries and fractions over the planned maximum;
      - a stop, a usage pause or a budget stop between looks keeps the last look's verdict.
    - Resuming after a crash mid-stage repeats no look and skips none.
    - The reserve on resume is the maximum's.
    - A real console sample of a report stopped early.
- [ ] **2. Isolated-run cost, reported beside actual cost. Risk: high** (a money metric; a new stored field).
  - **`internal/claude`:** record the first-request cache-read tokens and write TTL of the main session, and of the first launch of each subagent type (`subagent_type`, matched by `parent_tool_use_id`).
  - **`internal/run`:** compute the isolated-run cost with the dated price table, and store it beside the reported cost.
  - **Reports** show it as "isolated-run cost", with a note that tells it apart from the existing "cold-cache cost" column. Actual cost stays primary.
  - *Threats* ([checklist](../roles/reviewer.md#threat-checklist)):
    - crash and recovery: old records, without the field, still load and report;
    - persistence: an absent value stays absent, never zero;
    - no personal data: subagent types and skill names stay out of stored records and reports, as skill names do today.
  - *Acceptance:*
    - Golden stream tests:
      - a warm first request and a cold one;
      - subagents, including two launches of the same subagent type, where only the first launch is repriced;
      - a 5-minute TTL.
    - A run that started cold is unchanged.
    - Unknown models make the value absent, not zero.
    - Old records still load and report.
    - On recorded transcripts of a run with no other run inside the cache TTL, every request the rule reprices shows `cache_read` = 0. If one doesn't, the rule and the note change before merge.
- [ ] **7. Close the wave. Risk: low.**
  - Run the long simulation on production code, and record its figures in the note.
  - Run the approved `seq-v1` smoke check.
  - Update the docs, then the next chapter's wave-3 results (by the coordinator).
  - Archive this plan.
  - *Acceptance:* the exit gate is recorded, passed or failed, with its evidence.

## Deferred (resume needs the user)
Steps 3–6 keep the note's design (§1, §2, §4). Before any of them resumes, the user must settle:
- whether reuse is worth building at all: it pays only for cuts of 35% or more, after about 11–22 reuse experiments per key within 14 days (note §6);
- the reuse defaults: the 14-day window, the ±15% pass bound, 4 anchors, and the validation's day-effect sensitivity (its pass rate with no bias falls to 53–69% at day effects of 0.03–0.05);
- the drift panel's size and budget;
- approval of the real reused-against-fresh A/A (about $13–15 on samber/lo, about $140 on Agentium's tasks).

The review's reuse findings are already fixed in the note's text:
- one eligibility filter for stored and fresh runs, with estimated-cost runs kept;
- a 14-day window and validation lifetime;
- failures recorded per key family, reopened only by a new pin, fingerprint or `RunMethod`;
- day effects and ±12% biases simulated.

The deferred steps:
- **3. The reuse key and storage.** Risk: high.
- **4. The reused-against-fresh A/A.** Risk: high; paid.
- **5. Reuse in cost experiments**, with anchors and the bias allowance. Risk: high.
- **6. The drift chart:** the self-starting CUSUM, k = 0.5, h = 6. Risk: medium.

## Boundaries
- **Unchanged:** phase1-v1 and phase1-v2 analysis, `Decide`'s logic, and verdict thresholds.
- **Success:** no sequential success design.
- **Not built:** no reuse and no drift chart in this wave.
- **Approvals:** no paid run beyond the approved `seq-v1` smoke check. No new Go modules.

## Verification
- **Per step:** `python3 scripts/harness.py check changed`, the step's tests, and a reviewer walking the threat checklist.
- **For the gate:** `AGENTIUM_LONG_SIM=50 go test -v -run 'TestGroupSequentialBounds|TestGroupSequentialFalseVerdicts' ./internal/stats`, and the smoke check's report.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
