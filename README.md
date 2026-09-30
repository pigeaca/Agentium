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
> Agentium is early. Phase 1, context A/B for Claude Code from the command line, is done; Phase 2 adds a web UI, Codex and agent comparison. See the [roadmap](.agents/ROADMAP.md) and the [feasibility study](docs/research/2026-09-27-ai-development-lab.md).

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

`agentium experiment report` prints Markdown you can paste into a pull request (or `--json`). These excerpts are real reports from Phase 1's acceptance runs: Claude Code 2.1.281 with claude-sonnet-5, on tasks taken from this repository. Both experiments were stopped early to fit one usage window, so they are small. Every verdict is "exploratory": the report says the data is too thin instead of naming a winner.

**Context A/B: today's docs (`full`) against a minimal version (`minimal`), 4 complete pairs**

> - **Success 100% → 75%**, Δ -25 pp (95%: -105 to +55): exploratory: too few tasks or runs for a verdict.
> - **Cost +11%** (95%: -45% to +123%): exploratory: too few tasks or runs for a verdict.
>
> | Metric | Role | A | B | B vs A | 95% bootstrap | 95% t | Verdict |
> |---|---|---|---|---|---|---|---|
> | Success | guard | 100% | 75% | -25 pp | [-75, +0] pp | [-105, +55] pp | exploratory |
> | Cost | primary | $0.659 | $0.728 | +11% | [-27%, +62%] | [-45%, +123%] | exploratory |
> | Time | secondary | 163 s | 179 s | +10% | [-35%, +72%] | [-54%, +166%] | exploratory |
> | Output tokens | secondary | 14937 | 19075 | +28% | [-17%, +123%] | [-47%, +211%] | exploratory |
>
> | Arm | Context | Runs counted | First request (tokens) | Cost per run | Cold-cache cost | Cache-read share |
> |---|---|---|---|---|---|---|
> | A | `full` | 4 | 30586 | $0.783 | $7.772 | 97% |
> | B | `minimal` | 4 | 25979 (-4607) | $0.791 | $7.268 | 96% |
>
> ● success, ○ failure, × not counted; cost is the mean of counted runs.
>
> | Task | A | B | Cost A → B |
> |---|---|---|---|
> | deny-login-file | ● 1/1 | ● 1/1 | $0.265 → $0.485 |
> | documents-filter | ● 1/1 | ○ 0/1 | $1.135 → $0.706 |
> | judge-truncated-notice | - 0/0 | - 0/0 | - → - |
> | run-survives-erase | ● 1/1 | ● 1/1 | $1.217 → $1.376 |
> | scrub-whole-paths | ● 1/1 | ● 1/1 | $0.514 → $0.597 |
> | unreadable-files | × 0/0 | × 0/0 | - → - |

The minimal docs cut Claude Code's first request by about 4.6k tokens, but cost did not fall, and one task failed. With four pairs the intervals are far too wide to call either a result, and the report says so in words. The [full report](docs/examples/context-ab-report.md) also lists behavior flags (tests run, files changed, permission denials) and notes, such as the number of tasks and runs a verdict needs.

**A/A: the same context in both arms.** A sanity check that the method reports no difference when there is none. Cost was -6% (95%: -31% to +26%) and every run passed, so it found none. This is also how Agentium measures the noise that later experiments are planned with. See the [full report](docs/examples/aa-report.md).

A 72-run Quick-tier A/B, big enough for a verdict, is still to come: it waits for the [hardening plan](.agents/plans/2026-09-29-experiment-hardening.md).

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
