# Agent process kit from Orchid

- Date: 2026-09-27
- Status: In Progress
- Scope/approval: user asked to migrate all AI docs and development processes (rules, harness, roles, skills, hooks, templates, CI) from Orchid to Agentium. The stack is not decided, so the core is stack-neutral. Architecture and roadmap wait for the user's description of the idea (stubs only). Deliver as a GitHub PR to `main`; the repository is public.

## Outcome and boundaries
Agentium starts with the same way of working as Orchid's latest process: entrypoints and adapters for Claude and Codex, core/Git/collaboration rules (done = PR with green CI and recorded review), plans with acceptance criteria and metrics, read-only investigator/reviewer roles, the pre-commit guard, a stack-neutral harness (`check docs|staged|changed|ci`, `hooks`, `worktree new|deps|remove`, `metrics`, `doctor`), CI running the process checks, and the PR template.

Out of scope: Orchid-specific references (UI, palette, runtime, interfaces, persistence), the browser gallery and fixtures, Go/frontend checks until the stack is chosen, and any product code. Orchid keeps its own copy; nothing in Orchid changes.

## Acceptance
1. Entry points: `AGENTS.md`, `CLAUDE.md` (explicit imports), `.agents/README.md`, core rules, architecture stub and roadmap. They fit the 1800-word budget and contain no Orchid product terms.
2. Rules: git, collaboration, testing, secrets, supply-chain, error handling, observability and performance, all generalized for an undecided stack. Git rules use the discovered remote default (`origin/main`).
3. Roles, skills, templates and Claude adapters: investigator and reviewer, a product-increment skill, plan/handoff/PR templates, and `.claude/agents` and `.claude/skills` adapters that `check docs` validates.
4. The harness runs from the standard library only. Tests cover docs/adapters, the staged guard, check selection, worktree lifecycle (in temporary repositories with a containment guard), the optional offline dependency install, and metrics.
5. `.githooks/pre-commit` is enabled for this clone. CI on `main` runs `check ci` with its action pinned by SHA.
6. `docs/harness.md` and agent setup describe the commands, GitHub access for Agentium (SSH remote, HTTPS push through `gh`) and how to add stack checks later. A decision record explains what was carried over and why.
7. `check ci` passes locally, and the PR's CI run passes.

## Work
- [ ] Entrypoints, rules, references, roles, skills, templates, decisions
- [ ] Stack-neutral harness and tests
- [ ] Hooks, CI, PR template, docs
- [ ] Verify, review, PR

## Verification and handoff
`check ci` locally, the hook exercised by the real commit, a `worktree new/remove` probe in Agentium, a reviewer pass, and CI on the PR.

## Metrics
- Agent: Claude Code / claude-opus-5-5 / default
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
