# Phase 0 spike: what Phase 1 must do

On 2026-09-27 we ran Agentium's core idea once by hand, before building it: 60 Claude Code runs (Sonnet 5), comparing Agentium's full `CLAUDE.md` with a 64-word one on six small harness tasks. They cost an estimated $17.64 and took about 37 minutes. Everything worked, so **Phase 1 goes ahead**. The details, script and data are in git history at commit `9bae530`, with the plan in `.agents/plans/archive/2026-09-27-phase0-spike.md`.

## The runner must isolate each run

Each of these silently produced wrong measurements during the spike:

1. **Personal context leaks.** Run Claude Code with `--setting-sources project`; without it, 15 user-level skills loaded into test runs. Codex also loads the user's config and skills unless each run gets its own `CODEX_HOME`.
2. **Account connectors attach.** A claude.ai login adds connectors, such as tools that create, update and delete the user's documents. Disable them: `disableClaudeAiConnectors`, `ENABLE_CLAUDEAI_MCP_SERVERS=false`, `--strict-mcp-config`.
3. **Permission mode can change silently.** `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` forces the `default` mode. Record `permissionMode` from each run's init event, and reject runs whose mode, tools or version drifted.
4. **A fresh `CLAUDE_CONFIG_DIR` can't use a subscription login.** Isolated runs need an API key or a `claude setup-token` token.
5. **Hidden tests must be unreachable.** Clone a repository that holds only the base commit, run the hidden tests in a copy the agent can't read, and deny reads of other runs and of `~/.claude/projects`.
6. **Arms must pass the repository's own checks.** A minimal `CLAUDE.md` broke Agentium's `check docs`, and agents spent turns on that instead of the task.
7. **The sandbox without approvals blocks some normal commands,** such as `git stash` and multi-line `python3 -c`. Report denials per run.
8. **Prompt caching skews cost.** The arm that runs second can cost a third as much. Interleave the arms.
9. **Codex treats `.agents/` as read-only,** so it cannot edit files there.

## Defaults for the experiment planner

- Cost varies little from run to run (σ ≈ 0.19 in log cost). 12 tasks × 3 runs detect a cost change of roughly 12–21%, for about $21 on small tasks.
- How much the effect varies across tasks is still unknown: plan with τ = 0.10–0.25.
- Success needs tasks the agent solves only 20–80% of the time. The spike's tasks were too easy (57 of 60 passed), so success noise was not measured.
- Behavior checks such as "updated the tests" or "ran the checks" show effects clearly and cheaply, so make them first-class. Agentium's own case: with the full docs, agents updated tests in 16 of 30 runs; with a one-line instruction, in 30 of 30.

## Open

- Measure τ and success noise on more repositories and on harder tasks.
- Agentium's own docs: consider making "update and run the tests" a concrete line, and moving the harness out of `.agents/` (item 9).
