# Architecture

Status: **strategy accepted (hybrid); stack accepted: Go + React + SQLite** ([decision](decisions/2026-09-28-stack-go-react-sqlite.md)). The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) describes the design, and the [Phase 0 results](../docs/research/2026-09-27-phase0-spike-results.md) list what Phase 1 must do. Phase 1 is in progress ([plan](plans/2026-09-28-phase1-context-ab-cli.md)): `init`, context snapshots, tasks, single runs, calibration and experiments (design, preview, lock, interleaved resumable runs within a budget) work; statistics and reports follow.

## Product concept

Agentium runs coding agents (Claude Code and Codex first) on tasks from a developer's own repository. It compares agents, models and versions of the project's AI context (`AGENTS.md`, `CLAUDE.md`, skills, rules) on correctness, cost and speed. Context versions are first-class experiment arms. Results use a paired design with repeats and report plain verdicts: improved, regressed, no loss beyond the margin, or inconclusive.

Shape:
- **Hybrid.** Agentium owns context snapshots, tasks, experiment design, statistics and the UX.
- **Local runs** drive agent CLIs headlessly in isolated checkouts.
- **Harbor**, pinned and out of process, adds containers later.
- **One binary:** a Go core (Go 1.27.1, module `github.com/pigeaca/agentium`), a local React + TypeScript UI embedded in Phase 2, and SQLite through `mattn/go-sqlite3` from Phase 1.

## Go conventions

- `cmd/agentium` only wires packages together; code lives in `internal/<package>`.
- The standard library comes first. Every new module needs approval (see [dependencies](rules/supply-chain.md)), and `go.sum` is committed.
- Every blocking call takes a `context.Context`, and cancellation stops agent processes.
- Errors are wrapped with `%w` and context; there is no package-level mutable state.
- I/O goes through parameters (`io.Writer`, `Env`) so commands can be tested.
- Exit codes: 0 success, 1 runtime failure, 2 usage error.

## Code map

| Path | Responsibility |
|---|---|
| `AGENTS.md`, `CLAUDE.md`, `.agents/` | Shared instructions: rules, references, roles, skills, templates, plans, decisions |
| `go.mod`, `cmd/agentium` | Go module (pinned toolchain) and the `agentium` binary's entrypoint |
| `internal/cli` | Command-line parsing and dispatch (`init`, `context`, `task`, `run`, `experiment`, `version`, `help`) |
| `internal/home` | The data folder (`~/.agentium` or `AGENTIUM_HOME`, owner-only), outside every repository: database, artifacts, run workspaces and run records; the run lock (one process starts agents at a time) |
| `internal/store` | SQLite through `mattn/go-sqlite3` (cgo, WAL, foreign keys); embedded, ordered migrations; projects, snapshots, tasks, runs, calibrations and experiments (design, lock, status; runs keep their slot and attempt) |
| `internal/project` | Read-only repository discovery for `init`: git root and commit, Claude Code path and version, sign-in mode (presence only), test commands, instruction files |
| `internal/gitx` | Every git call: hooks, fsmonitor, prompts and optional index writes off, inherited `GIT_*` dropped; hook-free fetch of a user's commit into Agentium's bare repository (for task bases) |
| `internal/source` | Read-only views of a commit or the working tree; symlinks followed only to the repository's own files |
| `internal/claudectx` | Which files Claude Code loads, as experiments run it: instructions, `@` imports (5 hops, also from rules and nested files), rules, skill/subagent/command descriptions, harness files, linked documents, and warnings |
| `internal/snapshot` | Context versions as parentless commits in `projects/<id>/repo.git` in the data folder; diffs; overlay planning that refuses to change code or configuration and reports harness changes |
| `internal/checkout` | Isolated working copies: a fresh repository holding only the base commit (depth 1), so hidden tests and solutions are unreachable; safe file writes |
| `internal/runner` | Commands (through a shell or as arguments) in their own process group with a timeout and no credentials; a gentle stop (SIGINT, then SIGKILL); the group is killed when the command ends; the process group is reported when it starts |
| `internal/claude` | Claude Code headless and isolated (project settings only, no connectors, fixed permission mode, sandbox without network, denied paths and credentials, an allowlisted environment); stream-json metrics, outcomes and environment drift |
| `internal/task` | Tasks (base, instruction, verification; a solution split into hidden tests and reference by test-file rules) and validation per context arm |
| `internal/experiment` | Experiment designs (templates context A/B and A/A, arms, tasks eligible in every arm, repeats, caps, margins, seed) and the preview (tiers, cost estimates, detectable effects, floors); the lock; the seeded schedule of interleaved pairs; execution (concurrency within a window, a budget no run can pass, infrastructure retries, stop rules, resume from stored runs); what counts as fair and as a success |
| `internal/pricing` | Anthropic's list prices per model, dated, for cost estimates and for transcripts that end without Claude Code's cost |
| `internal/run` | One run: a workspace prepared as the arm (base, context, setup, context commit), Claude Code denied everything else, grading on a hidden copy (diff, hidden tests, verification), behavior flags, redacted records; a start file per run, so a run left by a dead process is recovered with its spend; the folders a run will use, predicted for runs that may overlap it |
| `scripts/harness.py` | Standard-library entrypoint for checks (docs, harness, Go, vulnerabilities), hooks, worktrees and metrics |
| `.claude/agents`, `.claude/skills` | Thin Claude adapters over `.agents/roles` and `.agents/skills` |
| `.githooks/pre-commit` | Shared pre-commit guard |
| `.github/` | CI, the PR template, issue forms and community files (contributing, security, conduct) |
| `docs/research/` | Product research; history, not default context |
