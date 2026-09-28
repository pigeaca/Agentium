# ADR: Hybrid strategy for the AI development lab

## Status

Accepted by the user on 2026-09-27.

## Context

The [feasibility study](../../docs/research/2026-09-27-ai-development-lab.md) compared three ways to build Agentium: from scratch, by extending an open-source harness (Harbor or Promptfoo), or as a hybrid. Two parts are already solved and change fast: agent execution in containers, and adapters for dozens of agents. Harbor handles both under Apache-2.0. What no existing tool provides is context versions as experiment arms, paired statistics, and a workflow simple enough for everyday use.

## Decision

Build the hybrid:
- **Agentium owns** the experiment model, context snapshots, the task library, the statistics and the UX.
- **Local runs** drive the agent CLIs headlessly in isolated git checkouts, so a first experiment needs no setup.
- **Containers** come later through Harbor, pinned and called as a separate process through its CLI and result files. Harbor is never embedded or forked.

The study's stack proposal (a Go core, a local React web UI and SQLite) is not part of this decision. It is confirmed when the MVP is planned, after the Phase 0 spike.

## Consequences

- The first work is the Phase 0 spike. It measures per-run cost and variance and checks run isolation before any product code.
- Two executor paths must stay behind one interface, and the Harbor boundary needs tests against its version changes.
- Container isolation and public benchmarks arrive after the MVP. Until then, local runs are for trusted repositories only.
