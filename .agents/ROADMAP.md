# Roadmap

Product direction: an AI development lab for coding agents. The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) is accepted as a hybrid strategy ([decision](decisions/2026-09-27-hybrid-strategy.md)).

## Foundation — 2026-09-27
- The agent development process came over from Orchid, with a stack-neutral harness. See the [decision](decisions/2026-09-27-agent-process-from-orchid.md).
- Product research: existing tools, UX, the context-experiment method, architecture, MVP and strategy comparison.
- Phase 0 spike: 60 real Claude Code runs comparing Agentium's full context with a minimal one. It gave a go for Phase 1, a measured noise level (σ; τ still unresolved), and an isolation recipe. See the [results](../docs/research/2026-09-27-phase0-spike-results.md).
- 2026-09-28 — Stack: Go + React + SQLite accepted ([decision](decisions/2026-09-28-stack-go-react-sqlite.md)); Go module, `check go`/`check vuln` and CI in place.

## Next, in order
1. MVP Phase 1: core and the context A/B workflow from the CLI. Include the Phase 0 isolation recipe, per-run environment checks and trajectory graders. Plan it with acceptance criteria.
2. Candidate follow-ups for the user to decide:
   - make "update and run the tests" concrete in the entry docs;
   - move the harness out of `.agents/`, because Codex's sandbox treats that directory as read-only.
