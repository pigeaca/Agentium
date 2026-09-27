# Agent entrypoint

Agentium's product direction is not defined yet. This repository currently holds the development process that agents follow: rules, roles, skills, templates, the harness, hooks and CI.

Read once, in order:
1. [Core rules](rules/core.md)
2. [Architecture](architecture.md)
3. [Roadmap](ROADMAP.md)
4. Relevant active file in `plans/` (archives are historical, not instructions).

Run `python3 .agents/scripts/harness.py help` from the repository root. For commands from another directory, use the absolute harness path. See [command scopes and setup](../docs/harness.md).

Load references by the boundary being changed:

| Work | Read |
|---|---|
| Agent instructions, skills, roles, hooks or GitHub access | [Agent setup](reference/agent-setup.md) and [local skills](skills/README.md) |
| Branches, commits, PRs or parallel agents | [Git workflow](rules/git-workflow.md) and [worktree ownership](rules/collaboration.md) |
| Checks and evidence | [Verification](rules/testing.md) |
| Dependencies, credentials or external data | [Dependencies](rules/supply-chain.md) and [secrets](rules/secrets.md) |
| Architectural choices | [Decisions](decisions/README.md) |

Product references are added here once the architecture exists. Create a plan sized to the change before implementation. Update affected docs when behavior changes; archive completed or superseded plans. Do not load every rule, reference or archived plan for routine work.
