# LLM judge pilot: results

Run on 2026-10-01 under the [judge pilot plan](../../.agents/plans/archive/2026-09-30-judge-pilot.md) and its [protocol](judge-pilot/protocol.md). The script, its tests, the labels, the key and every verdict are in [judge-pilot/](judge-pilot/). The appendix is the script's own output.

## Summary
- **Decision: no-go for now. Tests stay the only grader.** None of the three uses earned a GO under the rules fixed before any label:
  - catching false passes: **INCONCLUSIVE**;
  - judging quality between two passing fixes: **NO-GO**;
  - grading tasks without tests: **INCONCLUSIVE**.
- **The human labels could not support the comparison.** All 46 single changes were labelled "yes, fully", including the two that change nothing, and no pair was a tie. Warned about this, the user chose to run the judge anyway. The two verdicts that compare the judge with the labels therefore say little about the judge.
- **What the judge showed, whatever the labels:**
  - It misses both reliability bars, narrowly. It gave the same verdict on all 3 repeats in 87% of changes (the bar is 90%), and swapping a pair's order flipped its preference in 2 of 19 pairs (11%; the bar is 10%).
  - *Exploratory, outside the rules:*
    - It judged all 4 failing runs that have a change "partly", on every repeat, and both empty changes "no".
    - Three of those four runs are from `documents-filter`. There it judged every run "partly" for the same reason, whether the run passed or failed. Only one failing run (S39) shows it telling a failing run from passing ones.
  - *Exploratory:* it judged 18 of the 40 passing runs not fully fixed, by majority. All 18 come from 5 of the 10 tasks; on the other 5, its majority called every passing run fixed.
  - Its reasons are concrete. Many name an input and the wrong output it would give; others name a requirement of the instruction that the change misses. Whether those are real gaps the hidden tests miss, or the judge holding agents to extras in the reference that the instruction never asked for, is what careful labels were meant to settle.
- **Cost:** $12.07 for 176 judgements, 34% over the $9 estimate. That is $0.063 per single judgement and $0.088 per pair judgement, or about $0.19 per run at 3 repeats.

## What ran
- **Data:**
  - 46 graded runs (40 passed, 6 failed) on 10 tasks, from the experiments `aa`, `aa2`, `ab` and `ab16`;
  - 19 same-task pairs where both runs passed, 8 of them from A/A experiments;
  - both diffs are code only, without tests or documents.
- **Labels:** the user labelled all 65 items in the browser form on 2026-10-01. The form was closed and the label file's SHA-256 recorded (`7590b875…`) before the first counted judge call.
- **Judge:**
  - `claude-opus-5-5` at high effort, through Claude Code 2.1.284, with no tools and a JSON schema;
  - 138 single judgements (3 per change) and 38 pair judgements (both orders);
  - about 31 minutes of calls in all;
  - one pair call hit the usage limit and succeeded on the retry the protocol allows.

## The labels
- **Singles:** all 46 "yes". None is "partly" or "no", and none has a note.
- **Pairs:** 11 "second" and 8 "first". None is a tie.
- **Empty changes:** S35 and S38 show "(no change)" and are labelled "yes, fully", which a reading of the diff rules out.
- **Effect on each use:**
  - **False passes:** no passing run is labelled not fixed. A verdict needs at least 3, so it is INCONCLUSIVE by rule.
  - **Tasks without tests:** the rate leaves out runs where the label and the test result disagree. All 6 failing runs are labelled fixed, so none remains, and it is INCONCLUSIVE by rule.
    - This follows the protocol's Measures section ("the failing runs are counted after this") and the script committed before any label.
    - Read literally, the Verdicts section's "fewer than 3 failing runs have a change" would count the 4 failing runs. The checks would then apply and give NO-GO, since 22 of 40 is 55%, under the 90% bar.
    - Either reading gives no GO.
  - **Quality:** this is computed over 18 pairs, but its labels are as doubtful as the singles'.

This is a limit of this run, not a finding about the judge.

