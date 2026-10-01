# LLM judge pilot: protocol

Fixed on 2026-10-01, before any label, for the [judge pilot plan](../../../.agents/plans/2026-09-30-judge-pilot.md). The first version (`31e5365`) predates every judge call. Every change since is listed under Changes, with its reason; all were made before any label and before any judge verdict counted toward the results. The numbers come from [judge_pilot.py](judge_pilot.py), which [its tests](test_judge_pilot.py) check: `python3 -m unittest discover -s docs/research/judge-pilot`.

## Questions
1. **False passes.** On runs whose tests passed, does the judge agree with you on "real fix or not", and does it catch the false passes you find?
2. **Quality.** When both arms passed a task, does the judge prefer the same change as you?
3. **Tasks without tests.** Without seeing test results, does the judge's verdict match the tests?
4. **Baselines.** Does the judge beat simple rules: "always fixed", "always tie", "every change is fixed", and overlap with the reference's files?
5. **Reliability and cost.** Does it answer the same on repeats and with the pair order swapped? Does it prefer either arm when both had the same context (A/A)? What does a judgement cost?

## Data
The acceptance data folder (`~/.agentium-acceptance`) holds every graded run of the experiments `aa`, `aa2`, `ab` and `ab16`:
- **46 runs:** 40 passed and 6 failed. Two of the failing runs changed nothing.
- **19 pairs:** a task's arm A and arm B runs where both passed. Eight come from the A/A experiments and 11 from the A/Bs.

`judge_pilot.py prepare` builds the items in `~/.agentium-acceptance/judge-pilot`:
- **The candidate:** each run's stored `agent.diff`.
- **The reference:** `git diff` from the task's base to its solution, as the experiment's lock recorded the task.
- **Code only:** both diffs drop test files and documents (Markdown, reStructuredText, AsciiDoc) by Agentium's own rules. Neither side shows the hidden tests, and neither a missing nor an extra plan or README update decides a verdict. Every reference is now Go code only.
- **Length cap:** a diff is cut at 40,000 characters. None is that long.
- **Determinism:** Git's user settings are ignored, so the items are reproducible.
- **Order:** items are shuffled with seed 20261001 into S01–S46 and P01–P19, and each pair's order is drawn from the same seed.

## Blinding
- **What items hold:** `items.json` has only the instruction and the diffs. Runs, arms, experiments and results are in `key.json`.
- **Your rule:** label every item before opening `key.json`, the records or any judge output.
- **What you see:** the label form shows the judge's rubric (its system prompt) and its prompt, word for word.
- **Limits:** you may still infer results. Two empty changes are obvious failures. Some tasks ran only in `ab16`, whose runs all passed. You may remember Phase 1's outcomes. Answer from the diffs, not from memory.
- **S01:** its pricing verdicts were shown in the coordinator's session. S01, and the pair that holds S01's diff, are left out of every comparison with your labels. S01 stays in question 3, which compares the judge with tests, not with you.

## Labels (you)
Run `python3 docs/research/judge-pilot/judge_pilot.py label` from the repository root. It shows one item at a time, saves after every answer, and resumes where it stopped. Answer:
- **For a single change:** `y` (yes, fully), `p` (partly) or `n` (no), against the prompt's question.
- **For a pair:** `1`, `2` or `t` (tie).

An optional note can follow, after a space.

