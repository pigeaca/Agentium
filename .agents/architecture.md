# Architecture

Status: **strategy accepted (hybrid); stack still proposed.** The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) proposed the design below, and the user accepted its [hybrid strategy](decisions/2026-09-27-hybrid-strategy.md). No product code exists yet. When the stack is decided, record its conventions here and add its checks to the harness (see [adding stack checks](../docs/harness.md#adding-stack-checks)).

## Product concept

Agentium runs coding agents (Claude Code and Codex first) on tasks from a developer's own repository. It compares agents, models and versions of the project's AI context (`AGENTS.md`, `CLAUDE.md`, skills, rules) on correctness, cost and speed. Context versions are first-class experiment arms. Results use a paired design with repeats and report plain verdicts: improved, regressed, no loss beyond the margin, or inconclusive.

Shape:
- **Hybrid** (accepted). Agentium owns context snapshots, tasks, experiment design, statistics and the UX.
- **Local runs** drive agent CLIs headlessly in isolated git worktrees.
- **Harbor**, pinned and out of process, adds containers later.
- **Stack (proposed, awaiting the user's decision):** a Go core in a single binary, a local React web UI and SQLite.
- **Phase 0 spike, done:** it measured per-run cost and variance and settled how runs are isolated ([results](../docs/research/2026-09-27-phase0-spike-results.md)).

## Current contents: the development process only

| Path | Responsibility |
|---|---|
| `AGENTS.md`, `CLAUDE.md`, `.agents/` | Shared instructions: rules, references, roles, skills, templates, plans, decisions |
| `.agents/scripts/harness.py` | Standard-library entrypoint for checks, hooks, worktrees and metrics |
| `.claude/agents`, `.claude/skills` | Thin Claude adapters over `.agents/roles` and `.agents/skills` |
| `.githooks/pre-commit` | Shared pre-commit guard |
| `.github/` | CI, the PR template, issue forms and community files (contributing, security, conduct) |
| `docs/research/` | Product research; history, not default context |
