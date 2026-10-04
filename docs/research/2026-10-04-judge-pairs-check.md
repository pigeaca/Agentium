# Judge pairs: real check not run (money cap)

- Date: 2026-10-04
- Scope: [judge-pairs plan](../../.agents/plans/2026-10-01-judge-pairs.md) step 3, Acceptance 4: an A/A with `--judge-pairs`, 4 tasks × 1, to check arm bias, order flips and plausible reasons.
- Spend: $0.00. The check was **not run**.

## Why it was not run

The approved cap for this check is a worst case of at most $3 (approved as about $2). The preview for the required shape (A/A, `--judge-pairs`, 4 tasks × 1 run per arm) reports a worst case well above that:

```
agentium experiment new judge-pairs-check --task deny-login-file --task scrub-whole-paths \
  --task names-case-sensitive --task run-survives-erase --judge-pairs --run-budget 0.3 --budget 3
agentium experiment plan judge-pairs-check
...
Spend: at most $2.86 if every look runs (all 4 tasks; $11.60 if every run reaches its cap, overshoot included)
...
Worst case: every run reaches its $0.30 cap. Each pair's comparison may reach $2.00
  (4 calls at $0.50: both orders, each asked twice at most).
WARNING  the budget $3.00 is below the estimated $2.86 plus $7.35 held for runs in flight: expect it to stop the experiment early
```

The worst case is **$11.60**: 8 runs at the $0.30 cap ($2.40) plus four pair comparisons held at $2.00 each ($8.00) plus calibration. The pair holds alone (4 pairs × $2.00 = $8.00) already exceed $3, and the $2.00-per-pair hold is the judge's fixed per-call overshoot allowance (4 calls × $0.50), which a lower run cap or a different judge model cannot reduce. So **no sizing of "4 tasks × 1 with `--judge-pairs`" can bring the tool's worst case under $3.**

Per the money rules (the worst case must fit the check's approved cap; never raise a budget or cap myself; if a check would need more, stop and report), the check was not run. The expected cost ($2.86) is near the approved ~$2, and the $3 budget would in practice bound actual spend to about $3, but the stated worst case exceeds the cap, so proceeding was not authorized.

## What the coordinator/user can decide

Either:
- raise this check's cap to about $12 (the real worst case for 4 pairs), then run the A/A as specified; or
- confirm that the $3 budget bounding actual spend is acceptable despite the higher theoretical worst case; or
- run a smaller shape (e.g. 1 pair) that fits $3, accepting that it tests less.

No arm-bias, flip or reason data was produced. Step 3 of the judge-pairs plan stays open.
