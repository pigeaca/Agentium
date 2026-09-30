> A real report from Phase 1's acceptance runs, printed by `agentium experiment report aa2` at commit `fb02e4d` and not edited. Both arms use the same context, so a difference here is noise. The experiment was stopped after 12 of 36 runs to fit one usage window, so it is exploratory. See the [README](../../README.md#example-results).

---

# Experiment aa2

A/A calibration of context `base` (both arms).

- **Success 100% → 100%**, Δ +0 pp (95%: +0 to +0): exploratory: too few tasks or runs for a verdict.
- **Cost -6%** (95%: -31% to +26%): exploratory: too few tasks or runs for a verdict.

12 of 36 runs settled (stopped); spent $12.88 of $43.37. 6 task(s) × 3 run(s) per arm; claude-sonnet-5, effort default, Claude Code 2.1.281, sign-in login. Locked 2026-09-29 11:28 UTC (method phase1-v1).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.

| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 100% | 100% | +0 pp | [+0, +0] pp | [+0, +0] pp | exploratory |
| Cost | primary | $0.948 | $0.886 | -6% | [-24%, +16%] | [-31%, +26%] | exploratory |
| Time | secondary | 267 s | 260 s | -3% | [-29%, +34%] | [-39%, +54%] | exploratory |
| Output tokens | secondary | 20378 | 20992 | +3% | [-21%, +31%] | [-28%, +48%] | exploratory |

Success: pass@1 100% (A) and 100% (B); every run of a task passed (pass^k) in 100% and 100% of tasks.

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|
| A | `base` | 6 | 30206 | $1.014 | $10.791 | 97% |
| B | `base` | 6 | 30195 (-11) | $1.022 | $9.996 | 97% |

## Behavior

Runs counted in each arm, unless a total.

| | A | B |
|---|---|---|
| changed a test file | 6 | 6 |
| test files removed (total) | 0 | 0 |
| ran tests | 6 | 6 |
| ran the task's checks | 0 | 0 |
| committed | 0 | 0 |
| changed the checks | 0 | 0 |
| passed with changed runner configuration (a failure here) | 0 | 0 |
| permission denials (total) | 10 | 12 |
| files changed (mean) | 3.8 | 3.0 |
| lines changed (mean) | 92.3 | 93.8 |
| shell commands (mean) | 26.7 | 22.0 |

## Per task

● success, ○ failure, × not counted; cost is the mean of counted runs.

| Task | A | B | Cost A → B |
|---|---|---|---|
| deny-login-file | ● 1/1 | ● 1/1 | $0.510 → $0.384 |
| documents-filter | ● 1/1 | ●× 1/1 | $1.297 → $1.121 |
| judge-truncated-notice | ● 1/1 | ● 1/1 | $0.691 → $0.703 |
| run-survives-erase | ● 1/1 | ● 1/1 | $1.526 → $1.688 |
| scrub-whole-paths | ● 1/1 | ● 1/1 | $0.896 → $0.572 |
| unreadable-files | ● 1/1 | ● 1/1 | $1.164 → $1.662 |

## Notes

- The experiment is not finished (stopped: interrupted): 12 of 36 runs settled, and the results cover those.
- Runs not counted: 1 cancelled. Their spend is in the total.
- Not discriminating for success (every run passed, or every run failed, in both arms): deny-login-file, documents-filter, judge-truncated-notice, run-survives-erase, scrub-whole-paths, unreadable-files. They stay in for cost.
- Success is exploratory: 0 of 6 task(s) have 3 counted runs in both arms, below the floor of 20.
- Cost is exploratory: 0 of 6 task(s) have 3 counted runs in both arms, below the floor of 8.
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, at 95%, or at 90% for "no loss" and "equivalent", which are one-sided tests at 5%.
- Both arms use the same context, so any difference is noise. At the 5% level, about one verdict in twenty shows a difference by chance.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
