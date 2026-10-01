# The next chapter: fast, cheap verdicts

- Date: 2026-10-01
- Status: In Progress (2026-10-01): wave 1 started. The user approved this plan (the [roadmap page](https://claude.ai/artifact/NfBAZ7iHuMTahxazQMR92q)), and with it these paid checks:
  - one temp-folder probe session (cents);
  - the judge's small real check (a few dollars);
  - the Java and Rust pilot, which gets its estimate first.
- Scope: the coordinating plan for the roadmap. Items get their own plan file only when they start.

## Vision
- **The line:** know what your AI setup really changes, in an afternoon, for a few dollars.
- **Where we are:** the engine (isolation, hidden tests, paired statistics, honest verdicts) works and stays.
- **What's next:** make a verdict fast and cheap to get, then put it where developers work.

## North-star metrics
Every item says which one it moves.
- **Time to first verdict:** commands and minutes from `init` on a fresh clone to the first report with a verdict that is not exploratory.
  - Today: about 14 commands, plus importing, reviewing and validating 8–20 tasks by hand.
  - Target: 3 commands, under an hour.
- **Dollars per verdict:** spend up to that verdict.
  - Today: a success verdict needs about 120 runs, about $120–180 at this repository's $1–1.50 per run.
  - Target: $60 or less.
- **Measured** at each wave's end, by a scripted walkthrough on a public repository, recorded here.

## How the work runs in parallel
- **Limit:** at most two feature tracks and one maintenance track in flight, and at most three agents at once (reviewers included).
- **Each code step:** an implementer in its own worktree, then a reviewer, fixes, and a PR. The coordinator integrates.
- **Light process:**
  - a re-review only when fixes change behavior;
  - docs-only PRs need no review;
  - one-PR changes use an inline plan.
- **Package ownership per wave** avoids merge conflicts. When two items touch the same files, the later one starts after the earlier one merges.

## Waves
| Wave | Track | Item | Plan | Touches | Starts after | Moves |
|---|---|---|---|---|---|---|
| 0 | — | Judge step 2 (experiments); ticket tasks step 1 | [judge](2026-10-01-llm-judge.md), [tickets](2026-10-01-ticket-tasks.md) | cli, run, experiment, task, store | — (in review) | trust |
| 1 | Maintenance | Temp-folder isolation | [plan](2026-10-01-run-temp-isolation.md) | claude, run | now | trust |
| 1 | Speed | `task mine` and `task validate --all` | [plan](2026-10-01-task-mine.md) | new `internal/mine`, task, cli/task.go | core now; CLI after the tickets merge | time ↓↓ |
| 1 | Maintenance | Refactor round, steps 1–2: one spending record; command handlers into services | [plan](2026-10-01-refactor-round.md) | run, experiment, cli, report | judge step 2 merges | speed of change |
| 1 | Reach | Java and Rust step 3: Maven, Gradle and Cargo profiles | [plan](2026-09-30-java-rust.md) | buildtool, claude, run | temp isolation merges | reach |
| 2 | Maintenance | Refactor steps 3–4 (merge helpers, faster tests); judge step 3 (reports), then its real check | refactor, judge | claude, run, report, cli tests | Java/Rust step 3 | trust |
| 2 | Speed | Quick start: `agentium start`, calibration inside `experiment run` | its own plan when it starts | cli, experiment | `task mine`, refactor step 2 | time ↓ |
| 2 | Cost | Cheaper verdicts: run reuse, then early stopping | its own plan when it starts | experiment, stats, store | refactor steps 1–2 | $ ↓ |
| 2 | Reach | Java and Rust pilot (paid; estimate first) | [plan](2026-09-30-java-rust.md) | — | step 3 | reach |
| 2 | Automation | A1 headless foundation (`--json`, exit codes, `start --yes`, `agentium.toml`); A2 supply loop (`pool update`); A3 Linux runs spike (paid; approval) | [automation](2026-10-01-automation.md) | cli, new | quick start, task mining, temp isolation | time ↓ |
| 3 | Automation | A4 fast check on pull requests (`ci check`, policy exit codes, comment, GitHub Action); A5 deep watch (`watch`, drift, history, digest) | [automation](2026-10-01-automation.md) | cli, report, experiment, new | A1, A2, cheaper verdicts | reach ↑↑ |
| 3 | Reach | Claude Code skill `/agentium compare` | its own plan when it starts | new | A1 | reach |
| 3 | Trust | Judge: check its claims by execution; then pairs (1b) and ticket grading (step 2) | [pairs](2026-10-01-judge-pairs.md), [tickets](2026-10-01-ticket-tasks.md) | judge, experiment | judge step 3 | trust |
| 3 | Reach | Model and effort A/B | [plan](2026-10-01-model-ab.md) | experiment, run | refactor step 2 | reach |
| Later | — | Autopilot (A6: propose, test and open pull requests with context changes); Codex (deferred by the user); containers (Harbor); live Jira; benchmarks; team sharing | [automation](2026-10-01-automation.md) | — | — | — |

## Assignments (wave 1)
- **Temp-folder isolation:** the coordinator investigates (read-only), then an implementer fixes it in `claude/fix/run-temp-isolation`, and one probe session checks it.
- **`task mine` core:** an implementer, in `claude/feat/task-mine` from main `6aaa1e6`. The core first, in a new `internal/mine` package that touches no CLI files; the CLI follows once the ticket tasks merge.
- **Refactor step 1:** an implementer, once judge step 2 merges.
- **Java and Rust step 3:** an implementer, once temp isolation merges.

## Acceptance
1. Each wave's items meet their own plans' acceptance.
2. The north-star figures are measured and recorded here at the end of waves 1 and 2.
3. The roadmap and this table stay current; finished plans are archived.

## Verification
Per item, as in its plan. Per wave: the scripted walkthrough for the north-star metrics.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
