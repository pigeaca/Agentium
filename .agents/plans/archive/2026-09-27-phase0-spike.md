# Phase 0 spike: measure context experiments on real runs

- Date: 2026-09-27
- Status: Completed
- Scope: the user accepted the hybrid strategy and said "start the spike" (2026-09-27). Answers given for the spike:
  - authenticate Claude Code with the user's Claude subscription;
  - no Codex arm, only one or two runs to check that its JSON output parses, with the normal login and no isolation;
  - no Harbor and no installs;
  - tasks come from Agentium.

  **Agreed deviation.** A fresh `CLAUDE_CONFIG_DIR` cannot use a subscription login, so after the user logged in, runs used "login mode" instead of fresh config folders. That is the user's own config folder, restricted by:
  - `--setting-sources project`;
  - auto memory off and no session persistence;
  - claude.ai connectors and MCP disabled;
  - the account-dependent tools disallowed.

  Acceptance 6's "never modifies `~/.claude`" therefore covers the user's configuration, not Claude Code's own state files. After the runs, the reviewer found an empty `~/.claude/projects/-private-tmp/memory` directory.

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
- [x] Decision record: hybrid strategy accepted
- [x] Probes: authentication, context isolation, `system/init` contents, Codex JSON
- [x] Tasks, hidden tests, reference patches, validation (every arm)
- [x] Runner: scratch clones, context overlay, sandboxed headless runs, verification copies, metrics and behavior flags
- [x] Statistics and report generation (bootstrap and t-intervals, sensitivity check, τ range), with unit tests
- [x] Run the experiment; write up the results; update the study, roadmap and architecture

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
Planned: `python3 -m unittest` on the spike tests; `spike.py validate`; probe and experiment outputs; `harness.py check changed`; the PR's CI.

Results:
- Spike unit tests: 17/17 pass, with no network and no model calls. They are not run by CI: `check changed` has no rule for `spikes/`, so they ran by hand.
- `spike.py validate`: all 6 tasks are valid in both arms. The full arm passes the docs check; the minimal arm fails it by construction, and that is disclosed.
- **Experiment:**
  - 60 of 60 fair runs, estimated at $17.64 in about 37 minutes;
  - every run on the same CLI version (2.1.281), `claude-sonnet-5` and `acceptEdits`;
  - 0 connector tools and 0 file-tool reads of watched locations.
- Probe (rerun after the connector fix): 15 user-level skills excluded; 0 connector tools; the full docs add 3.3k tokens to the first request.
- Codex check: its JSON output parsed, but the task failed because Codex treats `.agents/` as read-only.
- `spike.py reparse` and `report` regenerate every number in the results doc. `harness.py check docs` passes (1173/1800 words).
- `git diff --check` and the pre-commit guard pass; reference patches are whitespace-exempt through `.gitattributes`.

Limitations are listed in the results doc: small, near-saturated tasks; the minimal-arm confound; τ unresolved; login mode; sibling checkouts readable; estimated costs.

## Review
Independent read-only reviewer (Opus): **changes requested**, 12 findings plus 1 informational.
- **High:**
  - a false claim that 2 full-arm runs tried to commit;
  - an undisclosed confound in the minimal arm (its `check docs` fails).
- **Medium:**
  - the behavior counts could not be reproduced from committed data;
  - gaps in hiding the tests;
  - τ is barely identified from six tasks.
- **Low:** the bootstrap's properties, mixed kinds of mean, timeouts dropping runs, the regression suite, probe gaps, stale docs, and docs validity.

All are addressed in `7fb3afe`, with sensitivity numbers, trajectory flags derived by `reparse`, hardened hidden paths, verification copies, t-intervals and a τ range. The same reviewer re-checks the fixes; see the PR.

## Metrics
- Agent: Claude Code / claude-opus-5-5 / default
- Elapsed: 240m
- Check-fix loops: 3 (pre-commit whitespace guard; smoke run blocked by forced default permission mode; account connectors found in probe)
- User corrections: 0
- Review: changes requested, 12 addressed
