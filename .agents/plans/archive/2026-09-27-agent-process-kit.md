# Agent process kit from Orchid

- Date: 2026-09-27
- Status: Completed
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
- [x] Entrypoints, rules, references, roles, skills, templates, decisions
- [x] Stack-neutral harness and tests
- [x] Hooks, CI, PR template, docs
- [x] Verify, review, PR

## Verification and handoff
Completed 2026-09-27. Criteria 1-6 are met locally; criterion 7 depends on the PR's CI run.
- `check ci`: docs pass (804/1800 entrypoint words) and 27 harness tests pass on Python 3.9.
- The pre-commit hook is enabled (`core.hooksPath=.githooks`). On the first real commit it caught a trailing blank line in `archive/INDEX.md`, which was fixed.
- Real runs in Agentium:
  - `worktree new claude/test/process-probe` created a worktree from `origin/main`, with no dependencies configured; `worktree remove` removed it and its branch.
  - `fetch_default` against the SSH remote fell back to HTTPS successfully.
- No personal paths, emails or secrets are in the committed files (the repository is public).

**Review:** the reviewer requested changes, and all findings were fixed with tests.
- Medium: fetching the default branch failed over SSH in agent shells, and `worktree new` then quietly used a stale base. The harness now falls back to HTTPS and refuses a stale base unless `--base` is explicit. The docs now describe an HTTPS fetch, and the incorrect "leave the upstream unset" advice is corrected.
- Low:
  - `.claude/settings.local.json` is added to `.gitignore` and to the staged guard.
  - `.gitignore` now covers the token, database and dump files that the secrets rule promises.
  - The decision record now names the dormant gofmt and pnpm hooks and the environment-clearing step for later.
  - `worktree remove` refuses ignored files that could be personal work; only regenerable dependency and build folders may go. It uses `--ignored=matching` so a nested `node_modules` counts as regenerable.
  - `check docs` skips `.claude/worktrees`.
- Nits: examples use neutral vocabulary, the context budget is linked, plain `check` is documented as `ci`, and the reference to Orchid's private harness is replaced with generic guidance.

**Left for the user:**
- Describe the idea so the architecture and roadmap can be written.
- Make sure the gh token includes Agentium (Contents, Pull requests and Workflows write).
- Optionally apply the HTTPS `insteadOf` Git setting.
- Add a `main` ruleset and "Automatically delete head branches".

## Metrics
- Agent: Claude Code / claude-opus-5-5 / default
- Elapsed: 20m
- Check-fix loops: 3 (test mocked print; hook caught trailing blank line; collapsed ignored-directory listing)
- User corrections: 0 (before user review)
- Review: changes requested, 11 fixed
