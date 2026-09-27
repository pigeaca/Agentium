# Phase 0 spike results: real context A/B runs

- Date: 2026-09-27
- Plan: `.agents/plans/archive/2026-09-27-phase0-spike.md`. Code and raw metrics: [`spikes/phase0/`](../../spikes/phase0/README.md). `python3 spike.py report` regenerates every number below from `results/context-ab/runs.jsonl`.
- Question: can a context A/B experiment run reliably and cheaply on real agent runs? How noisy is it compared with the [feasibility study's](2026-09-27-ai-development-lab.md) assumptions?

## Summary

- **Go for Phase 1.**
  - All 60 runs were fair attempts: none failed for infrastructure reasons, and all used the same CLI version, model, permission mode and tool set.
  - The whole experiment cost an estimated **$17.64**, about $0.29 per run. It ran in about 37 minutes, 3 at a time.
  - The pipeline worked end to end: isolation, hidden tests, metrics and paired statistics.
- **Cost is less noisy than assumed. The spread across tasks is not yet known.**
  - The run-to-run spread of log cost is **σ = 0.19**, measured over 48 degrees of freedom; the study assumed 0.35.
  - The spread of the cost effect across tasks, **τ**, comes from only 6 tasks (5 degrees of freedom). Its point estimate is 0.05, but values up to about 0.25 are consistent with the data.
  - So 12 tasks × 3 runs detect a cost change of **about 12–21%**, depending on τ. The study assumed 22%.
- **Success could not be calibrated.** The tasks were too easy: 57 of 60 runs passed. The study's success assumptions therefore stand. Quality comparisons need **discriminating tasks** that pass 20–80% of the time.
- **Answer for Agentium's own context** (six small harness tasks):
  - The full process docs add **3.3k tokens** to every session's first request (+13%).
  - They did **not** measurably change the cost per run: minimal/full = 1.08 [0.96, 1.23].
  - The success difference was inconclusive: −3.3 pp.
- **The clearest effect was on behavior, and hidden tests don't see it.** The minimal `CLAUDE.md` says in one line to run and update the tests.
  - Those agents updated `test_harness.py` in **30 of 30** runs and ran the unit tests in **30 of 30**.
  - With the full docs, which state the same rules abstractly across several files, agents did so in **16 of 30** and **20 of 30** runs.
- **One confound hits only the minimal arm and is disclosed below.** The repository's own `check docs` fails under a minimal `CLAUDE.md`. 11 minimal runs hit that and spent turns proving the failure was pre-existing. With those pairs excluded, the conclusions hold.

## Setup

| Item | Value |
|---|---|
| Agent | Claude Code 2.1.281 (the desktop app's build), headless, `claude-sonnet-5` at `high` effort, `acceptEdits` |
| Arms | `full`: the base commit's `CLAUDE.md`, which imports about 1,100 words of entry docs. `minimal`: a 64-word `CLAUDE.md`/`AGENTS.md` |
| Tasks | Six issue-style changes to `.agents/scripts/harness.py`, each with hidden tests. Every task, in every arm, is validated: it fails on the arm's context commit and passes with its reference patch |
| Design | 6 tasks × 2 arms × 5 runs; the two arms of each task and repeat run back to back in random order; 3 in parallel |
| Success | The hidden tests pass, and the checkout's `test_harness.py` passes as the agent left it. Every run kept at least the base's 27 tests; none removed tests |
| Isolation | See [the recipe](#isolation-recipe-what-it-took) |
| Auth | Login mode: the user's subscription, and the user's own config folder with project settings only. Costs are Claude Code's own `total_cost_usd` estimates |

## Results

Arithmetic means per run. The last column is the paired ratio of geometric means (minimal / full) or the paired difference. The intervals are 95%: a two-stage cluster bootstrap (tasks, then runs; 10,000 draws), and a t-interval on the six per-task differences.

| Metric | full | minimal | minimal vs full: bootstrap / t |
|---|---|---|---|
| Success | 29/30 | 28/30 | Δ −3.3 pp: [−16.7, +6.7] / [−11.9, +5.2]. Inconclusive |
| Estimated cost | $0.287 | $0.301 | 1.08: [0.96, 1.23] / [0.95, 1.24]. Inconclusive |
| Output tokens | 5,934 | 6,910 | 1.19: [1.02, 1.38] / [1.01, 1.40]. Exploratory* |
| Turns | 16.1 | 17.0 | 1.12: [0.93, 1.38] / [0.87, 1.44]. Inconclusive |
| Wall time | 101 s | 111 s | 1.27: [0.88, 1.74] / [0.83, 1.92]. Inconclusive; depends on running 3 in parallel |
| First-request context (probe) | 28,779 tokens | 25,443 tokens | Measured directly: +3.3k for full |

\* Output tokens are one of four secondary metrics, and the interval is not corrected for multiple comparisons. It is a lead, not a finding.

With only six tasks, both kinds of interval are rough. The percentile bootstrap over few clusters tends to run narrow. The two-stage bootstrap counts within-task noise twice and tends to run wide. The t-interval is a cross-check.

Behavior, counted from the transcripts. The flags are defined in `trajectory()` in `spike.py`, and `spike.py reparse` re-derives them into `runs.jsonl`:

| Runs that… | full | minimal |
|---|---|---|
| updated `test_harness.py` | 16/30 | 30/30 |
| ran the unit tests (`unittest`) | 20/30 | 30/30 |
| ran a harness check (`harness.py check …`) | 19/30 | 11/30 |
| saw the repo's own docs check fail on the `CLAUDE.md` import rule | 0/30 | 11/30 |
| used `git stash`, all to show that failure was pre-existing | 0/30 | 11/30 |
| committed their task work | 0/30 | 0/30 |
| Permission denials (total) | 16 | 9 |
| File-tool reads of watched locations: the work directory outside the run's own, the repository, `~/.claude`, `~/.codex` | 0 | 0 |

Per task, success (full / minimal) and mean cost:

| Task | full | minimal |
|---|---|---|
| branch-types | 5/5, $0.24 | 5/5, $0.26 |
| sensitive-files | 5/5, $0.14 | 5/5, $0.18 |
| secret-patterns | 4/5, $0.25 | 4/5, $0.24 |
| markdown-links | 5/5, $0.21 | 4/5, $0.21 |
| review-metric | 5/5, $0.33 | 5/5, $0.39 |
| worktree-list | 5/5, $0.55 | 5/5, $0.52 |

### The minimal-arm confound

The harness's `check docs` requires `CLAUDE.md` to import every entry doc. Under the minimal `CLAUDE.md` it therefore fails before the agent changes anything. No full run could see this. 11 of 30 minimal runs ran the check, saw it fail, and used `git stash` to show the failure was pre-existing. That work adds cost and turns to the minimal arm only.

Excluding those 11 task/repeat pairs:

| Metric (minimal / full) | Excluding the pairs | As reported |
|---|---|---|
| Cost | 1.05 [0.91, 1.21] | 1.08 [0.96, 1.23] |
| Output tokens | 1.23 [1.07, 1.39] | 1.19 [1.02, 1.38] |
| Turns | 1.04 [0.80, 1.36] | 1.12 [0.93, 1.38] |
| Wall time | 1.22 [0.74, 1.89] | 1.27 [0.88, 1.74] |

The conclusions do not change. **For Phase 1:** each arm must pass the repository's own checks, or the check must be declared part of what the arm changes. `validate` now reports the docs check per arm.

## Measured noise and what it means for experiment size

| Parameter | Study assumption | Measured | Confidence |
|---|---|---|---|
| Per-run log-cost spread σ | 0.35 | **0.187** | Good (48 degrees of freedom); for small tasks |
| Spread of the cost effect across tasks τ | 0.10 | 0.048 | Poor (5 degrees of freedom); values up to about 0.25 fit |
| Success variance w | 0.20 | 0.05 | Not transferable: a ceiling artifact at a 95% pass rate. Keep 0.20 |
| Cost per run | $1.31 (Sonnet 5) | **$0.29** | For small tasks of about 100 s |

Smallest detectable cost change (80% power, two-sided 5%), with the measured σ:

| Tasks × runs per arm | Runs | τ = 0.05 (measured) | τ = 0.10 | τ = 0.25 | Study |
|---|---|---|---|---|---|
| 12 × 3 | 72 | 12.1% | 13.7% | 21.1% | 21.7% |
| 20 × 5 | 200 | 7.7% | 9.2% | 15.9% | 14.1% |

At this task size, 72 runs cost about $21. A cost-focused context experiment is cheaper than the study feared, and more tasks will pin down τ. Quality claims still need the study's larger designs, and tasks the agent does not almost always solve.

## Isolation recipe: what it took

Each item was found during the spike and is now handled in `spike.py`:

1. **A fresh `CLAUDE_CONFIG_DIR` cannot use a subscription login.** A run failed with "Not logged in" during setup; that probe predates `probe.json`. Isolated runs need an API key or a `claude setup-token` token. Login mode runs in the user's own config folder instead.
2. **Personal context leaks by default.** Without `--setting-sources project`, a run loaded **15 user-level skills** (from the user's own skills and an installed plugin). With it, only the built-in ones loaded. A canary confirmed the project `CLAUDE.md` still loads.
   - The user-level-instruction canary only works in token mode. There is no user-level `CLAUDE.md` on this machine, so nothing could leak in login mode.
3. **A claude.ai login attaches account connectors.** A Claude Docs connector (create, update and delete tools) appeared. Disable it with `disableClaudeAiConnectors`, `ENABLE_CLAUDEAI_MCP_SERVERS=false` and `--strict-mcp-config`. Four more account-dependent tools appear only in the user's own config folder; they are disallowed. Every run recorded 0 connector tools.
4. **`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` silently forces the `default` permission mode.** The only warning is on stderr. The first smoke run could not edit anything because of it. Every run now records `permissionMode`, and a run in any other mode is discarded as unfair.
5. **The sandbox without an approval surface still denies some legitimate commands**, such as multi-line `python3 -c`, `git stash` and writes outside `$TMPDIR`. There were 25 denials in 60 runs, and the agents worked around them. The MVP should report denials per run and tune the allowlists or use `auto` mode.
6. **Hidden tests must be unreachable.**
   - Runs clone a bare repository holding only the base commit, with no remote.
   - The Read tool and the sandboxed shell were denied the spike sources and other runs' records.
   - Gaps found in review:
     - hidden tests were copied into a finished run's checkout while sibling runs were still going;
     - the work directory sat in a scratch folder next to the reference patches;
     - the user's Claude session transcripts were readable in login mode.
   - No transcript shows a run reading any of these: 0 file-tool reads of watched locations, and a manual review of the Bash commands.
   - The code now runs hidden tests in a copy under a denied path, denies `~/.claude/projects` in login mode, and refuses a work directory inside a hidden path.
   - Still readable: sibling runs' in-progress checkouts under `work/runs`.
7. **Prompt caching skews naive cost comparisons.** In the first probe, the arm that ran second cost about a third as much because it reused the cached shared prompt. Interleaving the arms is necessary.
8. **Claude Code 2.1.277+ reads `AGENTS.md` itself when no `CLAUDE.md` exists.** Both arms here have a `CLAUDE.md`, so each loads only what its `CLAUDE.md` imports. The study has been corrected.

## Codex check

One run of codex-cli 0.158 (the model was the user's default, `gpt-6-astra`), with the normal login and no isolation:
- **The JSON stream parsed.** Usage was 346k input tokens (309k cached), 4.8k output and 1.8k reasoning; 12 commands; 169 s. Codex reports no cost.
- **The task failed.** Codex's `workspace-write` sandbox treats **`.agents/` as read-only**, so it could not edit `.agents/scripts/harness.py`. It wrote a `.patch` file instead.
- **User config leaked.** Without isolation, it loaded the user's `config.toml` (including an MCP server entry) and used a personal skill. The MVP needs an isolated `CODEX_HOME` per run.

## Limitations

- **Small tasks.** Six small tasks in one small Python codebase, so results may not carry over to larger tasks or other stacks.
- **Too easy for success.** Near-perfect pass rates, so nothing here says whether the full docs change correctness on harder work.
- **The minimal-arm confound.** It is disclosed and tested above.
- **Success depends on each agent's own test file.** The regression suite is the checkout's `test_harness.py` as the agent left it, and minimal agents edited it more often. No run reduced the test count.
- **Estimated costs.** All costs are Claude Code's list-price estimates for the runs; the subscription was not billed per run.
- **Login mode.** Runs used the user's config folder with project settings only, not a fresh config folder per run.
- **One model and one agent.** Codex was not compared.

## Consequences

- **For the product:**
  - Default experiment templates to cost and behavior goals.
  - Make trajectory graders ("updated tests", "ran the required checks", "did not commit") first-class in the MVP, because they revealed the clearest effect.
  - Require discriminating tasks for success goals.
  - Planner defaults: σ ≈ 0.19 for cost. Treat τ as unknown until more repositories are measured, and plan with τ = 0.10–0.25.
  - Validate every arm against the repository's own checks before running it.
- **For the executor:** record the permission mode, tool list, connector tools, denials and watched-path reads for every run, and reject runs whose environment drifted. Keep the isolation recipe above.
- **For Agentium's own docs, a candidate follow-up for the user to decide:**
  - Make "update and run the tests" concrete in the entry docs.
  - Consider moving the harness out of `.agents/`, because Codex's sandbox cannot edit files there.
