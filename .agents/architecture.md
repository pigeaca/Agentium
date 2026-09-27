# Architecture

Status: **proposed, not accepted.** The user described the product on 2026-09-27. The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) proposes the design below, and nothing is built yet. A decision record will accept or change it. Once one exists, record the stack's conventions here and add its checks to the harness (see [adding stack checks](../docs/harness.md#adding-stack-checks)).

## Product concept (proposed)

Agentium runs coding agents (Claude Code and Codex first) on tasks from a developer's own repository. It compares agents, models and versions of the project's AI context (`AGENTS.md`, `CLAUDE.md`, skills, rules) on correctness, cost and speed. Context versions are first-class experiment arms. Results use a paired design with repeats and report plain verdicts: improved, regressed, no loss beyond the margin, or inconclusive.

Proposed shape, pending the decision:
- **Hybrid.** Agentium owns context snapshots, tasks, experiment design, statistics and the UX.
- **Local runs** drive agent CLIs headlessly in isolated git worktrees.
- **Harbor**, pinned and out of process, adds containers later.
- **Stack:** a Go core in a single binary, a local React web UI and SQLite.
- **First step:** a capped-budget spike that measures per-run cost and variance before the MVP.

## Current contents: the development process only

| Path | Responsibility |
|---|---|
| `AGENTS.md`, `CLAUDE.md`, `.agents/` | Shared instructions: rules, references, roles, skills, templates, plans, decisions |
| `.agents/scripts/harness.py` | Standard-library entrypoint for checks, hooks, worktrees and metrics |
| `.claude/agents`, `.claude/skills` | Thin Claude adapters over `.agents/roles` and `.agents/skills` |
| `.githooks/pre-commit` | Shared pre-commit guard |
| `.github/` | CI, the PR template, issue forms and community files (contributing, security, conduct) |
| `docs/research/` | Product research; history, not default context |
