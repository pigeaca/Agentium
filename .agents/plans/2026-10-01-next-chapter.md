# The next chapter: decisive verdicts, fast and affordable

- Date: 2026-10-01
- Status: In Progress (2026-10-01): wave 1.
  - **Approvals:** the user approved this plan and, with it, these paid checks: the temp-folder probes (done, $0.14) and the judge's small real check. The wave-2 external verdict (about $60) and the Java and Rust pilot get their own approval, with an estimate.
  - **Revised the same day** after an independent review, which the user accepted in full ("apply all"). The review found:
    - a north-star unit that counted "inconclusive";
    - a pull-request budget below the validation minimum;
    - verdicts promised for time and tokens, which never get one;
    - a smoke check that flags about 83% of unchanged pull requests;
    - cost features that break existing safeguards;
    - a security gap for teammates' pull requests;
    - a work-in-progress limit broken in every wave.
- Scope: the coordinating plan for the roadmap. Items get their own plan file only when they start. Later items stay one-line entries.

## Vision
- **The line:** know what your AI setup really changes, with a clear answer within a day, for tens of dollars, not hundreds.
- **Keep:** the engine (isolation, hidden tests, paired statistics, honest verdicts) stays as it is.
- **What's next:** make a *decisive* verdict reachable, then cheaper, then automatic.

## North-star metrics
- **Time to the first decisive verdict:** improved, regressed or no loss. Inconclusive and exploratory don't count. Each verdict states its detectable effect, and the clock runs from `init` on a fresh clone.
  - Today there has been none yet, on any repository.
  - Target: setup in 3 commands, and the first decisive verdict within a day.
- **Dollars to the first decisive verdict:** total spend up to it.
  - Today: a cost verdict at the floor (8 tasks × 1 run per arm, 16 runs) costs $16–24. The 16-run A/B cost $19.65 and was still inconclusive: it could only detect changes of about 25–30%.
  - A success verdict at the floor (20 × 3 per arm, 120 runs) costs about $120–180. At that size it only certifies a margin of about 21–25 percentage points, not the 15-point default.
  - Target: a decisive cost or model verdict for $40 or less. The success target is set after the wave-3 statistics note names a lever that reaches it: reuse with a pinned Claude Code, or a cheaper screening model (study §5.8).
- **Trust guard:** A/A experiments give a false decisive verdict at most 5% of the time, checked by the seeded simulation and by real A/A runs.
- **Measured:**
  - by a scripted walkthrough at the end of each wave, recorded here;
  - from wave 2, by Agentium itself: it records the time and spend up to each project's first decisive verdict.

## How the work runs in parallel
- **At most two feature tracks and one maintenance track per wave.** At most three agents at once, reviewers included. At most five plans in progress; the rest are Planned, Parked, or one-line roadmap entries.
- **Each code step:** an implementer in its own worktree, then a reviewer, fixes, and a PR. Re-review only when the fixes change behavior. Docs-only PRs need no review.
- **Package ownership:** `internal/cli/experiment_run.go` and `internal/experiment` are hotspots, so items touching them run one after another.

## Waves
| Wave | Features (at most 2) | Maintenance (1) | Exit gate |
|---|---|---|---|
| 0 (done) | The judge in experiments (#54); tasks from tickets (#55) | — | merged |
| 1 Finish | [Task mining](2026-10-01-task-mine.md); [judge](archive/2026-10-01-llm-judge.md) step 3 (reports), then its small real check | [Temp isolation](archive/2026-10-01-run-temp-isolation.md), then [refactor](2026-10-01-refactor-round.md) steps 1–2 | The walkthrough is measured |
| 2 First decisive verdict | Quick start (`agentium start`, calibration inside `experiment run`, the free context-lint hook, north-star tracking); [model and effort A/B](2026-10-01-model-ab.md) (big effects: the likeliest first decisive verdict) | Refactor steps 3–4; [Java and Rust](2026-09-30-java-rust.md) step 3 | A decisive verdict on an external public repository (about $60; approval) |
| 3 Cheaper verdicts | The statistics note, then run reuse with a pinned Claude Code; group-sequential stopping | [Automation](2026-10-01-automation.md) A1 (headless: `--json`, exit codes) | The simulation shows at most 5% false verdicts, and a reused-against-fresh A/A passes |
| 4 Where developers work | Automation A2 (task pool) and A4 (a warn-only cost screen on pull requests); A5 (scheduled watch) | A Python or TypeScript smoke test; the Java and Rust pilot (paid; estimate first) | Dollars, minutes and usage-window share per check are measured |
| Later | The judge gate, then [judge pairs](archive/2026-10-01-judge-pairs.md) (1b) and [ticket grading](archive/2026-10-01-ticket-tasks.md) (step 2); a Claude Code skill; autopilot; [Codex](archive/2026-10-01-phase2-agents-codex.md); hosted CI; live Jira; positioning against `claude plugin eval` and Promptfoo's CI | | |

## Wave 3 requirement: a statistics note before any code
Run reuse, early stopping and the deep watch each break a safeguard the engine has today:
- the version lock (`experiment/lock.go`), since Claude Code changed version five times in three days;
- interleaving in time (`execute.go`);
- the study's rule against optional stopping (§5.6, rule 7).

The note must fix these before any code:
- **The reuse key:** the snapshot digest, task base, model, effort, the Claude Code version (pinned per experiment through `AGENTIUM_CLAUDE`), a fingerprint of the invocation (sandbox settings, build profile, temp policy) and a maximum age. The prompt-cache state is a known source of cost noise (about 9%).
- **Validating reuse:** an A/A of reused against fresh runs before reuse can count toward a verdict.
- **Stopping:** a group-sequential design with a maximum sample, a schedule of looks, O'Brien–Fleming alpha spending and non-binding futility stops. The existing seeded simulation gates it at no more than 5% false verdicts.
- **Drift checks:** treated as a control chart with a false-alarm budget, not a fresh 5% test per new version.

## The judge gate (before pairs and ticket grading)
- **Why:** the pilot gave pairs a NO-GO (11% order flips, without labels) and grading without tests an INCONCLUSIVE.
- **The gate:**
  - check the judge's claims by running them: its "partly" verdicts on passing runs, turned into tests;
  - pre-register the thresholds before measuring, as the pilot did.
- **Only if it passes:** pairs step 1b and ticket step 2 start. Until then the judge stays a per-run second opinion.

## Assignments (wave 1)
- **Temp isolation:** step 2 done after two review rounds (`e27a899`, `f1e7630`); a re-review is running.
- **Task mining:** step 1 done (`4e1395e`) and in re-review. Step 2 (the CLI) builds `RunsTest` from the build-tool profiles.
- **Judge:** done (#61 and a real check: every run got a verdict; it flagged one passing run).
- **Refactor:** step 4 (faster CLI tests, test code only) started first with an implementer on `claude/refactor/faster-tests`. Steps 1–2 start after judge step 3 merges, since they share the report and CLI code.
- **Temp isolation:** the real probe passed on 2026-10-01 ($0.13), through Agentium's own `run once`: the shared Claude temp folders, `/tmp/claude`, the socket folders, npm logs and another run's root were all denied (Bash, Read and Write); the run's own temp folder worked; `go test` passed; the root was removed even with `--keep`.

## Acceptance
1. Each wave's items meet their own plans' acceptance, and each wave passes its exit gate.
2. The north-star figures are recorded here at each wave's end, with the trust guard.
3. At most five plans in progress; this table, the roadmap and the plans' statuses stay current.

## Verification
- **Per item:** as in its plan.
- **Per wave:** the scripted walkthrough, and the trust guard's simulation from wave 3.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
