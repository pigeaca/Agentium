# Harness

Run `python3 .agents/scripts/harness.py <command>` from the repository root, or use the absolute script path from another directory. The harness uses only the Python standard library (3.9+) and Git. It is the single entrypoint for process checks, the pre-commit guard, task worktrees and plan metrics.

| Command | Purpose | When to use |
|---|---|---|
| `doctor` | Show Python, git, gh, corepack, and the Go toolchain resolved for `go.mod` | Environment diagnosis |
| `check docs` | Doc links, Claude imports, skill and subagent adapters, context size, plan archive | Documentation change |
| `check harness` | Harness regression tests (temporary fixtures only) | Harness change |
| `check go` | Go code: gofmt (listing), `go vet ./...`, `go test -race -count=1 ./...` | Go change |
| `check vuln` | govulncheck at the pinned version (reads the online Go vulnerability database). Locally only when the tool is already cached | `go.mod`/`go.sum` change; CI |
| `check ci` (also plain `check`) | `docs` + `harness` + `go` (when `go.mod` exists); what CI runs, followed there by `check vuln` | Before opening a PR |
| `check changed [--dry-run] [base]` | Select and run the checks for everything changed since the merge base with the remote default branch | Before committing or opening a PR |
| `check staged` | Index-only guard: whitespace, credential-shaped additions, env/key files, staged Go formatting when present, docs | Pre-commit hook |
| `hooks` | Point `core.hooksPath` at the tracked `.githooks/` for all local worktrees | Once per clone |
| `worktree new <branch> [--base REF]` | Task worktree from the fetched remote default branch, without upstream, with offline dependency install | Starting any task |
| `worktree deps` | Offline install of locked dependencies into the current checkout | Existing worktree without dependencies |
| `worktree remove <branch>` | Remove a merged, clean task worktree and delete its local branch (never forced) | After the PR is merged |
| `metrics` | Summarize archived plans' Metrics blocks by client/model/effort | Reviewing model routing |

## Boundaries

Nothing is ever downloaded. The only installation is `worktree new`/`worktree deps`, which runs the lockfile's offline installer (today: `corepack pnpm install --frozen-lockfile --offline` with `COREPACK_ENABLE_NETWORK=0` for any `pnpm-lock.yaml` at the root or one level down). It links only packages already in the local cache and fails instead of fetching. Missing tools are reported by `doctor`; install them only with approval.

## Go toolchain

`go.mod` pins the exact Go version (`go 1.27.1`). The harness uses `go` from `PATH` when its version matches, otherwise `~/sdk/go<version>/bin/go` (where `golang.org/dl` installs it). If neither matches, it stops and prints the install command; nothing is installed without approval. Every Go command runs with:
- `GOTOOLCHAIN=local`, so Go never downloads a toolchain;
- `GOFLAGS=-mod=readonly`, so builds never rewrite `go.mod` or `go.sum`;
- provider credentials removed, so checks never reach live services.

`check vuln` runs `golang.org/x/vuln/cmd/govulncheck` at the version pinned in the harness. Locally it runs only if that version is already in the module cache (with `GOPROXY=off`), and otherwise it says it was skipped. CI (`CI=true`) may download that exact version.

## Pre-commit guard

Run `hooks` once per clone. Git then runs `.githooks/pre-commit` from the committing worktree, which calls `check staged`. It checks the index, not the working tree:
- whitespace errors,
- credential-shaped added lines, reported by location, never by value,
- `.env`, key and credential files,
- gofmt of staged Go blobs, using the pinned toolchain's gofmt,
- docs validation.

A fake credential in a test may carry `secret-scan: allow` on the same line. When `gofmt` is missing (some GUI Git clients lack the shell `PATH`), the Go step warns instead of blocking. The hook never runs test suites. `git config --unset core.hooksPath` disables it; bypassing it with `--no-verify` requires explicit user approval.

## Task worktrees

`worktree new claude/fix/login-timeout` fetches the remote default branch, retrying over HTTPS when an SSH GitHub remote fails. It refuses to continue on a stale base unless you pass `--base` explicitly. It creates the branch without an upstream, so a bare push or pull never targets the default branch; the first `git push -u` sets one. The worktree lands at `<repo>-worktrees/claude-fix-login-timeout` beside the primary checkout, and the command installs dependencies offline and prints the path. Branch names must follow the Git rules. If the offline install fails, the worktree is kept and the message names the missing setup.

`worktree remove <branch>` accepts only task branch names. It fetches, then refuses unless the branch is contained in the remote default branch and its worktree has no uncommitted or untracked files. It also refuses ignored files that could be personal work (`.idea/`, `.env.local`, notes); only regenerable ones such as `node_modules`, `dist`, `build` and caches may be deleted with the worktree. It uses `git worktree remove` and `git branch -d` without force, and never removes the current or primary checkout. Squash-merged branches are not detected as merged; remove those by hand after checking.

## Choosing checks

`check changed` diffs the working tree against the merge base with the remote default branch: committed, staged, unstaged and untracked files, with both sides of renames. It maps:
- Markdown, `.agents/`, `.claude/` or `docs/` → `check docs`,
- harness scripts or `.githooks/` → `check harness`,
- `*.go`, `go.mod` or `go.sum` → `check go`, and `go.mod`/`go.sum` also → `check vuln`,
- CI files → listed as a suggestion (verified by the PR's CI run),
- any other file → a suggestion that it has no mapped check yet.

`--dry-run` prints the selection without running it.

## Adding stack checks

Go is in place: `check go`, `check vuln`, the `check changed` rules, the environment and the CI job. When the React UI arrives in Phase 2, extend the harness in one change:
1. Add a `check web` scope (type check, lint, unit and browser tests) and include it in `check ci`.
2. Map the UI's paths to it in `plan_checks`, with tests in `ChangedCheckSelection`.
3. Register the lockfile's offline installer in `OFFLINE_INSTALLERS` (for example `pnpm-lock.yaml`).
4. Add CI steps with SHA-pinned actions, pin the Node and package-manager versions, and record the toolchain in a decision.

Browser suites should run on a per-worktree port, so parallel worktrees don't collide.
