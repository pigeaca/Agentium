# Experiment aa-1run

A/A calibration of context `base` (both arms).

- **Success 40% → 90%**, Δ +50 pp (95%: -1 to +101): exploratory: too few tasks or runs for a verdict.
- **Cost +1%** (95%: -14% to +19%): inconclusive (about 57 tasks would resolve it; this experiment has 10).

20 of 20 runs settled (done); spent $7.32 of $30.00. 10 task(s) × 1 run(s) per arm; claude-sonnet-5, effort default, Claude Code 2.1.281, sign-in login. Locked 2026-09-29 12:00 UTC (method phase1-v2).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.

| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 40% | 90% | +50 pp | [+10, +90] pp | [-1, +101] pp | exploratory |
| Cost | primary | $0.352 | $0.356 | +1% | [-11%, +15%] | [-14%, +19%] | inconclusive; about 57 tasks would resolve it |
| Time | secondary | 44 s | 44 s | +0% | [+0%, +0%] | [+0%, +0%] | exploratory |
| Output tokens | secondary | 3438 | 3438 | +0% | [+0%, +0%] | [+0%, +0%] | exploratory |

Success: pass@1 40% (A) and 90% (B); every run of a task passed (pass^k) in 40% and 90% of tasks.

## Noise

What the runs show, for planning later experiments: 10 task(s), 1.0 run(s) per task and arm on average; 95% ranges. Both arms use the same context, so this is the noise itself; compare it with the planner's defaults, which size every preview until a calibration replaces them. Cost ranges assume normal noise in log cost; with few tasks every range is wide.

| Component | Estimate | 95% range | Planner's default | How it was estimated |
|---|---|---|---|---|
| σ, per-run spread of log cost | 0.16 | 0.11–0.29 | 0.19: within the range | the paired differences' spread over √2: with one run per arm var(d) = 2σ² + τ², and τ = 0 in an A/A; chi-square range on 9 degrees of freedom; it assumes normal noise, and heavier tails make it too narrow |
| w, per-run variance of success | 0.25 | 0.05–0.42 | 0.20: within the range | half the paired differences' variance, as τ = 0 in an A/A; range from a bootstrap over tasks (2000 draws), which runs narrow with few tasks |

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Isolated-run cost | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|---|
| A | `base` | 10 | 30000 | $0.368 | - | $1.888 | 93% |
| B | `base` | 10 | 30000 (+0) | $0.364 | - | $1.884 | 93% |

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
| task-0 | ● 1/1 | ● 1/1 | $0.268 → $0.347 |
| task-1 | ○ 0/1 | ● 1/1 | $0.418 → $0.332 |
| task-2 | ● 1/1 | ● 1/1 | $0.584 → $0.442 |
| task-3 | ● 1/1 | ● 1/1 | $0.299 → $0.264 |
| task-4 | ○ 0/1 | ● 1/1 | $0.359 → $0.348 |
| task-5 | ○ 0/1 | ● 1/1 | $0.494 → $0.390 |
| task-6 | ○ 0/1 | ● 1/1 | $0.249 → $0.312 |
| task-7 | ● 1/1 | ○ 0/1 | $0.317 → $0.330 |
| task-8 | ○ 0/1 | ● 1/1 | $0.456 → $0.572 |
| task-9 | ○ 0/1 | ● 1/1 | $0.234 → $0.305 |

## Notes

- Not discriminating for success (every run passed, or every run failed, in both arms): task-0, task-2, task-3. They stay in for cost.
- Success is exploratory: 0 of 10 task(s) have 3 or more counted runs in both arms, below the floor of 20 tasks (method phase1-v2).
- Cost's verdict rests on tasks with fewer than 3 runs per arm, as method phase1-v2 allows: each task's difference carries the run-to-run noise, and the t-interval across tasks, the wider of the two here, decides. A seeded simulation of 8–12 tasks × 1 run (σ = 0.19, τ = 0.10–0.25, normal and skewed noise) checked it: false differences in at most 6% of experiments without a true difference, and 95% intervals that cover the true effect at least 93% of the time.
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, at 95%, or at 90% for "no loss" and "equivalent", which are one-sided tests at 5%.
- Both arms use the same context, so any difference is noise. At the 5% level, about one verdict in twenty shows a difference by chance.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
- Isolated-run cost is each run's cost had no other run warmed the prompt cache: the cache reads of the main session's first request and of each subagent type's first launch are repriced as cache writes, at the time to live the run wrote with, at Agentium's list prices of 2026-09-29. Unlike cold-cache cost, which reprices every cached read (the run's own included) as a bound, it keeps a run's reads of its own cache. It is at most the cold-cache cost, except when a subagent runs on a pricier model than the session or a run ended without Claude Code's result. Verdicts use the actual cost. 20 counted run(s) have no isolated-run cost (recorded before Agentium kept it, a model without a list price, or a subagent of unknown type), so their arm shows none.
