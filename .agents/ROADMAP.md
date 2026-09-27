# Roadmap

Product direction: an AI development lab for coding agents; see the [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) (proposal, not yet accepted).

## Foundation — 2026-09-27
- The agent development process came over from Orchid, with a stack-neutral harness. See the [decision](decisions/2026-09-27-agent-process-from-orchid.md).
- Product research: existing tools, UX, the context-experiment method, architecture, MVP and strategy comparison.

## Next, in order
1. The user reviews the study and accepts or changes the strategy and stack in a decision record.
2. Phase 0 spike, which needs the user's approval for paid API runs (proposed cap about $150), a dedicated API key and a pinned Harbor install. It measures per-run cost and variance, confirms run isolation and tests the Harbor mapping.
3. Add the chosen stack's checks to the harness: dependency install for `worktree new`, `check changed` rules and CI jobs.
4. MVP Phase 1: core and the context A/B workflow from the CLI. Plan it with acceptance criteria.
