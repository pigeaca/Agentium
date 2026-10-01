# Automation: Agentium without anyone running it

- Date: 2026-10-01
- Status: Planned (2026-10-01): on the user's Mac, driven by hooks with no daemon (see Decision). Requested ("How it can be automated? … introduce it in roadmap"). Phases start in the [next chapter](2026-10-01-next-chapter.md)'s waves 2–3. Paid steps (the Linux spike, real CI runs) need their own approval.
- Scope: how Agentium runs on its own in the user's development apps: on every change to AI context, on a schedule, and as new code lands.

## Why
Today a person runs every command. The value comes from three questions:
- Did this change to `CLAUDE.md`, skills or settings make the agent better, worse, or cost more?
- Did a new Claude Code version or model change results on our tasks?
- Do we have enough good tasks to answer those questions?

Each can run on a trigger, inside a budget, and report where the team already looks: the pull request, a weekly digest.

## Decision (the user, 2026-10-01): on the user's Mac, driven by hooks, no daemon
Nothing stays running. An event triggers one job, and the job exits.
- **Supply:** a git `post-merge` hook (after `git pull`), or a launchd job watching `.git/FETCH_HEAD` (any fetch), runs `agentium pool update --background`.
- **Fast check:** a git `pre-push` hook. When the pushed commits touch context paths (`CLAUDE.md`, `AGENTS.md`, `.claude/**`, rules, `.mcp.json`, settings), it runs `agentium ci check --background --comment`, which finds the pull request through `gh` and comments when the check ends. The push is never blocked.
- **Instant feedback:** a Claude Code `PostToolUse` hook, in the user's own settings (Agentium's runs load project settings only, so it cannot fire inside them). On edits to context files it runs a quick context check: size change, broken imports, warnings. No agent runs and no cost.
- **Deep watch:** a launchd calendar entry (nightly, or hourly) runs `agentium watch --once`. That is one budgeted pass in an idle usage window, and it picks up teammates' labelled pull requests that no local hook saw.
- **Background jobs:**
  - they are detached, logged to the data folder, and serialized by Agentium's run lock;
  - each head commit is checked once;
  - a pending job is queued, never run twice.
- **Installing:** `agentium hooks print git|claude|launchd` prints the snippets, chained with existing hooks such as this repository's `.githooks`. The user installs them; Agentium never writes the repository or the user's settings.
- **What it needs from the user:** a `gh` login allowed to comment on pull requests (the current fine-grained token cannot read checks, so comments carry the result), and the opt-in label for pull requests that hooks don't see.
- **Why:**
  - Hooks run only on events, so nothing polls.
  - The macOS sandbox recipes are proven.
  - The data folder persists, so base runs and calibrations are reused.
  - The subscription's usage windows are used rather than API prices.
