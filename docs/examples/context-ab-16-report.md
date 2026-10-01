# Experiment ab16

Context A/B: A = `full`, B = `minimal`. Goal: cheaper, without losing success.

- **Success 100% → 100%**, Δ +0 pp (95%: +0 to +0): exploratory: too few tasks or runs for a verdict.
- **Cost +5%** (95%: -11% to +25%): inconclusive (about 55 tasks would resolve it; this experiment has 8).

16 of 16 runs settled (done); spent $19.65 of $25.00. 8 task(s) × 1 run(s) per arm; claude-sonnet-5, effort default, Claude Code 2.1.274, sign-in login. Locked 2026-09-30 20:06 UTC (method phase1-v2).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.

| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 100% | 100% | +0 pp | [+0, +0] pp | [+0, +0] pp | exploratory |
| Cost | primary | $1.076 | $1.134 | +5% | [-8%, +20%] | [-11%, +25%] | inconclusive; about 55 tasks would resolve it |
| Time | secondary | 340 s | 374 s | +10% | [-1%, +28%] | [-7%, +30%] | exploratory |
| Output tokens | secondary | 28743 | 32598 | +13% | [+0%, +39%] | [-10%, +43%] | exploratory |

Success: pass@1 100% (A) and 100% (B); every run of a task passed (pass^k) in 100% and 100% of tasks.

## Noise

What the runs show, for planning later experiments: 8 task(s), 1.0 run(s) per task and arm on average; 95% ranges. Cost ranges assume normal noise in log cost; with few tasks every range is wide.

| Component | Estimate | 95% range | Planner's default | How it was estimated |
|---|---|---|---|---|
| σ, per-run spread of log cost | - | - | 0.19 | not separable from τ with one run per arm in an A/B: the paired differences' variance is 2σ² + τ²; the τ below takes the default σ |
| τ, spread of the cost effect across tasks | 0.00 | 0.00–0.33 | 0.10–0.25: overlaps the range | var(d) − 2σ², floored at zero, taking σ = 0.19 (the planner's default): one run per arm cannot separate σ from τ; chi-square range of var(d) on 7 degrees of freedom; it assumes normal noise, and heavier tails make it too narrow |

Success did not vary (every counted run passed, or every one failed), so there is no w.

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|
| A | `full` | 8 | 30520 | $1.181 | $11.162 | 97% |
| B | `minimal` | 8 | 25946 (-4575) | $1.275 | $12.039 | 97% |

## Context use

What the counted runs used of their context beyond what loads at start: context files and linked documents they read (with the Read tool, or named in a shell command), the project's skills they invoked, and the subagents they started.

| | A | B |
|---|---|---|
| files loaded at start | 6 | 6 |
| `.agents/roles/investigator.md` | 0 of 8 | 1 of 8 |
| subagent `investigator` | 0 of 8 | 1 of 8 |

Loaded at start: A, `.agents/README.md`, `.agents/ROADMAP.md`, `.agents/architecture.md`, `.agents/rules/core.md`, `AGENTS.md`, `CLAUDE.md`; B, `.agents/README.md`, `.agents/ROADMAP.md`, `.agents/architecture.md`, `.agents/rules/core.md`, `AGENTS.md`, `CLAUDE.md`.

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
| permission denials (total) | 4 | 3 |
| files changed (mean) | 4.2 | 4.4 |
| lines changed (mean) | 189.5 | 182.2 |
| shell commands (mean) | 18.8 | 18.2 |

## Per task

● success, ○ failure, × not counted; cost is the mean of counted runs.

| Task | A | B | Cost A → B |
|---|---|---|---|
| active-config-files | ● 1/1 | ● 1/1 | $0.876 → $0.963 |
| fresh-checkouts | ● 1/1 | ● 1/1 | $2.039 → $1.993 |
| judge-truncated-notice | ● 1/1 | ● 1/1 | $0.696 → $0.575 |
| run-survives-erase | ● 1/1 | ● 1/1 | $0.981 → $1.408 |
| scrub-whole-paths | ● 1/1 | ● 1/1 | $0.565 → $0.445 |
| stats-review | ● 1/1 | ● 1/1 | $1.210 → $1.463 |
| temp-files-in-data | ● 1/1 | ● 1/1 | $2.010 → $2.001 |
| unreadable-files | ● 1/1 | ● 1/1 | $1.069 → $1.352 |

## Notes

- Not discriminating for success (every run passed, or every run failed, in both arms): active-config-files, fresh-checkouts, judge-truncated-notice, run-survives-erase, scrub-whole-paths, stats-review, temp-files-in-data, unreadable-files. They stay in for cost.
- Success is exploratory: 0 of 8 task(s) have 3 or more counted runs in both arms, below the floor of 20 tasks (method phase1-v2).
- Cost's verdict rests on tasks with fewer than 3 runs per arm, as method phase1-v2 allows: each task's difference carries the run-to-run noise, and the t-interval across tasks, the wider of the two here, decides. A seeded simulation of 8–12 tasks × 1 run (σ = 0.19, τ = 0.10–0.25, normal and skewed noise) checked it: false differences in at most 6% of experiments without a true difference, and 95% intervals that cover the true effect at least 93% of the time.
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, at 95%, or at 90% for "no loss" and "equivalent", which are one-sided tests at 5%.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
