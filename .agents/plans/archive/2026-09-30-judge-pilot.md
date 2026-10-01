# LLM judge pilot

- Date: 2026-09-30
- Status: Complete (2026-10-01): **no-go for now; tests stay the only grader** ([results](../../../docs/research/2026-10-01-judge-pilot-results.md)). False passes and tasks without tests are INCONCLUSIVE, because the labels mark every change fixed. Quality is NO-GO. The judge misses both reliability bars on its own. PRs: #46 (protocol and script), #47 (browser label form), and the results PR with this archive. Judge calls: $12.07.
- Scope: on 2026-09-30 the user asked whether tests alone should decide results, and proposed an LLM judge "since we know results": each task has a reference solution to compare with. The [study](../../../docs/research/2026-09-27-ai-development-lab.md) puts rubric judges in Phase 3, never as the primary metric (§5.5). This pilot measures whether a reference-guided judge adds anything, before any product code is written.

## Why
- Today a run counts as fixed when the task's verification passes with the hidden tests restored. Nothing else looks at the fix.
- That leaves three gaps:
  - tests can be incomplete, so an agent can pass by special-casing inputs (a false pass);
  - two passing fixes can differ in quality;
  - a task without tests, such as a Jira ticket, cannot be graded at all.
- A judge has known costs:
  - it cannot run code;
  - it may anchor on the reference and punish a different fix that is just as correct;
  - it adds noise and money to every run;
  - judging Claude with Claude risks self-preference.
- Every graded run already saves `agent.diff` (`internal/run/run.go`), and each task holds its reference solution. The pilot therefore needs no new agent runs.

## Questions
Fixed before any labels or judge calls are seen.
1. **False passes.** On runs whose tests passed, does the judge agree with a human on "real fix or not"?
2. **Quality.** When both arms passed a task, does the judge prefer the same diff as the human?
3. **Tasks without tests.** Without seeing test results, does the judge's fixed/not-fixed verdict match the tests, over both passing and failing runs?
4. **Baseline.** Does the judge beat a deterministic measure: overlap with the reference in files touched, and the ratio of changed lines?
5. **Reliability.** Does it give the same answer on repeats and with the pair order swapped? On A/A pairs, where both arms had the same context, does its preference split evenly? What does one judgement cost?

## Data
- **The Phase 1 acceptance data:** 30 graded runs (24 passed, 6 failed) on 6 tasks, from the A/A and A/B.
- **The 16-run A/B's runs** (hardening step 6, done on 2026-10-01): 16 graded runs, all passed, on 8 tasks. Together that is 46 diffs (40 passed) and about 20 same-task pairs.
- **Only Agentium's own repository,** which is public. Diffs and instructions reach Anthropic through Claude Code, as the runs already do.
- **Where it lives:** the acceptance data folder, `~/.agentium-acceptance` (owner-only, outside every repository). An older snapshot in a session scratchpad under `/private/tmp` holds the same runs and is not needed.

## Method
- **Judge.** Claude Code headless (`claude -p`) on a pinned model (`claude-opus-5-5`) at a fixed effort. It runs with no tools and no project context, in an empty folder, under the same credential rules as runs.
- **What the judge sees:** the instruction, the reference solution's non-test diff, and the agent's diff (two diffs for a pair). It never sees the hidden tests, the test result or the arm.
- **Output:** strict JSON: `fixed` (yes, partly or no) with a one-line reason; for a pair, `prefer` (first, second or tie).
- **Blinding and repeats:** each single judgement runs 3 times. Each pair runs in both orders, so order flips are measured.
- **Human labels:** the user labels every diff and pair on the same form, before seeing any judge output. Arm and test result are hidden, and the order is shuffled with a recorded seed. This is about 2–3 hours of the user's time.
- **Baseline:** file overlap (Jaccard over non-test files) and changed-line ratio against the reference, computed from the diffs.
- **Code:** a standard-library Python script with unit tests in `docs/research/judge-pilot/`. It builds the prompts, calls the judge, parses the verdicts and computes every number. Protocol, labels and verdicts are committed; transcripts stay in the data folder, as in Phase 0.

