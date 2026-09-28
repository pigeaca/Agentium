# Architecture

Status: **strategy accepted (hybrid); stack accepted: Go + React + SQLite** ([decision](decisions/2026-09-28-stack-go-react-sqlite.md)). The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) describes the design, and the [Phase 0 results](../docs/research/2026-09-27-phase0-spike-results.md) list what Phase 1 must do. Product code is only a skeleton so far.

## Product concept

Agentium runs coding agents (Claude Code and Codex first) on tasks from a developer's own repository. It compares agents, models and versions of the project's AI context (`AGENTS.md`, `CLAUDE.md`, skills, rules) on correctness, cost and speed. Context versions are first-class experiment arms. Results use a paired design with repeats and report plain verdicts: improved, regressed, no loss beyond the margin, or inconclusive.

Shape:
- **Hybrid.** Agentium owns context snapshots, tasks, experiment design, statistics and the UX.
- **Local runs** drive agent CLIs headlessly in isolated checkouts.
- **Harbor**, pinned and out of process, adds containers later.
- **One binary:** a Go core (Go 1.27.1, module `github.com/pigeaca/agentium`), a local React + TypeScript UI embedded in Phase 2, and SQLite through `mattn/go-sqlite3` from Phase 1.

## Go conventions

- `cmd/agentium` only wires packages together; code lives in `internal/<package>`.
- The standard library comes first. Every new module needs approval (see [dependencies](rules/supply-chain.md)), and `go.sum` is committed.
- Every blocking call takes a `context.Context`, and cancellation stops agent processes.
- Errors are wrapped with `%w` and context; there is no package-level mutable state.
- I/O goes through parameters (`io.Writer`, `Env`) so commands can be tested.
- Exit codes: 0 success, 1 runtime failure, 2 usage error.

## Code map

| Path | Responsibility |
|---|---|
| `AGENTS.md`, `CLAUDE.md`, `.agents/` | Shared instructions: rules, references, roles, skills, templates, plans, decisions |
| `go.mod`, `cmd/agentium` | Go module (pinned toolchain) and the `agentium` binary's entrypoint |
| `internal/cli` | Command-line parsing and dispatch (`version`, `help`) |
| `.agents/scripts/harness.py` | Standard-library entrypoint for checks (docs, harness, Go, vulnerabilities), hooks, worktrees and metrics |
| `.claude/agents`, `.claude/skills` | Thin Claude adapters over `.agents/roles` and `.agents/skills` |
| `.githooks/pre-commit` | Shared pre-commit guard |
| `.github/` | CI, the PR template, issue forms and community files (contributing, security, conduct) |
| `docs/research/` | Product research; history, not default context |
