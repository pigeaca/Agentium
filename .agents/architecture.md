# Architecture

Status: **hybrid strategy and Go + SQLite stack accepted** ([decision](decisions/2026-09-28-stack-go-react-sqlite.md); [no web UI](decisions/2026-09-30-console-instead-of-web-ui.md)). Design: the [feasibility study](../docs/research/2026-09-27-ai-development-lab.md). Phase 1 is done ([plan](plans/archive/2026-09-28-phase1-context-ab-cli.md)).

## Product concept

Agentium runs coding agents (Claude Code and Codex first) on tasks from a developer's own repository. It compares agents, models and versions of the project's AI context (`AGENTS.md`, `CLAUDE.md`, skills, rules) on correctness, cost and speed. Context versions are first-class experiment arms. Results use a paired design with repeats and report plain verdicts: improved, regressed, no loss beyond the margin, or inconclusive.

Shape:
- **Hybrid.** Agentium owns context snapshots, tasks, experiment design, statistics and the UX.
- **Local runs** drive agent CLIs headlessly in isolated checkouts.
- **Containers** later, driven directly through the Docker CLI ([decision](decisions/2026-10-02-containers-direct-docker.md)).
- **One binary:** a Go core (Go 1.27.1, module `github.com/pigeaca/agentium`) and SQLite through `mattn/go-sqlite3`, used from the console.

## Go conventions

- `cmd/agentium` only wires packages together; code lives in `internal/<package>`.
- The standard library comes first. Every new module needs [approval](rules/supply-chain.md); `go.sum` is committed.
- Every blocking call takes a `context.Context`, and cancellation stops agent processes.
- Errors are wrapped with `%w` and context; there is no package-level mutable state.
- I/O goes through parameters (`io.Writer`, `Env`) so commands can be tested.
- Every git call starts a process (5 to 15 ms), so count them: a command that reads several commits reads them through one `source.Objects` (each object once). Budget: `experiment report` and `task list` answer in about a second on 100 runs and 20 tasks.
- Exit codes: 0 success, 1 runtime failure, 2 usage error.

## Code map

`cmd/agentium` wires the packages in `internal/`: the CLI and console (`cli`, `term`); the data folder and SQLite store (`home`, `store`); discovery, mining and the task pool (`project`, `mine`, `pool`); git and sources (`gitx`, `source`, `checkout`); context (`claudectx`, `snapshot`); running agents (`runner`, `buildtool`, `sandbox`, `agent`, `claude`, `codex`, `run`); experiments, statistics and reports (`task`, `experiment`, `stats`, `report`, `judge`, `pricing`); automation, as commands that the user, hooks or an AI call (`screen`, `ghx`); Agentium starts no background or detached processes on its own. The full table, with each package's responsibility, is in the [code map](reference/code-map.md).
