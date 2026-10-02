# Git workflow

## Standing authorization

Within the assigned task, agents may inspect and fetch, create task branches and worktrees, stage owned changes, commit, push task branches to the repository's existing remote, and create or update a PR. No repeated permission request is needed. User instructions such as "local only", "do not commit" or "leave on this branch" override the relevant steps. Tool/sandbox permissions and repository branch protection still apply.

**Done means a PR, not a local merge.** A task that changes files is complete when its commits are pushed, a PR against the remote default branch is open, the required CI checks pass (or each failure is explained as unrelated and pre-existing), and the review below is recorded. The user merges the PR, with one [exception](#merge-authorization). Do not report uncommitted, unpushed or red work as complete, and do not stop at implementation or merely offer to commit.

Merge into the local default branch only when publishing is unavailable (no remote, no access, or a failed push) or the user asks for local-only work. In that case, merge non-fast-forward from a clean, idle checkout and report publication as blocked.

Read-only analysis needs no commit. Before publishing, confirm the existing remote, intended base and complete diff; never guess a new destination or include unrelated staged work. This repository is public: everything pushed is published. Report remote access or approval failures accurately; never bypass a rejected operation. Git authorization does not authorize releases, deployment, provider calls or tracker/chat messages. GitHub access and reading CI are described in [agent setup](../reference/agent-setup.md#github-access).

## Branch names

Use `<agent>/<type>/<short-kebab-case-topic>`; add a real issue ID when provided. Codex uses `codex/`, Claude uses `claude/`, other clients use their configured prefix or `agent/`.

| Type | Example | Purpose |
|---|---|---|
| `feat` | `codex/feat/csv-export` | New behavior |
| `fix` | `claude/fix/login-timeout` | Bug fix |
| `analysis` | `codex/analysis/cache-strategy` | Research/prototype/report that needs files |
| `refactor` | `codex/refactor/config-loading` | Internal restructuring |
| `docs` | `claude/docs/agent-onboarding` | Documentation |
| `test` / `chore` | `codex/test/retry-backoff` | Verification / maintenance |

One task branch per independently writable worktree. Base new work on the remote default branch discovered from Git (currently `origin/main`), not on a possibly stale local branch; use another base when continuing an existing task/PR or an explicit dependency. Keep an existing suitable branch/worktree rather than renaming it for cosmetic consistency.

## Completing and publishing work

1. Inspect branch, status, staged diff and worktrees. Preserve the existing index and all unrelated changes. Do not switch a dirty/shared checkout; isolate new work in a task worktree.
2. Stage explicit owned paths/hunks. Review staged content and run the focused checks for the coherent change (`harness.py check changed` selects them). The shared pre-commit hook runs `harness.py check staged` (whitespace, credential-shaped additions, env/key files, Go formatting when present, docs); fix its findings rather than bypassing it, and use `--no-verify` only with explicit user approval. Never use blanket staging in a mixed checkout. If ownership is ambiguous, preserve it and isolate or ask about ownership.
3. Commit coherent changes with `type(scope): concise outcome`, e.g. `fix(auth): refresh expired tokens`. Record verification and limitations in the plan/PR, not generated logs or secrets. Do not create empty commits for analysis.
4. Push the task branch with its upstream and open a PR against the remote default branch using the [PR template](../../.github/pull_request_template.md). Open it as a draft while work or checks are incomplete.
5. **Review.** Unless the change is docs-only or used an inline plan, get an independent review from the read-only [reviewer role](../roles/reviewer.md), giving it the acceptance criteria, `base...head`, the worktree path and the checks run. Fix confirmed findings or record why not. For concurrency, persistence, security or public-contract changes, also ask the user for a review from the other client (Codex or Claude). Record the verdict in the PR.
6. Read the PR's CI results. Fix failures caused by the change on the same branch; explain pre-existing ones. Mark the PR ready when checks pass and the review is recorded. Report the PR URL, commit IDs, CI state and anything left for the user.

Rebase is allowed on an unpublished, exclusively owned branch. Coordinate shared changes; do not overwrite them to resolve conflicts.

## After the user merges

Sync the local default branch by fast-forward only (fetch, over HTTPS if SSH is unavailable, see [GitHub access](../reference/agent-setup.md#github-access); then `git merge --ff-only <remote>/<default>` in the clean primary checkout). If it has diverged, report it instead of resetting. Then remove the idle task worktree and its merged local branch (`harness.py worktree remove <branch>`). GitHub's "automatically delete head branches" setting removes the remote branch; otherwise leave it for the user.

Separate explicit approval is required for force-push, rewriting published/shared history, destructive reset/clean, deleting remote branches, direct pushes to protected/default branches, merging PRs (except under a [merge authorization](#merge-authorization)), release-branch merges, tags/releases and deployment.

### Merge authorization

Only the user grants a merge authorization, directly, with a scope and a time window; an agent relaying one does not count. While it holds, the coordinator may merge a PR in that scope whose recorded review is clean (or not needed: docs-only or inline-plan), either directly once CI passed or by launching `harness.py pr land <N>` ([harness](../../docs/harness.md#landing-pull-requests)). The merge must complete inside the window: give `pr land` a `--timeout` no longer than the time left. Never merge on red or pending CI.

## Worktree lifecycle

Use the [parallel-work rules](collaboration.md). Create task worktrees with `python3 scripts/harness.py worktree new <branch>` (or the client's managed worktree tool); inspect existing worktrees first. Never copy the repository. Remove only an owned, idle checkout after its work is merged or recoverably saved; `worktree remove` refuses dirty or unmerged work and never forces. Never delete a directory containing unaccounted work. Branch/worktree cleanup is not permission to close a PR.
