# Wave 3 statistics note: reuse, sequential stopping and drift

- Date: 2026-10-02 (revised the same day after review; the user's decisions recorded)
- Status:
  - **Decided** (the user, 2026-10-02):
    - α = **3.5%** for `seq-v1`, set after the review's arm-specific noise result. 4.5% was approved first, but broke the 5% guard there (§7).
    - Actual cost stays the primary metric, and the isolated-run cost (§1) is reported beside it.
    - Sequential stopping is built now. Run reuse, the reuse A/A and the drift chart are deferred, and this note keeps their design for later.
    - The `seq-v1` smoke check (about $3) is approved once the engine is built. The reuse A/A is not.
    - Futility stops are on by default (non-binding).
    - New cost experiments default to `seq-v1`. Existing and locked phase1-v2 experiments analyse, resume and report unchanged.
    - phase1-v2's excess over 5% on lopsided noise is a known limitation (§8).
    - A skew-robust interval (bootstrap-t or Johnson's t) is a later research item, for both methods.
    - The drift panel and the reuse defaults stay open until reuse resumes.
  - **Plan:** the implementation steps are in the [cheaper verdicts plan](../../.agents/plans/2026-10-02-cheaper-verdicts.md).
- Why it exists: the [next chapter](../../.agents/plans/2026-10-01-next-chapter.md#wave-3-requirement-a-statistics-note-before-any-code) requires this note before any code for run reuse, early stopping or the scheduled watch. Each of them breaks a safeguard the engine has today: the version lock, interleaving in time, and the study's rule against optional stopping (§5.6, rule 7).
- Evidence:
  - **Where:** seeded simulations in `internal/stats` (`sequential_test.go`, `drift_test.go`; test-only).
  - **Command:** every figure comes from `AGENTIUM_LONG_SIM=50 go test -v -run 'TestGroupSequential|TestReuseValidation|TestDriftChart' ./internal/stats`, about 2.3 minutes on 12 cores. Smaller factors are exploratory.
  - **Intervals:** rates carry 95% Wilson intervals.
- Trust guard: A/A experiments give a false decisive verdict at most 5% of the time. Here a *false difference* is "improved", "improved, but small" or "regressed" when the true mean difference is zero.

## Summary
1. **Group-sequential cost design (method `seq-v1`; in scope; the default for new cost experiments).**
   - **Design:** up to 16 tasks × 1 run per arm, with looks after 8, 12 and 16 tasks.
   - **Spending:** O'Brien–Fleming-type Lan–DeMets spending of a two-sided **3.5%** for "improved" and "regressed". Equivalence spends one-sided 5% per side. Futility stops are non-binding and on by default.
   - **Verdicts:** the existing `Decide` runs at each look's nominal levels, so the bootstrap and the t-interval must still agree.
   - **Simulated false differences** (t-interval alone, 120,000 experiments per group):
     - the same noise shape in both arms: 3.43–3.68%;
     - the 22 recorded per-task differences of three real experiments: 3.85% [3.75, 3.96];
     - arm-specific shapes: 4.50% [4.38, 4.62].

     Every group's upper bound is at or under 5%. phase1-v2's fixed 8-task design gives 5.38% and 5.79% on the last two groups (§7, §8).
   - **Gain at a true 20% cost cut** (τ = 0.10): one experiment ends decisively 74% of the time, against 47% for fixed 8 tasks, using 13.2 tasks on average.
2. **Isolated-run cost (in scope, reported beside actual cost).**
   - **What it is:** a run's cost with other runs' prompt cache removed.
   - **How much it can matter:** from the reports' data, cross-run cache hits could move a run's cost by **at most** 10–11% on Agentium-sized tasks and 42–57% on samber/lo. These are upper bounds, not measured hits.
3. **Run reuse (deferred; the design for later).**
   - **Key:** the task digest, context, model, effort, the pinned Claude Code (version and binary hash), an invocation fingerprint and a 14-day window.
   - **Scope:** cost only.
   - **Validation:** a reused-against-fresh A/A gives the key a bias allowance.
   - **Simulated:** false differences of 0.02–2.7%, including day effects and biases up to ±12%.
   - **Economics:** poor. Reuse pays only for cuts of 35% or more, after about 10–20 reuse experiments per key within 14 days.
4. **Drift (deferred).** A self-starting two-sided CUSUM (k = 0.5, h = 6) on a fixed panel's mean log cost.
   - **False alarms:** 3.59% [3.47, 3.71] of charts within 52 checks.
   - **Detection:** a 20% change after 12 checks is caught in a median 5–6 checks.
5. **North star.**
   - **What sequential stopping buys:** *decisiveness per experiment* and a 5% guard that holds, not dollars.
     - Against fixed 8 tasks, runs per decisive verdict are +5% at a 20% cut (τ = 0.10), −2% at τ = 0.25, and +23–27% at 35% cuts.
     - It shortens the time to a first decisive verdict, because fewer experiments end inconclusive.
   - **The dollar lever is small tasks:** on samber/lo a decisive cost verdict costs about $1.7–5.
   - **Success:** no wave-3 lever reaches a success verdict for $40 on Agentium-sized tasks. On small tasks the existing 20 × 3 floor already costs about $12–34.

## 0. What the engine does today (unchanged by this note)
- **Fixed design:** the verdict comes from one analysis of the locked design (`phase1-v2`: the cost floor is 8 tasks × 1 run).
- **Analysis:** per-task log differences go through a two-stage bootstrap (10,000 draws) and a t-interval. Each bound is the wider of the two, at 95% for differences and 90% for equivalence and no loss (`stats.Decide`).
- **Its gate:** `TestOneRunCostVerdictsSimulation` allows up to 6% false differences, and measured 4.6–5.7%. The fixed 8-task design here gave 5.4–5.7% "any verdict" without a true difference.
- **Interleaving:** runs are interleaved pair by pair (`experiment.Schedule`).
- **Version lock:** a lock pins the Claude Code version (`Lock.Check`).
- **Measured noise** (log cost):
  - Phase 0: σ = 0.19.
  - The real A/A: σ = 0.20, with a range of 0.13–0.50.
  - Spread of the per-task differences with one run per arm: 0.20 (ab16), 0.27 (model A/B) and 0.29 (aa2).
  - τ: estimated 0.00 in both real A/Bs, with ranges up to 0.33 and 0.47. The planner keeps τ = 0.10–0.25.

## 1. Isolated-run cost, and the reuse key (deferred)
**Isolated-run cost (in scope: reported beside actual cost).**
- **The effect it removes:** Claude Code writes its prompt cache for an hour. A run whose shared prefix (tools, system prompt, context) is still cached from another run reads that prefix at the cache-read rate instead of writing it.
- **Its name:** the *isolated-run cost* is what the run would have cost if no other run had warmed the cache. It differs from the report's existing "cold-cache cost" (`internal/report/report.go`): that column reprices *every* cached read as a write, the run's own included, as a sensitivity bound.
- **The rule:** start from the run's reported cost. Then reprice, at the cache-write rate the session used, the cache-read tokens of:
  - the main session's first request;
  - the first launch of each subagent *type* (the Task tool's `subagent_type`, matched by `parent_tool_use_id`).

  The write rate is 1 hour unless the stream's `ephemeral_5m` field says otherwise. A later launch of a type already launched in the same run reads the prefix that run wrote itself, so it is not repriced. The repricing uses Agentium's dated price table.
- **What stays the same:**
  - A run that started cold is unchanged.
  - A model missing from the price table leaves the value absent, not zero.
  - Actual cost stays the primary metric (the user's decision).
- **To confirm on recorded transcripts (step 2 of the plan):**
  - on a run with no other run inside the cache TTL, every request this rule reprices must show `cache_read` = 0;
  - otherwise some of the rule's reads are the run's own (two subagent types sharing a prefix, say), and the rule changes before it ships.
- **Upper bounds from report data** (first-request tokens × (1-hour write rate − read rate); not measured cross-run hits):

| Real experiment | First request | Cost per run | Bound | Share |
|---|---|---|---|---|
| aa2, Sonnet 5 | 30,206 | $1.014 | $0.115 | at most 11% |
| ab16, Sonnet 5 | 30,520 | $1.181 | $0.116 | at most 10% (the plan's "about 9%") |
| model A/B, Opus 5.5 (samber/lo) | 15,432 | $0.284 | $0.120 | at most 42% |
| model A/B, Sonnet 5.5 (samber/lo) | 15,439 | $0.103 | $0.059 | at most 57% |

- **Why reuse needs it:** interleaving spreads this effect over both arms of a fresh experiment. A reused arm ran at another time in another cache state, so the effect becomes a *bias*, and it grows as runs get cheaper. Every reuse comparison (the validation, reuse experiments, anchors, the drift chart) would therefore use the isolated-run cost.

**The reuse key (deferred).** A stored run may stand in for a fresh run of an arm only when **every** field below matches. Then the run is *eligible*, and its key is *validated* (§2).

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
| Price table | `pricing.Date` | The isolated-run cost prices tokens with it |
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

**Eligible runs: one filter for stored and fresh runs.**
- **The filter is the analysis's own counting rule:** a fair outcome (`ok`, `capped`, `timeout`), a cost above zero, and an isolated-run cost that can be computed.
  - Estimated-cost runs count with their estimate (`CostEstimated`: a timeout or recovered run stopped before Claude Code reported, `internal/run/run.go` and `recover.go`). That is how fresh arms count them today.
  - Dropping them from stored runs only would bias stored cost downward, since they are the long, expensive runs.
- **Every fresh arm uses the same filter.** A fresh run without a computable isolated-run cost is left out of a reuse comparison exactly as a stored one would be.
  - No other outcome-based exclusion applies to stored runs. Recovered runs, drift notes and warm-up notes are treated as the analysis treats them in fresh arms.
  - Only calibration and `run once` runs are never eligible: they are not experiment runs, and their prompts or conditions differ.
- **Selection is fixed and blind to outcomes:** per task, the 4 most recent eligible runs at lock time. The lock records their IDs. A task without stored runs disables reuse for the whole experiment.

**What invalidates.**
- **A key:**
  - any field change;
  - an alarm on the key's anchor chart (§4);
  - its validation turning 14 days old (below);
  - the user's reset.
- **A run** stops being eligible when it passes 14 days.
- **The user's reset** only turns reuse *off* for a key. It never re-enables a failed key family (§2).

**Reuse would count toward cost only.**
- **Success:** a reused arm leaves success *exploratory*.
  - At w ≈ 0.20, a success validation of 16 × 4 against 16 × 4 has a standard error of about 8 pp, so its allowance would exceed 15 pp.
  - Success never decides in the cost designs anyway: they have at most 16 tasks, below the 20-task success floor.
- **Time** stays exploratory, as today.

**The bias allowance.**
- **Rule:** a validated key carries `a`, the far end of its validation interval (§2). A reuse experiment calls "improved" only when the interval's upper end is below −a. It calls "regressed" only when the lower end is above +a.
- **No equivalence:** a reuse experiment gives no "equivalent" verdict, because a bias up to `a` could fake it.
- **Why the guard holds:** in the direction of the bias, a false verdict needs either a key whose bias exceeds `a`, or a tail event when it does not. The validation bounds the first at 2.5% and the efficacy test bounds the second at 1.75%, so the total stays under 5%.
- **The caveat:** a day effect common to one occasion's runs is outside this argument. The simulation measures it (§2, §7).

## 2. Validating reuse: the reused-against-fresh A/A (deferred)
- **When:** once per key family, before any reuse counts. The arms are the stored runs and fresh runs of the same context, model and pinned Claude Code.
- **Size:**
  - 16 tasks.
  - **Stored side:** 4 eligible runs per task. Every stored run must be at least 48 hours old when the fresh side starts, and at most 14 days old.
  - **Fresh side:** 4 runs per task, scheduled repeat by repeat in a seeded random task order. That makes 64 fresh runs, plus 64 stored ones if no baseline exists.
- **Metric:** log isolated-run cost. The per-task difference is the mean over the 4 fresh runs minus the mean over the 4 stored ones.
- **Analysis:** the two-stage cluster bootstrap (10,000 draws) and the t-interval, at 95%. Take the widest interval [L, U].
- **Pass rule (to be fixed before any validation runs):**
  - [L, U] contains 0, so no bias is detected;
  - max(|L|, |U|) ≤ log 1.15 = 0.140;
  - at least 14 of 16 tasks have at least 3 counted runs on each side.

  The allowance is a = max(|L|, |U|), recorded with the key.
- **Lifetime: 14 days, the same as the stored-run window.**
  - The fresh side's runs become the stored runs that later reuse experiments draw on.
  - They age out at 14 days, and anchors replenish only 4 tasks per experiment, so the validation expires when they do.
  - A key is then revalidated, and the 14 days are the window for the break-even in §6.
- **Failures are recorded per key family:** (project, context, model, effort, Claude Code pin).
  - A family whose validation failed may be validated again only after a change to the Claude Code pin (version or binary hash), the invocation fingerprint or `RunMethod`.
  - Changing a timeout, a run cap, concurrency or the task set does not reopen it. Repeating until a pass is optional stopping.
- **The report on a failure shows:**
  - the estimate and the per-task table;
  - the first-request cache-read shares of both sides;
  - the false-fail rate: with no bias at all, the validation fails 12% of the time without day effects, and 31–47% with day effects of 0.03–0.05.

  So a failure is weak evidence of bias. It still stays final for the family, which is the conservative side.
- **Anchors** keep checking a passing key:
  - every reuse experiment runs 4 fresh runs of the reused arm, on seeded random tasks of its first stage;
  - they stay out of the experiment's analysis;
  - they feed the key's anchor chart (§4) and later become stored runs.
- **Simulated** (16 × 4 against 16 × 4, σ = 0.19, 5,000 validations per row, then a `seq-v1` reuse experiment at 3.5% per pass):

| True bias (stored over fresh) | Noise, day effect | Passes | Mean allowance | Bias beyond the allowance, among passes | False differences after a pass |
|---|---|---|---|---|---|
| 0 | normal, none | 87.8% [86.9, 88.7] | 0.105 | 0% | 0.02% [0.00, 0.13] |
| −4.9% / +5.1% | normal, none | 56.7% / 56.7% | 0.112 | 0% | 0.07% / 0.18% |
| +10.5% | normal, none | 10.3% | 0.122 | 8.9% [6.8, 11.7] | 0.78% [0.30, 1.98] |
| −11.3% / +12.7% | normal, none | 3.1% / 3.8% | 0.125 | 27.9% / 29.1% | 1.95% / 2.65% [1.14, 6.04] |
| 0 / +5.1% | skewed, none | 87.6% / 57.1% | 0.104 / 0.112 | 0% | 0.02% / 0.28% |
| +10.5% / +12.7% | skewed, none | 10.3% / 3.9% | 0.123 / 0.125 | 7.0% / 24.9% | 1.36% / 1.55% |
| 0 / +5.1% | normal, 0.03 | 68.9% / 50.6% | 0.108 / 0.110 | 0% | 0.17% / 0.67% |
| 0 / +5.1% | normal, 0.05 | 53.4% / 44.4% | 0.108 / 0.109 | 0% | 1.24% [0.88, 1.73] / 2.34% [1.79, 3.06] |

The day effect is one common log-cost shift per occasion: each side of the validation, and each arm of each reuse experiment.

## 3. Group-sequential stopping (method `seq-v1`; in scope)
**Design.**
- **Maximum:** 16 tasks × 1 run per arm.
- **Looks:** after the first 8, 12 and 16 tasks of the locked task order, at information fractions 0.5, 0.75 and 1.
  - With 12–15 eligible tasks there are two looks: 8 and all.
  - With 8–11 there is one look. That is a fixed design at the 3.5% level, with a 96.5% efficacy interval, so it is not `phase1-v2`, which uses 95%.
- **Default:** `seq-v1` is the method of every *new* cost experiment. Experiments locked under `phase1-v1` or `phase1-v2` keep their method: they analyse, resume and report exactly as before.
- **Efficacy** ("improved", "regressed"):
  - O'Brien–Fleming-type Lan–DeMets spending, α(t) = 2 − 2Φ(z₁₋α/₄ / √t) per side, of a two-sided α = **3.5%**;
  - symmetric boundaries.
- **When tasks are lost:**
  - An interim look's fraction is its counted tasks over the *planned* maximum.
  - Boundaries already used at earlier looks stay fixed. Only the current look's boundary is computed, from what is left to spend.
  - The final look spends the remainder whatever its count.
- **Equivalence:** each one-sided test spends a one-sided 5% with the same function.
- **Futility:** non-binding, and on by default (the user's decision).
  - At an interim look with no verdict, the experiment stops when the conditional power to cross the final efficacy boundary is below 10%.
  - The power is computed under the observed trend: 1 − Φ((c₃ − |T|/√f_k)/√(1 − f_k)), with T the look's t-statistic and f_k its information fraction.
  - Futility never changes the efficacy boundaries, so ignoring a futility stop cannot raise false differences. The gate counts experiments as if every futility stop were ignored.
  - The lock records the futility setting.
- **Why 3.5%:**
  - At 5%, false differences were 5.12% [4.99, 5.24] under normal noise: t-intervals at nominal levels run slightly liberal with 7–15 degrees of freedom.
  - At 4.5%, arm-specific noise shapes gave 5.66% [5.53, 5.79], and the recorded differences' upper bound was 5.03%.
  - At 4.0%, an ad-hoc probe put the arm-specific upper bound at 5.06%.
  - 3.5% is the level tested where every group's upper bound stays at or under 5% (the user's decision, 2026-10-02).

**Boundaries** (two-sided 3.5%; numerical integration, checked against gsDesign's published values to ±0.002 and by 400,000 Brownian paths):

| Look | Tasks | Fraction | Spent so far (two-sided) | Efficacy z | Nominal level of the look's efficacy intervals | Equivalence z | Level of the look's equivalence intervals |
|---|---|---|---|---|---|---|---|
| 1 | 8 | 0.50 | 0.16% | 3.164 | 99.84% | 2.538 | 98.89% |
| 2 | 12 | 0.75 | 1.22% | 2.523 | 98.84% | 2.016 | 95.62% |
| 3 | 16 | 1.00 | 3.50% | 2.155 | 96.88% | 1.720 | 91.46% |

**How it composes with today's rules.**
- **The bootstrap and the t-interval must agree:** kept. At look k, `Decide` gets the bootstrap and t-intervals at the look's efficacy level in its "95%" slots, and at its equivalence level in its "90%" slots. `Decide`'s logic is unchanged.
- **Floors:**
  - The first look is the cost floor (8 tasks × 1 run). No look comes before a floor.
  - A look with fewer than 8 counted tasks, after failures, gives no verdict and the experiment continues.
  - Success stays exploratory at every look, since 16 < 20.
- **"No loss" and "equivalent":**
  - The cost designs have no cost guard, so "no loss" does not arise.
  - "Equivalent" spends its own one-sided 5% per side. At the margin (a true +10%), it came out 0.71% [0.65, 0.77] of the time.
  - In practice it is out of reach at 16 tasks: the 91% half-width there is about 0.12–0.13 at σ = 0.19, wider than the margin of 0.095.
- **Pairing and interleaving:**
  - The schedule becomes stages. Stage k holds tasks n_{k−1}+1 … n_k of the seeded task order, each pair adjacent and its arms in seeded random order, as today.
  - The executor settles a stage, retries included, before analysing it. It starts no run of the next stage until the look's decision.
  - A look includes exactly the stage prefix. Finish order correlates with cost.
- **Rule 7 (no optional stopping):**
  - `seq-v1` replaces the study's "99% at interim looks" and extension stages with the spending above.
  - There is no "run more" beyond 16.
  - An experiment stopped by budget or by the user between looks keeps the verdict of its last completed look (inconclusive if none).
- **The lock** records the looks, α, the spending function, the computed nominal levels and the futility setting. A resume refuses a changed design.
- **Budget:** the preview and the reserve use the maximum (16 tasks), on every resume too. The preview also shows the expected spend at no effect and at −20%.
- **Reporting:**
  - "stopped at look k of 3 (n tasks)";
  - the look's interval (a repeated confidence interval);
  - a note that an early stop overstates the effect's size on average.

## 4. Drift checks for the scheduled watch (deferred)
- **The chart:** one per (project, model, effort, context, panel). The panel is a fixed set of tasks.
  - Each check runs every panel task once, on the current pinned Claude Code, and records the version.
  - The point is y_j = the mean log isolated-run cost over the panel.
- **Self-starting standardization (Hawkins):**
  - The first 4 checks are the reference.
  - After that, each point is standardized against all earlier in-control points: U_j = Φ⁻¹(F_t,m−1((y_j − ȳ) / (s·√(1 + 1/m)))).
  - This is exactly standard normal under normal noise, whatever the spread and the day effects.
  - **Why self-starting:** a version that standardized against a fixed 4-check reference falsely alarmed within 52 checks in about 30–45% of charts.
- **CUSUM:** S⁺ = max(0, S⁺ + U − 0.5) and S⁻ = max(0, S⁻ − U − 0.5). It alarms when either exceeds **h = 6**. A point that does not alarm joins the baseline.
  - **The CUSUM accumulates and can alarm from the first check after the reference,** warm-up included. Those alarms are real and count against the false-alarm budget below.
  - "Warming up" (the first 12 checks) only means that *no* alarm is not yet evidence of no change.
- **False-alarm budget:** at most 5% of charts alarm within 52 checks.
  - **Measured:** 3.59% [3.47, 3.71] pooled over 90,000 charts. The worst scenario is 4.04% [3.53, 4.62].
  - **Scenarios:** σ = 0.19 and 0.35; day effects of 0, 0.03 and 0.05; normal, skewed and heavy-tailed noise.
  - **For comparison:** h = 5 gives 10.0%. A fresh 5% test per check gives about 93%.
- **Detection** (σ = 0.19, day effect 0.03, change after 12 checks):

| Change | 8 tasks: alarm within 8 checks, median delay | 16 tasks |
|---|---|---|
| +10% / −10% | 17% / 25%, 29 / 17 checks | 38% / 46%, 11 / 9 checks |
| +20% / −20% | 79% / 94%, 6 / 5 checks | 97% / 100%, 4 / 4 checks |
| +35% | 99.8%, 4 checks | 100%, 3 checks |

  - After only the 4 reference checks, a 20% change is caught within 8 checks 3% of the time.
- **On an alarm:** the watch reports the change and the version where the CUSUM started climbing, and invalidates reuse for every key with that version. It then proposes one pre-declared interleaved A/B of the two pinned versions.
- **The anchor chart** of a reuse key is the same chart, with each reuse experiment's 4 anchors as a point. It detects *changes* in bias.
- **Cost:** a check is one run per panel task. With 8 tasks that is about $0.80 on samber/lo-sized tasks, and about $9 on Agentium-sized tasks. The panel size and budget are open (deferred).

## 5. The exit gate, exactly
The user narrowed the gate on 2026-10-02 to sequential stopping:
1. **The long simulation passes against the production `seq-v1` implementation.**
   - The plan moves the spending, boundary and look logic into `internal/stats` and `internal/experiment`, and the tests call them.
   - The run is `AGENTIUM_LONG_SIM=50 go test -run 'TestGroupSequentialBounds|TestGroupSequentialFalseVerdicts' ./internal/stats`.
   - **Each null group must pass,** at 120,000 experiments each, futility stops ignored, counting on the t-interval alone (an upper bound that does not depend on bootstrap draws): at most 5.0% false differences, with the Wilson 95% upper bound at most 5.0%. The groups are:
     - the same noise in both arms: normal, skewed and heavy-tailed;
     - arm-specific shapes;
     - the recorded differences.
   - "Equivalent" at a true +10% must come out at most 5%.
   - **The default CI run** is a regression check: at most 7.0% per group and 5.5% pooled, on the t-interval alone.
   - `TestOneRunCostVerdictsSimulation` (phase1-v2) still passes unchanged.
   - **Status today, on the test-only implementation at 3.5%:** every group passes. The largest upper bound is 4.62% (arm-specific). The figures are in §7.
2. **A real `seq-v1` A/A smoke check runs** on one real project: at most 32 runs, about $3, approved by the user once the engine is built. It checks the stage barrier, the looks and the report end to end.

**Deferred with reuse:**
- the real reused-against-fresh A/A: about $13–15 on samber/lo, about $140 on Agentium's tasks; not approved;
- the reuse simulation's threshold: false differences at most 5% for biases within ±5%, without day effects;
- the drift simulation's threshold: at h = 6, at most 5% false alarms within 52 checks in every scenario.

## 6. What this means for the north star
**Context A/B on cost.** The table assumes σ = 0.19 and one run per arm. "Decisive" is the chance of a decisive verdict in one experiment; runs are means, and runs per decisive verdict = runs / decisive. Dollars use $0.10 a run (samber/lo, Sonnet 5.5) and $1.10 a run (Agentium's tasks, Sonnet 5).

| True cost cut, τ | Design | Decisive | Runs | $ per experiment (lo / Agentium) | Runs per decisive verdict |
|---|---|---|---|---|---|
| 20%, 0.10 | fixed 8 (phase1-v2) | 47.1% | 16 | 1.6 / 18 | 34.0 |
| | fixed 16 | 83.0% | 32 | 3.2 / 35 | 38.6 |
| | **seq-v1 (3.5%)** | **74.2%** | **26.4** | **2.6 / 29** | **35.6** |
| | reuse of arm A (deferred; allowance ≈ 0.105) | 40.6% | 15.3 + 4 anchors | 1.9 / 21 | 47.6 (+ validation) |
| 20%, 0.25 | fixed 8 | 31.7% | 16 | 1.6 / 18 | 50.5 |
| | seq-v1 | 52.4% | 26.0 | 2.6 / 29 | 49.6 |
| 35%, 0.10 | fixed 8 | 95.1% | 16 | 1.6 / 18 | 16.8 |
| | seq-v1 | 99.9% | 21.4 | 2.1 / 24 | 21.4 |
| | reuse (deferred) | 99.8% | 11.1 + 4 | 1.5 / 17 | 15.1 (+ validation) |
| 35%, 0.25 | fixed 8 | 81.2% | 16 | 1.6 / 18 | 19.7 |
| | seq-v1 | 98.1% | 23.8 | 2.4 / 26 | 24.3 |
| 63% (model A/B) | fixed 8 = seq-v1 | 100% | 16 | 3.10 measured | 16 |
| none | seq-v1 (futility) | 3.2–3.4% any verdict | 20.6 | 2.1 / 23 | — |
| | fixed 16 | 4.9–6.6% | 32 | 3.2 / 35 | — |

- **Sequential stopping at 3.5%** does not save dollars per decisive verdict. Against fixed 8 tasks:
  - at a 20% cut it takes +5% runs per decisive verdict at τ = 0.10, and −2% at τ = 0.25;
  - at a 35% cut it takes +27% (τ = 0.10) and +23% (τ = 0.25), because the stricter first look (99.84%) sends effects that fixed 8 would already decide on to 12 tasks.
- **What it buys:**
  - a single experiment is far more often decisive (47% → 74% at a 20% cut), which shortens the time to the first decisive verdict;
  - a 5% guard that holds on lopsided noise, which phase1-v2's does not (5.38% and 5.79%, §7);
  - a third fewer runs than a fixed 16-task design when nothing changed.
- **Reuse (deferred):** it saves runs only for cuts of 35% or more. At 20% it loses, because the allowance eats power.
  - At 35% (τ = 0.10) it saves 6.3 runs per decisive verdict against `seq-v1` (21.4 → 15.1), and 1.7 against fixed 8.
  - A validation costs 64–128 runs, and must be repeated every 14 days (§2). So reuse breaks even after about 10–20 reuse experiments per key within 14 days against `seq-v1`, or 38–75 against fixed 8.
  - It is not a lever for the first verdict.
- **Dollars per decisive cost verdict on samber/lo-sized tasks:** about $1.7–5 across the rows above. The most expensive is a 20% cut at τ = 0.25 ($5.0 with `seq-v1`, $5.1 with fixed 8).
- **Time** (estimates):
  - samber/lo-sized runs (30–60 s of agent time): about 1.5 minutes per pair at concurrency 2, so a 13-task `seq-v1` experiment takes about 20 minutes.
  - Agentium-sized runs (4–6 minutes): about 1.5 hours.
- **Success:** no lever here reaches a success verdict for $40 on Agentium-sized tasks.
  - The 20 × 3 floor costs about $130 on Sonnet 5, and certifies only a 21–25 pp margin.
  - On small tasks the same floor costs about $12–34 (120 runs at $0.10–0.28).
  - **Recommended success target:** set it on small-task repositories, where the floor already fits $40.

## 7. Simulation evidence
**Model.** Each task has a level (removed by pairing). Each arm's log cost is level + effect + σ·noise. A task's effect is the mean effect + τ·noise.
- **Noise shapes,** standardized to mean 0 and variance 1:
  - normal;
  - a skewed lognormal (skewness about 0.9, as `TestOneRunCostVerdictsSimulation`), and its mirror;
  - a strongly skewed lognormal (shape 0.6, skewness about 2.3), and its mirror;
  - Student's t with 5 degrees of freedom (kurtosis 9).
- **Why the same shape in both arms tests little:** with one run per arm and the same noise in both, each task's difference is symmetric whatever the shape. Those groups test the tails more than the skew.
- **The arm-specific group** gives the arms different shapes with equal mean log cost, and in one scenario different spreads (0.19 against 0.35), as two models or contexts could:
  - skewed against normal;
  - skewed against its mirror;
  - strongly skewed against normal;
  - strongly skewed against its mirror, at τ = 0 and 0.10;
  - strongly skewed (σ 0.19) against normal (σ 0.35).
- **The recorded group** resamples the 22 per-task log differences of `aa-report.md`, `context-ab-16-report.md` and `model-ab-report.md`, each experiment's centered and scaled by its own spread.
- **Analysis:** the production `NewBootstrap`, `TInterval` and `Decide` at each look's levels, with 200 bootstrap draws (50 in the default run). The t-alone counts, which the assertions use, are an upper bound and differ from the widest-interval counts by at most 0.05 points.
- **Seeds:** each scenario has its own seed, so the counts are reproducible.

**False differences without a true difference** (`seq-v1` at 3.5%, 20,000 experiments per scenario, 120,000 per group):

| Group | Futility ignored | Futility obeyed | t-interval alone (gate) |
|---|---|---|---|
| normal | 3.65% [3.54, 3.76] | 3.45% [3.35, 3.55] | 3.68% [3.57, 3.79] |
| skewed | 3.50% [3.40, 3.60] | 3.30% [3.20, 3.40] | 3.54% [3.43, 3.64] |
| heavy-tailed | 3.40% [3.30, 3.50] | 3.21% [3.12, 3.31] | 3.43% [3.33, 3.53] |
| arm-specific shapes | 4.45% [4.34, 4.57] | 4.23% [4.11, 4.34] | 4.50% [4.38, 4.62] |
| recorded differences | 3.81% [3.70, 3.92] | 3.55% [3.45, 3.66] | 3.85% [3.75, 3.96] |
| all, pooled (600,000) | 3.76% [3.71, 3.81] | — | 3.80% [3.75, 3.85] |
| A/A only (τ = 0, like shapes) | 3.48% [3.38, 3.58] | — | — |

- **Within the arm-specific group** (t alone):

| Arms | False differences |
|---|---|
| skewed against normal | 3.80% |
| skewed against its mirror | 4.25% |
| strongly skewed against normal | 3.87% |
| strongly skewed against normal, wider spread | 3.50% |
| **strongly skewed against its mirror, τ = 0** | **6.54% [6.21, 6.90]** |
| strongly skewed against its mirror, τ = 0.10 | 5.01% [4.72, 5.33] |

  - **Why the starred row fails:** its per-task difference has a skewness of about −1.6, which a t-interval on 8 tasks does not survive at any α tested (7.64% at 4.5%, 6.54% at 3.5%).
  - **It is not a dollar difference:** equal mean log cost leaves arm B 0.55% cheaper in arithmetic mean, yet its false verdicts mostly say "regressed" (1,286 against 122 "improved" in 20,000, at 4.5%).
- **phase1-v2 on the same scenarios,** logged by the same test, at its 95% level: the recorded differences give 5.38% [5.25, 5.51] and the arm-specific shapes 5.79% [5.66, 5.92]. An ad-hoc probe at 48,000 experiments each gave 5.31% and 5.76%.
- **The α probe** that informed the decision (ad-hoc, not committed; 144,000 same-shape and 48,000 recorded and arm-specific experiments per design, t alone):

| | 4.5% | 4.0% | 3.5% |
|---|---|---|---|
| Same-shape | 4.60% [4.49, 4.71] | 4.05% [3.95, 4.16] | 3.52% [3.43, 3.62] |
| Recorded | 4.95% [4.76, 5.15] | 4.43% [4.25, 4.62] | 3.85% [3.68, 4.02] |
| Arm-specific | 5.71% [5.51, 5.92] | 4.87% [4.68, 5.06] | 4.55% [4.37, 4.74] |

- **Power** (5,000 experiments per row; "improved" at a true cut):

| Cut | τ | fixed 8 | seq-v1 (3.5%) | fixed 16 | seq-v1 tasks used |
|---|---|---|---|---|---|
| 10% | 0.10 / 0.25 | 13.5% / 10.4% | 20.8% / 14.4% | 27.3% / 18.4% | 11.9 / 11.3 |
| 20% | 0.10 / 0.25 | 47.1% / 31.7% | 74.2% / 52.4% | 83.0% / 62.1% | 13.2 / 13.0 |
| 35% | 0.10 / 0.25 | 95.1% / 81.2% | 99.9% / 98.1% | 99.98% / 99.1% | 10.7 / 11.9 |
| 63% | 0.10 / 0.25 | 100% / 100% | 100% / 100% | 100% / 100% | 8.0 / 8.2 |

  Skewed noise at τ = 0.25 gives within 4 points of the normal rows. With no effect, `seq-v1` uses 10.3 tasks (futility obeyed).
- **Reuse:** see §2. Reuse experiments use arm A = 4 stored runs per task with the true bias, arm B = 1 fresh run, the `seq-v1` looks at 3.5% without futility, and the allowance from a passing validation.
  - At no bias: "improved" at a true 20% cut 40.6% (15.3 fresh runs), at 35% 99.8% (11.1), at 63% 100% (8.0).
- **Drift:** see §4.
- **Runtime:** the default run, which CI runs under the race detector, takes about 31 s for `internal/stats` on 2 cores (`GOMAXPROCS=2`), 13.5 s of it the existing phase1-v2 simulation.

## 8. Assumptions and limitations
- **Tasks:** independent, and the effects across tasks are independent of the noise. Missing tasks (infrastructure failures) are not informative.
- **Configuration:** the simulated one is one run per arm. Futility uses the t-statistic as if it were normal; since futility is non-binding, this can only cost power.
- **Noise:** real cost noise could be bimodal (cache states). The isolated-run cost removes the known bimodality; its effect on σ is unmeasured.
- **The worst arm-specific scenario stays above 6% at any α tested:** both arms strongly and oppositely skewed, a per-task difference skewed about −1.6, 6.54% at 3.5%. That is a t-test limit at 8 tasks.
  - The pooled arm-specific group passes, at 4.50% [4.38, 4.62].
  - A skew-robust interval (bootstrap-t or Johnson's skew-corrected t) is the research item for it, for both methods (the user's decision).
- **phase1-v2's known limitation** (the user's decision): its fixed 8-task design at 95% exceeds the 5% guard on lopsided noise.
  - **Measured:** 5.38% [5.25, 5.51] on resampled real differences (5.31% in the earlier probe), and 5.79% [5.66, 5.92] on arm-specific shapes (5.76%).
  - **Unchanged:** existing and locked phase1-v2 experiments are not re-analysed. New cost experiments use `seq-v1`.
- **Reuse bias** is modeled as a constant per key, with the validation's noise independent of the experiment's. In practice later stored runs are the validation's fresh side, which correlates them. Anchors were not simulated.
- **Day effects in reuse** were simulated at γ = 0.03 and 0.05, as one shift per occasion. They cut the validation's pass rate with no bias from 88% to 53–69%. After a pass they raise false differences to 0.2–2.3%, which is still under 5%.
- **Biases of ±12%:** the validation passes 3–4% of the time. When it does, the bias exceeds the allowance in 25–29% of passes, and false differences after a pass are 0.6–2.7%.
- **The drift chart** assumes check-to-check independence, without autocorrelated day effects, and step changes. Slow drifts get absorbed into a self-starting baseline.
- **The cache bound** counts each run's first request only, and is an upper bound. Step 2 checks the repricing rule on recorded transcripts.
- **The source of the plan's "about 9%"** was not recorded. It matches the bound for ab16.
- **σ was swept at 0.19 and 0.35.** Results under like-shaped normal noise do not depend on σ.

## 9. Decisions and what stays open
The user's decisions (2026-10-02):
1. α for `seq-v1`: 4.5% was approved first, then lowered to **3.5%** after the arm-specific result.
2. Actual cost stays the primary metric. The isolated-run cost is reported beside it.
3. Sequential stopping is built now. The reuse key, the reuse A/A, reuse in experiments and the drift chart are deferred, and this note keeps their design.
4. The `seq-v1` smoke check (about $3) is approved once the engine is built. The reuse A/A is not approved.
5. Futility stops are on by default (non-binding).
6. The drift panel is deferred.
7. The reuse defaults (14-day window, the ±15% pass bound, 4 anchors) stay open until reuse resumes. They must be fixed before any validation runs.
8. phase1-v2: existing and locked experiments stay unchanged, and new cost experiments default to `seq-v1`. Its excess over 5% on lopsided noise is a known limitation (§8).
9. A skew-robust interval (bootstrap-t or Johnson's t) is a later research item, for both methods.
