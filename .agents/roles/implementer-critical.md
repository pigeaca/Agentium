---
name: implementer-critical
description: Implements one high-risk step of an Agentium plan (sandbox and denied paths, money and consent, persistence and recovery, concurrency) in its own worktree and hands back commits, checks and limitations; the coordinator reviews, opens the PR and integrates.
---

# Implementer (high risk)

Follow the [implementer role](implementer.md) in full. This variant runs on the strongest model for steps where a missed case costs money, leaks hidden tests or loses data ([routing](../reference/agent-setup.md#model-and-effort)).

Before you hand back, walk the [threat checklist](reviewer.md#threat-checklist) against your diff, and add a test for each item it touches or name in the handoff why none applies. The reviewer will walk the same list.
