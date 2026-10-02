# Code map

Where each part of Agentium lives. Loaded on demand (not default context): read it when you need to find or place code. The [architecture](../architecture.md) holds the shape and conventions; code owns the details.

| Path | Responsibility |
|---|---|
| `AGENTS.md`, `CLAUDE.md`, `.agents/` | Shared instructions: rules, references, roles, skills, templates, plans, decisions |
| `go.mod`, `cmd/agentium` | Go module (pinned toolchain) and the `agentium` binary's entrypoint |
| `internal/cli` | Command-line parsing, dispatch and printing; each handler calls one service; `start` composes setup |
| `internal/term` | Console styles (terminal only, `NO_COLOR`) and fitted tables |
| `internal/home` | The owner-only data folder (`~/.agentium` or `AGENTIUM_HOME`), outside every repository: database, artifacts, workspaces, records, `deps/`, Agentium's own caches and temp files, the run lock and file locks that wait until cancelled |
| `internal/store` | SQLite through `mattn/go-sqlite3` (cgo, WAL, foreign keys); embedded, ordered migrations; projects, snapshots, tasks, runs, calibrations, experiments |
| `internal/project` | Read-only discovery for `init`: git root and commit, Claude Code path and version, sign-in mode (presence only), test commands, instruction files |
| `internal/mine` | Task candidates from git history: explained scores, rejections, import; the pool's bounded range scan with patch IDs |
| `internal/pool` | The task pool: state, watermark, a pass and its recovery; stale, retire and health rules; toolchain versions |
| `internal/gitx` | Every git call (hooks, fsmonitor, prompts and optional index writes off; inherited `GIT_*` dropped); hook-free fetch of task bases, one at a time per repository |
| `internal/ghx` | GitHub through the user's own `gh` (never inside a run): the open pull request of a commit, one marked comment edited in place (only the user's own), a warn-only commit status; the repository from the git remote |
| `internal/source` | Read-only views of a commit or the working tree; symlinks followed only to the repository's own files |
| `internal/claudectx` | Which files Claude Code loads in experiments (instructions, `@` imports, rules, skill/subagent/command descriptions, harness files, linked documents); warnings; lint and its hook |
| `internal/snapshot` | Context versions as parentless commits in the data folder's `projects/<id>/repo.git`; diffs; overlays that refuse code or configuration changes and report harness changes |
| `internal/checkout` | Isolated working copies: a fresh repository holding only the base commit (depth 1), so hidden tests and solutions are unreachable; safe file writes |
| `internal/runner` | Commands in their own process group, with a timeout and no credentials (`EnvPolicy`); a gentle stop (SIGINT, then SIGKILL); leftover groups killed and reported |
| `internal/buildtool` | Build-tool profiles (Go, Maven, Gradle, Cargo): commands, caches, offline deps, warm-up, daemons, local binding |
| `internal/claude` | Claude Code headless and isolated (project settings only, no connectors, fixed permission mode, a sandbox without network, denied paths, its own build cache and temp root, an allowlisted environment); stream-json metrics, outcomes, drift |
| `internal/task` | Tasks (base, instruction, verification; a solution split into hidden tests and reference by test-file rules), validation (per arm, batch; flaky, weak-test checks), and unstated-requirement gaps |
| `internal/experiment` | Designs and methods (phase1-v2; seq-v1 for cost: stages, looks, futility), services for `new`, `plan`, readiness, `run` (window, budget, usage pauses, retries, stage barrier, resume); the lock and schedule; analysis (roles, floors, verdicts, noise) |
| `internal/judge` | The opt-in LLM judge, which decides nothing: the pilot's prompts, code-only diffs, majority of repeats, pairs in both orders |
| `internal/stats` | Paired analysis: the two-stage cluster bootstrap, t-intervals, variance components, detectable effects, verdict rules (§5.6), group-sequential spending and boundaries; a CUSUM drift chart (pure, no caller); reproduces the Phase 0 spike |
| `internal/report` | An experiment's report: verdicts in words, metrics with both intervals, looks, noise, context, its use and costs, behavior, the judge, per-task results and notes, for a terminal, Markdown or JSON (no personal names or paths); `Load` reads the store; the north star (time and spend to the first decisive verdict) |
| `internal/pricing` | Anthropic's dated list prices per model, for cost estimates and transcripts without Claude Code's cost |
| `internal/run` | One run or calibration: the arm's workspace with everything else denied, hidden grading, behavior flags, context use, redacted records; its spend (`Spend`) and isolated-run cost; recovery of dead runs; its temp root |
| `internal/screen` | The pull-request cost screen: `Classify` reads a pushed range (and the merge base with the default branch) through `gitx`, `source` and `claudectx`, and calls it read context (a candidate), harness (refused in automated mode) or neither |
| `scripts/harness.py` | Standard-library entrypoint: checks, hooks, worktrees, metrics, PR merges ([harness](../../docs/harness.md)) |
| `.claude/agents`, `.claude/skills` | Thin Claude adapters over `.agents/roles` and `.agents/skills` |
| `.githooks/pre-commit` | Shared pre-commit guard |
| `.github/` | CI, the PR template, issue forms, community files |
| `docs/research/` | Product research; history, not default context |
