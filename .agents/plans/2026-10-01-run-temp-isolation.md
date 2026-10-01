# Runs isolated from Claude Code's shared temp folder

- Date: 2026-10-01
- Status: Planned, not started. Found in the Java and Rust recipe spike; security-relevant.
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

## Design (to confirm in step 1)
- **Step 1 (read-only):** find out how Claude Code picks its temp folder (for example `CLAUDE_CODE_TMPDIR` or `TMPDIR`), what its sandbox lets agents write there by default, and whether `denyRead` can cover the shared folder while allowing the run's own.
- **Step 2:** set the run's temp folder per run, deny the rest, and add probes as tests. A golden update for Go is expected and must be reviewed line by line.

## Acceptance
1. A real probe session (paid, small; approval) shows that the agent cannot read or write another session's temp folder, and that Claude Code runs normally.
2. Unit tests cover the new settings and environment.
3. The golden file changes only in the lines this fix adds.

## Work
- [ ] **1. Investigation** (read-only, and one probe session with approval).
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