- **Limit:** hooks see only this machine's pulls and pushes; the scheduled pass covers the rest.
- **Hosted runners stay later.** They need the Linux spike (A3), secrets, and a cached data folder.
- **Credentials on hosted runners** (asked by the user on 2026-10-01):
  - **A Claude Code sign-in, chosen per team:**
    - `ANTHROPIC_API_KEY` (billed per token at API prices, bounded by the per-pull-request and monthly budgets);
    - or a `claude setup-token` token as `CLAUDE_CODE_OAUTH_TOKEN` (the subscription's usage windows, shared with the user's own sessions; the usage gate pauses near the limit; check the plan's terms for automated use).

    Agentium's `api-key` and `token-file` sign-in modes already support both. The secret reaches Claude Code only, and the sandbox denies those variables to the agent's shell.
  - **A GitHub token:** the workflow's own `GITHUB_TOKEN` with `pull-requests: write`, for comments; no extra secret.
  - **Forks:** GitHub gives no secrets to workflows from fork pull requests, which matches "trusted pull requests only".
  - **On the user's Mac:** none of these. `watch` uses the existing Claude Code login and the `gh` login.

## Design: three loops

### 1. Supply: a task pool that keeps itself fresh
- **Trigger:** every merge to the default branch (CI on push), or a schedule.
- **Steps:** `agentium pool update` runs `task mine --since <last>` on the new commits, validates the candidates in a batch, re-validates tasks whose checks are stale, and retires tasks whose base is too old.
- **Cost:** no agent runs, only local builds and tests. It costs CPU time, not money.
- **Output:** a pool-health line: valid tasks, how many are flaky or weak, how many await review. The pool is what the other two loops draw from.

### 2. Fast check: every pull request that changes AI context
- **Trigger:** a pull request in the same repository that touches `CLAUDE.md`, `AGENTS.md`, `.claude/**`, `.agents/**`, `.mcp.json` or the configured paths, or a bump of the Claude Code version or model in settings.
  - Opt-in with a label or a config, and never for pull requests from forks.
- **Steps:** `agentium ci check --base <ref> --head <ref>` snapshots the base and head contexts and picks K tasks from the pool. It runs a small paired A/B within the pull request's budget.
  - It reuses the base arm's runs from earlier checks of the same base context ([cheaper verdicts](2026-10-01-next-chapter.md), wave 2) and stops early.
  - The judge gives a second opinion when it is on.
- **What it can honestly say in minutes and a few dollars:**
  - a cost, time and token verdict, which is achievable at 8 tasks × 1 run;
  - a success smoke check: tasks that passed on the base and fail on the head are listed;
  - the context's size and what the runs used of it.

  Success verdicts need the deep loop.
- **Output:**
  - a pull request comment (the Markdown report) and a check;
  - JSON as an artifact;
  - an exit code set by policy: fail on "regressed", warn on "inconclusive", pass on "improved" or "no loss".
- **Cost guardrails:** a per-pull-request budget (default $5), a monthly cap, and subscription usage windows.

### 3. Deep watch: scheduled evaluation on the default branch
- **Trigger:** nightly or weekly, a new Claude Code version, a new model, or an experiment that has collected enough runs.
- **Steps:** `agentium watch` continues long experiments a budgeted slice at a time.
  - It reuses runs and stops early, building toward success verdicts over days instead of one expensive session.
  - It re-runs an A/A baseline when Claude Code or the model changes, to catch drift.
  - It can run a model and effort comparison when a new model appears.
- **Output:** history and trends (`agentium history`) and a digest: a GitHub issue or discussion through `gh`, or a Markdown file the CI job publishes. Alerts on drift.

## Shared foundations
- **Headless mode:**
  - every command has `--json`, stable exit codes and no prompts;
  - one non-interactive path from a fresh clone (`agentium start --yes`);
  - a repository config file the team commits, `agentium.toml`, holding triggers, budgets, task selection, the gate policy, the model and the judge. Agentium reads it and never writes the repository.
- **State between runs:**
  - The data folder (SQLite, records, snapshots) persists between CI jobs, so runs and calibrations can be reused.
  - On a self-hosted runner or a developer machine it simply stays on disk. On hosted runners it needs the CI cache, which is a limit to measure.
- **Where it runs:**
  - **Recommended first:** a self-hosted runner or the developer's machine (macOS). The sandbox recipes are proven there, the data folder persists, and a subscription login with its usage windows works (`watch` can wait for the window to reset overnight).
  - **Hosted Linux runners later:** Agentium's Linux sandbox is not yet verified on a real run. An API key or a `claude setup-token` token goes in CI secrets; sign-in modes already support both.
- **Packaging:** a GitHub Action, a thin wrapper that installs `agentium` and restores the data folder. It runs `pool update`, `ci check` or `watch`, uploads the reports and posts the comment.
- **Security:**
  - only trusted pull requests (same repository, opt-in label);
  - secrets scoped to the job;
  - the agent sandbox as for local runs;
  - [temp isolation](2026-10-01-run-temp-isolation.md) first, since runners are shared.

## Later: autopilot (exploratory)
- **The idea:** Agentium proposes context changes from its own data, for example dropping files and skills no run used ([context use](archive/2026-09-30-task-checks-context-use.md)), or a shorter `CLAUDE.md`. It tests each one in the deep loop and opens a pull request with the winner and its report.
- **Needs:** run reuse, early stopping, the judge, and trust in the fast check. It is the user who merges, never Agentium.

## Phases
| Phase | Wave | What | Needs | Moves |
|---|---|---|---|---|
| A1 Headless foundation | 2 | `--json` and exit codes everywhere, non-interactive `start --yes`, `agentium.toml` | quick start | time ↓ |
| A2 Supply loop | 2 | `agentium pool update --background` and pool health; the `post-merge` hook | [task mining](2026-10-01-task-mine.md) | time ↓↓ |
| A3 Linux runs (spike, paid; approval) | Later | a real run in Claude Code's Linux sandbox; a recipe or a list of gaps (only for hosted runners) | temp isolation | reach |
| A4 Fast check, local | 3 | `agentium ci check` with `--background` and `--comment`, the policy exit codes, `agentium hooks print git`, the Claude Code context hook (a GitHub Action comes later, for hosted runners) | A1, A2, run reuse, early stopping | reach ↑↑, $ ↓ |
| A5 Deep watch | 3 | `agentium watch --once` from a launchd calendar entry (usage windows, overnight), drift on new Claude Code versions and models, history and digest; `hooks print launchd` | A1, run reuse, early stopping | trust, reach |
| A6 Autopilot | Later | propose, test and open pull requests with context changes | A4, A5, the judge | time ↓, $ ↓ |

## Acceptance (per phase, refined when it starts)
1. **A1:** a fresh clone reaches a preview with no prompts (`start --yes`). Every command's `--json` is documented, and exit codes are fixed by tests.
2. **A2:** after a merge, `pool update` adds the new commit's task within one CI job, with no agent runs. Pool health is printed and in JSON.
3. **A3:** a recipe for Linux runs, or a written list of gaps, from one short real session.
4. **A4:** on a test repository, a pull request that changes `CLAUDE.md` gets a comment and a check within the budget. A fork pull request does not trigger it, and the exit code follows the policy. Measured: minutes and dollars per check.
5. **A5:** a scheduled job continues an experiment toward a success verdict across several days within its budget, and a Claude Code version change triggers the drift check.

## Boundaries
- Agentium never writes the user's repository: comments, checks and digests go through the CI job or `gh`.
- No new Go modules without approval.
- Real runs in CI only on trusted pull requests.

## Verification
Per phase: unit and CLI tests with a fake Claude Code; one real end-to-end run on a test repository (paid; approval); a reviewer.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
