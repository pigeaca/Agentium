# Harness

Run `python3 scripts/harness.py <command>` from the repository root, or use the absolute script path from another directory. The harness uses only the Python standard library (3.9+) and Git, and runs on POSIX systems only (macOS, Linux), because the test slots use `fcntl`. It is the single entrypoint for process checks, the pre-commit guard, task worktrees, landing PRs and plan metrics.

| Command | Purpose | When to use |
|---|---|---|
| `doctor` | Show Python, git, gh, corepack, and the Go toolchain resolved for `go.mod` | Environment diagnosis |
| `check docs` | Doc links, Claude imports, skill and subagent adapters, context size, plan archive | Documentation change |
| `check harness` | Harness regression tests (temporary fixtures only) | Harness change |
| `check go` | Go code: gofmt (listing), `go vet ./...`, `go test -race -count=1 ./...` (from `check changed`: only affected packages), the tests holding one of two [per-clone test slots](#test-slots) | Go change |
| `check vuln` | govulncheck at the pinned version (reads the online Go vulnerability database). Locally only when the tool is already cached | `go.mod`/`go.sum` change; CI |
| `check ci` (also plain `check`) | `docs` + `harness` + `go` (when `go.mod` exists); what CI runs, followed there by `check vuln` | Before opening a PR |
| `check changed [--dry-run] [base]` | Select and run the checks for everything changed since the merge base with the remote default branch; like `check ci`, ends with `[harness] exit=<code>` | Before committing or opening a PR |
| `check staged` | Index-only guard: whitespace, credential-shaped additions, env/key files, staged Go formatting when present, docs | Pre-commit hook |
| `hooks` | Point `core.hooksPath` at the tracked `.githooks/` for all local worktrees | Once per clone |
| `worktree new <branch> [--base REF]` | Task worktree from the fetched remote default branch, without upstream, with offline dependency install | Starting any task |
| `worktree deps` | Offline install of locked dependencies into the current checkout | Existing worktree without dependencies |
| `worktree remove <branch>` | Remove a merged, clean task worktree and delete its local branch (never forced) | After the PR is merged |
| `pr land <N> [--dry-run] [--update] [--timeout MINUTES]` | Wait for CI on PR N's up-to-date head commit, then merge it; never on red or pending CI ([details](#landing-pull-requests)) | Under a user's merge authorization |
| `metrics` | Summarize archived plans' Metrics blocks by client/model/effort | Reviewing model routing |

## Boundaries

No toolchain, module or tool is ever downloaded locally. The exceptions: `check vuln` reads the online Go vulnerability database, and CI (`CI=true`) may download pinned tools and modules. The only installation is `worktree new`/`worktree deps`, which runs the lockfile's offline installer (today: `corepack pnpm install --frozen-lockfile --offline` with `COREPACK_ENABLE_NETWORK=0` for any `pnpm-lock.yaml` at the root or one level down). It links only packages already in the local cache and fails instead of fetching. Missing tools are reported by `doctor`; install them only with approval.

## Go toolchain

`go.mod` pins the exact Go version (`go 1.27.1`; a `toolchain` line, if one is ever added, takes precedence, as it does for setup-go). The harness uses `go` from `PATH` when its version matches, otherwise `~/sdk/go<version>/bin/go` (where `golang.org/dl` installs it). Tools such as gofmt come from that toolchain's `GOROOT`, so a symlinked `go` works. If neither matches, it stops and prints the install command; nothing is installed without approval. Every Go command runs with:
- `GOTOOLCHAIN=local`, so Go never downloads a toolchain;
- `GOFLAGS=-mod=readonly`, so builds never rewrite `go.mod` or `go.sum` (it replaces any personal `GOFLAGS`);
- locally, `GOPROXY=off` for `check go`, so a dependency missing from the module cache fails instead of downloading. Fetch new dependencies once, with approval, using `go mod download`;
- provider credentials removed, so checks never reach live services.

`check vuln` runs `golang.org/x/vuln/cmd/govulncheck` at the version pinned in the harness. Locally it runs only if that version and its dependencies are already in the module cache (with `GOPROXY=off`), and otherwise it says it was skipped. To enable it locally, fetch it once with approval: `GOFLAGS= go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -version`. CI (`CI=true`) may download that exact version.

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
- `*.go`, `go.mod`, `go.sum`, any file under `cmd/` or `internal/` (embedded migrations, test data), or the judge pilot's script → `check go`, and `go.mod`/`go.sum` also → `check vuln`,
- CI files → listed as a suggestion (verified by the PR's CI run),
- any other file → a suggestion that it has no mapped check yet.

`--dry-run` prints the selection without running it.

**Tests run by `check changed`:** its `check go` still formats and vets everything, but it runs the race tests only for the packages the change can affect:
- each changed file's package (embedded files and test data count for the nearest package folder above them);
- every package whose test binary depends on one of those, according to `go list -test`.

It tests every package when `go.mod` or `go.sum` changed, when `go list` fails, or when a changed path maps to no package: a deleted package, or a file outside `cmd/` and `internal/` that tests read, such as the judge pilot's script. It assumes a test that reads another package's files also imports that package, as every such test here does. `--dry-run` also prints the packages it would test. `check go` on its own, `check ci` and CI always test every package.

## Test slots

Parallel worktrees running `go test -race` at once starve each other: three at once slowed one package's tests from 25 s to several minutes. So `check go`, whether run directly, from `check changed` or from `check ci`, takes one of two per-clone slots before its race tests. The slots are `agentium-test-slot-<n>.lock` files in the git common dir (`git rev-parse --git-common-dir`), which every worktree of the clone shares, held with `flock` (outside a git checkout, in the system temp dir). Each slot is tried without blocking; when both are taken, the harness prints `[harness] waiting for a test slot (another worktree is running tests)` once and retries every slot twice a second, taking whichever frees first. The kernel releases a slot when the process exits for any reason, including Ctrl-C, so there is never a stale lock. gofmt and `go vet` run without a slot.

## Exit line

`check changed` and `check ci` (and plain `check`) end with `[harness] exit=<code>`: 0 on success, otherwise the failing code, also for a usage error. Report that line; an exit status read through a pipe such as `| tail` is the pipe's, not the check's.

## Landing pull requests

`pr land <N>` is the harness's auto-merge (GitHub's own can't wait for CI on this private, free-plan repository without branch protection). It calls `gh` with `--repo <owner>/<name>` taken from the `origin` remote (HTTPS or SSH) and:
1. refuses at once, with exit 1 and the reason, a PR that is not open, is a draft, has conflicts, or targets a branch other than the repository's default;
2. refuses a head that lacks the base branch's current tip (`compare/<base>...<head>` reports `behind_by > 0`): "update the branch (gh pr update-branch N), then land again". With `--update` it runs `gh pr update-branch <N>` instead (GitHub merges the base into the PR branch) and waits on the new head. Requiring an up-to-date head means CI tested exactly what the merge produces;
3. polls the Actions API (`actions/runs?head_sha=<sha>&event=pull_request`) every 20 s, up to `--timeout` minutes (default 40), until every `pull_request` run of the workflow named `CI` for that commit and PR has completed;
4. merges with `gh pr merge <N> --merge --match-head-commit <sha>` when they all passed, so GitHub refuses the merge if another commit arrived in between;
5. exits 1 naming the run's URL on failure or cancellation, and on timeout; it never merges then.

The PR is re-read on every poll: a new head commit restarts the wait on that commit within the same deadline (its old CI run, cancelled by the push, doesn't count), and a PR that becomes closed, draft, conflicting or out of date is refused (or updated, with `--update`). A failed GitHub read is retried twice, after 5 and 10 s; the merge and the update are never retried. A base that moves between the last poll and the merge is not caught; the next PR's CI runs on the result. `--dry-run` reports the PR, its head, whether it is up to date, its CI state and what it would do, without waiting, updating or merging. It never pushes, and changes no branch except through `--update`. Use it only under the user's [merge authorization](../.agents/rules/git-workflow.md#merge-authorization), with a `--timeout` that ends inside its window.

## Adding stack checks

Go is in place: `check go`, `check vuln`, the `check changed` rules, the environment and the CI job. There is no web UI ([decision](../.agents/decisions/2026-09-30-console-instead-of-web-ui.md)). If a frontend or another stack is added later, which needs a new decision, extend the harness in one change:
1. Add a `check web` scope (type check, lint, unit and browser tests) and include it in `check ci`.
2. Map the UI's paths to it in `plan_checks`, with tests in `ChangedCheckSelection`.
3. Register the lockfile's offline installer in `OFFLINE_INSTALLERS` (for example `pnpm-lock.yaml`).
4. Add CI steps with SHA-pinned actions, pin the Node and package-manager versions, and record the toolchain in a decision.

Browser suites should run on a per-worktree port, so parallel worktrees don't collide.
