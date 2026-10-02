# Wave 3 statistics note: reuse, sequential stopping and drift

- Date: 2026-10-02
- Status: proposed. The user decides the [open questions](#9-open-questions-for-the-user). No production code changes until then. The implementation steps are in the [cheaper verdicts plan](../../.agents/plans/2026-10-02-cheaper-verdicts.md).
- Why it exists: the [next chapter](../../.agents/plans/2026-10-01-next-chapter.md#wave-3-requirement-a-statistics-note-before-any-code) requires this note before any code for run reuse, early stopping or the scheduled watch. Each of them breaks a safeguard the engine has today: the version lock, interleaving in time, and the study's rule against optional stopping (§5.6, rule 7).
- Evidence: seeded simulations in `internal/stats` (`sequential_test.go`, `drift_test.go`; test-only). Every figure below comes from `AGENTIUM_LONG_SIM=50 go test -v -run 'TestGroupSequential|TestReuseValidation|TestDriftChart' ./internal/stats` (89 s on 12 cores). Rates carry 95% Wilson intervals.
- Trust guard: A/A experiments give a false decisive verdict at most 5% of the time. Here a *false difference* is "improved", "improved, but small" or "regressed" when the true mean difference is zero.

## Summary
1. **Group-sequential cost design (method `seq-v1`).**
   - **Looks:** up to 16 tasks × 1 run per arm, with looks after 8, 12 and 16 tasks.
   - **Spending:** O'Brien–Fleming-type Lan–DeMets spending of a two-sided **4.5%** for "improved" and "regressed". Equivalence spends a one-sided 5% on each side. Futility stops are non-binding.
   - **Verdicts:** at every look, the existing `Decide` runs on intervals at that look's nominal level. The bootstrap and the t-interval must still agree.
   - **Simulated false differences:** 4.53% [4.46, 4.60] over 360,000 experiments without a true difference, and at most 4.58% [4.46, 4.70] for any noise shape.
   - **Gain at a true 20% cost cut** (τ = 0.10): one experiment ends decisively 80% of the time, against 48% for today's fixed 8 tasks, using 13.1 tasks on average.
2. **The reuse key.**
   - **What it covers:**
     - the task digest;
     - the arm's context;
     - model and effort;
     - Claude Code's version and binary hash;
     - sign-in and host;
     - an invocation fingerprint;
     - the price table;
     - a maximum age of 14 days.
   - **Cost only.** Reuse counts toward cost verdicts only, never success.
   - **Cost metric:** reuse uses a *cold-start cost* that removes the cross-run prompt cache. The cache moves a run's cost by up to 10–11% on Agentium-sized tasks and up to 42–57% on small ones.
3. **Validating reuse.**
   - **Design:** an A/A of 16 tasks × 4 stored runs against 4 fresh runs.
   - **Pass rule:** the 95% interval of log(fresh/stored) contains zero and stays within ±log 1.15.
   - **Bias allowance:** a passing key carries an allowance, the far end of that interval. Reuse verdicts must clear it.
   - **Simulated:** false differences of 0.05–2.1% at true biases up to ±10%, and a pass rate of 88% with no bias.
4. **Drift.**
   - **Chart:** a self-starting two-sided CUSUM (k = 0.5, h = 6) on a fixed 8-task panel's mean log cost, one point per check.
   - **False alarms:** 3.59% [3.47, 3.71] of charts alarm within 52 checks. A fresh 5% test per check would alarm in about 93% of them.
   - **Detection:** a 20% change after 12 checks is caught in a median 5–6 checks.
5. **North star.**
   - **Sequential stopping** buys *time* to a decisive cost verdict, by more decisive experiments. It barely changes dollars per decisive verdict.
   - **Reuse** pays only for large effects (35% or more), and only after about 9–18 experiments per key in 30 days.
   - **The dollar lever is small tasks:** on samber/lo a decisive cost verdict costs $2–4.
   - **Success:** no wave-3 statistic reaches a success verdict for $40 on Agentium-sized tasks. On small tasks the existing 20 × 3 floor already costs about $12–34.

## 0. What the engine does today (unchanged by this note)
- **Fixed design:** the verdict comes from one analysis of the locked design (`phase1-v2`: the cost floor is 8 tasks × 1 run).
- **Analysis:** per-task log differences go through a two-stage bootstrap (10,000 draws) and a t-interval. Each bound is the wider of the two, at 95% for differences and 90% for equivalence and no loss (`stats.Decide`).
- **Its gate:** `TestOneRunCostVerdictsSimulation` allows up to 6% false differences, and measured 4.6–5.7%. The fixed 8-task design here gave 4.7–5.4% "any verdict" without a true difference.
- **Interleaving:** runs are interleaved pair by pair (`experiment.Schedule`).
- **Version lock:** a lock pins the Claude Code version (`Lock.Check`).
- **Measured noise** (log cost):
  - Phase 0: σ = 0.19.
  - The real A/A: σ = 0.20, with a range of 0.13–0.50.
  - Spread of the per-task differences with one run per arm: 0.20 (ab16), 0.27 (model A/B) and 0.29 (aa2).
  - τ: estimated 0.00 in both real A/Bs, with ranges up to 0.33 and 0.47. The planner keeps τ = 0.10–0.25.

## 1. The reuse key
A stored run may stand in for a fresh run of an arm only when **every** field below matches. Then the run is *eligible*, and its key is *validated* (§2).

| Field | Source today | Why |
|---|---|---|
| Project | `projects.id` | The same bare repository and data folder |
| Task digest | `LockedTask.Digest`: name, instruction, base, solution commit, hidden-test paths, reference paths, setup, verify; plus `tasks.grading` | The prompt and workspace the agent sees, and the grading inputs. The solution commit content-addresses the hidden tests. |
| Arm context | the snapshot commit (`Arm.Snapshot`; for `base`, the task base), `LockedArm.Files` digests, the overlay's harness changes | What loads into the run |
| Model | requested (`Design.ArmModel`) and reported at init (`LockedArm.Model`) | Aliases can move |
| Effort | `Design.ArmEffort` with `Record.EffortRecorded` true | An unknown effort is not "default" |
| Run cap and timeout | `RunBudgetUSD`, `Timeout` | Both censor cost ("capped", "timeout") |
| Claude Code | `Metrics.CLIVersion` and the SHA-256 of the executable `AGENTIUM_CLAUDE` names | The version changed five times in three days, so the CLI must be pinned |
| Calibrated environment | `LockedArm.Tools`, `Skills`, `SlashCommands` (the `claude.Expect` sets) | Bundled tools and skills change behavior |
| Sign-in | `Lock.SignIn` | Already a resume condition |
| Host | `Lock.Host` (OS and architecture) and the selected build profiles' toolchain identity (`go version`, the JDK release, `cargo --version`) | The sandbox and builds differ |
| Invocation fingerprint | SHA-256 of a canonical JSON, described below | Catches unintended changes to how a run is driven |
| Price table | `pricing.Date` | The cold-start adjustment prices tokens with it |
| Concurrency | `Design.Concurrency` | Contention changes timeouts |
| Age | stored run's start to the new lock | At most **14 days** |

**The invocation fingerprint** is computed by `internal/run` from:
- **`RunMethod`:** a new constant in `internal/run`, bumped by hand whenever run or grading behavior changes. Examples are the prompt suffix, grading rules, behavior flags, warm-up recipes and the overlay.
- **The Claude Code arguments,** with per-run values replaced by placeholders:
  - `--permission-mode`, `--permission-prompts`, `--setting-sources`;
  - `--strict-mcp-config`, `--disallowedTools`;
  - `--output-format`, `--no-session-persistence`.
- **The per-run settings** (`Invocation.settings`), with paths replaced by role names:
  - the sandbox switches;
  - the network allowlist and `allowLocalBinding`;
  - the denied-path *roles*;
  - the credential rules, `autoMemoryEnabled` and `disableClaudeAiConnectors`.
- **The environment allowlist's names and prefixes** (`EnvironFor`), never values.
- **The selected build-tool profiles,** with their agent environment, caches and warm steps.
- **The run's policies:** the temp policy (own `TempRoot`, shared temp folders denied), the build-cache policy and the deps-folder policy.

Agentium versions that change none of these keep the fingerprint, so reuse survives a refactor. A change that matters must change the fingerprint or bump `RunMethod`. A golden test pins the fingerprint, as `TestGoProfileGolden` pins a Go run today.

**Eligible runs.**
- **Outcome:** a counted experiment run, with outcome `ok`, `capped` or `timeout`. Capped and timed-out runs must stay in, or reuse would bias cost downward.
- **What disqualifies a run:**
  - recovered after a crash (`Recovered`);
  - an estimated cost (`CostEstimated`);
  - a missing transcript, which the cold-start cost needs;
  - recorded drift;
  - a warm-up failure note;
  - calibration and `run once` runs, whose prompts or conditions differ.
- **Selection is fixed and blind to outcomes:** per task, the 4 most recent eligible runs at lock time. The lock records their IDs. A task without stored runs disables reuse for the whole experiment.

**What invalidates.**
- **A key:**
  - any field change;
  - the age limit;
  - an alarm on the key's anchor chart (§4);
  - its validation turning 30 days old;
  - a failed validation, which is final for that key;
  - the user's reset.
- **A run** stops being eligible when it passes 14 days or loses its transcript.

**Prompt cache: the cold-start cost.**
- **How the cache moves cost:** Claude Code writes its prompt cache for an hour. A run whose shared prefix (tools, system prompt, context) is still cached from another run reads it at the cache-read rate instead of writing it.
- **Upper bound per run:** first-request tokens × (1-hour write − read rate).

| Real experiment | First request | Cost per run | Warm-vs-cold bound | Share |
|---|---|---|---|---|
| aa2, Sonnet 5 | 30,206 | $1.014 | $0.115 | 11% |
| ab16, Sonnet 5 | 30,520 | $1.181 | $0.116 | 10% (the plan's "about 9%") |
| model A/B, Opus 5.5 (samber/lo) | 15,432 | $0.284 | $0.120 | 42% |
| model A/B, Sonnet 5.5 (samber/lo) | 15,439 | $0.103 | $0.059 | 57% |

- **Why it matters for reuse:** interleaving spreads this effect over both arms. A reused arm ran at another time in another cache state, so the effect becomes a *bias*, not noise, and it grows as runs get cheaper.
- **The cost metric for every reuse comparison (the validation, reuse experiments, anchors, the drift chart) is the cold-start cost:**
  - Start from the run's reported cost.
  - For the first request of the main session and of each subagent session (first assistant message per `parent_tool_use_id`), reprice that request's cache-read tokens at the cache-write rate the session used. That rate is 1 hour unless the stream's `ephemeral_5m` field says otherwise.
  - Use Agentium's dated price table for the repricing.
  - The run then costs what it would have cost with nobody else's cache. A run that started cold is unchanged.
  - The raw cost stays in every report.
  - A model missing from the price table makes the run ineligible.

**Reuse counts toward cost only.**
- **Cost:** reuse counts once the key is validated, with the bias allowance below.
- **Success:** a reused arm leaves success *exploratory*.
  - At w ≈ 0.20, a success validation of 16 × 4 against 16 × 4 has a standard error of about 8 pp, so its allowance would exceed 15 pp.
  - Success never decides in the cost designs anyway: they have at most 16 tasks, below the 20-task success floor.
- **Time:** stays exploratory, as today. Time depends on API load at the hour of the run.

**The bias allowance.**
- **Rule:** a validated key carries `a`, the far end of its validation interval (§2). A reuse experiment calls "improved" only when the interval's upper end is below −a. It calls "regressed" only when the lower end is above +a.
- **No equivalence:** a reuse experiment gives no "equivalent" verdict, because a bias up to `a` could fake it.
- **Why the guard holds:** in the direction of the bias, a false verdict needs either a key whose bias exceeds `a`, or a 2.5% tail event when it does not. The validation gives the first at most 2.5%, and the efficacy test gives the second at most 2.25%. The total stays under 5%, and the simulation shows far less (§7).

## 2. Validating reuse (the reused-against-fresh A/A)
- **When:** once per key, before any reuse counts. The arms are the stored runs and fresh runs of the same context, model and pinned Claude Code.
- **Size:**
  - 16 tasks.
  - **Stored side:** 4 eligible runs per task (normally a baseline experiment of 16 × 4, or earlier experiments). Every stored run must be at least 48 hours old when the fresh side starts, and at most 14 days old.
  - **Fresh side:** 4 runs per task, scheduled repeat by repeat in a seeded random task order. That makes 64 fresh runs, plus 64 stored ones if no baseline exists.
- **Metric:** log cold-start cost. The per-task difference is the mean over the 4 fresh runs minus the mean over the 4 stored ones.
- **Analysis:** the two-stage cluster bootstrap (10,000 draws) and the t-interval, at 95%. Take the widest interval [L, U].
- **Pass rule (pre-registered):**
  - [L, U] contains 0, so no bias is detected;
  - max(|L|, |U|) ≤ log 1.15 = 0.140;
  - at least 14 of 16 tasks have at least 3 counted runs on each side.

  The allowance is a = max(|L|, |U|), recorded with the key.
- **If it fails:**
  - Reuse stays off for that key, and fresh experiments remain available.
  - The report shows the estimate, the per-task table, and the first-request cache-read shares of both sides, to help find the cause.
  - **No retry on the same key.** Repeating until it passes is optional stopping. Only a key change, such as a new pin or a fingerprint change, gets a new validation.
- **Lifetime:** 30 days after the fresh side ends, or until an anchor alarm or a key change. The fresh side's runs then become the stored runs that later experiments reuse.
- **Anchors** keep checking it:
  - every reuse experiment runs 4 fresh runs of the reused arm, on seeded random tasks of its first stage;
  - they stay out of the experiment's analysis;
  - they feed the key's anchor chart (§4) and later become stored runs.
- **Simulated** (16 × 4 against 16 × 4, σ = 0.19, 5,000 validations per row; normal noise, skewed in brackets):

| True bias (stored over fresh) | Passes | Mean allowance | False differences after a pass |
|---|---|---|---|
| 0 | 88.8% [87.9, 89.6] (87.4%) | 0.105 | 0.05% [0.01, 0.16] (0.11%) |
| −5% | 56.0% (56.8%) | 0.112 | 0.21% [0.10, 0.47] (0.07%) |
| +5% | 56.5% (54.8%) | 0.112 | 0.07% [0.02, 0.26] (0.44%) |
| +10% | 11.1% (10.3%) | 0.122 | 0.90% [0.38, 2.09] (2.13% [1.19, 3.78]) |

## 3. Group-sequential stopping (method `seq-v1`)
**Design.**
- **Maximum:** 16 tasks × 1 run per arm.
- **Looks:** after the first 8, 12 and 16 tasks of the locked task order (information fractions 0.5, 0.75 and 1). With 12–15 eligible tasks there are two looks (8 and all); with 8–11, one look, which is the fixed design.
- **Efficacy** ("improved", "regressed"):
  - O'Brien–Fleming-type Lan–DeMets spending, α(t) = 2 − 2Φ(z₁₋α/₄ / √t) per side, of a two-sided α = **4.5%**;
  - symmetric boundaries;
  - boundaries recomputed for the actual information fractions when tasks are lost;
  - the final look spends what is left.
- **Equivalence:** each one-sided test spends a one-sided 5% with the same function.
- **Futility:** non-binding. At an interim look with no verdict, the experiment stops when the conditional power to cross the final efficacy boundary is below 10%. The power is computed under the observed trend: 1 − Φ((c₃ − |T|/√f_k)/√(1 − f_k)), with T the look's t-statistic and f_k its information fraction. Futility never changes the efficacy boundaries, so ignoring a futility stop cannot raise false differences, and the gate below counts experiments as if every futility stop were ignored. The lock records whether futility stops apply (default on).
- **Why 4.5%, not 5%:** a first long run at 5% gave 5.12% [4.99, 5.24] under normal noise, because t-intervals at nominal levels run slightly liberal with 7–15 degrees of freedom. Spending 4.5% leaves room for that and for noise shapes the simulation does not cover.

**Boundaries** (two-sided 4.5%; numerical integration, checked against gsDesign's published values to ±0.002 and by 400,000 Brownian paths):

| Look | Tasks | Fraction | Spent so far (two-sided) | Efficacy z | Nominal level of the look's efficacy intervals | Equivalence z | Level of the look's equivalence intervals |
|---|---|---|---|---|---|---|---|
| 1 | 8 | 0.50 | 0.25% | 3.023 | 99.75% | 2.538 | 98.89% |
| 2 | 12 | 0.75 | 1.68% | 2.408 | 98.40% | 2.016 | 95.62% |
| 3 | 16 | 1.00 | 4.50% | 2.056 | 96.02% | 1.720 | 91.46% |

**How it composes with today's rules.**
- **The bootstrap and the t-interval must agree:** kept. At look k, `Decide` gets the bootstrap and t-intervals at the look's efficacy level in its "95%" slots, and at its equivalence level in its "90%" slots. `Decide`'s logic is unchanged: regressed, improved (small when the estimate is within the margin), equivalent, otherwise inconclusive.
- **Floors:**
  - The first look is the cost floor (8 tasks × 1 run). No look comes before a floor.
  - A look with fewer than 8 counted tasks, after failures, gives no verdict and the experiment continues.
  - Success stays exploratory at every look, since 16 < 20.
  - A success-primary sequential design (looks at 20, 30, 40 × 3, say) is later work.
- **"No loss" and "equivalent":**
  - The cost designs have no cost guard, so "no loss" does not arise. The success guard is exploratory.
  - "Equivalent" spends its own one-sided 5% per side.
  - At the margin (a true +10%), "equivalent" came out 0.71% [0.66, 0.77] of the time.
  - In practice equivalence within ±10% is out of reach at 16 tasks: the 91% half-width there is about 0.12–0.13 at σ = 0.19, wider than the margin (0.095).
- **Pairing and interleaving:**
  - The schedule becomes stages. Stage k holds tasks n_{k−1}+1 … n_k of the seeded task order, each pair adjacent and its arms in seeded random order, as today.
  - The executor finishes and settles a stage, retries included, before analysing it. It starts no run of the next stage until the look's decision.
  - A look includes exactly the stage prefix, never tasks that finished early from a later stage. Finish order correlates with cost, since cheaper runs end sooner.
- **Rule 7 (no optional stopping):**
  - `seq-v1` replaces the study's "99% at interim looks" and extension stages with the pre-declared spending above.
  - There is no "run more" beyond 16. A larger question is a new experiment.
  - An experiment stopped by budget or by the user between looks keeps the verdict of its last completed look (inconclusive if none). Later runs are shown as exploratory.
- **The lock** records the looks, α, the spending function, the computed nominal levels, the futility threshold and whether it applies. A resume refuses a changed design.
- **Budget:** the preview and the reserve use the maximum (16 tasks). The preview also shows the expected spend at no effect and at −20% (table in §6).
- **Reporting:**
  - "stopped at look k of 3 (n tasks)";
  - the look's interval, which is a repeated confidence interval with simultaneous coverage;
  - a note that an early stop overstates the effect's size on average (the interval is valid, the point estimate is not unbiased).

## 4. Drift checks for the scheduled watch
- **The chart:** one per (project, model, effort, context, panel). The panel is 8 tasks chosen once by seed from validated tasks.
  - Each check runs every panel task once, on the current pinned Claude Code, and records the version.
  - The point is y_j = the mean log cold-start cost over the panel. The panel is fixed, so task difficulty is a constant.
- **Self-starting standardization (Hawkins):**
  - The first 4 checks are the reference. The 4 rounds of a validation's 16 × 4 runs can serve, restricted to the panel.
  - After that, each point is standardized against all earlier in-control points: U_j = Φ⁻¹(F_t,m−1((y_j − ȳ) / (s·√(1 + 1/m)))).
  - This is exactly standard normal under normal noise, whatever the spread and the day effects. No σ or day-effect parameter is assumed.
  - **Why self-starting:** a first version that standardized against a fixed 4-check reference falsely alarmed within 52 checks in about 30–45% of charts (h = 5 and 6), because the reference's own error persists in every check.
- **CUSUM:** S⁺ = max(0, S⁺ + U − 0.5) and S⁻ = max(0, S⁻ − U − 0.5). It alarms when either exceeds **h = 6**. A point that does not alarm joins the baseline.
- **False-alarm budget:** at most 5% of charts alarm within 52 checks (a year of weekly checks).
  - **Measured:** 3.59% [3.47, 3.71] pooled over 90,000 charts. The worst scenario is 4.04% [3.53, 4.62].
  - **Scenarios:** σ = 0.19 and 0.35; day effects of 0, 0.03 and 0.05; normal, skewed and heavy-tailed noise.
  - **Run length without a change:** median 730–940 checks.
  - **For comparison:** h = 5 gives 10.0%. A fresh 5% test per check gives 1 − 0.95⁵² ≈ 93%.
- **Detection** (σ = 0.19, day effect 0.03, change after 12 checks):

| Change | 8 tasks: alarm within 8 checks, median delay | 16 tasks |
|---|---|---|
| +10% / −10% | 17% / 25%, 29 / 17 checks | 38% / 46%, 11 / 9 checks |
| +20% / −20% | 79% / 94%, 6 / 5 checks | 97% / 100%, 4 / 4 checks |
| +35% | 99.8%, 4 checks | 100%, 3 checks |

  - **A young chart detects little:** within the first 8 checks after only 4 reference checks, a 20% change is caught 3% of the time. The chart reports "warming up" until 12 checks and claims nothing then. False alarms still count against the budget.
- **On an alarm:**
  - The watch reports the change and the version where the CUSUM started climbing.
  - It invalidates reuse for every key with that version, and proposes a proper interleaved A/B of the two pinned versions: one pre-declared test, triggered by the alarm.
  - The chart restarts with a new reference once the user accepts the new level.
- **The anchor chart** of a reuse key is the same chart.
  - Each point is a reuse experiment's 4 anchors: the mean of log(fresh) − mean log(stored), per anchor task.
  - Its in-control level is the key's bias, so it detects *changes* in bias, not the level, which the validation bounds.
- **Cost:** a check is 8 runs. That is about $0.80 on samber/lo-sized tasks with Sonnet 5.5, and about $9 on Agentium-sized tasks with Sonnet 5. Weekly checks fit the watch's $20 weekly cap.

## 5. The exit gate, exactly
"The simulation shows at most 5% false verdicts, and a reused-against-fresh A/A passes" means all of these, recorded in the plan:

1. **Simulation**, run against the production implementation:
   - The plan moves the spending, boundary and look logic into `internal/stats` and `internal/experiment`, and the tests call them.
   - The run is `AGENTIUM_LONG_SIM=50 go test -run 'TestGroupSequentialFalseVerdicts|TestReuseValidationAndAllowance|TestDriftChart' ./internal/stats`. It must pass and log, for each noise shape:
     - false differences, with futility stops ignored, at most 5.0%, and their Wilson 95% upper bound at most 5.0%. The null scenarios are σ ∈ {0.19, 0.35} × τ ∈ {0, 0.10, 0.25}, 120,000 experiments per noise shape;
     - "equivalent" at a true +10% at most 5%;
     - reuse false differences at most 5% for true biases within ±5%;
     - h = 6 false alarms within 52 checks at most 5% in every scenario.
   - The default CI run, with smaller sizes, stays as a regression check: at most 5.0% pooled and 6.0% per noise shape.
   - `TestOneRunCostVerdictsSimulation` (phase1-v2) still passes unchanged.
2. **A real reused-against-fresh A/A passes** the rule in §2, on one real project, with a pinned Claude Code and the cold-start cost.
   - **Suggested project:** samber/lo with Sonnet 5.5, if it has 16 valid tasks. Otherwise the user picks one.
   - **Estimated cost:** 64 fresh runs, plus 64 stored ones if no baseline exists. That is about $13–15 there with calibrations, and about $140 on Agentium's own tasks. It is paid and needs approval.
   - **If it fails,** the gate fails honestly: reuse stays off, the wave closes with sequential stopping and the chart, and the user decides what follows.
3. **Recommended, not part of the gate:** a real `seq-v1` A/A on the same project (at most 32 runs, about $3). It checks that the stage barrier, looks and reports work end to end.

## 6. What this means for the north star
**Context A/B on cost.** The table assumes σ = 0.19 and one run per arm. Probability is that of a decisive verdict in one experiment; runs are means. Dollars use $0.10 a run (samber/lo, Sonnet 5.5) and $1.10 a run (Agentium's own tasks, Sonnet 5).

| True cost cut, τ | Design | Decisive | Runs | $ (lo / Agentium) | Runs per decisive verdict |
|---|---|---|---|---|---|
| 20%, 0.10 | fixed 8 (today) | 47.8% | 16 | 1.6 / 18 | 33 |
| | fixed 16 | 83.3% | 32 | 3.2 / 35 | 38 |
| | **seq-v1** | **80.2%** | **26.2** | **2.6 / 29** | **33** |
| | reuse of arm A (validated, allowance ≈ 0.105) | 45.0% | 15.1 + 4 anchors | 1.9 / 21 | 42 (+ validation) |
| 20%, 0.25 | fixed 8 | 30.8% | 16 | 1.6 / 18 | 52 |
| | seq-v1 | 56.4% | 26.0 | 2.6 / 29 | 46 |
| 35%, 0.25 | fixed 8 | 80.6% | 16 | 1.6 / 18 | 20 |
| | seq-v1 | 98.7% | 23.0 | 2.3 / 25 | 23 |
| | reuse (τ = 0.10) | 99.9% | 10.7 + 4 | 1.5 / 16 | 15 (+ validation) |
| 63% (model A/B) | fixed 8 = seq-v1 | 100% | 16 | 3.10 measured | 16 |
| none | seq-v1 (futility) | 3.8–4.8% any verdict | 21 | 2.1 / 23 | — |
| | fixed 16 | 5.0–6.4% | 32 | 3.2 / 35 | — |

- **Sequential stopping** keeps dollars per decisive verdict about level at 20% effects: −2% at τ = 0.10 and −11% at τ = 0.25.
  - It turns a coin-flip experiment into a likely-decisive one (48% → 80%), which shortens the time to the first decisive verdict.
  - It spends a third fewer runs than a fixed 16-task design when nothing changed.
  - The guard is tighter than today's: 4.5% against about 5%.
- **Reuse** saves runs per experiment only for effects of 35% or more. At 20% it loses, because the allowance eats power.
  - The validation costs 64–128 runs per key every 30 days.
  - At a 35% cut it saves 6–8 runs per experiment, so it breaks even after about 9–18 reuse experiments per key within those 30 days.
  - So reuse is not the lever for a first verdict. It earns its place as the watch's baseline and for repeated screens of big changes against one base.
- **Time** (estimates):
  - samber/lo-sized runs (30–60 s of agent time): about 1.5 minutes per pair at concurrency 2, so a 13-task `seq-v1` experiment takes about 20 minutes.
  - Agentium-sized runs (4–6 minutes): about 1.5 hours.
  - Both fit "within a day".
- **Success:** no lever here reaches a success verdict for $40 on Agentium-sized tasks.
  - The 20 × 3 floor costs about $130 on Sonnet 5, and certifies only a 21–25 pp margin.
  - On small tasks the same floor costs about $12–34 (120 runs at $0.10–0.28).
  - **Recommended success target:** set it on small-task repositories, where the floor already fits $40. Leave sequential success designs to a later wave.

## 7. Simulation evidence
**Model.** Each task has a level (removed by pairing). Each arm's log cost is level + effect + σ·noise. A task's effect is the mean effect + τ·noise.
- **Noise shapes,** standardized:
  - normal;
  - a skewed lognormal (skewness about 0.9, as `TestOneRunCostVerdictsSimulation`);
  - Student's t with 5 degrees of freedom (kurtosis 9).
- **Analysis:** the production `NewBootstrap`, `TInterval` and `Decide` at each look's levels, with 200 bootstrap draws. With one run per cell the percentile bootstrap is narrower than the t-interval at these sizes, so the widest-interval rule follows the t-interval. The t-alone counts are logged as an upper bound and differ by at most 0.05 points.
- **Seeds:** each scenario has its own seed, so the counts are reproducible.

**False differences without a true difference** (`seq-v1`, 20,000 experiments per scenario, 120,000 per shape):

| Noise | With futility stops ignored | With them obeyed | A/A only (τ = 0) |
|---|---|---|---|
| normal | 4.58% [4.46, 4.70] | 4.30% [4.18, 4.41] | 4.52% [4.32, 4.73] |
| skewed | 4.56% [4.45, 4.68] | 4.27% [4.16, 4.39] | 4.52% [4.32, 4.73] |
| heavy-tailed | 4.45% [4.34, 4.57] | 4.20% [4.09, 4.32] | 4.30% [4.11, 4.50] |
| all, pooled | 4.53% [4.46, 4.60] | — | 4.45% [4.33, 4.56] |

- The worst single scenario was 4.75% [4.46, 5.05] (normal, σ = 0.19, τ = 0.10).
- Under normal noise every scenario has the same rate. The analysis is scale-free, so σ and τ change only the noise shape's mix.
- **Power** (5,000 experiments per row; "improved" at a true cut):

| Cut | τ | fixed 8 | seq-v1 | fixed 16 | seq-v1 tasks used |
|---|---|---|---|---|---|
| 10% | 0.10 / 0.25 | 15.4% / 10.6% | 26.1% / 16.0% | 28.2% / 18.6% | 12.0 / 11.5 |
| 20% | 0.10 / 0.25 | 47.8% / 30.8% | 80.2% / 56.4% | 83.3% / 62.4% | 13.1 / 13.0 |
| 35% | 0.10 / 0.25 | 94.7% / 80.6% | 99.96% / 98.7% | 100% / 99.2% | 10.2 / 11.5 |
| 63% | 0.10 / 0.25 | 100% / 100% | 100% / 100% | 100% / 100% | 8.0 / 8.1 |

  Skewed noise at τ = 0.25 gives within 5 points of the normal rows.
- **Reuse:** see §2. Reuse experiments use arm A = 4 stored runs per task with the true bias, arm B = 1 fresh run, the `seq-v1` looks without futility, and the allowance from a passing validation.
  - At no bias: "improved" at a true 20% cut 45.0% (15.1 fresh runs), at 35% 99.9% (10.7), at 63% 100% (8.0).
- **Drift:** see §4.
- **Runtime:**
  - The default run, which CI runs under the race detector, adds about 8 s to `internal/stats` (13 s in all).
  - The long run takes 89 s without the race detector.

## 8. Assumptions and limitations
- **Tasks:** independent, and the effects across tasks are independent of the noise. Missing tasks (infrastructure failures) are not informative.
- **The simulated configuration** is one run per arm. The two-stage bootstrap with more runs per cell is wider, which is conservative. Futility uses the t-statistic as if it were normal; since futility is non-binding, this can only cost power.
- **Noise shapes:** three; real cost noise could be bimodal (cache states). The cold-start cost removes the known bimodality, and its effect on σ is unmeasured.
- **Reuse bias** is modeled as a constant per key, with the validation's noise independent of the experiment's. In practice the stored runs of later experiments are the validation's fresh side, which correlates them. Anchors were not simulated.
- **The drift chart** assumes check-to-check independence, without autocorrelated day effects, and step changes. Slow drifts get absorbed into a self-starting baseline. The chart covers cost only.
- **The cache bound** counts each run's first request only. Cross-run cache hits after the first request are assumed to be the run's own. The implementation must confirm this on recorded transcripts.
- **The source of the plan's "about 9%"** was not recorded. It matches the bound for ab16.
- **σ was swept at 0.19 and 0.35.** Results under normal noise do not depend on σ.

## 9. Open questions for the user
1. **α:** approve 4.5% (two-sided) for `seq-v1`, stricter than phase1-v2's about 5%?
2. **Cold-start cost:** make it the *primary* cost metric of `seq-v1`, with raw cost still reported? Or use it only for reuse, the validation and the chart? It removes cross-run cache effects of up to 10–57% per run, but changes what "cost" means in a verdict.
3. **Reuse scope in wave 3:** the economics favor building, in order:
   1. `seq-v1`;
   2. the cold-start cost, the reuse key and the validation (the gate);
   3. the chart;
   4. reuse inside A/B verdicts last, or deferred to wave 4, since it pays only for effects of 35% or more at about 9–18 experiments per key a month.

   Which cut?
4. **Paid checks** (approval and a budget): the real reused-against-fresh A/A (about $13–15 on samber/lo; about $140 on Agentium's tasks) and the real `seq-v1` A/A smoke check (about $3).
5. **Futility:** stop by default (it saves about a third of the runs when nothing changed; non-binding) or ask at each look?
6. **Drift panel and budget:** 8 tasks a week, about $0.80 on small tasks or $9 on Agentium-sized ones, with a ±10% change found in a median 17–29 checks? Or 16 tasks, which double the cost and halve the delay?
7. **Defaults:** stored runs at most 14 days old, validations valid 30 days, a validation passing at ±15%. Accept them, or set your own before any validation runs? They must be fixed beforehand.
