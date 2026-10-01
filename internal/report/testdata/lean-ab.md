# Experiment lean-ab

Context A/B: A = `base`, B = `lean`. Goal: cheaper, without losing success.

- **Success 73% → 76%**, Δ +2 pp (95%: -20 to +23): exploratory: too few tasks or runs for a verdict.
- **Cost -20%** (95%: -21% to -19%): improved.

60 of 60 runs settled (done); spent $21.95 of $60.00. 10 task(s) × 3 run(s) per arm; claude-sonnet-5, effort default, Claude Code 2.1.281, sign-in login. Locked 2026-09-29 12:00 UTC (method phase1-v2).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.

| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 73% | 76% | +2 pp | [-20, +23] pp | [-7, +10] pp | exploratory |
| Cost | primary | $0.391 | $0.315 | -20% | [-21%, -19%] | [-20%, -20%] | improved |
| Time | secondary | 44 s | 44 s | +0% | [+0%, +0%] | [+0%, +0%] | exploratory |
| Output tokens | secondary | 3438 | 3433 | +0% | [+0%, +0%] | [+0%, +0%] | exploratory |

Success: pass@1 73% (A) and 76% (B); every run of a task passed (pass^k) in 30% and 30% of tasks.

## Noise

What the runs show, for planning later experiments: 10 task(s), 3.0 run(s) per task and arm on average; 95% ranges. Cost ranges assume normal noise in log cost; with few tasks every range is wide.

| Component | Estimate | 95% range | Planner's default | How it was estimated |
|---|---|---|---|---|
| σ, per-run spread of log cost | 0.04 | 0.03–0.05 | 0.19: above the range, so plans may overstate this noise | the pooled spread of runs within each task and arm, on 39 degrees of freedom; chi-square range; it assumes normal noise, and heavier tails make it too narrow |
| τ, spread of the cost effect across tasks | 0.00 | 0.00–0.00 | 0.10–0.25: not compared: no spread detected (range truncated at zero) | var(d) − 2σ²/R, floored at zero; the range spans both variances' chi-square ranges at 97.5%, so it holds with at least 95%; it assumes normal noise, and heavier tails make it too narrow |
| w, per-run variance of success | 0.24 | 0.13–0.33 | 0.20: within the range | the pooled variance of runs within each task and arm; range from a bootstrap over tasks (2000 draws), which runs narrow with few tasks |
| τ, spread of the success effect across tasks | 0.00 | 0.00–0.00 | 0.10–0.25: not compared: no spread detected (range truncated at zero) | var(d) − 2w/R, floored at zero; range from a bootstrap over tasks (2000 draws), which runs narrow with few tasks |

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|
| A | `base` | 30 | 30000 | $0.397 | $1.917 | 93% |
| B | `lean` | 29 | 27000 (-3000) | $0.320 | $1.840 | 93% |

## Context use

What the counted runs used of their context beyond what loads at start: path-scoped rules and folder instructions that loaded for the files the agent worked with; context files and linked documents the agent or its subagents read (with the Read tool, or given to cat, sed, grep and the like); the project's skills and commands they invoked; and the subagents they started (the project's and Claude Code's by name, any other only counted). Not recorded or not recoverable for 1 of B's 29 counted runs.

| | A | B |
|---|---|---|
| files loaded at start | 2 | 1 |
| `docs/testing.md` | 15 of 30 | 8 of 28 |
| skill `review-change` | 6 of 30 | 0 of 28 |
| subagent `Explore` | 1 of 30 | 1 of 28 |
| other subagents | 0 of 30 | 3 of 28 |

Loaded at start: A, `AGENTS.md`, `CLAUDE.md`; B, `CLAUDE.md`.

## Behavior

Runs counted in each arm, unless a total.

| | A | B |
|---|---|---|
| changed a test file | 30 | 0 |
| test files removed (total) | 0 | 0 |
| ran tests | 30 | 29 |
| ran the task's checks | 30 | 0 |
| committed | 0 | 0 |
| changed the checks | 0 | 0 |
| passed with changed runner configuration (a failure here) | 1 | 0 |
| permission denials (total) | 0 | 0 |
| files changed (mean) | 2.0 | 2.0 |
| lines changed (mean) | 13.0 | 13.0 |
| shell commands (mean) | 6.0 | 6.0 |

## Per task

● success, ○ failure, × not counted; cost is the mean of counted runs.

| Task | A | B | Cost A → B |
|---|---|---|---|
| task-0 | ●●● 3/3 | ●●● 3/3 | $0.324 → $0.259 |
| task-1 | ●●○ 2/3 | ●●○ 2/3 | $0.405 → $0.324 |
| task-2 | ○○● 1/3 | ●○● 2/3 | $0.486 → $0.389 |
| task-3 | ○●● 2/3 | ○●● 2/3 | $0.324 → $0.259 |
| task-4 | ●●● 3/3 | ●●● 3/3 | $0.405 → $0.324 |
| task-5 | ●●○ 2/3 | ●●○ 2/3 | $0.486 → $0.389 |
| task-6 | ●○● 2/3 | ×○● 1/2 | $0.324 → $0.264 |
| task-7 | ○●● 2/3 | ○●● 2/3 | $0.405 → $0.324 |
| task-8 | ××●●● 3/3 | ●●● 3/3 | $0.486 → $0.389 |
| task-9 | ●●○ 2/3 | ●●○ 2/3 | $0.324 → $0.259 |

## Notes

- Runs not counted: 1 unfair (the environment drifted), 1 infrastructure failure, 1 cancelled. Their spend is in the total.
- Environment drift in unfair runs: tools differ (added Monitor; missing none); 1 file tool call(s) reached <agentium data>/projects.
- 1 run(s) ended without Claude Code's cost: it was estimated from their transcripts at list prices.
- 1 run(s) were cut short when Agentium stopped, and recovered with what they spent.
- Arm A: 1 run(s) passed with test-runner configuration changed beyond the task's reference; they count as failures.
- Not discriminating for success (every run passed, or every run failed, in both arms): task-0, task-4, task-8. They stay in for cost.
- Success is exploratory: 9 of 10 task(s) have 3 or more counted runs in both arms, below the floor of 20 tasks (method phase1-v2).
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, at 95%, or at 90% for "no loss" and "equivalent", which are one-sided tests at 5%.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
