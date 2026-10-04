# Judge pairs: real check (judge-pairs step 3)

- Date: 2026-10-04
- Scope: [judge-pairs plan](../../.agents/plans/archive/2026-10-01-judge-pairs.md) step 3, Acceptance 4: an A/A with `--judge-pairs`, 4 tasks × 1, to check arm bias, order flips and whether the reasons are plausible.
- Spend: **$2.06** (8 runs $1.59, pair judge $0.46; the context's calibration was reused, $0). Cap: the user's hard cap of $12 (2026-10-04, "Allow up to $12").
- The first attempt, the same day, was not run: its preview's worst case ($11.60) was above the check's first cap of $3, and no sizing of 4 pairs fits $3.

Console output below is real and trimmed.

## Preview

```
agentium experiment new judge-pairs-check --task deny-login-file --task scrub-whole-paths \
  --task names-case-sensitive --task run-survives-erase --judge-pairs --run-budget 0.3 --budget 12
agentium experiment plan judge-pairs-check
...
Spend: at most $1.91 if every look runs (all 4 tasks; $11.60 if every run reaches its cap, overshoot included)
...
Worst case: every run reaches its $0.30 cap. Each pair's comparison may reach $2.00 (4 calls at $0.50: both orders, each asked twice at most).
```

The worst case, $11.60 ≤ $12, is 8 runs × ($0.30 cap + $0.15 overshoot allowance) = $3.60, plus 4 pairs × 4 calls × `judge.CallCapUSD` ($0.50) = $8.00. The context was already calibrated, so the worst case holds nothing for a calibration.

## Run

```
[1/8] run-survives-erase, arm A, repeat 1: ok, $0.28 (spent $0.28 of $12.00)
[2/8] run-survives-erase, arm B, repeat 1: ok, $0.29 (spent $0.57 of $12.00)
Compared pair run-survives-erase, repeat 1 (pair judge, unvalidated): prefers A, $0.18
[3/8] names-case-sensitive, arm A, repeat 1: ok, $0.18 (spent $0.94 of $12.00)
[4/8] names-case-sensitive, arm B, repeat 1: ok, $0.17 (spent $1.11 of $12.00)
Compared pair names-case-sensitive, repeat 1 (pair judge, unvalidated): prefers B, $0.10
[5/8] deny-login-file, arm A, repeat 1: ok, $0.13 (spent $1.33 of $12.00)
[6/8] deny-login-file, arm B, repeat 1: ok, $0.14 (spent $1.47 of $12.00)
Compared pair deny-login-file, repeat 1 (pair judge, unvalidated): tie, $0.06
[8/8] scrub-whole-paths, arm A, repeat 1: ok, $0.19 (spent $1.72 of $12.00)
[7/8] scrub-whole-paths, arm B, repeat 1: ok, $0.21 (spent $1.93 of $12.00)
Compared pair scrub-whole-paths, repeat 1 (pair judge, unvalidated): prefers A, $0.12

Experiment judge-pairs-check: done
  8 of 8 runs settled; spent $2.06 of $12.00 (the judge $0.46 of it, not in the arms' costs)
```

All 8 runs passed their hidden tests (graded in the sandbox), so all 4 pairs were compared, in both orders.

## Report (Judge pairs section)

```
Judge prefers: too few to say (3 tasks with a preference, 5 needed).
Comparisons: 4 complete (1 tie, 0 flips), 0 incomplete, 0 not asked (a change had no code); tasks: 1 tie.
Pair judge cost: $0.46, in the spend and not in the arms' costs.
```

| Task | Prefers | Why (Change 1 is A's), abridged |
|---|---|---|
| deny-login-file | tie | - |
| names-case-sensitive | B | B matches the reference's grep fix and adds a final cancellation check; A widens the shared grep signature with an extra `exact` flag |
| run-survives-erase | A | A sends every failure after the agent ran through one path that adds a note and counts commits as the reference does; B only adds an `Lstat` check and returns grading errors with no note |
| scrub-whole-paths | A | A treats non-ASCII bytes as name characters, so a home path is not replaced inside a longer name with an accented letter; B would replace it |

## Findings

- **Arm bias:** A preferred on 2 tasks, B on 1, 1 tie. In an A/A both arms are the same setup, so any preference is noise; 2 to 1 is what chance gives. The report says "too few to say" (the floor is 5 tasks with a preference). No bias is shown, and none could be shown with 3 votes.
- **Order flips:** 0 of 4 comparisons flipped (the tie was a tie in both orders). The pilot saw 2 flips in 19; 0 in 4 is consistent with that rate and does not measure it.
- **Reasons:** plausible and specific. Two were checked against the diffs and are accurate: in scrub-whole-paths, A's name-byte test counts bytes ≥ 0x80 and B's does not; in names-case-sensitive, A adds an `exact bool` parameter to the shared `grep` and B adds `ctx.Err()` checks. The judge picks out real differences between two passing fixes and ties the near-identical pair.
- **Cost:** $0.115 a pair on average (estimate $0.176), well inside the $2.00 hold per pair.

## What 4 pairs can and cannot show

They show that the pair judge runs end to end on real runs: it compares both orders, records ties, costs what the estimate says or less, and gives reasons that match the code. They cannot show the absence of an arm bias or measure the flip rate: with 3 votes, any split is consistent with chance, and the binomial test needs at least 5. The pair judge stays unvalidated and exploratory, and the pilot's NO-GO on order flips (11%) is neither confirmed nor refuted.
