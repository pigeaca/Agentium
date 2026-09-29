# Agent entrypoint

Agentium is a local-first lab, being built, that measures how coding agents, models and project context affect results. The strategy (hybrid) and stack (Go + React + SQLite) are decided; Phase 1, context A/B from the command line, works. Besides the Go code, this repository holds the development process that agents follow: rules, roles, skills, templates, the harness, hooks and CI.

Read once, in order:
1. [Core rules](rules/core.md)
2. [Architecture](architecture.md)
3. [Roadmap](ROADMAP.md)
4. Relevant active file in `plans/` (archives are historical, not instructions).

Run `python3 scripts/harness.py help` from the repository root. For commands from another directory, use the absolute harness path. See [command scopes and setup](../docs/harness.md).

Load references by the boundary being changed:

| Work | Read |
|---|---|
| Agent instructions, skills, roles, hooks or GitHub access | [Agent setup](reference/agent-setup.md) and [local skills](skills/README.md) |
| Branches, commits, PRs or parallel agents | [Git workflow](rules/git-workflow.md) and [worktree ownership](rules/collaboration.md) |
| Checks and evidence | [Verification](rules/testing.md) |
| Dependencies, credentials or external data | [Dependencies](rules/supply-chain.md) and [secrets](rules/secrets.md) |
| Architectural choices | [Decisions](decisions/README.md) |
| Reading efficiently | [Context budget](reference/context-budget.md) |
| Product scope, competitors, experiment method or proposed architecture | [Feasibility study](../docs/research/2026-09-27-ai-development-lab.md) and [Phase 0 results](../docs/research/2026-09-27-phase0-spike-results.md), sections as needed |

Product references are added here once the architecture exists. Create a plan sized to the change before implementation. Update affected docs when behavior changes; archive completed or superseded plans. Do not load every rule, reference or archived plan for routine work.
