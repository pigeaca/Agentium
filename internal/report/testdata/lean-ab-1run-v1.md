# Experiment lean-ab-1run

Context A/B: A = `base`, B = `lean`. Goal: cheaper, without losing success.

- **Success 40% → 90%**, Δ +50 pp (95%: -1 to +101): exploratory: too few tasks or runs for a verdict.
- **Cost -22%** (95%: -35% to -6%): exploratory: too few tasks or runs for a verdict.

20 of 20 runs settled (done); spent $6.61 of $30.00. 10 task(s) × 1 run(s) per arm; claude-sonnet-5, effort default, Claude Code 2.1.281, sign-in login. Locked 2026-09-29 12:00 UTC (method phase1-v1).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.

| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 40% | 90% | +50 pp | [+10, +90] pp | [-1, +101] pp | exploratory |
| Cost | primary | $0.352 | $0.276 | -22% | [-33%, -9%] | [-35%, -6%] | exploratory |
| Time | secondary | 44 s | 44 s | +0% | [+0%, +0%] | [+0%, +0%] | exploratory |
| Output tokens | secondary | 3438 | 3438 | +0% | [+0%, +0%] | [+0%, +0%] | exploratory |

Success: pass@1 40% (A) and 90% (B); every run of a task passed (pass^k) in 40% and 90% of tasks.

## Noise

What the runs show, for planning later experiments: 10 task(s), 1.0 run(s) per task and arm on average; 95% ranges. Cost ranges assume normal noise in log cost; with few tasks every range is wide.

| Component | Estimate | 95% range | Planner's default | How it was estimated |
|---|---|---|---|---|
| σ, per-run spread of log cost | - | - | 0.19 | not separable from τ with one run per arm in an A/B: the paired differences' variance is 2σ² + τ²; the τ below takes the default σ |
| τ, spread of the cost effect across tasks | 0.00 | 0.00–0.39 | 0.10–0.25: overlaps the range | var(d) − 2σ², floored at zero, taking σ = 0.19 (the planner's default): one run per arm cannot separate σ from τ; chi-square range of var(d) on 9 degrees of freedom; it assumes normal noise, and heavier tails make it too narrow |
| w, per-run variance of success | - | - | 0.20 | not separable from τ with one run per arm in an A/B |

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|
| A | `base` | 10 | 30000 | $0.368 | $1.888 | 93% |
| B | `lean` | 10 | 30000 (+0) | $0.293 | $1.813 | 93% |

## Behavior

Runs counted in each arm, unless a total.

| | A | B |
|---|---|---|
| changed a test file | 0 | 0 |
| test files removed (total) | 0 | 0 |
| ran tests | 10 | 10 |
| ran the task's checks | 0 | 0 |
| committed | 0 | 0 |
| changed the checks | 0 | 0 |
| passed with changed runner configuration (a failure here) | 0 | 0 |
| permission denials (total) | 0 | 0 |
| files changed (mean) | 2.0 | 2.0 |
| lines changed (mean) | 13.0 | 13.0 |
| shell commands (mean) | 6.0 | 6.0 |

## Per task

● success, ○ failure, × not counted; cost is the mean of counted runs.

| Task | A | B | Cost A → B |
|---|---|---|---|
| task-0 | ● 1/1 | ● 1/1 | $0.268 → $0.223 |
| task-1 | ○ 0/1 | ● 1/1 | $0.418 → $0.270 |
| task-2 | ● 1/1 | ● 1/1 | $0.584 → $0.379 |
| task-3 | ● 1/1 | ● 1/1 | $0.299 → $0.239 |
| task-4 | ○ 0/1 | ● 1/1 | $0.359 → $0.315 |
| task-5 | ○ 0/1 | ● 1/1 | $0.494 → $0.242 |
| task-6 | ○ 0/1 | ● 1/1 | $0.249 → $0.184 |
| task-7 | ● 1/1 | ○ 0/1 | $0.317 → $0.272 |
| task-8 | ○ 0/1 | ● 1/1 | $0.456 → $0.600 |
| task-9 | ○ 0/1 | ● 1/1 | $0.234 → $0.204 |

## Notes

- Not discriminating for success (every run passed, or every run failed, in both arms): task-0, task-2, task-3. They stay in for cost.
- Success is exploratory: 0 of 10 task(s) have 3 or more counted runs in both arms, below the floor of 20 tasks (method phase1-v1).
- Cost is exploratory: 0 of 10 task(s) have 3 or more counted runs in both arms, below the floor of 8 tasks (method phase1-v1).
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, at 95%, or at 90% for "no loss" and "equivalent", which are one-sided tests at 5%.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
