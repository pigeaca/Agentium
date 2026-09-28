# Phase 1: context A/B from the command line

- Date: 2026-09-28
- Status: In Progress (plan awaiting approval by merge)
- Scope: the user asked to plan Phase 1 (2026-09-28) and decided:
  - **Claude Code only.** Codex arrives in Phase 2, with agent comparison and the UI.
  - **Include both spike follow-ups as step 0.** Move the harness out of `.agents/`, and make the "run and update the tests" rule concrete.

  Already approved: Go 1.27.1, and `github.com/mattn/go-sqlite3` for storage. Any other dependency needs a new approval.

## Outcome

A developer can answer "did my AI-instruction change help, hurt, or change the cost?" on their own trusted repository, using only the `agentium` command, with no scripts. The steps: register the repo, snapshot two versions of its context, pick or import tasks, plan the experiment with a cost and detectable-effect preview, run it (resumable and budget-capped), and read a report with honest verdicts.

The commands this phase delivers (names can change in review; the behavior can't):

```text
agentium init                                  register this repo; detect Claude Code, sign-in mode, test commands, context
agentium context show | snapshot | list | diff effective context per agent; named snapshots; diffs
agentium task add | import | validate | list   custom tasks; import from a commit or merged PR; fail-on-base / pass-with-reference check
agentium experiment new | plan | run | report  context A/B or A/A; preview; interleaved resumable runs; Markdown/JSON report
agentium run show <id>                         one run: summary, diff, test output, environment checks
```

## Acceptance

Each item maps to evidence. Changing any of them needs the user's agreement.

1. **Setup.** `agentium init` on Agentium and on a second real repository reports:
   - the Claude Code version and path;
   - the sign-in mode (API key, token file, or login), never a credential value;
   - proposed verification commands;
   - the effective context size.

   It writes nothing into the repository. *Evidence:* command output on both repos; a test that the repo's files and refs are unchanged.
2. **Context.**
   - `context show` resolves what Claude Code loads: `CLAUDE.md` in the working directory and its parents, `@` imports (with cycle and missing-file warnings), `.claude/rules`, skill descriptions, and `AGENTS.md` when there's no `CLAUDE.md`. It shows sizes and token estimates labelled as estimates, and flags harness-affecting files such as hooks, permissions and `.mcp.json`.
   - Snapshots from HEAD, a ref or the working tree live in Agentium's own data folder, never in the user's repo. They can be listed and diffed.
   - An overlay that touches non-context paths is rejected.

   *Evidence:* fixture-repo tests for each rule.
3. **Tasks.**
   - `task add`: an instruction, a base ref, verification commands, and optional hidden tests.
   - `task import --commit <sha>` / `--pr <n>` (through `gh`): the base is the parent; the test-file changes become the hidden tests; the rest becomes the reference patch; the instruction is flagged "review for solution leaks".
   - `task validate`: the hidden tests fail on the base and pass with the reference patch, in every arm of an experiment.

   *Evidence:* tests; at least 12 validated tasks imported from real history.
4. **Isolation.** Every run applies all nine requirements in [the Phase 0 results](../../docs/research/2026-09-27-phase0-spike-results.md). Each one is covered by an automated test using a **fake `claude`** that emits recorded stream-json, or by a per-run check stored with the run. A run whose environment drifted (permission mode, tools, connector tools, CLI version) is recorded as unfair and never counted. *Evidence:* the test list maps 1:1 to the nine items.
5. **Experiments.**
   - Templates: context A/B, and A/A calibration.
   - `experiment plan` shows the runs, the estimated cost and the smallest detectable effects per tier (Quick, Confident, Custom), using σ = 0.19, τ = 0.10–0.25 and w = 0.20 as defaults.
   - A lock (versions, snapshot digests, task hashes, flags, price-table date) is written before the first run.
   - Runs are interleaved in randomized pairs, with a concurrency limit, per-run and total budget caps, retries only for infrastructure failures, and resume after a crash.

   *Evidence:* end-to-end tests with the fake agent, including kill-and-resume losing no finished run and a budget stop.
6. **Reports.**
   - Paired analysis (two-stage cluster bootstrap plus a t-interval) with the study's §5.6 verdict rules: improved, regressed, no loss beyond the margin, equivalent, inconclusive, and "exploratory" below the floors.
   - Behavior counts (tests updated, tests run, checks run, commits, denials), environment and honesty notes, and a per-task table.
   - Markdown and JSON output, plus `run show` for one run.

   *Evidence:* golden-file tests, and the statistics reproduce the Phase 0 spike's committed numbers from its 60 runs (git history `9bae530`).
7. **Real-run acceptance (approval gate).** With the user's go-ahead and budget, an A/A calibration and a context A/B run on Agentium, using the CLI only. The A/A test must not report a difference. Both reports must be produced, and spending must stay within the preview's estimate. *Evidence:* the reports, and the costs compared with the preview.
8. **Process.** Each step below lands as its own PR with green CI and a recorded review. The architecture code map, the README usage and the roadmap stay current. *Evidence:* the PRs.

## Work

Each step is one PR, in order. The estimates are rough and assume agent-assisted work.

- [x] **0. Setup follow-ups** (about 0.5 day; done in the harness-to-scripts PR). Move `.agents/scripts/harness.py` and its tests to `scripts/`, updating the hook, CI, docs, adapters and the harness's own path checks. Add a concrete rule to the core rules: "Run the affected tests and add or update tests for changed behavior before finishing."
- [x] **1. Storage and `init`** (about 3 days; done in the store-and-init PR). Data folder `~/.agentium` (override with `AGENTIUM_HOME`): an SQLite database through `mattn/go-sqlite3` with embedded migrations, plus an artifacts folder. Register the repo; detect Claude Code (PATH or a configured path), its version and sign-in mode; propose test commands from `go.mod`, `package.json`, `pyproject.toml` or the harness. CI gets cgo, `go.sum` and the module cache.
- [x] **2. Context resolver and snapshots** (about 4 days; done in the context-snapshots PR). The Claude Code resolver with warnings; snapshots as commits in a per-project bare repository inside the data folder, built from read-only views (commits are read in place with `ls-tree`/`cat-file`; never a push from the user's repo, which would run its pre-push hooks); list, diff, `--include` for linked documents, and overlay planning. Findings for later steps: git always runs with hooks, fsmonitor and optional index writes off and without inherited `GIT_*`; the data folder must be outside the repository; a `.claude` folder that is a symlink to a directory is reported, not followed; nested `pkg/.claude` folders are not resolved yet.
- [ ] **3. Tasks** (about 4 days). Add, import (commit, and PR through `gh`) and validate, with test-file detection for Go, Python, JS/TS and `tests/` folders. Import 12 or more tasks from Agentium's and one other repository's history.
- [ ] **4. Claude Code adapter and local executor** (about 5 days):
  - run preparation: a clone at the base, the context overlay (`snapshot.PlanOverlay`, reporting harness changes), and the isolation recipe; the checkout's parent folders must hold no `CLAUDE.md`, `CLAUDE.local.md` or `AGENTS.md`, since Claude Code loads those; base commits are kept with `gitx.FetchCommit` so a rebase or gc in the user's repo cannot lose them;
  - an environment check that the resolver matches the real CLI: the first request's size per arm, and the `AGENTS.md` fallback (loaded only without `CLAUDE.md`) confirmed on the pinned version;
  - three sign-in modes (API key or token passed only to the child; login mode with project settings only);
  - the stream-json parser, trajectory flags, environment checks and classification;
  - verification on a hidden copy, secret redaction of transcripts, timeouts and process-group kill;
  - a fake `claude` for tests; `agentium run once` for debugging.
- [ ] **5. Experiments** (about 4 days). Templates, the lock, the planner and price table (verified Anthropic prices, dated), the interleaved scheduler with concurrency, caps, infrastructure retries, resume, and orphan cleanup.
- [ ] **6. Statistics and reports** (about 4 days). A Go port of the spike's statistics, checked against its 60 runs; verdict rules and floors; Markdown and JSON reports; `run show`.
- [ ] **7. Real-run acceptance** (about 1 day, paid; separate approval). The A/A calibration and a Quick-tier context A/B on Agentium. Record the results; update the planner defaults if the measured noise differs.

That's about 5 weeks in total (roughly 25 working days), consistent with the study's 4–5 week estimate.

## Boundaries

- **Scope.** Claude Code only: no Codex, no web UI, no containers (Harbor), no LLM judges. Local runs are for trusted repositories only.
- **The user's repo is never modified.** Agentium never writes to it: no files, refs or hooks. All checkouts and snapshots live in Agentium's data folder.
- **Network:** only the agent's own model calls, plus `gh` reads for PR import. No telemetry. Harness checks stay offline (`GOPROXY=off` locally).
- **Credentials:** never stored, printed or committed. Transcripts are redacted with the pre-commit guard's credential patterns before they're saved.
- **Dependencies:** `mattn/go-sqlite3` is the only new one. Anything else needs vetting and approval first.
- **Cost:** tests never call real models; only step 7 spends money, after approval.

## Verification

- Every step: `harness.py check changed` (Go: vet and race tests; docs; harness), CI, and a review.
- End-to-end behavior is tested with the fake `claude` (deterministic, free).
- Statistics are checked against the spike's golden data.
- Real behavior is proven once, in step 7.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
