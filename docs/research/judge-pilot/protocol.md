# LLM judge pilot: protocol

Fixed on 2026-10-01, before any label or judge call, for the [judge pilot plan](../../../.agents/plans/2026-09-30-judge-pilot.md). Anything changed after the first label or judge call is listed at the end, with the reason. The numbers come from [judge_pilot.py](judge_pilot.py), which [its tests](test_judge_pilot.py) check: `python3 -m unittest discover -s docs/research/judge-pilot`.

## Questions
1. **False passes.** On runs whose tests passed, does the judge agree with you on "real fix or not"?
2. **Quality.** When both arms passed a task, does the judge prefer the same change as you?
3. **Tasks without tests.** Without seeing test results, does the judge's verdict match the tests, over passing and failing runs?
4. **Baseline.** Does the judge beat a deterministic check of overlap with the reference?
5. **Reliability and cost.** Does it answer the same on repeats and with the pair order swapped? Does it prefer either arm when both had the same context (A/A)? What does a judgement cost?

## Data
The acceptance data folder (`~/.agentium-acceptance`) holds every graded run of the experiments `aa`, `aa2`, `ab` and `ab16`:
- **46 runs:** 40 passed and 6 failed. Two of the failing runs changed nothing at all, which makes them easy cases for question 3.
- **19 pairs:** a task's arm A and arm B runs where both passed. Eight come from the A/A experiments and 11 from the A/Bs.

`judge_pilot.py prepare` builds the items in `~/.agentium-acceptance/judge-pilot`:
- **Each run's change:** its stored `agent.diff`.
- **The reference:** `git diff` from the task's base to its solution over the reference files, as the experiment's lock recorded the task.
- **Tests left out:** both diffs drop test files by Agentium's own rule, so neither side shows the hidden tests.
- **Length cap:** a diff is cut at 40,000 characters. None is that long now.
- **Order:** items are shuffled with seed 20261001 into S01–S46 and P01–P19, and each pair's order is drawn from the same seed.

## Blinding
- **What items hold:** `items.json` has only the instruction and the diffs. Runs, arms, experiments and results are in `key.json`.
- **Your rule:** you label every item before opening `key.json`, the records or any judge output.
- **What you see:** the label form is the judge's own prompt, word for word.

## Labels (you)
Run `python3 docs/research/judge-pilot/judge_pilot.py label`. It shows one item at a time, saves after every answer, and resumes where it stopped. Answer:
- **For a single change:** `y` (yes, fully), `p` (partly) or `n` (no), against the prompt's question.
- **For a pair:** `1`, `2` or `t` (tie).

An optional note can follow, after a space.

## Judge
- **Call:** Claude Code headless (`claude -p`, 2.1.274), model `claude-opus-5-5`, effort `high`. It runs with:
  - no tools (`--tools ""`) and the system prompt below instead of Claude Code's own;
  - a JSON schema for the answer;
  - project settings only, and no MCP servers;
  - an empty working folder, with no session kept;
  - the user's sign-in, auto memory off.
- **Repeats:** each single is judged 3 times. Each pair is judged once in each order.
- **Malformed replies:** a reply that is not valid is asked once more. If it fails again, it is recorded as an error and left out.

System prompt:

> You review code changes for a coding task against a reference change a developer made. You see diffs only: you cannot run code, and you do not know whether any change passes tests. Judge behavior, not style: a different approach is fine when it achieves the same result. Reply with the JSON the schema asks for, and nothing else.

Single prompt: the task instruction, the reference change and the candidate change, each in its own tag, then:

> Does the candidate change do what the task asks, as the reference does? Answer "yes" if it fully does, "partly" if it does only some of it or only for some inputs, and "no" if it does not, or only works around what checks it. Give the reason in one sentence.

Pair prompt: the instruction, the reference, change 1 and change 2, then:

> Which change is the better fix for the task: closer to doing exactly what was asked, more likely correct in cases a test might not cover, and no broader than needed? Answer "first", "second" or "tie", and give the reason in one sentence.

The exact text is `SYSTEM_PROMPT`, `SINGLE_PROMPT` and `PAIR_PROMPT` in the script.

## Measures
- **Fixed or not:** "yes" counts as fixed; "partly" and "no" count as not fixed.
- **The judge's verdict on a single:** the majority of its three answers. With no majority (one of each), it is "partly".
- **The judge's verdict on a pair:** its preference when both orders agree once mapped back. Otherwise the pair is an order flip and counts as a tie. A tie in your label is a category of its own.
- **Baseline:**
  - a single counts as fixed when its change touches at least half of the reference's files;
  - in a pair, the change with the higher file overlap (Jaccard) with the reference wins; on equal overlap, the one closer in changed lines to the reference wins, and otherwise it is a tie.
- **Rates:** every rate is given with its 95% Wilson interval. With 40 diffs, 80% agreement spans about 65–89%.
- **Other figures:** Cohen's kappa for question 1, and an exact binomial test (two-sided) for the A/A split.

## Go/no-go
**A secondary judge score is worth building** (false passes and quality) only if all of these hold:
- it agrees with you on fixed or not in at least 80% of the passing runs;
- it gives the same verdict on all 3 repeats in at least 90% of singles;
- the pair order flips its preference in at most 10% of pairs;
- on the A/A pairs, its arm preference is not significant (p ≥ 0.05);
- it agrees with you on fixed or not more often than the baseline does.

**Grading tasks without tests** is "promising" when at least 90% of all runs' verdicts match their tests. Six failing runs cannot confirm more than that.

Otherwise the answer is no-go, and tests stay the only grader. A "go" adds a decision record and a product plan; a "no-go" records why.

## Cost
- **Pricing:** three real calls price a judgement before the judge calls are approved. They run on a copy of the items and are not part of the results.
- **Size of the judge run:** 46 × 3 single and 19 × 2 pair judgements, 176 in all.
- **Spending cap:** `--budget` stops the calls at the approved amount.

## What is committed
Committed:
- this protocol, the script and its tests;
- `labels.json`, `key.json` and `verdicts.jsonl`, once they exist;
- the results document.

Not committed: `items.json`, which `prepare` rebuilds from the data folder, and the judge's transcripts.

## Changes after the first label or judge call
None yet.
