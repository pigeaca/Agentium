# Runs isolated from Claude Code's shared temp folder

- Date: 2026-10-01
- Status: In Progress (2026-10-01): step 1 done (two probe sessions, $0.14, approved with the [next chapter](2026-10-01-next-chapter.md)); step 2 under way with an implementer on `claude/fix/run-temp-isolation`.
- Scope: a sandbox gap that affects every project and every run, found on 2026-10-01.

## Why
- **The gap:** an agent in a run's sandbox wrote a file under `/private/tmp/claude-<uid>/`, Claude Code's per-user temp folder, and reading it is not denied. Every Claude Code session of the user shares that folder: other runs, the user's own sessions, and the desktop app's scratch folders.
- **Cross-run channel:** a run can leave files there that a later run reads, in the same arm or the other.
- **Reads:** a run can read other sessions' temp files, which may hold task material such as diffs or solutions.
- **Agentium's own data** is denied to agents, but this folder is not.

## Outcome
- A run's agent can write and read only its own temp folder: per run, inside the run's build cache or workspace area.
- Other folders under `/private/tmp/claude-<uid>/` are denied to it.
- Claude Code still works (shell snapshots and its own temp files).

## Findings (step 1, 2026-10-01)
- **How Claude Code picks its temp folder:** `CLAUDE_CODE_TMPDIR` replaces the root of its per-user temp folder (`<root>/claude-<uid>`), which holds its own files, its sockets (`cc-socks`), and the shells' `TMPDIR`.
  - If the path is too long for a Unix socket (about 104 bytes), the shells' `TMPDIR` falls back to the shared `/tmp/claude-<uid>`.
  - That is what happened with a long per-run folder: the agent's `TMPDIR` stayed `/tmp/claude-501`.
- **What the sandbox allows:** it lets agents write the shared `/tmp/claude-<uid>`, and `denyRead` alone does not stop writes. It supports `denyWrite` too.
- **The probe that worked** (a run's sandbox, through the spike harness):
  - setup: `CLAUDE_CODE_TMPDIR=/tmp/ag-<id>` (short, made by Agentium with mode 0700), with `denyRead` and `denyWrite` for `/private/tmp/claude-<uid>` and `/tmp/claude-<uid>`;
  - results:
    - writing and reading the shared folder were both denied;
    - the agent's `TMPDIR` was `/tmp/ag-<id>/claude-501`, readable and writable;
    - `cargo test --offline` built and passed 876 tests;
    - no permission denials.

## Design (step 2)
- **Each run's own temp root:**
  - **Where:** short enough for sockets. Prefer a folder in the data folder (for example `<data>/t/<8 characters>`), re-allowed for reading and writing inside the denied data folder if Claude Code's `allowRead` works there; otherwise `/tmp/ag-<random>`.
  - **Created:** by Agentium, owner-only, before the run, and removed with the workspace.
  - **Guarded:** a test checks the path length.
- **Settings:** the run's settings deny reading and writing `/tmp/claude-<uid>` (both forms). Agentium sets `CLAUDE_CODE_TMPDIR` for the agent, next to `CLAUDE_CONFIG_DIR`. The allowlist still drops other `CLAUDE_*` variables.
- **Other runs' temp roots:** they stay unreadable. Either they live in the denied data folder, or the run denies `/tmp/ag-*` siblings by listing the ones in flight (`internal/run` already predicts overlapping runs' folders).
- **The judge's calls** (no tools) need no change.
- **Golden test:** the Go golden file changes only by the new variable and the deny entries, reviewed line by line.
- **Step 1 (read-only):** find out how Claude Code picks its temp folder (for example `CLAUDE_CODE_TMPDIR` or `TMPDIR`), what its sandbox lets agents write there by default, and whether `denyRead` can cover the shared folder while allowing the run's own.
- **Step 2:** set the run's temp folder per run, deny the rest, and add probes as tests. A golden update for Go is expected and must be reviewed line by line.

## Acceptance
1. A real probe session (paid, small; approval) shows that the agent cannot read or write another session's temp folder, and that Claude Code runs normally.
2. Unit tests cover the new settings and environment.
3. The golden file changes only in the lines this fix adds.

## Work
- [x] **1. Investigation** (read-only, and two probe sessions with approval).
- [ ] **2. Fix and tests.**
- [ ] **3. Real probe (paid; approval),** then archive.

## Boundaries
No new Go modules. Only Claude Code's settings and the run's environment change.

## Verification
Unit and golden tests, `harness.py check changed`, CI, a reviewer, and one probe session.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
