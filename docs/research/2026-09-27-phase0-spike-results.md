# Phase 0 spike results: real context A/B runs

- Date: 2026-09-27
- Plan: `.agents/plans/archive/2026-09-27-phase0-spike.md`. Code and raw metrics: [`spikes/phase0/`](../../spikes/phase0/README.md).
- Question: can a context A/B experiment run reliably and cheaply on real agent runs? How noisy is it compared with the [feasibility study's](2026-09-27-ai-development-lab.md) assumptions?

## Summary

- **Go for Phase 1.**
  - All 60 runs were fair attempts: none failed for infrastructure reasons, and all used the same CLI version, model and permission mode.
  - The whole experiment cost an estimated **$17.64**, about $0.29 per run, and took about 45 minutes at 3 runs in parallel.
  - The pipeline worked end to end: isolation, hidden tests, metrics and paired statistics.
- **Cost is less noisy than assumed.** The run-to-run spread of log cost is **σ = 0.19**; the study assumed 0.35. Cost experiments need about a third of the runs the study estimated: 12 tasks × 3 runs now detect about **12%** cost changes, instead of 22%.
- **Success could not be calibrated.** The tasks were too easy: 57 of 60 runs passed. Success variance was 0.05 against the assumed 0.20, but that is a ceiling artifact, so the study's success assumptions stand. Quality comparisons need **discriminating tasks** that pass 20–80% of the time.
- **Answer for Agentium's own context** (six small harness tasks):
  - The full process docs add **3.4k tokens** to every session's first request (28.9k vs 25.6k, +13%).
  - They did **not** raise the cost per run: minimal/full = 1.08 [0.96, 1.23].
  - The success difference was inconclusive: −3.3 pp [−17, +7].
- **The largest effect was on behavior, and hidden tests don't see it.** The minimal `CLAUDE.md` says in one line to run and update the tests, and those agents did so in **30 of 30** runs. With the full docs, agents updated tests in **16 of 30** and ran them in 22 of 30. In 2 full-arm runs the agent tried to commit despite the instruction not to, following the Git rules.

## Setup

| Item | Value |
|---|---|
| Agent | Claude Code 2.1.281 (the desktop app's build), headless, `claude-sonnet-5` at `high` effort |
| Arms | `full`: the base commit's `CLAUDE.md`, which imports about 1,100 words of entry docs. `minimal`: a 64-word `CLAUDE.md`/`AGENTS.md` |
| Tasks | Six issue-style changes to `.agents/scripts/harness.py`, each with hidden tests; all validated to fail on the base and pass with a reference patch |
| Design | 6 tasks × 2 arms × 5 runs; the two arms of each task and repeat run back to back in random order; 3 in parallel |
| Success | Hidden tests pass and the checkout's own harness tests pass |
| Isolation | See [the recipe](#isolation-recipe-what-it-took) |
| Auth | The user's subscription (login mode). Costs are Claude Code's own `total_cost_usd` estimates |

## Results

| Metric (mean per run) | full | minimal | minimal / full, 95% interval |
|---|---|---|---|
| Success | 29/30 (96.7%) | 28/30 (93.3%) | Δ −3.3 pp [−16.7, +6.7]: inconclusive |
| Estimated cost | $0.287 | $0.301 | 1.08 [0.96, 1.23]: inconclusive |
| Output tokens | 5,934 | 6,910 | 1.19 [1.02, 1.38]: exploratory* |
| Turns | 16.1 | 17.0 | 1.12 [0.93, 1.38]: inconclusive |
| Wall time | 101 s | 111 s | 1.27 [0.88, 1.74]: inconclusive |
| First-request context | 28,929 tokens | 25,570 tokens | measured directly: +3.4k for full |
| Runs that updated `test_harness.py` | 16/30 | 30/30 | behavior |
| Runs that ran the unit tests | 22/30 | 30/30 | behavior |
| Runs that ran `check docs`/`ci` | 17/30 | 13/30 | behavior |
| Runs that attempted `git commit` | 2/30 | 0/30 | behavior |

\* Output tokens are one of four secondary metrics, so this interval is not corrected for multiple comparisons. It is a lead, not a finding.

Intervals come from a two-stage cluster bootstrap (tasks, then runs; 10,000 draws) on paired per-task differences. With only six tasks they are rough.

Per task, success (full / minimal) and mean cost:

| Task | full | minimal |
|---|---|---|
| branch-types | 5/5, $0.24 | 5/5, $0.26 |
| sensitive-files | 5/5, $0.14 | 5/5, $0.18 |
| secret-patterns | 4/5, $0.25 | 4/5, $0.24 |
| markdown-links | 5/5, $0.21 | 4/5, $0.21 |
| review-metric | 5/5, $0.33 | 5/5, $0.39 |
| worktree-list | 5/5, $0.55 | 5/5, $0.52 |

## Measured noise and what it means for experiment size

| Parameter | Study assumption | Measured | Note |
|---|---|---|---|
| Per-run log-cost spread σ | 0.35 | **0.187** | Holds for small tasks; larger tasks may vary more |
| Spread of cost effect across tasks τ | 0.10 | **0.048** | |
| Success variance w | 0.20 | 0.05 | A ceiling artifact (95% pass rate), so keep 0.20 for planning |
| Cost per run | $1.31 (Sonnet 5) | **$0.29** | Small tasks, about 100 s each |

Smallest detectable cost change with the measured σ and τ (80% power, two-sided 5%), next to the study's numbers:

| Tasks × runs per arm | Runs | Measured | Study |
|---|---|---|---|
| 12 × 3 | 72 | **12.1%** | 21.7% |
| 20 × 3 | 120 | 9.5% | 17.3% |
| 20 × 5 | 200 | 7.7% | 14.1% |

At this task size, 72 runs cost about $21. A cost-focused context experiment is therefore cheaper than the study feared. Quality claims still need the study's larger designs, and tasks the agent does not almost always solve.

## Isolation recipe: what it took

Each item was found during the spike and is now handled in `spike.py`:

1. **A fresh `CLAUDE_CONFIG_DIR` cannot use a subscription login.** The run fails with "Not logged in". Isolated runs need an API key or a `claude setup-token` token. Login mode runs in the user's own config folder instead.
2. **Personal context leaks by default.** Without `--setting-sources project`, a run loaded **15 user-level skills** (from the user's own skills and an installed plugin). With it, only the built-in skills loaded. A canary test confirmed that the project `CLAUDE.md` still loads.
3. **A claude.ai login attaches account connectors.** A Claude Docs connector (create, update and delete tools) appeared. Disable it with `disableClaudeAiConnectors`, `ENABLE_CLAUDEAI_MCP_SERVERS=false` and `--strict-mcp-config`. Four more account-dependent tools appear only in the user's own config folder; they are disallowed so both auth modes offer the same tools.
4. **`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` silently forces the `default` permission mode.** The only warning is on stderr. The first smoke run could not edit anything because of it. Every run now records `permissionMode` from the init event, and a run in any other mode is discarded as unfair.
5. **The sandbox without an approval surface still denies some legitimate commands**, such as multi-line `python3 -c`, `git stash` and writes outside `$TMPDIR`. There were 25 denials in 60 runs (17 full, 8 minimal). The agents worked around them, but the MVP should report denials per run and tune the allowlists or use `auto` mode.
6. **Hidden tests must be unreachable.** Runs clone a bare repository holding only the base commit, with no remote. The Read tool and the sandboxed shell are denied the spike sources and other runs' records.
7. **Prompt caching skews naive cost comparisons.** The first "Reply OK" probe cost $0.118 and the next one $0.041, because it reused the cached shared prompt. Interleaving the arms is necessary.
8. **Claude Code 2.1.277+ reads `AGENTS.md` itself when no `CLAUDE.md` exists.** Both arms here have a `CLAUDE.md`, so each loads only what its `CLAUDE.md` imports. The study has been corrected.

## Codex check

One run of codex-cli 0.158 (the model was the user's default, `gpt-6-astra`), with the normal login and no isolation:
- **The JSON stream parsed.** Usage was 346k input tokens (309k cached), 4.8k output and 1.8k reasoning; 12 commands; 169 s. Codex reports no cost.
- **The task failed.** Codex's `workspace-write` sandbox treats **`.agents/` as read-only**, so it could not edit `.agents/scripts/harness.py`. It wrote a `.patch` file instead.
- **User config leaked.** Without isolation, it loaded the user's `config.toml` (including an MCP server entry) and used a personal skill. The MVP needs an isolated `CODEX_HOME` per run.

## Limitations

- **Small tasks.** Six small tasks in one small Python codebase, so results may not carry over to larger tasks or other stacks.
- **Too easy for success.** Near-perfect pass rates, so nothing here says whether the full docs change correctness on harder work.
- **Estimated costs.** All costs are Claude Code's list-price estimates for the runs; the subscription was not billed per run.
- **Login mode.** Runs used the user's config folder with project settings only, not a fresh config folder per run.
- **One model and one agent.** Codex was not compared.

## Consequences

- **For the product:**
  - Default experiment templates to cost and behavior goals.
  - Make trajectory graders ("updated tests", "ran the required checks", "did not commit") first-class in the MVP, because they revealed the clearest effect.
  - Require discriminating tasks for success goals.
  - Use σ ≈ 0.19 as the planner's default for cost until more repositories are measured.
- **For the executor:** record the permission mode, tool list, connector tools and denials for every run, and reject runs whose environment drifted. Keep the isolation recipe above.
- **For Agentium's own docs, a candidate follow-up for the user to decide:**
  - Make "update and run the tests" concrete in the entry docs.
  - Consider moving the harness out of `.agents/`, because Codex's sandbox cannot edit files there.
