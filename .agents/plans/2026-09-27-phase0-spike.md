# Phase 0 spike: measure context experiments on real runs

- Date: 2026-09-27
- Status: In Progress
- Scope: the user accepted the hybrid strategy and said "start the spike" (2026-09-27). Answers given for the spike:
  - authenticate Claude Code with the user's Claude subscription;
  - no Codex arm, only one or two runs to check that its JSON output parses, with the normal login and no isolation;
  - no Harbor and no installs;
  - tasks come from Agentium.

## Acceptance
1. **Isolation probes recorded.**
   - Does the subscription login work with a fresh `CLAUDE_CONFIG_DIR`?
   - Does `--setting-sources project` load the project `CLAUDE.md` while excluding user-level instructions and skills?
   - What does `system/init` report (skills, plugins, MCP servers)?

   Evidence: probe summaries in the results doc.
2. **Six tasks on the harness**, each with an instruction, hidden tests and a reference patch. Each is validated: its hidden tests fail on the base commit; with the reference patch they pass, and so do the existing harness tests. Evidence: the `spike.py validate` output.
3. **The experiment.** Six tasks × two context arms × five runs = 60 Claude Code runs, interleaved by task and repeat.
   - Arms: A is the full Agentium context; B is a minimal `CLAUDE.md`/`AGENTS.md`.
   - Either all 60 complete, or the stop reason is recorded: the estimated-cost cap of $150 (the sum of `total_cost_usd`) or a plan usage limit.
   - Each run records: success, estimated cost, tokens (input, output, cache read, cache write), turns, tool calls, duration, first-request context tokens, and the CLI version.
4. **Analysis.**
   - Measured spread: the per-run log-cost `σ`, the success variance `w`, and the across-task spread `τ`.
   - Paired estimates with confidence intervals for cost, time and success.
   - The minimum-detectable-effect table recomputed from the measured values and compared with the study's assumptions.
5. **Codex check.** One or two `codex exec --json` runs, whose token usage is extracted by the spike parser.
6. **Code quality.** The spike code uses only the Python standard library. Its unit tests (parsers and statistics) pass. It never modifies `~/.claude` or `~/.codex`.
7. **Docs.** A results doc in `docs/research/`; the study's assumptions and the roadmap updated; the strategy decision recorded.
8. **Completion.** The PR's CI passes and a reviewer verdict is recorded.

## Work
- [ ] Decision record: hybrid strategy accepted
- [ ] Probes: authentication, context isolation, `system/init` contents, Codex JSON
- [ ] Tasks, hidden tests, reference patches, validation
- [ ] Runner: scratch clones, context overlay, sandboxed headless runs, verification, metrics
- [ ] Statistics and report generation, with unit tests
- [ ] Run the experiment; write up the results; update the study, roadmap and architecture

## Boundaries
- **Where runs happen.** Every run happens in a scratch clone of Agentium outside the repository, pinned to base commit `432b604`, with the git remote removed.
- **Claude Code settings:**
  - sandboxed Bash with no network (`strictAllowlist`, no allowed domains) and no unsandboxed fallback;
  - `acceptEdits`, with web tools disabled;
  - `--permission-prompts none`, `--no-session-persistence`, `--max-budget-usd 3` and a 20-minute timeout per run.
- **Instructions.** The task instruction tells the agent it is already in its task checkout, and not to commit, push or open a PR.
- **Storage.** Transcripts stay in a scratch directory. Only metrics JSON lines and summaries are committed.
- **Nothing else:** no new dependencies, and no changes to the user's Claude or Codex configuration.

## Verification
- `python3 -m unittest` on the spike tests; `spike.py validate`; probe and experiment outputs.
- `python3 .agents/scripts/harness.py check changed`, and the PR's CI.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
