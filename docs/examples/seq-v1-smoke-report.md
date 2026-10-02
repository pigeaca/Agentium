> A real report from the wave-3 `seq-v1` smoke check on [samber/lo](https://github.com/samber/lo) (MIT), printed by `agentium experiment report seq-v1-smoke --markdown` at commit `4815470` and not edited. It is an A/A calibration (both arms run the same context), so a difference here is noise. The experiment had a hard $3.50 budget, calibration included, with each run capped at $0.50 so that one pair fits. Runs on `claude-sonnet-5` cost about $0.40 each, so the budget stopped it after 6 of 32 runs, before look 1 (8 tasks). Every result below is exploratory. See the [gallery](../gallery.md).

---
# Experiment seq-v1-smoke

A/A calibration of context `baseline` (both arms).

- **Success 100% → 100%**, Δ +0 pp (95%: +0 to +0): exploratory: too few tasks or runs for a verdict.
- **Cost +12%** (99.84%: -98% to +5526%): exploratory: no look yet: the first comes once its stage is settled.

6 of 32 runs settled (budget); spent $2.62 of $3.50 (calibration $0.12 of it). 16 task(s) × 1 run(s) per arm; claude-sonnet-5, effort default, Claude Code 2.1.285, sign-in login. Locked 2026-10-02 10:54 UTC (method seq-v1).

Method seq-v1: no look yet: look 1 of 3 comes once the first 8 tasks are settled.

First decisive verdict: none yet ($2.62 spent since init).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others. Intervals are at 95%, cost's at 99.84%, its look's level.

| Metric | Role | A | B | B vs A | Bootstrap | t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 100% | 100% | +0 pp | [+0, +0] pp | [+0, +0] pp | exploratory |
| Cost | primary | $0.387 | $0.432 | +12% | [-12%, +49%] | [-98%, +5526%] | exploratory (no look yet: the first comes once its stage is settled) |
| Time | secondary | 142 s | 154 s | +8% | [-43%, +130%] | [-81%, +528%] | exploratory |
| Output tokens | secondary | 8607 | 8962 | +4% | [-17%, +36%] | [-44%, +93%] | exploratory |

Success: pass@1 100% (A) and 100% (B); every run of a task passed (pass^k) in 100% and 100% of tasks.

## Noise

What the runs show, for planning later experiments: 3 task(s), 1.0 run(s) per task and arm on average; 95% ranges. Both arms use the same context, so this is the noise itself; compare it with the planner's defaults, which size every preview until a calibration replaces them. Cost ranges assume normal noise in log cost; with few tasks every range is wide.

| Component | Estimate | 95% range | Planner's default | How it was estimated |
|---|---|---|---|---|
| σ, per-run spread of log cost | 0.19 | 0.10–1.19 | 0.19: within the range | the paired differences' spread over √2: with one run per arm var(d) = 2σ² + τ², and τ = 0 in an A/A; chi-square range on 2 degrees of freedom; it assumes normal noise, and heavier tails make it too narrow |

Success did not vary (every counted run passed, or every one failed), so there is no w.

## Context and cost per arm

Means over counted runs. The first request is what Claude Code sent first: the context overhead.

| Arm | Context | Runs counted | First request (tokens) | Cost per run | Isolated-run cost | Cold-cache cost | Cache-read share |
|---|---|---|---|---|---|---|---|
| A | `baseline` | 3 | 25055 | $0.395 | $0.437 | $3.269 | 95% |
| B | `baseline` | 3 | 25029 (-26) | $0.436 | $0.479 | $3.790 | 95% |

## Context use

What the counted runs used of their context beyond what loads at start: path-scoped rules and folder instructions that loaded for the files the agent worked with; context files and linked documents the agent or its subagents read (with the Read tool, or given to cat, sed, grep and the like); the project's skills and commands they invoked; and the subagents they started (the project's and Claude Code's by name, any other only counted).

| | A | B |
|---|---|---|
| files loaded at start | 0 | 0 |
| `docs/CLAUDE.md` | 1 of 3 | 1 of 3 |

Loaded at start: A, none; B, none.

## Behavior

Runs counted in each arm, unless a total.

| | A | B |
|---|---|---|
| changed a test file | 2 | 3 |
| test files removed (total) | 0 | 0 |
| ran tests | 3 | 3 |
| ran the task's checks | 0 | 0 |
| committed | 0 | 0 |
| changed the checks | 0 | 0 |
| passed with changed runner configuration (a failure here) | 0 | 0 |
| permission denials (total) | 2 | 1 |
| files changed (mean) | 3.0 | 3.3 |
| lines changed (mean) | 110.7 | 124.3 |
| shell commands (mean) | 13.0 | 12.0 |

## Per task

● success, ○ failure, × not counted; cost is the mean of counted runs.

| Task | A | B | Cost A → B |
|---|---|---|---|
| add-foreachcondition-implement-485-cbfd1c6 | - 0/0 | - 0/0 | - → - |
| added-cut-cutprefix-cutsuffix-666-21a523d | - 0/0 | - 0/0 | - → - |
| adding-mean-and-meanby-414-97074ee | - 0/0 | - 0/0 | - → - |
| feat-add-nthor-and-nthorempty-functions-8755356 | - 0/0 | - 0/0 | - → - |
| feat-add-unionby-and-unionbyerr-878-8c82fb8 | ● 1/1 | ● 1/1 | $0.474 → $0.507 |
| feat-adding-filtervalues-and-fix-579fdad | - 0/0 | - 0/0 | - → - |
| feat-adding-lo-bufferwithcontext-580-bb32fc7 | - 0/0 | - 0/0 | - → - |
| feat-support-for-buffer-iterator-824-56ef3be | - 0/0 | - 0/0 | - → - |
| feature-intersect-by-653-43ae3d7 | - 0/0 | - 0/0 | - → - |
| fix-it-mode-align-behavior-with-lo-mode-02d371c | - 0/0 | - 0/0 | - → - |
| fix-iter-tuples-support-break-iteration-27e2842 | ● 1/1 | ● 1/1 | $0.292 → $0.435 |
| fix-nth-reject-indexes-that-do-not-fit-795f6b9 | - 0/0 | - 0/0 | - → - |
| fix-rename-issortedbykey-to-issortedby-48d8fe4 | - 0/0 | - 0/0 | - → - |
| perf-intersect-scan-small-exclude-lists-ff25ada | ● 1/1 | ● 1/1 | $0.418 → $0.366 |
| perf-intersect-scan-the-small-subset-42c8e5d | - 0/0 | - 0/0 | - → - |
| refactor-improve-samplesby-performance-73a8fc6 | - 0/0 | - 0/0 | - → - |

## Notes

- The experiment is not finished (budget: the next run would not fit the $3.50 budget ($2.62 spent, $0.50 per run at most)): 6 of 32 runs settled, and the results are its last look's.
- No look was analysed yet, so cost has no verdict: the results cover every run so far, at look 1's levels. No look yet: look 1 of 3 comes once the first 8 tasks are settled.
- A seeded simulation of method seq-v1 (up to 16 tasks × 1 run, the same looks and levels; normal, skewed, heavy-tailed, arm-specific and recorded noise) gave false differences in at most 5% of experiments per group of scenarios without a true difference (the wave-3 statistics note, §7). One extreme scenario, both arms strongly and oppositely skewed, gave 6.5%.
- Not discriminating for success (every run passed, or every run failed, in both arms): feat-add-unionby-and-unionbyerr-878-8c82fb8, fix-iter-tuples-support-break-iteration-27e2842, perf-intersect-scan-small-exclude-lists-ff25ada. They stay in for cost.
- Success is exploratory: 0 of 3 task(s) have 3 or more counted runs in both arms, below the floor of 20 tasks (method seq-v1).
- Verdicts are given for success (guard) and cost (primary); time and output tokens are exploratory. A verdict needs the bootstrap and the t-interval to agree: the intervals in the summary are the wider of the two, cost's at its look's levels ("equivalent" is two one-sided tests at the look's equivalence level), the others' at 95%, or at 90% for "no loss".
- Both arms use the same context, so any difference is noise. Method seq-v1 spends 3.5% over all its looks: about one experiment in thirty shows a difference by chance.
- Cold-cache cost reprices every cached read as a one-hour cache write, at Agentium's list prices of 2026-09-29.
- Isolated-run cost is each run's cost had no other run warmed the prompt cache: the cache reads of the main session's first request and of each subagent launch that could not have read this run's own cache (a type's first, a parallel one, or one after its prefix expired) are repriced as cache writes, at the time to live the run wrote with, at Agentium's list prices of 2026-09-29. Unlike cold-cache cost, which reprices every cached read (the run's own included) as a bound, it keeps a run's reads of its own cache. It is at most the cold-cache cost, except when a subagent runs on a pricier model than the session or a run ended without Claude Code's result. Verdicts use the actual cost.
