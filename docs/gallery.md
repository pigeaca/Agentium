# Gallery

Real console output from Agentium. Back to the [README](../README.md); commands are in the [guide](guide.md).

This is real output, printed by today's `agentium` (built from `main` at b6df097 on 2026-10-02) in a 120-column terminal; the pictures were redone from stored data on that date. Paths into the home folder are shortened to `~`, and `…` marks lines left out. Where the data comes from:
- **`start`, `experiment plan` and `run show`** (redone on 2026-10-04 for the designed screens): a small fixture repository (a shell library with 16 commits, each adding a function and its test) and the test stand-in for Claude Code, so no run was paid for; the plan and the run are of a seq-v1 experiment on its 16 mined tasks, `lean-vs-base`, with the sandbox grader. The path of the stand-in and of the temporary folders are written as `~/…`.
- **`pool update --dry-run`:** a fresh clone of [Masterminds/semver](https://github.com/Masterminds/semver), free runs that stop before any agent run (Claude Code 2.1.285). The candidates picture was redone on 2026-10-03 with the build that made `pool update` the way to mine, on a new data folder: the pool reads the last 270 days, so older commits are counted as outside it.
- **The `ab` experiment** (the report): Phase 1's acceptance runs on 2026-09-29, Claude Code 2.1.281 with claude-sonnet-5, on tasks taken from this repository. It compared today's docs (`full`) with a minimal version (`minimal`) and was stopped after 4 complete pairs, so every verdict is "exploratory": the report says the data is too thin instead of naming a winner.
- **The run screens:** recorded on 2026-10-03 from a real `experiment run` with the stand-in agent, no paid runs.
- **The Judge section:** the `judge-check` experiment, 2026-10-01 (Claude Code 2.1.285).
- **The decisive verdict:** the model A/B on samber/lo, 2026-10-02 (Claude Code 2.1.285).

**1. Start it:** one command from a repository folder registers it, saves its context, mines and validates tasks, creates an experiment and prints its preview. `--accept-mined` accepts the instructions it mined without your review (see `agentium start -h`); it stops before any run, which would cost money.

<img src="images/console-start.svg" alt="agentium start --accept-mined on a small library: registered, snapshot saved, tasks mined and validated, an A/A experiment created, and its preview as bars: what it may spend against the budget">

**2. Look at the candidates:** `pool update --dry-run` ranks tasks from the git history, explains each score and lists what it set aside, without importing anything.

<img src="images/console-mine.svg" alt="agentium pool update --dry-run: ranked commits with scores and their reasons, then the commits set aside and why, and the pool's health">

**3. Plan it:** what is missing, what it may spend as bars against the budget, and when it checks the answer, before anything runs (`--details` for the sizes and the detectable effects).

<img src="images/console-plan.svg" alt="agentium experiment plan: the question in plain words, a box of checks, bars for the likely spend, the most and the budget, and a bar for each check of the answer">

**4. Run it:** the live screen. Each version's current task moves through fresh copy, Claude works and hidden tests (both in a sandbox) to the result, and the answer updates as results come in. Recorded from a real `experiment run` with a stand-in agent (Agentium's test double for Claude Code), at its real pace:

<img src="images/console-run-dashboard.svg" alt="agentium experiment run: for each version, the current task moving through the steps with a dot travelling between them; the answer so far in a green box; the last four results below">

With `--view log` (or `AGENTIUM_VIEW=log`) it is a plain, append-only log instead: a line per run and a small box at each check of the answer.

<img src="images/console-run-log.svg" alt="agentium experiment run --view log: a coloured line per run and a box each time the answer is checked">

**5. Report it:** verdicts in words, then the metrics with both intervals, noise, context and cost per arm, what the runs used of their context (files read on demand, project skills, subagents), behavior (tests run, files changed, denials), per-task results and notes. On a terminal it looks like this; piped, with `--out FILE` or with `--markdown`, it is Markdown you can paste into a pull request, like the [full report](examples/context-ab-report.md) (`--json` for everything).

<img src="images/console-report.svg" alt="agentium experiment report ab on a terminal: exploratory verdicts in yellow, the metrics table, and per-task results">

**6. Read the judge:** with `--judge`, the report adds a Judge section. In `judge-check` it called one passing run "partly" fixed, with its reason, for you to check by hand; the tests still decide.

<img src="images/console-judge.svg" alt="The Judge section of agentium experiment report judge-check: verdicts among passing and failing runs, agreement, cost, and the passing run the judge did not call fixed">

**7. Look at one run:** the run as a chain: a fresh copy, Claude works and the hidden tests (both in a sandbox, dashed purple), the result (`--details` prints every line; `--diff` and `--log` show the agent's changes and the grading output).

<img src="images/console-run-show.svg" alt="agentium run show: four boxes joined by dotted lines, fresh copy, Claude works and hidden tests in dashed purple sandbox outlines, and a green result">

In the `ab` experiment the minimal docs cut Claude Code's first request by about 4.6k tokens, but cost did not fall, and one task failed. With four pairs the intervals are far too wide to call either a result, and the report says so in words.

The [A/A report](examples/aa-report.md) is the sanity check: the same context in both arms. It reported no difference (cost −6%, 95%: −31% to +26%), and every run passed.

A larger A/B, 8 tasks × 1 run per arm ([report](examples/context-ab-16-report.md)), found the same: every run passed in both arms, and cost was inconclusive (+5%, 95%: −11% to +25%), with about 55 tasks needed to settle it.

The first decisive verdict came from a model A/B on [samber/lo](https://github.com/samber/lo), 8 mined tasks × 1 run per arm ([report](examples/model-ab-report.md)). Sonnet 5.5 cost 63% less than Opus 5.5 per run (95%: −70% to −53%), and the whole experiment cost $3.10. Success (75% against 50%) was exploratory at that size. The report's headline and first table, with the plain verdict "improved" and the time and spend up to it:

<img src="images/console-verdict.svg" alt="agentium experiment report opus-vs-sonnet: Sonnet costs 63% less, verdict improved in green, success exploratory, and the first decisive verdict line">