## Go/no-go
*Changed on 2026-10-01 after an independent review, before any label. The user accepted the change by labelling without objection, as offered:* the rules below could be met by a judge that answers yes to every change. The [protocol](../../../docs/research/judge-pilot/protocol.md#verdicts) replaces them with three verdicts (false passes, quality, tasks without tests), each GO, NO-GO or INCONCLUSIVE. It adds trivial baselines, a requirement to catch the false passes found, and documents left out of both diffs. The first version:

Every agreement is reported with its 95% Wilson interval. With 40–50 diffs, an 80% agreement spans roughly 65–89%, so a "go" means "worth building", not "proven".
- **Go for a secondary judge score** (false passes and quality) when all of these hold:
  - it agrees with the human on "fixed" in at least 80% of passing runs;
  - it gives the same verdict on all 3 repeats in at least 90% of judgements;
  - pair order flips its preference in at most 10% of pairs;
  - it shows no significant arm preference on A/A pairs;
  - it agrees with the human more often than the baseline does.
- **Grading tasks without tests** is only "promising" when it matches the tests on at least 90% of runs. The few failing runs cannot confirm more than that.
- **Otherwise no-go:** tests stay the only grader. The baseline alone may still earn a report line.

## Acceptance
1. The pilot's data is in a durable folder, and its runs and diffs are complete. *Evidence:* step 1's record.
2. Protocol, prompt, label form, thresholds and seed are committed before any label or judge call. *Evidence:* commit order.
3. The script passes its unit tests (prompt building, blinding, verdict parsing, baseline, intervals) and a dry run with a fake judge. *Evidence:* the test output.
4. The human labels are complete and blind. *Evidence:* the committed label file, with no arm or result columns.
5. Every question is answered: agreement with interval, consistency, order flips, the A/A split, cost per judgement, and a comparison with the baseline. *Evidence:* `docs/research/<date>-judge-pilot-results.md`, whose numbers the script regenerates.
6. A go/no-go for each use: false passes, quality, and tasks without tests. The roadmap is updated. A "go" adds a decision record and a product plan; a "no-go" records why.
7. Nothing in `internal/` changes, and no dependency is added.

## Work
- [x] **1. Preserve the data.** Checked on 2026-09-30: `~/.agentium-acceptance` already held it. Its runs table matches the `/private/tmp` snapshot row for row (35 task runs and 5 calibrations; 24 passed, 6 failed, 5 cancelled), its 35 `agent.diff` files are byte-identical, and it keeps 40 `stream.jsonl` transcripts. No copy was needed.
- [x] **2. Protocol and script** ([protocol](../../../docs/research/judge-pilot/protocol.md), committed before any label or judge call).
  - **Checks:** `judge_pilot.py` passes its 14 unit tests. A fake-judge dry run on the real items went from 176 judgements through to the go/no-go table.
  - **The judge's Claude Code:** the installed 2.1.274 refuses Opus 5.5, so the judge runs on the desktop app's bundled 2.1.284. This is recorded in the protocol.
  - **Three priced calls:** $0.038–0.042 each, mostly output (high effort). That puts the full run at about $9, and $13.50 with a 50% margin.
  - **S01:** its pricing verdicts were shown in the session, so it is left out of the comparisons with your labels.
  - **Pairs:** 19 rather than 20–25; 8 of them are A/A.
- [x] **3. Human labels** (the user), on 2026-10-01; the A/B's diffs are in.
  - **Form:** paging long diffs in the terminal form was clumsy, so a browser form was added before any label (`label --web`, #47).
  - **Frozen** before the judge run, with SHA-256 `7590b875…`.
  - **Label quality:** all 46 singles are "yes", including the two empty changes, and the pairs have no tie and no note. Told that this leaves two verdicts without data, the user chose to run the judge anyway.
- [x] **4. Judge calls (paid; approved by the user, $15 cap).**
  - 176 judgements in about 31 minutes, for $12.07 (estimate $9).
  - One pair call hit the usage limit and succeeded on its retry.
  - The run had to stay inside the sandbox: a sandbox-bypassing launch was refused, and the sandboxed one worked.
- [x] **5. Analysis:** [results](../../../docs/research/2026-10-01-judge-pilot-results.md), go/no-go, roadmap update.
  - The script changed only in presentation after the results (protocol, Changes item 6).
  - Follow-ups are proposed in the results, not planned: check the judge's claims by running them, careful labels, and a steadier judge.

## Boundaries
- No product code and no new dependencies. The judge is called only through the installed Claude Code.
- No new agent runs; only existing diffs are judged.
- Out of scope, and depending on this pilot's outcome: a judge in reports, and tasks imported from issue trackers such as Jira (credentials and live integrations need their own scope).

## Verification
Unit tests for the script, the fake-judge dry run, `harness.py check changed`, CI, and a reviewer read of the protocol before step 3.

## Metrics
- Agent: Claude Code desktop / claude-opus-5-5 / default (coordinator); `reviewer` subagents on Opus / high
- Elapsed: about 6h over 2026-09-30 to 2026-10-01, including the user's labelling and a 31-minute judge run
- Check-fix loops: 4 (protocol review: changes requested, then approved with notes; label form; results presentation)
- User corrections: 1 (the terminal label form did not work for the user, so a browser form was built)
- Paid calls: 4 pricing and verification calls (about $0.16) and the judge run ($12.07)
- Review: protocol and script reviewed before labelling (#46); results reviewed before merge
