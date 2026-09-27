# AI Development Lab feasibility research

- Date: 2026-09-27
- Status: Completed
- Scope: the user's product brief "AI Development Lab — Technical Research & Feasibility Study" (2026-09-27). Research only: no application code, dependencies or stack decision beyond a recommendation.

## Acceptance
1. One research report in `docs/research/` covering the nine requested deliverables, in order: executive summary, existing solutions, feature matrix, UX proposal, context experimentation design, technical architecture, MVP specification, roadmap, risks and open questions. It ends by comparing the three strategies (from scratch, extend open source, hybrid) with effort estimates and a recommendation. Evidence: section checklist in the review.
2. Every important claim about a tool or SDK is checked against current official docs or source and linked. Unverified statements are labelled as hypotheses. Evidence: source links per section; a spot check of at least ten claims before the PR.
3. The context A/B methodology gives concrete minimums (paired design, repeats, the statistic used, and sample sizes computed rather than guessed) and states when results are inconclusive. Evidence: the calculation is reproducible from numbers in the report.
4. The MVP covers workflow A (agent comparison) and workflow B (context optimization) on shared infrastructure, and lists what is postponed. Evidence: MVP section.
5. Agent docs point to the research without loading it by default: `AGENTS.md` product direction, `architecture.md` (proposed, pending the user's decision), `ROADMAP.md` next steps. Entry docs stay within the word budget. Evidence: `check docs`.
6. The PR's CI passes.

## Work
- [x] Research existing solutions and agent SDKs (sources recorded)
- [x] Context experiment methodology and sample-size calculation
- [x] Write the report
- [x] Update AGENTS.md, README, architecture, roadmap and the reference table
- [x] Verify links and claims, run checks, open the PR

## Boundaries
Nothing is installed or run against paid APIs. Tools are evaluated from docs, source and release history only. The stack and strategy stay proposals until the user accepts them in a decision record.

## Verification
Planned: `python3 .agents/scripts/harness.py check changed`, a claim spot check, and the PR's CI.

Results:
- `check changed` selected `check docs`, which passed: entrypoints 1105/1800 words, links valid.
- A script confirmed that every reference link is defined and used, and that every in-page anchor resolves.
- Sample sizes and budgets come from a standard-library script using the formulas and assumptions stated in report section 5.6. The script is not committed; the report carries enough to reproduce it.
- Claims spot-checked against their sources:
  - Harbor: the task format, `n_attempts`, skill digests, containers only, the compare view (averages only), the Claude adapter's cost capture, and its `litellm>=1.92.0` dependency.
  - Claude Code: `total_cost_usd` is a client-side estimate, `settingSources` defaults, `--bare` skips `CLAUDE.md`, `-p` runs repository hooks, and `plugin eval` defaults and isolation.
  - Codex: `exec --json` usage fields with no cost, and the `AGENTS.md` 32 KiB cap.
  - Anthropic prices and cache multipliers, and the Sonnet 5 sampling-parameter 400.
  - The abstracts of both context-file papers, and the Promptfoo and Langfuse acquisitions and licenses.

Limitations: nothing was run against paid APIs. Per-run cost and variance are labelled estimates for the Phase 0 spike to measure. Several isolation behaviors are marked as hypotheses.

## Metrics
- Agent: Claude Code / claude-opus-5-5 / default
- Elapsed: 25m
- Check-fix loops: 0
- User corrections: 0
- Review: not required (docs-only)
