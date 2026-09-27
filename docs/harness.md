# Harness

Run `python3 .agents/scripts/harness.py <command>` from the repository root, or use the absolute script path from another directory. The harness uses only the Python standard library (3.9+) and Git. It is the single entrypoint for process checks, the pre-commit guard, task worktrees and plan metrics.

| Command | Purpose | When to use |
|---|---|---|
| `doctor` | Show Python and whether git, gh, gofmt and corepack are installed | Environment diagnosis |
| `check docs` | Doc links, Claude imports, skill and subagent adapters, context size, plan archive | Documentation change |
| `check harness` | Harness regression tests (temporary fixtures only) | Harness change |
| `check ci` (also plain `check`) | `docs` + `harness`; what CI runs | Before opening a PR |
| `check changed [--dry-run] [base]` | Select and run the checks for everything changed since the merge base with the remote default branch | Before committing or opening a PR |
| `check staged` | Index-only guard: whitespace, credential-shaped additions, env/key files, staged Go formatting when present, docs | Pre-commit hook |
| `hooks` | Point `core.hooksPath` at the tracked `.githooks/` for all local worktrees | Once per clone |
| `worktree new <branch> [--base REF]` | Task worktree from the fetched remote default branch, without upstream, with offline dependency install | Starting any task |
| `worktree deps` | Offline install of locked dependencies into the current checkout | Existing worktree without dependencies |
| `worktree remove <branch>` | Remove a merged, clean task worktree and delete its local branch (never forced) | After the PR is merged |
| `metrics` | Summarize archived plans' Metrics blocks by client/model/effort | Reviewing model routing |

## Boundaries

Nothing is ever downloaded. The only installation is `worktree new`/`worktree deps`, which runs the lockfile's offline installer (today: `corepack pnpm install --frozen-lockfile --offline` with `COREPACK_ENABLE_NETWORK=0` for any `pnpm-lock.yaml` at the root or one level down). It links only packages already in the local cache and fails instead of fetching. Missing tools are reported by `doctor`; install them only with approval.

## Pre-commit guard

Run `hooks` once per clone. Git then runs `.githooks/pre-commit` from the committing worktree, which calls `check staged`. It checks the index, not the working tree:
- whitespace errors,
- credential-shaped added lines, reported by location, never by value,
- `.env`, key and credential files,
- gofmt of staged Go blobs when Go files exist,
- docs validation.

A fake credential in a test may carry `secret-scan: allow` on the same line. When `gofmt` is missing (some GUI Git clients lack the shell `PATH`), the Go step warns instead of blocking. The hook never runs test suites. `git config --unset core.hooksPath` disables it; bypassing it with `--no-verify` requires explicit user approval.

## Task worktrees

`worktree new claude/fix/login-timeout` fetches the remote default branch, retrying over HTTPS when an SSH GitHub remote fails. It refuses to continue on a stale base unless you pass `--base` explicitly. It creates the branch without an upstream, so a bare push or pull never targets the default branch; the first `git push -u` sets one. The worktree lands at `<repo>-worktrees/claude-fix-login-timeout` beside the primary checkout, and the command installs dependencies offline and prints the path. Branch names must follow the Git rules. If the offline install fails, the worktree is kept and the message names the missing setup.

`worktree remove <branch>` accepts only task branch names. It fetches, then refuses unless the branch is contained in the remote default branch and its worktree has no uncommitted or untracked files. It also refuses ignored files that could be personal work (`.idea/`, `.env.local`, notes); only regenerable ones such as `node_modules`, `dist`, `build` and caches may be deleted with the worktree. It uses `git worktree remove` and `git branch -d` without force, and never removes the current or primary checkout. Squash-merged branches are not detected as merged; remove those by hand after checking.

## Choosing checks

`check changed` diffs the working tree against the merge base with the remote default branch: committed, staged, unstaged and untracked files, with both sides of renames. It maps:
- Markdown, `.agents/`, `.claude/` or `docs/` → `check docs`,
- harness scripts or `.githooks/` → `check harness`,
- CI files → listed as a suggestion (verified by the PR's CI run),
- any other file → a suggestion that it has no mapped check yet.

`--dry-run` prints the selection without running it.

## Adding stack checks

When the stack is chosen, extend the harness in one change:
1. Add `check` scopes for the stack's tests (e.g. `check go` running vet, gofmt, race tests and `govulncheck`), and include them in `check ci`.
2. Add rules to `plan_checks` that map the stack's paths to those scopes (e.g. Go packages to `go test ./pkg/...`), with tests in `ChangedCheckSelection`.
3. Add the stack's lockfile and offline installer to `OFFLINE_INSTALLERS` if it has one.
4. Clear provider credentials and database URLs in the harness `ENV`, so checks never reach live services.
5. Add CI jobs with SHA-pinned actions, and pin tool and toolchain versions.
6. Record the stack in the architecture and a decision record.

Common pieces worth adding with a Go or web stack:
- a combined Go CI scope (vet, gofmt, race tests with a coverage floor, pinned `govulncheck`);
- browser suites that run on a per-worktree port;
- a disposable-database check whose tools get `LC_ALL=C`, because macOS PostgreSQL aborts without a valid locale.
