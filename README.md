# Agentium

Agentium is a planned local-first lab for AI coding agents. It runs agents such as Claude Code and Codex on tasks from your own repository, and measures how the agent, the model or the project's AI context (`AGENTS.md`, `CLAUDE.md`, skills) changes correctness, cost and speed, with honest statistics. The design is at the research stage: see [the feasibility study](docs/research/2026-09-27-ai-development-lab.md).

## Development process

Agentium is developed with AI coding agents (Claude Code and Codex) under shared rules in [`.agents/`](.agents/README.md). Every task gets a plan with acceptance criteria and runs in its own worktree. A pre-commit guard checks each commit, an independent reviewer reads the change, and work lands as a PR with green CI. A standard-library Python harness is the single entrypoint:

```sh
python3 .agents/scripts/harness.py help
python3 .agents/scripts/harness.py hooks        # once per clone: enable the pre-commit guard
python3 .agents/scripts/harness.py worktree new claude/feat/<topic>
python3 .agents/scripts/harness.py check changed
```

See [the harness reference](docs/harness.md) and [agent setup](.agents/reference/agent-setup.md).

## License

[Apache 2.0](LICENSE)
