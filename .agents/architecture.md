# Architecture

Status: **hybrid strategy and Go + SQLite stack accepted** ([decision](decisions/2026-09-28-stack-go-react-sqlite.md); [no web UI](decisions/2026-09-30-console-instead-of-web-ui.md)). Design: the [feasibility study](../docs/research/2026-09-27-ai-development-lab.md). Phase 1 is done ([plan](plans/archive/2026-09-28-phase1-context-ab-cli.md)).

## Product concept

Agentium runs coding agents (Claude Code and Codex first) on tasks from a developer's own repository. It compares agents, models and versions of the project's AI context (`AGENTS.md`, `CLAUDE.md`, skills, rules) on correctness, cost and speed. Context versions are first-class experiment arms. Results use a paired design with repeats and report plain verdicts: improved, regressed, no loss beyond the margin, or inconclusive.

Shape:
- **Hybrid.** Agentium owns context snapshots, tasks, experiment design, statistics and the UX.
- **Local runs** drive agent CLIs headlessly in isolated checkouts.
- **Containers** later, driven directly through the Docker CLI ([decision](decisions/2026-10-02-containers-direct-docker.md)).
- **One binary:** a Go core (Go 1.27.1, module `github.com/pigeaca/agentium`) and SQLite through `mattn/go-sqlite3`, used from the console.

## Go conventions

- `cmd/agentium` only wires packages together; code lives in `internal/<package>`.
- The standard library comes first. Every new module needs [approval](rules/supply-chain.md); `go.sum` is committed.
- Every blocking call takes a `context.Context`, and cancellation stops agent processes.
- Errors are wrapped with `%w` and context; there is no package-level mutable state.
- I/O goes through parameters (`io.Writer`, `Env`) so commands can be tested.
- Exit codes: 0 success, 1 runtime failure, 2 usage error.

## Code map

| Path | Responsibility |
|---|---|
| `AGENTS.md`, `CLAUDE.md`, `.agents/` | Shared instructions: rules, references, roles, skills, templates, plans, decisions |
| `go.mod`, `cmd/agentium` | Go module (pinned toolchain) and the `agentium` binary's entrypoint |
| `internal/cli` | Command-line parsing, dispatch and printing; each handler calls one service; `start` composes setup |
| `internal/term` | Console styles (terminal only, `NO_COLOR`) and fitted tables |
| `internal/home` | The owner-only data folder (`~/.agentium` or `AGENTIUM_HOME`), outside every repository: database, artifacts, workspaces, records, `deps/`, Agentium's own caches and temp files, the run lock |
| `internal/store` | SQLite through `mattn/go-sqlite3` (cgo, WAL, foreign keys); embedded, ordered migrations; projects, snapshots, tasks, runs, calibrations and experiments |
| `internal/project` | Read-only discovery for `init`: git root and commit, Claude Code path and version, sign-in mode (presence only), test commands, instruction files |
| `internal/mine` | Task candidates from git history: explained scores, rejections, import |
| `internal/gitx` | Every git call (hooks, fsmonitor, prompts and optional index writes off; inherited `GIT_*` dropped); hook-free fetch of task bases |
| `internal/ghx` | GitHub through the user's own `gh` (never inside a run): the open pull request of a commit, one marked comment edited in place (only the user's own), a warn-only commit status; the repository from the git remote |
| `internal/source` | Read-only views of a commit or the working tree; symlinks followed only to the repository's own files |
| `internal/claudectx` | Which files Claude Code loads in experiments (instructions, `@` imports, rules, skill/subagent/command descriptions, harness files, linked documents); warnings; lint and its hook |
| `internal/snapshot` | Context versions as parentless commits in the data folder's `projects/<id>/repo.git`; diffs; overlays that refuse code or configuration changes and report harness changes |
| `internal/checkout` | Isolated working copies: a fresh repository holding only the base commit (depth 1), so hidden tests and solutions are unreachable; safe file writes |
| `internal/runner` | Commands in their own process group, with a timeout and no credentials (`EnvPolicy`); a gentle stop (SIGINT, then SIGKILL); leftover groups killed and reported |
| `internal/buildtool` | Build-tool profiles (Go, Maven, Gradle, Cargo): commands, caches, offline deps, warm-up, daemons, local binding |
| `internal/claude` | Claude Code headless and isolated (project settings only, no connectors, fixed permission mode, a sandbox without network, denied paths, its own build cache and temp root, an allowlisted environment); stream-json metrics, outcomes, drift |
| `internal/task` | Tasks (base, instruction, verification; a solution split into hidden tests and reference by test-file rules), validation (per arm, batch; flaky, weak-test checks), and unstated-requirement gaps |
| `internal/experiment` | Designs (context A/B and A/A, eligible tasks, caps, margins, seed), services for `new`, `plan`, readiness, `run` (window, budget, usage pauses, retries, stop rules, resume); the lock and schedule; counting and analysis (roles, floors, verdicts, noise) |
| `internal/judge` | The opt-in LLM judge, which decides nothing: the pilot's prompts, code-only diffs, majority of repeats, pairs in both orders |
| `internal/stats` | Paired analysis: the two-stage cluster bootstrap, t-intervals, variance components with ranges, detectable effects and verdict rules (§5.6); reproduces the Phase 0 spike |
| `internal/report` | An experiment's report: verdicts in words, metrics with both intervals, noise, context, its use and costs, behavior, the judge, per-task results and notes, for a terminal, Markdown or JSON (no personal names or paths); `Load` reads the store; the north star (time and spend to the first decisive verdict) |
| `internal/pricing` | Anthropic's dated list prices per model, for cost estimates and transcripts without Claude Code's cost |
| `internal/run` | One run or calibration: the arm's workspace with everything else denied, hidden grading, behavior flags, context use, redacted records; its spend (`Spend`) and isolated-run cost; recovery of dead runs; its temp root |
| `scripts/harness.py` | Standard-library entrypoint: checks, hooks, worktrees, metrics, PR merges ([harness](../docs/harness.md)) |
| `.claude/agents`, `.claude/skills` | Thin Claude adapters over `.agents/roles` and `.agents/skills` |
| `.githooks/pre-commit` | Shared pre-commit guard |
| `.github/` | CI, the PR template, issue forms, community files |
| `docs/research/` | Product research; history, not default context |