## Judge
- **Call:** Claude Code headless (`claude -p`, the desktop app's bundled 2.1.284; see Changes), model `claude-opus-5-5`, effort `high`. It runs with:
  - no tools (`--tools ""`) and the system prompt below instead of Claude Code's own;
  - a JSON schema for the answer;
  - project settings only, and no MCP servers;
  - an empty working folder, with no session kept.
- **Environment:** like runs, the judge keeps only an allowlist of the caller's variables:
  - `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `LANG`, `LC_*`, `TERM`, `TZ`, `CLAUDE_CONFIG_DIR`;
  - proxies and certificate files;
  - auto memory, auto-updates and claude.ai connectors turned off.

  It signs in with the user's own login. A verification call used 2,939 input tokens for a 4,760-character prompt: Claude Code's own scaffolding, and no user memory (none exists on this machine).
- **Repeats:** each single is judged 3 times; each pair once in each order.
- **Malformed answers:** an answer without a valid verdict is asked once more within the same judgement. If it fails again, it stays an error.
- **Calls that bring no answer:** a non-zero exit with no JSON, an error result (such as a refused model) or a 600-second timeout is recorded and tried again on the next `judge` run, at most 3 times per judgement. A timed-out call's cost is unknown.
- **Budget:** `--budget` stops new calls once the recorded cost reaches it. It can be passed by at most one judgement.

System prompt (also shown to you):

> You review code changes for a coding task against a reference change a developer made. You see diffs only: you cannot run code, and you do not know whether any change passes tests. Judge behavior, not style: a different approach is fine when it achieves the same result. Reply with the JSON the schema asks for, and nothing else.

Single prompt: the task instruction, the reference change and the candidate change, each in its own tag, then:

> Does the candidate change do what the task asks, as the reference does? Answer "yes" if it fully does, "partly" if it does only some of it or only for some inputs, and "no" if it does not, or only works around what checks it. Give the reason in one sentence.

Pair prompt: the instruction, the reference, change 1 and change 2, then:

> Which change is the better fix for the task: closer to doing exactly what was asked, more likely correct in cases a test might not cover, and no broader than needed? Answer "first", "second" or "tie", and give the reason in one sentence.

The exact text is `SYSTEM_PROMPT`, `SINGLE_PROMPT` and `PAIR_PROMPT` in the script.

## Measures
- **Fixed or not:** "yes" counts as fixed; "partly" and "no" count as not fixed.
- **The judge's verdict on a single:** the majority of the answers it gave. With no majority, it is "partly": one of each, or two different answers when a repeat ended in an error.
- **The judge's verdict on a pair:** its preference when both orders agree once mapped back. Otherwise the pair is an order flip and counts as a tie.
- **False passes:** passing runs you label not fixed.
- **Question 3's rate:** it leaves out the empty changes and the false passes you found, because there the tests themselves are in doubt. The plain rate over all runs is reported too.
- **Baselines:**
  - **"Always fixed"** (question 1), **"always tie"** (question 2) and **"every change is fixed"** (question 3).
  - **File overlap:** a single counts as fixed when it touches at least half of the reference's files. In a pair, the change with the higher file overlap (Jaccard) with the reference wins; on equal overlap, the one closer in changed lines to the reference wins, and otherwise it is a tie.
- **Rates:** every rate is given with its 95% Wilson interval. With 40 diffs, 80% agreement spans about 65–89%.
- **Other figures:** Cohen's kappa for question 1, and an exact two-sided binomial test for the A/A split.

## Verdicts
Each use gets **GO**, **NO-GO** or **INCONCLUSIVE**. A check with no cases is "n/a", which is not "met".

**Secondary score for false passes.** INCONCLUSIVE if you find fewer than 3 false passes. Otherwise GO only if all of these hold:
- it agrees with you on fixed or not in at least 80% of passing runs;
- it catches (judges not fixed) at least two-thirds of your false passes;
- it agrees with you more often than "always fixed" and than the file baseline;
- it gives the same verdict on all 3 repeats in at least 90% of singles.

**Secondary score for quality.** INCONCLUSIVE if you prefer one change in fewer than 5 pairs. Otherwise GO only if all of these hold:
- it agrees with you in at least 70% of pairs (first, second or tie);
- it agrees with you more often than "always tie" and than the file baseline;
- the pair order flips its preference in at most 10% of pairs;
- its arm preference on the A/A pairs is not significant (p ≥ 0.05).

**Grading tasks without tests** (at best "promising", never proven with this data). INCONCLUSIVE if fewer than 3 failing runs have a change. Otherwise GO only if all of these hold:
- it matches the tests in at least 90% of question 3's runs;
- it judges at least 3 of the failing runs with a change not fixed;
- it matches more often than "every change is fixed".

A GO adds a decision record and a product plan. A NO-GO or INCONCLUSIVE records why, and tests stay the only grader.

## Limits
- **Intervals are too narrow:** the 46 diffs come from 10 tasks, up to 8 per task, so they are not independent.
- **The A/A check is weak:** with 8 A/A pairs it is a sanity check of the pipeline. A 7–1 split gives p = 0.07 and passes. It fails only when every preference, at least 6 of them, goes to the same arm.
- **Few failures:** only 4 failing runs have a change, so question 3 can say "promising" at most.
- **Bias toward "tested":** a judge that reads diffs cannot run them, and neither can you.

## Cost
- **Pricing:** three pricing calls and one verification call ran on items that are not part of the results. They cost $0.036–0.042 each, most of it output at high effort.
- **Size of the judge run:** 46 × 3 single and 19 × 2 pair judgements, 176 in all, at about $9. A $15 cap is proposed.

## What is committed
Committed:
- this protocol, the script and its tests;
- `labels.json`, `key.json` and `verdicts.jsonl`, once they exist;
- the results document.

Not committed: `items.json`, which `prepare` rebuilds, and the judge's transcripts.

## Changes after the first judge call
All on 2026-10-01, before any label and before any verdict that counts.

1. **Claude Code version.** The first three pricing calls failed at no cost: the installed Claude Code 2.1.274 (Homebrew's newest is 2.1.277) refuses `claude-opus-5-5` and needs 2.1.280 or newer. The judge runs on the desktop app's bundled 2.1.284, passed with `--judge-cmd "$HOME/Library/Application Support/Claude/claude-code/2.1.284/claude.app/Contents/MacOS/claude"`.
2. **S01 left out of the label comparisons.** Its pricing verdicts were shown in the coordinator's session (see Blinding).
3. **After an independent review of the protocol:**
   - **Documents dropped:** both diffs leave out documents, not only tests. Eight of the ten references changed plan or README files that no run touched, which made "as the reference does" ambiguous and weakened the file baseline.
   - **Verdicts rewritten:** the first version's checks could be met by a judge that answers yes to every change. There are now three verdicts with INCONCLUSIVE when the data cannot tell, trivial baselines, a requirement to catch your false passes, a quality verdict (the first version had none), "n/a" for checks with no cases, and question 3 measured without empty changes and without your false passes.
   - **Environment allowlist:** the judge's environment is an allowlist, like runs'. The first calls inherited an enclosing Claude Code session's variables. One verification call confirmed the change.
   - **Retries:** errors are split. A malformed answer is asked twice and then kept as an error; a call that brought no answer is tried again at most 3 times. The first version retried every error without a limit. A timeout is now recorded instead of stopping the run.
   - **Label form:** it now shows the judge's rubric. The pair holding S01's diff is left out too. The blinding limits and the statistical limits are stated.
