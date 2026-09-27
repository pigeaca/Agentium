# Agent entrypoints and discovery

`.agents` contains shared project policy, roles and skills. Agent-specific files only load or route to that content; they must not fork testing, approval or product rules. Paths in Markdown links are relative to the containing file. Repository commands run from the repository root, or use an absolute harness path.

| Client | Entrypoint | Skills and roles |
|---|---|---|
| Codex / clients honoring AGENTS.md | Root `AGENTS.md` specifies the short read order | Open canonical `.agents/skills/<name>/SKILL.md` and `.agents/roles/<name>.md` explicitly |
| Claude Code | Root `CLAUDE.md` explicitly imports the five shared entrypoint files | `.claude/skills/<name>/SKILL.md` and `.claude/agents/<name>.md` delegate to the canonical files |
| Other clients | Use their documented instruction loader to point at `AGENTS.md` | Open the canonical files explicitly |

Claude's ordinary prose links do not guarantee automatic file loading. Keep the standalone `@path` imports in `CLAUDE.md`; do not put them in code spans or fences. Active plans and archives are not imported automatically.

## Read-only roles

Canonical role instructions live in `.agents/roles/`; Claude subagents in `.claude/agents/<name>.md` add only discovery, tools, model and effort, and link back to the role. Both roles are read-only: neither edits, commits, merges nor installs.

| Role | Use | Claude subagent settings |
|---|---|---|
| [investigator](../roles/investigator.md) | Answer one question before planning or delegation; returns a handoff with `path:line` evidence | Sonnet, medium effort, plan mode, `Read/Grep/Glob/Bash` |
| [reviewer](../roles/reviewer.md) | Independent review of a finished change against acceptance criteria, diff and check results | Opus, high effort, `Read/Grep/Glob/Bash` |

Give the reviewer the plan (or inline acceptance), `base...head`, the worktree path and the checks already run, not the author's conversation. Codex uses the same roles by opening the canonical file, e.g. "review `claude/fix/x` against its plan as described in `.agents/roles/reviewer.md`". New `.claude/agents` files load after a client restart; until then, a general-purpose subagent told to follow the role file is equivalent.

## Model and effort

Pick the tier from the task's risk and ambiguity, not its size. These are starting points; change them when the metrics below show a better fit.

| Work | Tier | Claude Code | Codex |
|---|---|---|---|
| File search, log summaries, classification, mechanical edits | fast | Haiku, low | fast model, low reasoning |
| Investigation before planning | balanced | `investigator` subagent (Sonnet, medium) | balanced model, medium |
| Routine features, tests, docs, inline-plan fixes | balanced | Sonnet or Opus, medium | balanced model, medium |
| Concurrency, persistence, security, public contracts, cross-package refactors, unclear requirements | deep | Opus, high (xhigh when stuck) | strongest model, high |
| Independent review | deep | `reviewer` subagent (Opus, high) | strongest model, high |
| Long multi-hour increments | deep | Fable when available | strongest model, high |

- **Re-tier after investigation.** If the work turns out to touch a contract, schema or several packages, stop and re-plan at the deep tier.
- **Do not escalate for environment failures.** Missing tools, ports, permissions, locales or flaky infrastructure need fixing, not a stronger model.
- **Bound repair loops.** After two unsuccessful fix attempts on the same failing check, stop, record what was tried, and ask the user or escalate.
- Record the exact model ID and effort in the plan's Metrics block.

## Metrics

Plan templates end with a Metrics block (agent/model/effort, elapsed minutes, check-fix loops, user corrections, review verdict). `python3 .agents/scripts/harness.py metrics` summarizes archived plans that filled it, grouped by client, model and effort. Inline-plan changes are not measured. Treat small samples as anecdotes.

## GitHub access

The remote is `origin` → `pigeaca/Agentium` (**public**); the default branch is `main`. Agents use the GitHub CLI (`gh`, Homebrew at `/opt/homebrew/bin/gh`; prepend it to `PATH` if a shell lacks it). The user logs `gh` in with a fine-grained personal access token, and `gh auth setup-git` makes it Git's HTTPS credential helper. Never ask for, print or store the token.

The clone's `origin` uses SSH, and agent shells may have no SSH key registered with GitHub. Push task branches over HTTPS without changing the remote: `git push -u https://github.com/pigeaca/Agentium.git <branch>`, then `git branch --set-upstream-to=origin/<branch>` after a `git fetch origin` succeeds (or leave the upstream unset and always push with the HTTPS URL).

The token must include this repository, with Contents, Pull requests and **Workflows** read/write (GitHub rejects pushes that touch `.github/workflows/` without Workflows), plus Actions and Commit statuses read. Fine-grained tokens cannot have a Checks permission, so `gh pr checks`, the check-runs API and the desktop app's CI monitor fail with 403. Read CI through the Actions API instead:

```bash
gh api 'repos/pigeaca/Agentium/actions/runs?branch=<branch>&per_page=3' --jq '.workflow_runs[] | [.head_sha[0:8], .status, .conclusion, .id] | @tsv'
gh api repos/pigeaca/Agentium/actions/runs/<run-id>/jobs --jq '.jobs[] | [.name, .conclusion, .id] | @tsv'
gh api --allow-escape-sequences repos/pigeaca/Agentium/actions/jobs/<job-id>/logs
```

Recommended repository settings (the user applies them; the token deliberately lacks Administration): a ruleset on `main` that requires a pull request and the CI status checks and blocks force pushes, and "Automatically delete head branches".

## Enforcement

`python3 .agents/scripts/harness.py hooks` points `core.hooksPath` at the tracked `.githooks/` for every worktree of the local repository (each worktree runs its own checkout's hook; branches without `.githooks/` run none). The pre-commit hook runs `check staged`: whitespace errors, credential-shaped added lines, env/key files, gofmt of staged Go files when Go exists, and docs validation. It never runs test suites. CI runs `check ci` (docs and harness tests) on pushes and pull requests to `main`; stack jobs are added with the stack. Prose rules describe the preferred process; the hook and CI are the guarantees.

## Validation

`python3 .agents/scripts/harness.py check docs` validates direct imports, current Markdown links, skill and subagent adapter coverage, entrypoint size and plan-archive consistency. It does not emulate Claude's loader or prove model compliance; a manual Claude session can confirm loaded memory with `/context` and skills in the `/` menu.

Personal `.claude/settings.local.json`, user-level memory and client permissions are separate configuration; this repository does not rewrite or validate them.

Sources checked for the adapter design: [Claude memory/imports](https://code.claude.com/docs/en/memory#import-additional-files), [Claude skill discovery](https://code.claude.com/docs/en/skills#where-skills-live), [Claude subagent frontmatter](https://code.claude.com/docs/en/sub-agents). Recheck these when changing client-specific adapters.
