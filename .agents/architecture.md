# Architecture

Pending: the user will describe Agentium's idea (requested 2026-09-27). Record the product concepts, stack and code map here once they are decided, and add the stack's checks to the harness (see [adding stack checks](../docs/harness.md#adding-stack-checks)).

Current contents are the development process only:

| Path | Responsibility |
|---|---|
| `AGENTS.md`, `CLAUDE.md`, `.agents/` | Shared instructions: rules, references, roles, skills, templates, plans, decisions |
| `.agents/scripts/harness.py` | Standard-library entrypoint for checks, hooks, worktrees and metrics |
| `.claude/agents`, `.claude/skills` | Thin Claude adapters over `.agents/roles` and `.agents/skills` |
| `.githooks/pre-commit` | Shared pre-commit guard |
| `.github/` | CI and the PR template |
