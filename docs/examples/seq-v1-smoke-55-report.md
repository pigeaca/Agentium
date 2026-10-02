> A real report from the wave-3 `seq-v1` smoke check rerun on [samber/lo](https://github.com/samber/lo) (MIT), printed by `agentium experiment report seq-v1-smoke-55 --markdown` at commit `80c7f9b` and not edited. It is an A/A calibration (both arms run the same context), so a difference here is noise. It ran on `claude-sonnet-5-5` with a hard $5 budget (calibration included) and runs capped at $0.30. Look 1 (8 tasks) continued; look 2 (12 tasks) stopped for futility. The first attempt, on `claude-sonnet-5`, stopped at its budget before look 1 ([report](seq-v1-smoke-report.md)). See the [gallery](../gallery.md).

---
# Experiment seq-v1-smoke-55

A/A calibration of context `baseline` (both arms).

- **Success 92% → 92%**, Δ +0 pp (95%: +0 to +0): exploratory: too few tasks or runs for a verdict.
- **Cost +4%** (98.84%: -16% to +28%): inconclusive.

24 of 32 runs settled (done); spent $2.25 of $5.00 (calibration $0.08 of it). 16 task(s) × 1 run(s) per arm; claude-sonnet-5-5, effort default, Claude Code 2.1.285, sign-in login. Locked 2026-10-02 15:30 UTC (method seq-v1).

Method seq-v1: stopped at look 2 of 3 (12 tasks) for futility: cost is unlikely to reach a verdict by the last look (conditional power 0%).

First decisive verdict: none yet ($4.87 spent since init).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others. Intervals are at 95%, cost's at 98.84%, its look's level.

| Metric | Role | A | B | B vs A | Bootstrap | t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 92% | 92% | +0 pp | [+0, +0] pp | [+0, +0] pp | exploratory |
| Cost | primary | $0.085 | $0.088 | +4% | [-12%, +23%] | [-16%, +28%] | inconclusive |
| Time | secondary | 26 s | 27 s | +2% | [-15%, +22%] | [-17%, +27%] | exploratory |
| Output tokens | secondary | 1903 | 2027 | +7% | [-13%, +28%] | [-15%, +34%] | exploratory |

Success: pass@1 92% (A) and 92% (B); every run of a task passed (pass^k) in 92% and 92% of tasks.

## Looks

Method seq-v1 looks once each stage's runs are settled, at exactly the tasks of the stages up to it, and stops at the first look whose cost verdict is decisive; an interim look without a verdict stops for futility when the chance of one by the last look (conditional power) is below 10%. Each look's cost interval is at its own level, from O'Brien–Fleming-type spending of a two-sided 3.5% over all looks: the wider of the bootstrap and the t-interval on each side.

| Look | Tasks counted | Cost B vs A | Interval | Level | Verdict | Conditional power | Decision |
|---|---|---|---|---|---|---|---|
| 1 of 3 | 8 of 8 | +11% | [-28%, +71%] | 99.84% | inconclusive | 27% | continue |
| 2 of 3 | 12 of 12 | +4% | [-16%, +28%] | 98.84% | inconclusive | 0% | futility |

## Noise

What the runs show, for planning later experiments: 12 task(s), 1.0 run(s) per task and arm on average; 95% ranges. Both arms use the same context, so this is the noise itself; compare it with the planner's defaults, which size every preview until a calibration replaces them. Cost ranges assume normal noise in log cost; with few tasks every range is wide.

| Component | Estimate | 95% range | Planner's default | How it was estimated |
|---|---|---|---|---|
| σ, per-run spread of log cost | 0.17 | 0.12–0.29 | 0.19: within the range | the paired differences' spread over √2: with one run per arm var(d) = 2σ² + τ², and τ = 0 in an A/A; chi-square range on 11 degrees of freedom; it assumes normal noise, and heavier tails make it too narrow |
| w, per-run variance of success | 0.00 | 0.00–0.00 | 0.20: not compared: no spread detected (range truncated at zero) | half the paired differences' variance, as τ = 0 in an A/A; range from a bootstrap over tasks (2000 draws), which runs narrow with few tasks |

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Isolated-run cost | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|---|
| A | `baseline` | 12 | 15359 | $0.088 | $0.118 | $0.462 | 89% |
| B | `baseline` | 12 | 15355 (-4) | $0.093 | $0.123 | $0.465 | 89% |

## Context use

What the counted runs used of their context beyond what loads at start: path-scoped rules and folder instructions that loaded for the files the agent worked with; context files and linked documents the agent or its subagents read (with the Read tool, or given to cat, sed, grep and the like); the project's skills and commands they invoked; and the subagents they started (the project's and Claude Code's by name, any other only counted).

| | A | B |
|---|---|---|
| files loaded at start | 0 | 0 |
| `docs/CLAUDE.md` | 1 of 12 | 1 of 12 |

Loaded at start: A, none; B, none.

## Behavior

Runs counted in each arm, unless a total.

| | A | B |
|---|---|---|
| changed a test file | 11 | 12 |
| test files removed (total) | 0 | 0 |
| ran tests | 12 | 12 |
| ran the task's checks | 1 | 0 |
| committed | 0 | 0 |
| changed the checks | 0 | 0 |
| passed with changed runner configuration (a failure here) | 0 | 0 |
| permission denials (total) | 4 | 4 |
| files changed (mean) | 2.7 | 2.8 |
| lines changed (mean) | 54.2 | 59.2 |
| shell commands (mean) | 4.7 | 4.8 |

## Per task

● success, ○ failure, × not counted; cost is the mean of counted runs.

| Task | A | B | Cost A → B |
|---|---|---|---|
| add-foreachcondition-implement-485-cbfd1c6 | ● 1/1 | ● 1/1 | $0.071 → $0.075 |
| added-cut-cutprefix-cutsuffix-666-21a523d | - 0/0 | - 0/0 | - → - |
| adding-mean-and-meanby-414-97074ee | ● 1/1 | ● 1/1 | $0.086 → $0.063 |
| feat-add-nthor-and-nthorempty-functions-8755356 | - 0/0 | - 0/0 | - → - |
| feat-add-unionby-and-unionbyerr-878-8c82fb8 | ● 1/1 | ● 1/1 | $0.078 → $0.123 |
| feat-adding-filtervalues-and-fix-579fdad | ● 1/1 | ● 1/1 | $0.067 → $0.067 |
| feat-adding-lo-bufferwithcontext-580-bb32fc7 | ● 1/1 | ● 1/1 | $0.100 → $0.114 |
| feat-support-for-buffer-iterator-824-56ef3be | - 0/0 | - 0/0 | - → - |
| feature-intersect-by-653-43ae3d7 | ○ 0/1 | ○ 0/1 | $0.114 → $0.100 |
| fix-it-mode-align-behavior-with-lo-mode-02d371c | ● 1/1 | ● 1/1 | $0.087 → $0.061 |
| fix-iter-tuples-support-break-iteration-27e2842 | ● 1/1 | ● 1/1 | $0.078 → $0.117 |
| fix-nth-reject-indexes-that-do-not-fit-795f6b9 | - 0/0 | - 0/0 | - → - |
| fix-rename-issortedbykey-to-issortedby-48d8fe4 | ● 1/1 | ● 1/1 | $0.053 → $0.058 |
| perf-intersect-scan-small-exclude-lists-ff25ada | ● 1/1 | ● 1/1 | $0.145 → $0.170 |
| perf-intersect-scan-the-small-subset-42c8e5d | ● 1/1 | ● 1/1 | $0.081 → $0.077 |
| refactor-improve-samplesby-performance-73a8fc6 | ● 1/1 | ● 1/1 | $0.094 → $0.094 |

## Notes

- The results are look 2's: the runs of the tasks up to it (12 of them counted), with cost's intervals at 98.84% (and 95.62% for "equivalent") and the other metrics' at 95% (90%).
- A seeded simulation of method seq-v1 (up to 16 tasks × 1 run, the same looks and levels; normal, skewed, heavy-tailed, arm-specific and recorded noise) gave false differences in at most 5% of experiments per group of scenarios without a true difference (the wave-3 statistics note, §7). One extreme scenario, both arms strongly and oppositely skewed, gave 6.5%.
- Not discriminating for success (every run passed, or every run failed, in both arms): add-foreachcondition-implement-485-cbfd1c6, adding-mean-and-meanby-414-97074ee, feat-add-unionby-and-unionbyerr-878-8c82fb8, feat-adding-filtervalues-and-fix-579fdad, feat-adding-lo-bufferwithcontext-580-bb32fc7, feature-intersect-by-653-43ae3d7, fix-it-mode-align-behavior-with-lo-mode-02d371c, fix-iter-tuples-support-break-iteration-27e2842, fix-rename-issortedbykey-to-issortedby-48d8fe4, perf-intersect-scan-small-exclude-lists-ff25ada, perf-intersect-scan-the-small-subset-42c8e5d, refactor-improve-samplesby-performance-73a8fc6. They stay in for cost.
- Success is exploratory: 0 of 12 task(s) have 3 or more counted runs in both arms, below the floor of 20 tasks (method seq-v1).
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, cost's at its look's levels ("equivalent" is two one-sided tests at the look's equivalence level), the others' at 95%, or at 90% for "no loss".
- Both arms use the same context, so any difference is noise. Method seq-v1 spends 3.5% over all its looks: about one experiment in thirty shows a difference by chance.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
- Isolated-run cost is each run's cost had no other run warmed the prompt cache: the cache reads of the main session's first request and of each subagent launch that could not have read this run's own cache (a type's first, a parallel one, or one after its prefix expired) are repriced as cache writes, at the time to live the run wrote with, at Agentium's list prices of 2026-09-29. Unlike cold-cache cost, which reprices every cached read (the run's own included) as a bound, it keeps a run's reads of its own cache. It is at most the cold-cache cost, except when a subagent runs on a pricier model than the session or a run ended without Claude Code's result. Verdicts use the actual cost.
