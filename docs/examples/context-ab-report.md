> A real report from Phase 1's acceptance runs, printed by `agentium experiment report ab` at commit `fb02e4d` and not edited. Claude Code 2.1.281 and claude-sonnet-5 worked on tasks taken from this repository. The experiment was stopped after 4 complete pairs to fit one usage window, so it is exploratory. See the [README](../../README.md#example-results).

---

# Experiment ab

Context A/B: A = `full`, B = `minimal`. Goal: cheaper, without losing success.

- **Success 100% → 75%**, Δ -25 pp (95%: -105 to +55): exploratory: too few tasks or runs for a verdict.
- **Cost +11%** (95%: -45% to +123%): exploratory: too few tasks or runs for a verdict.

8 of 12 runs settled (stopped); spent $7.33 of $24.00. 6 task(s) × 1 run(s) per arm; claude-sonnet-5, effort default, Claude Code 2.1.281, sign-in login. Locked 2026-09-29 16:22 UTC (method phase1-v1).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.

| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 100% | 75% | -25 pp | [-75, +0] pp | [-105, +55] pp | exploratory |
| Cost | primary | $0.659 | $0.728 | +11% | [-27%, +62%] | [-45%, +123%] | exploratory |
| Time | secondary | 163 s | 179 s | +10% | [-35%, +72%] | [-54%, +166%] | exploratory |
| Output tokens | secondary | 14937 | 19075 | +28% | [-17%, +123%] | [-47%, +211%] | exploratory |

Success: pass@1 100% (A) and 75% (B); every run of a task passed (pass^k) in 100% and 75% of tasks.

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|
| A | `full` | 4 | 30586 | $0.783 | $7.772 | 97% |
| B | `minimal` | 4 | 25979 (-4607) | $0.791 | $7.268 | 96% |

## Behavior

Runs counted in each arm, unless a total.

| | A | B |
|---|---|---|
| changed a test file | 4 | 4 |
| test files removed (total) | 0 | 0 |
| ran tests | 4 | 4 |
| ran the task's checks | 0 | 0 |
| committed | 0 | 0 |
| changed the checks | 0 | 0 |
| passed with changed runner configuration (a failure here) | 0 | 0 |
| permission denials (total) | 3 | 7 |
| files changed (mean) | 3.0 | 2.8 |
| lines changed (mean) | 45.5 | 41.0 |
| shell commands (mean) | 21.0 | 20.5 |

## Per task

● success, ○ failure, × not counted; cost is the mean of counted runs.

| Task | A | B | Cost A → B |
|---|---|---|---|
| deny-login-file | ● 1/1 | ● 1/1 | $0.265 → $0.485 |
| documents-filter | ● 1/1 | ○ 0/1 | $1.135 → $0.706 |
| judge-truncated-notice | - 0/0 | - 0/0 | - → - |
| run-survives-erase | ● 1/1 | ● 1/1 | $1.217 → $1.376 |
| scrub-whole-paths | ● 1/1 | ● 1/1 | $0.514 → $0.597 |
| unreadable-files | × 0/0 | × 0/0 | - → - |

## Notes

- The experiment is not finished (stopped: interrupted): 8 of 12 runs settled, and the results cover those.
- Runs not counted: 2 cancelled. Their spend is in the total.
- Not discriminating for success (every run passed, or every run failed, in both arms): deny-login-file, run-survives-erase, scrub-whole-paths. They stay in for cost.
- Success is exploratory: 0 of 4 task(s) have 3 counted runs in both arms, below the floor of 20.
- Cost is exploratory: 0 of 4 task(s) have 3 counted runs in both arms, below the floor of 8.
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, at 95%, or at 90% for "no loss" and "equivalent", which are one-sided tests at 5%.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