## Results by question
**False passes (INCONCLUSIVE).** The judge agrees with the labels on 22 of 39 passing runs (56%). Cohen's kappa is 0.00, as it must be when every label is the same.

**Quality (NO-GO).**
- The judge agrees with the labels on 6 of 18 pairs (33%). That is below the file-overlap baseline (9 of 18) and the 70% bar.
- The order-flip check, 2 of 19, fails as well, and it does not depend on labels. So careful pair labels alone would not have turned this into a GO.
- The A/A check passes: arm A was preferred in 4 of 6 preferences (p = 0.69), so there is no sign of an arm bias.

**Tasks without tests (INCONCLUSIVE).** No failing run remains under the rule. Outside the rules:
- All 4 failing runs with a change were judged not fixed (95% interval 51–100%). They come from only 2 tasks, though, and on `documents-filter` (3 of the 4) every passing run was judged "partly" too.
- 22 of 40 passing runs were judged fixed.

Taking the tests as the truth, the judge matches them on 28 of 46 runs (61%). That assumes the 18 passing runs it flags were real fixes, which is exactly what is not known.

**Reliability.** 40 of 46 changes got the same verdict on all 3 repeats. The 6 that varied are all passing runs, and each moved by one step: yes and partly, or partly and no. Three are from `scrub-whole-paths`, the task the judge flags most.

## What the judge objects to
Paraphrased from its reasons, one task at a time:
- **`scrub-whole-paths`** (7 of 8 passing runs). The fix checks for a path boundary after the folder but not before it, and it replaces the given spelling before the resolved one. With a data folder of `/var/x`, the text `/private/var/x/...` becomes `/private<agentium data>/...`. Some fixes also stop replacing a folder followed by a space, colon or quote.
- **`run-survives-erase`** (5 of 6). Shared by all five: a run is not always stored with its spend when Agentium fails after the agent ran, for example while reading the transcript, listing skills or grading. Three also count commits with `git -C repo`, so git can find an enclosing repository.
- **`documents-filter`** (3 of 3, plus the 3 failing runs). The test-data check skips only folders named `testdata`, while the reference also skips `test`, `tests`, `fixtures` and others. Whether the instruction asked for that is the anchoring question. Some runs are also faulted for refusing `.markdown` or `.mdx` files.
- **`fresh-checkouts`** (2 of 2). Binary files' `-1` line counts are compared with a pull request's line counts, so a squash-merged pull request with a binary file is wrongly rejected. One run also writes solution files before setup, against the instruction's order.
- **`stats-review`** (1 of 2). A floor check keeps a condition the reference removed.

Most claims can be checked by running them: build the input, apply the agent's change, and see the output. The others name a requirement, which can be checked against the instruction.

## Decision
- **Under the pilot's rules, no use got a GO.**
- **The user's decision (2026-10-01):** build the judge anyway, without relying on this pilot. It gives an opt-in verdict per run, shown alongside the tests and never deciding ([decision](../../.agents/decisions/2026-10-01-llm-judge-alongside-tests.md), [plan](../../.agents/plans/2026-10-01-llm-judge.md)). Tests remain the only grader of pass and fail.
- Even with careful labels, the judge as configured fails both reliability bars. A product judge would need to be steadier first.
- Exploratory, outside the rules: the judge flags passing runs on only some tasks, not across the board, and its claims are specific. A careful look is still worth having. Its "not fixed" on all 4 failing runs with a change shows less than it seems, because on `documents-filter` it judged passing and failing runs alike.

## What would settle it
1. **Check the judge's claims by running them.** This needs no human labels. For each "partly" or "no", ask the judge for one concrete failing input, turn it into a test, and run that test against both the agent's change and the reference. A claim counts only when the reference passes and the agent's change fails. That separates real false passes from anchoring on the reference, and the 18 flagged passing runs on 5 tasks are enough for a first look.
2. **Careful labels.** Label only the singles, grouped by task (one reading of each reference), ideally by someone who did not run the experiments.
3. **A steadier judge.** Before any product use, test whether a tighter rubric or a different effort lifts repeat agreement above 90% and keeps order flips under 10%.

