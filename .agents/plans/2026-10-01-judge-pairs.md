# Judge: which arm fixed it better

- Date: 2026-10-01
- Status: In Progress (2026-10-02): resumed without the judge gate, by the user's decision ("integrate it without any proofs"). Step 1a merged (#53). Step 1b starts after `seq-v1` merges (it touches `internal/experiment` and `internal/report`). Because the pilot's NO-GO on order flips (11%) stands unrefuted, pair preferences are labeled "judge, unvalidated" in reports, are exploratory, and never count as a decisive verdict or toward the north star.
- Scope: the user's question on 2026-09-30, "who fixed the bug, who did it better". It follows the [decision](../decisions/2026-10-01-llm-judge-alongside-tests.md) to use a judge alongside tests.

## Why
When both arms pass a task, tests cannot say which fix is better. The pilot's pair judge showed two things, without any labels:
- swapping the order flipped its preference in 2 of 19 pairs;
- it showed no arm bias on A/A pairs (p = 0.69).

So each comparison must be asked in both orders, and a flip counts as a tie.

## Outcome
- With `--judge-pairs`, after a task's paired runs in both arms have passed, the judge compares their changes in both orders.
- The report gets a "Judge prefers" line per arm, with a paired interval and plain words; the A/A template is the bias check. Each task's row shows which change the judge preferred and why.
- Like the per-run judge, it decides nothing: success and cost verdicts are unchanged.

## Design
- **Prompt:** the pilot's pair prompt and schema ("first", "second" or "tie"), asked in both orders. When the two orders disagree once mapped back, the result is a tie and counts as a flip.
- **Pairing:** the same slots the experiment already pairs for its paired statistics: a task's run in each arm with the same repeat index.
- **Statistics:** a preference share per arm over the pairs that have a preference, with an exact binomial test and a Wilson interval. The flip rate is reported too. With fewer than 5 preferences, the result is "too few to say".
- **Cost:** about $0.18 per pair (two orders), counted in the preview and the budget like the per-run judge.

## Acceptance
1. **Unit tests:**
   - pairing over paired slots;
   - both orders asked, mapped back, and flips counted as ties;
   - the statistics, and the floor of 5 preferences.
2. **CLI tests** with a fake Claude Code: `--judge-pairs` is validated and locked, and the preview and budget include it.
3. **Reports:** the preference line and the per-task cells in all three formats; reports without it are unchanged (golden files).
4. **Real check (paid; approval):** an A/A with `--judge-pairs`, 4 tasks × 1, shows no arm bias and plausible reasons.
5. **Docs:** README and help.

## Work
- [ ] **1a. Pair core** (`internal/judge`): the pilot's pair prompt and schema, word for word, both orders, mapping back, flips counted as ties, and the preference statistics with their floor. Unit tests with a fake caller.
- [x] **1b. Experiments** (note from 1a's review: with repeats above 1, pairs of one task are not independent; add an honesty note or cluster by task): `--judge-pairs`, pairing over paired slots, the preview and the budget (after the per-run judge's step 2). Done (2026-10-02, branch `claude/feat/judge-pairs-experiments`):
  - `experiment new --judge-pairs` (with `--judge-model`/`--judge-effort`; `--judge-repeats` stays the per-run judge's) is stored as `Design.JudgePairs` (repeats 1), validated and locked with the design; `experiment show`, `plan` and `run` name it as unvalidated.
  - Pairing: `experiment.PairsOf` pairs each arm's settled run by the schedule's `Slot.Pair` (task and repeat index); a pair is compared only when both runs count as passing (`Success`) and the task has a reference in code. The comparison (`run.PairJudgement`, the 1a core's verdict and the arm-A run's ID) is stored on the pair's arm-B run, so `run.Spend` (`PairJudgeUSD`) carries it into every total.
  - Beside the runs: a pair judge goroutine compares one pair at a time as soon as a run completes a passing pair, so it holds no slot, no seq-v1 look and no stage barrier; the execution waits for queued comparisons only at its end. A seq-v1 experiment that ends at a look leaves its unrun slots unpaired.
  - Money: $2.00 a pair (4 calls at $0.50, with the judge's overshoot allowance per call off the default), held by `Execute` from a pair's first run (`Plan.PairHoldUSD`) and handed to the comparison before the completing run returns; work outside the schedule is counted through `Plan.Outside`. The preview, worst case, minimum budget, reserve and default budget include it; the estimate is $0.176 a pair.
  - Resume: each call's cost is stored as it lands as a stopped comparison; a resume compares the pairs without a comparison or with a stopped one once (funded first, else a budget stop), keeping the earlier spend. A pair judge at a usage limit pauses the experiment as the judge does.
  - Clustering: `experiment.PairPreferenceOf` counts the preference once per task (the arm its comparisons preferred more often), and the flip rate per pair; with one run per arm both agree. Chosen over an honesty note so that the binomial test and the Wilson interval stay valid at repeats above 1. Step 2 shows it.
  - JSON (additive): `experiment.judge_pairs`, `progress.pair_judge_usd` and `uncompared_pairs` (`judge_usd` is both judges'), a run's `pair_judge_cost_usd`.
  - Review of #126: no queued comparison starts while `--wait` waits for the usage window, and a pause at the usage limit drops the queue for the resume; a pair judge's usage limit pauses the runs at once (`Plan.Paused`, checked before every start and every seq-v1 stage); the comparisons' budget accounting has unit and CLI tests that fail under each reviewer mutation; `Recover` removes a leftover `pair-judge` folder on any command that takes the run lock; an interrupt never lowers a stored comparison's cost. Not done: the usage gate's projection leaves comparisons out, as it leaves the per-run judge out (no measured share of the window per comparison).
- [ ] **2. Report.**
- [ ] **3. Real check (paid; approval).**

## Boundaries
- Pairs whose two runs both passed only. Quality is never compared across failing runs.
- No new Go modules.

## Verification
Unit, CLI and golden tests; `harness.py check changed`; CI; a reviewer per step; one paid real check.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
