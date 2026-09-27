# ADR: Agent development process carried over from Orchid

## Status

Accepted by the user on 2026-09-27.

## Context

Orchid (a separate Go + React project) developed a working process for building software with AI coding agents. Its parts: short always-loaded instructions for Claude and Codex; plans with acceptance criteria and metrics; task worktrees; a pre-commit guard; independent read-only review; PR-based completion with CI; and a standard-library harness. Using it end to end showed that most of the value was in this process, not in Orchid's product code. Agentium is a new project with a different, not yet described idea and no chosen stack.

## Decision

Copy the process, not the product. Agentium carries over:
- the entrypoints, core, Git, collaboration and verification rules,
- the investigator and reviewer roles,
- the product-increment skill,
- the plan, handoff and PR templates,
- the pre-commit guard,
- a stack-neutral harness: docs/adapter validation, the staged guard, check selection, worktree lifecycle with an optional offline dependency install, and metrics.

It leaves behind Orchid's UI, palette, runtime, interface and persistence references, its browser gallery and fixtures, and all Go/frontend checks. Orchid keeps its own copy unchanged.

## Consequences

- Agents work in Agentium the same way from the first commit, and CI guards the process files.
- Choosing the stack requires one follow-up change: record it in the architecture, add `check changed` rules and dependency installation to the harness, and add CI jobs.
- The two copies can drift. Improvements made in one project are ported deliberately, not synchronized automatically. If more projects adopt the process, packaging it (for example as a Claude Code plugin) becomes worth considering.
