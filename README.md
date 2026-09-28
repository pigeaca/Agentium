# Agentium

Agentium is a local-first lab for AI coding agents, being built now. It runs agents such as Claude Code (and later Codex) on tasks from your own repository, and measures how the agent, the model or the project's AI context (`AGENTS.md`, `CLAUDE.md`, skills) changes correctness, cost and speed, with honest statistics. Background: [the feasibility study](docs/research/2026-09-27-ai-development-lab.md); progress: [the roadmap](.agents/ROADMAP.md).

## Try it

Agentium is early (Phase 1). With Go 1.27.1:

```sh
go build -o agentium ./cmd/agentium
./agentium init /path/to/your/repo    # registers it; never writes to the repository
cd /path/to/your/repo
agentium context show                  # what Claude Code loads at session start, and on demand
agentium context snapshot baseline     # save the committed context (HEAD) as a version
# edit CLAUDE.md, rules or skills, then:
agentium context snapshot trimmed --working-tree   # --include-linked adds linked docs
agentium context diff baseline trimmed --patch
agentium task import --commit <sha>    # base: its parent; hidden tests: its test-file changes
agentium task validate <name> --snapshot trimmed   # tests fail on the base, pass with the reference, in each arm
agentium run calibrate --snapshot trimmed         # short real runs: sandbox, large outputs, context size, tool set (re-run if a check is unverified)
agentium run once <name> --snapshot trimmed        # one real Claude Code run, graded with the hidden tests (costs money)
```

Data lives in `~/.agentium` (override with `AGENTIUM_HOME`).

## Development process

Agentium is developed with AI coding agents (Claude Code and Codex) under shared rules in [`.agents/`](.agents/README.md). Every task gets a plan with acceptance criteria and runs in its own worktree. A pre-commit guard checks each commit, an independent reviewer reads the change, and work lands as a PR with green CI. A standard-library Python harness is the single entrypoint:

```sh
python3 scripts/harness.py help
python3 scripts/harness.py hooks        # once per clone: enable the pre-commit guard
python3 scripts/harness.py worktree new claude/feat/<topic>
python3 scripts/harness.py check changed
```

See [the harness reference](docs/harness.md) and [agent setup](.agents/reference/agent-setup.md).

To contribute, read [CONTRIBUTING](.github/CONTRIBUTING.md). Report vulnerabilities privately under the [security policy](.github/SECURITY.md).

## License

[Apache 2.0](LICENSE)
