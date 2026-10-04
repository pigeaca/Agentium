# Judge grading: real check (ticket-tasks step 4)

- Date: 2026-10-04
- Scope: [ticket-tasks plan](../../.agents/plans/2026-10-01-ticket-tasks.md) step 4, Acceptance 5: one judge-graded task from an Agentium pull request without tests, 2 runs per arm.
- Spend: $1.06 (paid part, approved cap: worst case ≤ $33, approved as about $5–8 expected). Preview worst case $25.60, expected $6.30.

Console output below is real, trimmed, home paths shown as `~`.

## The task

`fix-small-mining-round`, judge-graded, created from commit `2623356b` ("fix(start): a one- or two-task mining round with no valid task no longer stops mining"), base its parent `df31850e`. The commit changes one file (`internal/cli/start_tasks.go`, 9 lines) and no test files, so it is gradeable only by the judge against the reference. The instruction states the symptom and the desired behavior without naming the fix (the reference uses `mined >= 3`):

> When a mining round yields only one or two tasks and none of them validate, `start` stops mining entirely ... Fix `start` so that a mining round of only one or two tasks, all of them invalid, does not stop further mining. Only a larger all-invalid round should stop it. The existing cap on how many mining rounds may run must still apply.

`task validate` on it: valid (instruction 204 words, reference 1 code file, 9 changed lines; hidden-test checks skipped).

## The experiment

A/A calibration (both arms `base`), `--task fix-small-mining-round --goal better --repeats 2` = 1 task × 2 runs per arm = 4 runs, graded by the judge (claude-opus-5-5, effort high, majority of 5), `--run-budget 1.25 --budget 33`.

```
Experiment judge-grade-check: done
  4 of 4 runs settled; spent $1.06 of $33.00 (the judge $0.46 of it, not in the arms' costs)
ARM  CONTEXT  SETTLED  FAIR  SUCCESSES  UNFAIR  INFRA  CANCELLED   COST
A    base         2/2     2          0       0      0          0  $0.31
  arm A: the judge says 2 of 2 judge-graded run(s) fixed (unvalidated; not in SUCCESSES)
B    base         2/2     2          0       0      0          0  $0.29
  arm B: the judge says 2 of 2 judge-graded run(s) fixed (unvalidated; not in SUCCESSES)
```

## Judge votes and grades

| Run | Arm | Judge votes | Grade |
|---|---|---|---|
| 1 | B | yes,yes,yes,yes,yes | fixed (5 of 5) |
| 2 | A | yes,yes,yes,yes,yes | fixed (5 of 5) |
| 3 | B | yes,yes,yes,yes,yes | fixed (5 of 5) |
| 4 | A | yes,yes,yes,yes,yes | fixed (5 of 5) |

- Pending: 0. Ungraded (refusal/tie): 0.
- Judge cost: $0.46 total, ~$0.115 per grade (5 calls each on claude-opus-5-5 high). Cheaper than the $0.325/grade estimate because the diffs are tiny.
- Agent runs: ~$0.14–0.16 each.

## Do the grades look right?

Yes. All four agents produced essentially the reference fix: a size-2 constant and `mined > 2 && minedValid == 0` (equivalent to the reference's `mined >= 3`), with the `maxMineFactor` cap untouched. The judge's reason on one run:

> The candidate's check `mined > 2 && minedValid == 0` behaves the same as the reference's `mined >= 3`: an all-invalid round of one or two tasks no longer stops mining, a larger one still does with the same message, and the maxMineFactor cap on rounds is untouched.

That is a correct, specific reading of the diff. The judge did not over- or under-call any run.

## Does this validate the judge?

**No.** This is one easy task on which the correct fix is nearly forced (there is essentially one natural edit, matching the reference), and all four runs were correct, so there was no incorrect-but-plausible fix to test the judge's false-positive rate. The check shows the mechanism works end to end (grading without tests, 5-call majority, pending/ungraded accounting, cost, labelling apart from tests) and that the judge does not produce false negatives on an obviously-correct fix. It is weak evidence: one task, all positive, no negative control. The plan's validation criterion is not met, so judge-graded outcomes stay "judge, unvalidated", kept apart, and an experiment with judge-graded tasks still never counts toward the north star.
