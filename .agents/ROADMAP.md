# Roadmap

Product direction: an AI development lab for coding agents. The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) is accepted as a hybrid strategy ([decision](decisions/2026-09-27-hybrid-strategy.md)).

## Foundation — 2026-09-27
- The agent process and a stack-neutral harness came from Orchid ([decision](decisions/2026-09-27-agent-process-from-orchid.md)).
- Product research: tools, UX, method, architecture, MVP and strategy.
- Phase 0 spike: 60 real Claude Code runs, full context against minimal. It gave a go for Phase 1, a noise level and an isolation recipe ([results](../docs/research/2026-09-27-phase0-spike-results.md)).
- 2026-09-28 — Stack: Go + React + SQLite accepted ([decision](decisions/2026-09-28-stack-go-react-sqlite.md)); Go module, `check go`/`check vuln` and CI in place.

- 2026-09-29 — MVP Phase 1 done: context A/B from the CLI, for Claude Code. Real runs: an A/A found no difference, and an A/B the size of one usage window ran (exploratory). See the [plan](plans/archive/2026-09-28-phase1-context-ab-cli.md).
- 2026-09-30 — Clearer console output, no web UI ([plan](plans/archive/2026-09-30-clearer-console.md), [decision](decisions/2026-09-30-console-instead-of-web-ui.md)).

## Next, in order
1. The 20-run context A/B, the last step of [hardening](plans/2026-09-29-experiment-hardening.md).
2. [Task checks and context use](plans/2026-09-30-task-checks-context-use.md).
3. [LLM judge pilot](plans/2026-09-30-judge-pilot.md), on the A/B's diffs.
4. [Java and Rust](plans/2026-09-30-java-rust.md).
5. Phase 2: agent comparison and Codex.
