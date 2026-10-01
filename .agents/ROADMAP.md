# Roadmap

Product direction: an AI development lab for coding agents. The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) is accepted as a hybrid strategy ([decision](decisions/2026-09-27-hybrid-strategy.md)).

## Foundation — 2026-09-27
- Agent process and harness from Orchid ([decision](decisions/2026-09-27-agent-process-from-orchid.md)).
- Product research: tools, UX, method, architecture, MVP and strategy.
- Phase 0 spike: 60 real runs gave a go for Phase 1, a noise level and an isolation recipe ([results](../docs/research/2026-09-27-phase0-spike-results.md)).
- 2026-09-28 — Stack: Go + SQLite ([decision](decisions/2026-09-28-stack-go-react-sqlite.md)), with CI.

- 2026-09-29 — MVP Phase 1 done: context A/B from the CLI, for Claude Code, with real A/A and A/B runs ([plan](plans/archive/2026-09-28-phase1-context-ab-cli.md)).
- 2026-09-30 — Clearer console output, no web UI ([plan](plans/archive/2026-09-30-clearer-console.md), [decision](decisions/2026-09-30-console-instead-of-web-ui.md)).
- 2026-10-01 — Experiment hardening; 16-run A/B inconclusive ([plan](plans/archive/2026-09-29-experiment-hardening.md)).
- 2026-10-01 — Run estimates ([plan](plans/archive/2026-10-01-run-estimates.md)); task checks and context use ([plan](plans/archive/2026-09-30-task-checks-context-use.md)).
- 2026-10-01 — Judge pilot: no GO ([results](../docs/research/2026-10-01-judge-pilot-results.md)); built anyway ([decision](decisions/2026-10-01-llm-judge-alongside-tests.md)).

## Next, in order
1. [Judge per run](plans/2026-10-01-llm-judge.md), alongside [Java and Rust](plans/2026-09-30-java-rust.md); [temp isolation](plans/2026-10-01-run-temp-isolation.md).
2. Judge: [which arm is better](plans/2026-10-01-judge-pairs.md), [ticket tasks](plans/2026-10-01-ticket-tasks.md); [model and effort A/B](plans/2026-10-01-model-ab.md).
3. [Codex](plans/2026-10-01-phase2-agents-codex.md), deferred.
