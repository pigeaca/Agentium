# Experiment opus-vs-sonnet

Model A/B on context `base`: A = `claude-opus-5-5`, B = `claude-sonnet-5-5`. Goal: cheaper, without losing success.

- **Success 50% → 75%**, Δ +25 pp (95%: -14 to +64): exploratory: too few tasks or runs for a verdict.
- **Cost -63%** (95%: -70% to -53%): improved.

16 of 16 runs settled (done); spent $3.10 of $54.00. 8 task(s) × 1 run(s) per arm; A claude-opus-5-5, B claude-sonnet-5-5, effort A default, B default, Claude Code 2.1.285, sign-in login. Locked 2026-10-02 04:38 UTC (method phase1-v2).

First decisive verdict: improved on cost in opus-vs-sonnet, 1h 5m after init, $3.34 spent up to it.

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.

| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 50% | 75% | +25 pp | [+0, +62] pp | [-14, +64] pp | exploratory |
| Cost | primary | $0.269 | $0.101 | -63% | [-68%, -56%] | [-70%, -53%] | improved |
| Time | secondary | 61 s | 31 s | -49% | [-56%, -39%] | [-59%, -36%] | exploratory |
| Output tokens | secondary | 4456 | 2329 | -48% | [-57%, -39%] | [-58%, -35%] | exploratory |

Success: pass@1 50% (A) and 75% (B); every run of a task passed (pass^k) in 50% and 75% of tasks.

## Noise

What the runs show, for planning later experiments: 8 task(s), 1.0 run(s) per task and arm on average; 95% ranges. Cost ranges assume normal noise in log cost; with few tasks every range is wide.

| Component | Estimate | 95% range | Planner's default | How it was estimated |
|---|---|---|---|---|
| σ, per-run spread of log cost | - | - | 0.19 | not separable from τ with one run per arm in an A/B: the paired differences' variance is 2σ² + τ²; the τ below takes the default σ |
| τ, spread of the cost effect across tasks | 0.00 | 0.00–0.47 | 0.10–0.25: overlaps the range | var(d) − 2σ², floored at zero, taking σ = 0.19 (the planner's default): one run per arm cannot separate σ from τ; chi-square range of var(d) on 7 degrees of freedom; it assumes normal noise, and heavier tails make it too narrow |
| w, per-run variance of success | - | - | 0.20 | not separable from τ with one run per arm in an A/B |

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|
| A | `base` | 8 | 15432 | $0.284 | $1.908 | 92% |
| B | `base` | 8 | 15439 (+7) | $0.103 | $0.572 | 90% |

## Context use

What the counted runs used of their context beyond what loads at start: path-scoped rules and folder instructions that loaded for the files the agent worked with; context files and linked documents the agent or its subagents read (with the Read tool, or given to cat, sed, grep and the like); the project's skills and commands they invoked; and the subagents they started (the project's and Claude Code's by name, any other only counted).

| | A | B |
|---|---|---|
| files loaded at start | 0 | 0 |
| `docs/CLAUDE.md` | 3 of 8 | 0 of 8 |

Loaded at start: A, none; B, none.

## Behavior

Runs counted in each arm, unless a total.

| | A | B |
|---|---|---|
| changed a test file | 8 | 7 |
| test files removed (total) | 0 | 0 |
| ran tests | 8 | 8 |
| ran the task's checks | 0 | 0 |
| committed | 0 | 0 |
| changed the checks | 0 | 0 |
| passed with changed runner configuration (a failure here) | 0 | 0 |
| permission denials (total) | 9 | 3 |
| files changed (mean) | 4.0 | 2.1 |
| lines changed (mean) | 93.0 | 46.9 |
| shell commands (mean) | 7.4 | 5.4 |

## Per task

● success, ○ failure, × not counted; cost is the mean of counted runs.

| Task | A | B | Cost A → B |
|---|---|---|---|
| feat-add-nthor-and-nthorempty-functions-8755356 | ○ 0/1 | ● 1/1 | $0.228 → $0.100 |
| feat-add-unionby-and-unionbyerr-878-8c82fb8 | ● 1/1 | ● 1/1 | $0.415 → $0.135 |
| feat-support-for-buffer-iterator-824-56ef3be | ○ 0/1 | ○ 0/1 | $0.463 → $0.108 |
| feature-intersect-by-653-43ae3d7 | ○ 0/1 | ○ 0/1 | $0.311 → $0.111 |
| fix-correct-dropbyindex-handling-of-c70160e | ● 1/1 | ● 1/1 | $0.219 → $0.073 |
| fix-it-mode-align-behavior-with-lo-mode-02d371c | ○ 0/1 | ● 1/1 | $0.161 → $0.073 |
| fix-iter-tuples-support-break-iteration-27e2842 | ● 1/1 | ● 1/1 | $0.236 → $0.133 |
| fix-nth-reject-indexes-that-do-not-fit-795f6b9 | ● 1/1 | ● 1/1 | $0.239 → $0.092 |

## Notes

- Not discriminating for success (every run passed, or every run failed, in both arms): feat-add-unionby-and-unionbyerr-878-8c82fb8, feat-support-for-buffer-iterator-824-56ef3be, feature-intersect-by-653-43ae3d7, fix-correct-dropbyindex-handling-of-c70160e, fix-iter-tuples-support-break-iteration-27e2842, fix-nth-reject-indexes-that-do-not-fit-795f6b9. They stay in for cost.
- Success is exploratory: 0 of 8 task(s) have 3 or more counted runs in both arms, below the floor of 20 tasks (method phase1-v2).
- Cost's verdict rests on tasks with fewer than 3 runs per arm, as method phase1-v2 allows: each task's difference carries the run-to-run noise, and the t-interval across tasks, the wider of the two here, decides. A seeded simulation of 8–12 tasks × 1 run (σ = 0.19, τ = 0.10–0.25, normal and skewed noise) checked it: false differences in at most 6% of experiments without a true difference, and 95% intervals that cover the true effect at least 93% of the time.
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, at 95%, or at 90% for "no loss" and "equivalent", which are one-sided tests at 5%.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