None of these is on the roadmap yet; the roadmap moves on to Java and Rust.

## Reproduce
1. Rebuild `items.json` from the acceptance data folder: `python3 docs/research/judge-pilot/judge_pilot.py prepare`.
2. Copy `labels.json`, `key.json` and `verdicts.jsonl` from `docs/research/judge-pilot/` into its work folder (`~/.agentium-acceptance/judge-pilot`).
3. Run `judge_pilot.py analyze`.

After the results, the script changed only in presentation, and gained the exploratory section (protocol, Changes item 6). The rules did not change.

## Appendix: the script's analysis

### Q1. False passes: on runs that passed their tests, does the judge agree with you on "fixed"?

- Passing runs you judged not fixed (false passes): 0 of 39, 0% (95%: 0–9%).
- The judge caught (judged not fixed): n/a (no cases) of them.
- The judge agrees with you, fixed or not: 22 of 39, 56% (95%: 41–71%); Cohen's kappa 0.00.
- Same three-way answer (yes, partly, no): 22 of 39, 56% (95%: 41–71%).
- Baseline "always fixed" agrees with you: 39 of 39, 100% (95%: 91–100%).
- Baseline "touches at least half of the reference's files" agrees with you: 39 of 39, 100% (95%: 91–100%).

### Q2. Quality: when both arms passed, does the judge prefer the same change as you?

- Pairs where you preferred one change: 18 of 18.
- The judge agrees with you (first, second or tie): 6 of 18, 33% (95%: 16–56%).
- Baseline "always tie" agrees with you: 0 of 18, 0% (95%: 0–18%).
- Baseline "more file overlap, then size closer to the reference" agrees with you: 9 of 18, 50% (95%: 29–71%).

### Q3. Tasks without tests: without seeing results, does the judge's verdict match the tests?

- Runs with a change, without the 6 where your label and the tests disagree: 22 of 40, 55% (95%: 40–69%) match their tests; all runs: 28 of 46, 61% (95%: 46–74%).
- Failing runs with a change, judged not fixed: n/a (no cases).
- Passing runs, judged fixed: 22 of 40, 55% (95%: 40–69%).
- Baseline "every change is fixed" matches: 40 of 40, 100% (95%: 91–100%).

### Exploratory, outside the verdict rules

- Failing runs with a change, judged not fixed, whatever your label: 4 of 4, 100% (95%: 51–100%).
- Passing runs judged not fixed, by task: scrub-whole-paths 7 of 8, run-survives-erase 5 of 6, documents-filter 3 of 3, fresh-checkouts 2 of 2, stats-review 1 of 2, active-config-files 0 of 2, deny-login-file 0 of 6, judge-truncated-notice 0 of 5, temp-files-in-data 0 of 2, unreadable-files 0 of 4.

### Q5. Reliability and cost

- Same verdict on all 3 repeats: 40 of 46, 87% (95%: 74–94%).
- Pair order flips the preference: 2 of 19, 11% (95%: 3–31%).
- A/A pairs (the same context in both arms): 8 judged; arm A preferred in 4 of the 6 with a preference (two-sided p = 0.69; no difference is expected, and with so few pairs this is a sanity check).
- Judgements: 176 (0 ended in an error); calls cost $12.07 in all, $0.068 on average (claude-opus-5-5, effort high).

### Verdicts (rules fixed in protocol.md)

**Secondary score for false passes: INCONCLUSIVE** (you found 0 false passes; at least 3 are needed to tell whether the judge catches them).

**Secondary score for quality: NO-GO**

- NOT met: agrees with you in >= 70% of pairs
- met: agrees with you more often than "always tie"
- NOT met: agrees with you more often than the file baseline
- NOT met: pair order flips its preference in <= 10% of pairs
- met: no significant arm preference on A/A pairs (p >= 0.05)

**Grading tasks without tests (promising, not proven): INCONCLUSIVE** (0 failing runs with a change remain once runs where you and the tests disagree are left out; at least 3 are needed).
