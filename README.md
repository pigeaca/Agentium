<div align="center">

# Agentium

**A local-first lab that measures what really makes AI coding agents better on your code.**

[![CI](https://github.com/pigeaca/Agentium/actions/workflows/ci.yml/badge.svg)](https://github.com/pigeaca/Agentium/actions/workflows/ci.yml) ![Go 1.27.1](https://img.shields.io/badge/Go-1.27.1-00ADD8?logo=go&logoColor=white) ![Status: early, first decisive verdict](https://img.shields.io/badge/status-early%20%E2%80%94%20first%20decisive%20verdict-orange) [![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

</div>

Agentium runs coding agents such as Claude Code on tasks from your own repository, and measures how the agent, the model or your project's AI context (`AGENTS.md`, `CLAUDE.md`, rules, skills) changes correctness, cost and speed. Results come with honest statistics. It is early: Codex support is planned, and there is no web UI.

## What you get

<img src="docs/images/console-verdict.svg" alt="agentium experiment report opus-vs-sonnet: Sonnet costs 63% less, verdict improved in green, success exploratory, and the first decisive verdict line">

A model A/B on [samber/lo](https://github.com/samber/lo): Sonnet cost 63% less than Opus per run, a decisive verdict reached 65 min and $3.34 after `init`.
Success was exploratory at 8 tasks. [Full report](docs/examples/model-ab-report.md).

## How it works

- **Your tasks, not a benchmark.** A past commit becomes a task: its parent is the start, its test changes are hidden tests.
- **Context versions as arms.** Snapshot `CLAUDE.md`, rules and skills, change them, compare head to head.
- **Fair, isolated runs.** A fresh checkout and a sandbox without network; no hidden tests, solutions, other runs or credentials.
- **Plain verdicts.** Paired runs with repeats: improved, regressed, no loss beyond the margin, or inconclusive, with intervals.

## Requirements

- Go 1.27.1 and a C compiler (SQLite uses cgo); Git
- [Claude Code](https://claude.com/claude-code), signed in (or `ANTHROPIC_API_KEY`)
- Your project's build tool: Go, Maven, Gradle or Cargo (notes in the [guide](docs/guide.md#build-tools-and-offline-dependencies))

## Quick start

```sh
go install ./cmd/agentium          # from a clone of this repository
cd /path/to/your/repo
agentium start
```

`start` never writes to your repository. It:

- registers the repository and snapshots your context as `baseline`;
- mines and validates 8 tasks from your history;
- creates an experiment and previews runs, cost and detectable effect;
- stops there: nothing paid runs without `--yes`.

> [!WARNING]
> `--yes`, `run` and `experiment run` start real Claude Code runs. They cost money or use your plan's limits.

Mined instructions need your review for solution leaks (`agentium task show NAME`, then `task edit NAME --reviewed`); `--accept-mined` skips that review. Details: [guide](docs/guide.md#what-start-does).

## Commands at a glance

| Command | Purpose |
|---|---|
| `context show / snapshot / diff / lint` | See, version and compare your context (free) |
| `task mine / validate / show / edit` | Turn commits into validated tasks (free) |
| `run once` | One graded run (paid) |
| `experiment new / plan / run / report` | Design, price, run and read an experiment (`run` is paid) |
| `experiment new --judge` | Add an LLM second opinion; tests still decide |
| `experiment new --template model-ab` | Compare two models or efforts on one context |

Everything else is in the [guide](docs/guide.md).

## See it

<img src="docs/images/console-start.svg" alt="agentium start --accept-mined on a Go library: registered, snapshot saved, tasks mined and validated, an A/A experiment created and its preview">

`start` on a Go library: tasks mined, experiment previewed, nothing run.

<img src="docs/images/console-report.svg" alt="agentium experiment report ab on a terminal: exploratory verdicts in yellow, the metrics table, and per-task results">

A report: verdicts in words, metrics with intervals, per-task results. More in the [gallery](docs/gallery.md).

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
