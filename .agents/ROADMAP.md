# Roadmap

Product direction: an AI development lab for coding agents. The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) is accepted as a hybrid strategy ([decision](decisions/2026-09-27-hybrid-strategy.md)).

## Foundation — 2026-09-27
- The agent development process came over from Orchid, with a stack-neutral harness. See the [decision](decisions/2026-09-27-agent-process-from-orchid.md).
- Product research: existing tools, UX, the context-experiment method, architecture, MVP and strategy comparison.
- Phase 0 spike: 60 real Claude Code runs comparing Agentium's full context with a minimal one. It gave a go for Phase 1, a measured noise level (σ; τ still unresolved), and an isolation recipe. See the [results](../docs/research/2026-09-27-phase0-spike-results.md).
- 2026-09-28 — Stack: Go + React + SQLite accepted ([decision](decisions/2026-09-28-stack-go-react-sqlite.md)); Go module, `check go`/`check vuln` and CI in place.

- 2026-09-29 — MVP Phase 1 done: context A/B from the CLI, for Claude Code. Real runs: an A/A found no difference, and an A/B the size of one usage window ran (exploratory). See the [plan](plans/archive/2026-09-28-phase1-context-ab-cli.md).

## Next, in order
1. Clearer console output ([plan](plans/2026-09-30-clearer-console.md); [no web UI](decisions/2026-09-30-console-instead-of-web-ui.md)): colors, fitted tables, progress, terminal reports.
2. The 20-run context A/B, the last step of [hardening](plans/2026-09-29-experiment-hardening.md).
3. Phase 2: agent comparison and Codex.
