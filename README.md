<div align="center">

# Agentium

**A local-first lab that measures what really makes AI coding agents better on your code.**

[![CI](https://github.com/pigeaca/Agentium/actions/workflows/ci.yml/badge.svg)](https://github.com/pigeaca/Agentium/actions/workflows/ci.yml) ![Go 1.27.1](https://img.shields.io/badge/Go-1.27.1-00ADD8?logo=go&logoColor=white) ![Status: Phase 1 done](https://img.shields.io/badge/status-Phase%201%20done-orange) [![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

</div>

Agentium runs coding agents such as Claude Code (Codex comes later) on tasks from your own repository. It measures how the agent, the model or your project's AI context (`AGENTS.md`, `CLAUDE.md`, rules, skills) changes correctness, cost and speed, and it reports the results with honest statistics.

- **Your tasks, not a benchmark.** A past commit becomes a task: its parent is the starting point, and its test changes become hidden tests. You can also write tasks by hand.
- **Context versions as experiment arms.** Snapshot `CLAUDE.md`, rules and skills, change them, and compare the versions head to head.
- **Fair, isolated runs.** Every run starts from a fresh checkout, and its commands run in a sandbox without network. It can't see the hidden tests, the solution, other runs or your credentials.
- **Plain verdicts.** Paired runs with repeats give one of four verdicts: improved, regressed, no loss beyond the margin, or inconclusive. Each comes with its intervals and honesty notes.

> [!NOTE]
> Agentium is early. Phase 1, context A/B for Claude Code from the command line, is done; Phase 2 makes the console output clearer and adds Codex and agent comparison; no web UI is planned. See the [roadmap](.agents/ROADMAP.md) and the [feasibility study](docs/research/2026-09-27-ai-development-lab.md).

## Requirements

- Go 1.27.1 and a C compiler (SQLite is built with cgo)
- Git
- [Claude Code](https://claude.com/claude-code), signed in. `ANTHROPIC_API_KEY` or a token file in `AGENTIUM_CLAUDE_TOKEN_FILE` works too.

## Quick start

```sh
go install ./cmd/agentium          # from a clone of this repository; puts agentium in your Go bin folder
agentium init /path/to/your/repo   # registers it; Agentium never writes to your repository
cd /path/to/your/repo
```

**1. Version your context**

```sh
agentium context show                              # what Claude Code loads at start, and on demand
agentium context snapshot baseline                 # save the committed context (HEAD) as a version
# edit CLAUDE.md, rules or skills, then:
agentium context snapshot trimmed --working-tree   # --include-linked adds linked docs
agentium context diff baseline trimmed --patch
```

**2. Turn past commits into tasks**

```sh
agentium task import --commit <sha>                # base: its parent; hidden tests: its test-file changes
agentium task edit <name> --reviewed               # once the instruction doesn't give the solution away (--accept-gaps: hidden tests need texts or names nothing states)
agentium task validate <name> --snapshot trimmed   # tests fail on the base and pass with the reference, in each arm
```

**3. Run and compare**

> [!WARNING]
> These commands start real Claude Code runs. They cost money, or use your plan's limits.

```sh
agentium run calibrate --snapshot trimmed     # short checks: sandbox, large outputs, context size, tools
agentium run once <name> --snapshot trimmed   # one run, graded with the hidden tests
agentium experiment new lean --b trimmed      # an A/B: each task's own context against trimmed, on a sample of valid tasks
agentium experiment plan lean                 # runs, estimated cost, detectable effects; what is missing
agentium experiment run lean                  # locks it, then runs interleaved pairs within the budget; resumable; pauses before your plan's usage limit (--wait waits for the reset)
agentium experiment show lean                 # the lock and the progress per arm
agentium experiment report lean               # verdicts, intervals, per-task results (--json for everything)
```

Data lives in `~/.agentium`; set `AGENTIUM_HOME` to use another folder.

## Example results

This is real output from Phase 1's acceptance runs: Claude Code 2.1.281 with claude-sonnet-5, on tasks taken from this repository. It is copied from the terminal as printed. Paths into the home folder are shortened to `~`, and `…` marks lines left out. The experiment compared today's docs (`full`) with a minimal version (`minimal`). It was stopped after 4 complete pairs to fit one usage window, so every verdict is "exploratory": the report says the data is too thin instead of naming a winner.

**1. Plan it:** what it costs and what it can detect, before anything runs.

```console
$ agentium experiment plan ab
Experiment ab: context A/B, A = full, B = minimal
  arm A: context full (d638b824df11)
  arm B: context minimal (a12708938200)
  model claude-sonnet-5, effort the CLI's default; each run up to $2.00 and 20m0s; 2 at a time
  goal: cheaper, with success as the guard (margins: cost 10%, success 15 pp); budget $24.00
  tasks (6, seed 1919198069636433): deny-login-file, documents-filter, judge-truncated-notice, run-survives-erase, scrub-whole-paths, unreadable-files

Before it runs:
  ok       Claude Code 2.1.281 at ~/Library/Application Support/Claude/claude-code/2.1.281/claude.app/Contents/MacOS/claude
  ok       context full calibrated 2026-09-29 16:22: first request 31087 tokens
  ok       context minimal calibrated 2026-09-29 16:22: first request 26564 tokens
  ok       6 task(s), each valid in every arm's context

Sizes (runs count both arms):
SIZE              TASKS RUNS/ARM  RUNS  EST. COST WORST CASE  COST CHANGE SUCCESS CHANGE NO-LOSS GUARD  EXPLORATORY
Quick                6*        3    36     $25.82     $72.00       19–29%       43–51 pp      38–45 pp  cost, success
Confident            6*        5    60     $43.04    $120.00       16–27%       34–43 pp      30–38 pp  cost, success
This experiment       6        1    12      $8.61     $24.00       28–34%       73–78 pp      65–69 pp  cost, success
…
Floors: verdicts on cost need 8 tasks and on success 20, each with 3 runs per arm; below them a metric is exploratory.
```

**2. Run it:** pairs of runs, interleaved, within the budget. It was stopped here with Ctrl-C, and `experiment run ab` would resume it.

```console
$ agentium experiment run ab
Checking experiment ab before its first run:
  ok       Claude Code 2.1.281 at ~/Library/Application Support/Claude/claude-code/2.1.281/claude.app/Contents/MacOS/claude
  ok       context full calibrated 2026-09-29 16:22: first request 31087 tokens
  ok       context minimal calibrated 2026-09-29 16:22: first request 26564 tokens
  ok       6 task(s), each valid in every arm's context
Locked: Claude Code 2.1.281, claude-sonnet-5, sign-in login, 12 runs in a seeded order (seed 1919198069636433), prices of 2026-09-29.
Running up to 2 at a time; each run up to $2.00 and 20m0s; budget $24.00. Ctrl-C stops it; run it again to resume.
[2/12] scrub-whole-paths, arm A, repeat 1: ok, $0.51 (spent $0.51 of $24.00)
[1/12] scrub-whole-paths, arm B, repeat 1: ok, $0.60 (spent $1.11 of $24.00)
[4/12] deny-login-file, arm A, repeat 1: ok, $0.27 (spent $1.38 of $24.00)
[3/12] deny-login-file, arm B, repeat 1: ok, $0.48 (spent $1.86 of $24.00)
[6/12] documents-filter, arm B, repeat 1: ok, $0.71 (spent $2.57 of $24.00)
[5/12] documents-filter, arm A, repeat 1: ok, $1.13 (spent $3.70 of $24.00)
[7/12] run-survives-erase, arm A, repeat 1: ok, $1.22 (spent $4.92 of $24.00)
[8/12] run-survives-erase, arm B, repeat 1: ok, $1.38 (spent $6.30 of $24.00)
[10/12] unreadable-files, arm A, repeat 1: cancelled, $0.32 (spent $6.61 of $24.00)
[9/12] unreadable-files, arm B, repeat 1: cancelled, $0.71 (spent $7.33 of $24.00)

Experiment ab: stopped: interrupted
  8 of 12 runs settled; spent $7.33 of $24.00
ARM  CONTEXT               SETTLED   FAIR  SUCCESSES  UNFAIR  INFRA  CANCELLED      COST
A    full                      4/6      4          4       0      0          1     $3.45
B    minimal                   4/6      4          3       0      0          1     $3.88
Successes need a pass with the hidden tests; unfair (drifted), infrastructure and cancelled runs are not counted.
Stopped. To continue: agentium experiment run ab
```

**3. Report it:** Markdown you can paste into a pull request (`--json` for everything). The rest of the report covers context and cost per arm, behavior (tests run, files changed, denials), per-task results and notes. See the [full report](docs/examples/context-ab-report.md).

```console
$ agentium experiment report ab
# Experiment ab

Context A/B: A = `full`, B = `minimal`. Goal: cheaper, without losing success.

- **Success 100% → 75%**, Δ -25 pp (95%: -105 to +55): exploratory: too few tasks or runs for a verdict.
- **Cost +11%** (95%: -45% to +123%): exploratory: too few tasks or runs for a verdict.

8 of 12 runs settled (stopped); spent $7.33 of $24.00. 6 task(s) × 1 run(s) per arm; claude-sonnet-5, effort default, Claude Code 2.1.281, sign-in login. Locked 2026-09-29 16:22 UTC (method phase1-v1).

## Metrics

A and B: the success rate, or the geometric mean per run. B vs A is paired by task: a difference for success, a ratio of geometric means for the others.

| Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
|---|---|---|---|---|---|---|---|
| Success | guard | 100% | 75% | -25 pp | [-75, +0] pp | [-105, +55] pp | exploratory |
| Cost | primary | $0.659 | $0.728 | +11% | [-27%, +62%] | [-45%, +123%] | exploratory |
| Time | secondary | 163 s | 179 s | +10% | [-35%, +72%] | [-54%, +166%] | exploratory |
| Output tokens | secondary | 14937 | 19075 | +28% | [-17%, +123%] | [-47%, +211%] | exploratory |

Success: pass@1 100% (A) and 75% (B); every run of a task passed (pass^k) in 100% and 75% of tasks.

## Noise

What the runs show, for planning later experiments: 4 task(s), 1.0 run(s) per task and arm on average; 95% ranges. Cost ranges assume normal noise in log cost; with few tasks every range is wide.

| Component | Estimate | 95% range | Planner's default | How it was estimated |
|---|---|---|---|---|
| σ, per-run spread of log cost | - | - | 0.19 | not separable from τ with one run per arm in an A/B: the paired differences' variance is 2σ² + τ²; the τ below takes the default σ |
| τ, spread of the cost effect across tasks | 0.35 | 0.00–1.63 | 0.10–0.25: overlaps the range | var(d) − 2σ², floored at zero, taking σ = 0.19 (the planner's default): one run per arm cannot separate σ from τ; chi-square range of var(d) on 3 degrees of freedom; it assumes normal noise, and heavier tails make it too narrow |
| w, per-run variance of success | - | - | 0.20 | not separable from τ with one run per arm in an A/B |

…
```

**4. Look at one run:** the minimal-docs run that failed its hidden tests (`--diff` and `--log` show the agent's changes and the grading output).

```console
$ agentium run show 20260929T162923Z-1c89cb
Run 20260929T162923Z-1c89cb: task documents-filter, arm B
  outcome      ok; verification failed
  cost         $0.7061, 20 turn(s), 1m31s, first request 26102 tokens
  changes      5 file(s), +23 -8, 0 commit(s); tests changed: true, test files removed: 0
  behavior     ran tests: true, ran the checks: false, 21 Bash command(s), 0 denial(s)
  environment  Claude Code 2.1.281, claude-sonnet-5, permission mode acceptEdits, 12 tool(s), 17 skill(s)
  records      ~/.agentium-acceptance/records/20260929T162923Z-1c89cb
  experiment   ab, slot 5 (from 0), attempt 1
  files        agent.diff, setup.log, started.json, stderr.txt, stream.jsonl, verify.log
```

The minimal docs cut Claude Code's first request by about 4.6k tokens, but cost did not fall, and one task failed. With four pairs the intervals are far too wide to call either a result, and the report says so in words.

The [A/A report](docs/examples/aa-report.md) is the sanity check: the same context in both arms. It reported no difference (cost −6%, 95%: −31% to +26%), and every run passed.

A larger A/B, 12 tasks × 1 run per arm, is planned in the [hardening plan](.agents/plans/2026-09-29-experiment-hardening.md).

## Development

Agentium is built by AI coding agents (Claude Code and Codex) under shared rules in [`.agents/`](.agents/README.md):

- every task gets a plan with acceptance criteria, and its own worktree;
- a pre-commit guard checks each commit, and an independent reviewer reads each change;
- work lands as a pull request with green CI.

A standard-library Python harness is the single entrypoint:

```sh
python3 scripts/harness.py help
python3 scripts/harness.py hooks                               # once per clone: enable the pre-commit guard
python3 scripts/harness.py worktree new claude/feat/<topic>
python3 scripts/harness.py check changed
```

See the [harness reference](docs/harness.md) and [agent setup](.agents/reference/agent-setup.md).

## Contributing

Read [CONTRIBUTING](.github/CONTRIBUTING.md) first. Report vulnerabilities privately, as the [security policy](.github/SECURITY.md) describes.

## License

[Apache 2.0](LICENSE)
