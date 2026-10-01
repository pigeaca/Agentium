# Architecture Decision Records

Decisions record the why behind significant choices, with their context and consequences. Plans record the what.

Format: `# ADR: <Title>`, then `## Status` (Accepted | Superseded by <file> | Deprecated), `## Context`, `## Decision`, `## Consequences`. Files are named `YYYY-MM-DD-<short-slug>.md`. Accepted records are not rewritten; a new record supersedes an old one and says so.

## Index

- [2026-09-27-agent-process-from-orchid.md](2026-09-27-agent-process-from-orchid.md) — development process carried over from Orchid, stack-neutral harness
- [2026-09-27-hybrid-strategy.md](2026-09-27-hybrid-strategy.md) — hybrid strategy for the lab: own experiments, context and statistics; local runs first; Harbor later
- [2026-09-28-stack-go-react-sqlite.md](2026-09-28-stack-go-react-sqlite.md) — Go 1.27.1 core, React UI (superseded), SQLite via mattn/go-sqlite3, pinned toolchain and CI
- [2026-09-30-console-instead-of-web-ui.md](2026-09-30-console-instead-of-web-ui.md) — no web UI; Phase 2 makes the console output clearer instead
- [2026-10-01-llm-judge-alongside-tests.md](2026-10-01-llm-judge-alongside-tests.md) — an opt-in, per-run LLM judge shown alongside tests, never deciding; built despite the pilot's no GO
