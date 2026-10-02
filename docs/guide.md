# Guide

The full manual flow. For a first run, use `agentium start` from the [README](../README.md) quick start; this guide covers each step by hand, and the details behind it.

## Contents

- [Registering a repository](#registering-a-repository)
- [Versioning context](#versioning-context) and [context lint](#context-lint)
- [Turning commits into tasks](#turning-commits-into-tasks) and [the task pool](#the-task-pool)
- [Build tools and offline dependencies](#build-tools-and-offline-dependencies)
- [Running and comparing](#running-and-comparing)
- [Experiment templates](#experiment-templates)
- [The judge](#the-judge-second-opinion)
- [Scripting and automation](#scripting-and-automation)
- [Data folder and environment](#data-folder-and-environment)

## Requirements in detail

- Go 1.27.1 and a C compiler (SQLite is built with cgo); Git.
- Your project's own build tool, on the machine that runs Agentium: Go, Maven or Gradle (`mvnw` and `gradlew` preferred; Java and Kotlin, with a JDK) or Cargo. Go, Maven and Cargo are proven in real runs; Gradle too, except worker-daemon tools such as Checkstyle and PMD (see [build tools](#build-tools-and-offline-dependencies)).
- [Claude Code](https://claude.com/claude-code), signed in. `ANTHROPIC_API_KEY` or a token file in `AGENTIUM_CLAUDE_TOKEN_FILE` works too.

## What `start` does

`agentium start` never writes to your repository and makes no paid run on its own. It skips steps already done, so run it again to resume:

- registers the repository (`init`) and, if the project has no snapshot, saves the committed context as `baseline` (arm A);
- mines and validates tasks until 16 are ready; when the history has no more candidates, 8 or more will do (the cost floor), with fewer looks;
- creates the experiment `quick-...`, a cost experiment (method `seq-v1`, below) on those tasks x 1 run per arm: an A/A calibration of your context, or with `--b SNAPSHOT` a comparison of the context with that snapshot;
- prints the preview: the looks, the maximum and expected spend, and what is missing;
- stops there. `--yes` (or answering `y` on a terminal) runs the experiment, within its budget (`--budget USD` raises it). The run first calibrates each context that lacks a calibration (a short paid run, about $0.1 to $0.2, counted in the budget and shown in the preview).

Mined instructions need your review for solution leaks (`agentium task show NAME`, then `agentium task edit NAME --reviewed`, or `agentium task rm NAME` for one you will not accept), so a first `start` stops there and lists them. `start --accept-mined` accepts the tasks it mined without your review: it checks only solution headings, reference-file names and unstated test requirements, so an instruction that explains the fix passes. The default A/A calibration never counts toward the first decisive verdict.

`start` and `experiment report` also show how long it took, and what was spent, to your first decisive verdict (improved, regressed or no loss; inconclusive does not count), from finished experiments only.

## Registering a repository

```sh
agentium init /path/to/your/repo   # registers it; Agentium never writes to your repository
cd /path/to/your/repo
```

## Versioning context

```sh
agentium context show                              # what Claude Code loads at start, and on demand
agentium context snapshot baseline                 # save the committed context (HEAD) as a version
# edit CLAUDE.md, rules or skills, then:
agentium context snapshot trimmed --working-tree   # --include-linked adds linked docs
agentium context diff baseline trimmed --patch
```

### Context lint

Free: `agentium context lint [--ref REF]` checks the working tree (or a commit) and reports the size change against your latest snapshot, broken `@` imports, `AGENTS.md` files that Codex would cut off (it reads only the first 32 KiB of the chain from the root down) and the warnings `show` gives. It runs no agent and exits 0 even when it finds problems. To see it after every edit of a context file in Claude Code, run `agentium context lint --print-hook` and merge the printed `PostToolUse` hook into your own `~/.claude/settings.json` (Agentium never writes your settings, and its runs load project settings only, so the hook never fires inside them).

## Turning commits into tasks

```sh
agentium task mine --dry-run                       # commits that would make good tasks (tests and code changed, small, a clear message), and why others don't
agentium task mine --limit 10                      # import the best 10 and validate them: base: the parent; hidden tests: the test-file changes
agentium task edit <name> --reviewed               # once the instruction doesn't give the solution away (--accept-gaps: hidden tests need texts or names nothing states)
agentium task validate --all --snapshot trimmed    # tests fail on the base and pass with the reference, in each arm (--jobs N at a time)
agentium task import --commit <sha>                # one commit by hand (mining skips commits that are already tasks)
agentium task validate <name> --repeat 3           # run every stage 3 times: a task whose runs disagree is flaky, and experiments reject it
agentium task validate <name> --weak-tests          # which parts of the reference the hidden tests do not need (a warning, not a gate; a later validate without the flag drops the list)
```

Validation also warns when a verify command's `go test` filter keeps one of the task's own hidden tests from running: a `-skip` pattern that matches a test function the solution adds or changes in a hidden `_test.go` file, or a `-run` pattern that does not. Grading would never run that test. It is a warning, not a gate: the status stays as the stages found it, and `task show` repeats it.

### The task pool

Free, no agent runs: `agentium pool update` is one pass over the pool.

```sh
agentium pool update --dry-run    # what a pass would mine, validate, re-validate and retire; writes nothing
agentium pool update              # mine the commits since the last pass, validate them, re-validate stale tasks, retire dead ones
agentium pool status              # valid (and weak), flaky, invalid, awaiting review, retired; the last pass; the oldest valid base
```

- **Mining** reads the default branch's commits since the last pass, oldest first (at most 2,000 per pass; the next pass reads on), within 270 days by committer date, and imports up to `--limit` (default 10) as tasks that need your review, validated `--jobs` at a time (default 2). Commits that are tasks already, and rebased or cherry-picked copies of any task's change (same patch ID), are skipped; so is a mined task's commit once you remove the task. A candidate whose base would retire within 30 days is not imported.
- **Re-validation:** a task is stale when its last validation is more than 30 days old, was made with other versions of the build tools (Go, Maven, the JDK, Cargo, rustc; `task validate` records them), or is flaky and untried for 7 days (it is tried with `--repeat 3`). It keeps its arms and repeats, and its weak-tests result. A task that a locked, unfinished experiment uses is kept as it is ("kept for experiment X"). While an experiment is running, re-validations are skipped with a warning (they would slow its runs; the pass checks again before each `--jobs` tasks, and stops when one starts), and new tasks are validated one at a time. Without a detected test command, a pass mines nothing and says so, but still re-validates and retires.
- **Retirement** is a flag with a reason, never a delete: a base 270 or more days old, or a hidden test or reference file the solution has that is gone from the default branch. Retired tasks leave new experiments; locked ones keep them.
- **Review:** mined tasks wait for `agentium task edit NAME --reviewed`. `--accept-mined` accepts only the valid tasks this pass imported, after `start --accept-mined`'s checks; it accepts nothing when the pool's state file was unreadable.

### Build tools and offline dependencies

Mined tasks verify with your build tool's test command (`go test ./...`, `./mvnw -q test` or `mvn -q test`, `./gradlew test` or `gradle test`, `cargo test`); `--verify` changes it. Dependencies for the agent's offline builds are fetched by a run's setup, once per base commit and tool set, into the `deps` folder of your data folder (`~/.agentium`, or `AGENTIUM_HOME`). Build files are detected only at the repository root: a Maven or Cargo build in a subfolder is not detected, so its agent gets no offline dependencies, and your caches stay denied to it. Plugins that fetch their own tools when a task runs (Spotless, say) are not warmed, so those tasks fail offline in the agent's sandbox. Gradle tools that run in a separate worker process (Checkstyle, PMD) also fail in the agent's sandbox: Claude Code's sandbox allows only IPv4 localhost connections, and Gradle starts those workers without the option that keeps Java on IPv4. Your machine's grading is unaffected; only the agent can't run them itself. Gradle's file-lock service needs the sandbox's local binding, which lets the agent bind any local port and reach localhost services (outbound network to other hosts stays blocked): agent runs on a Gradle project refuse to start until you run `agentium init --allow-local-binding`. Validation fetches those dependencies first, as a run's setup does, into the same `deps` folder (a task's runs then find them ready), so it runs the verification with what grading uses: for a Python project, the same venv, interpreter and `PYTHONPATH`. When another run or validation is fetching for the same project, it waits up to 15 minutes, then validates without the fetched dependencies and says so in a note. Validation builds and runs tests on your machine, two tasks at a time by default: `--jobs` above 1 assumes your tests can run side by side (no fixed ports, shared `/tmp` paths or databases), so use `--jobs 1` if they cannot.

For a Python project the venv holds its dependencies only, never the project: tests import the project from the checkout (`PYTHONPATH`). So that tests asking `importlib.metadata` for the project's version still work, each base also gets the project's metadata, made at warm-up by `uv build --wheel` (or the venv's `pip wheel`) and reduced to its headers: a read-only `.dist-info` with nothing but `METADATA`, after the checkout on `PYTHONPATH`, for the agent, validation and grading alike. A checkout holds one commit, so a version computed from git (setuptools-scm, hatch-vcs) reads `0.0.0`, with a note; console scripts and entry points of the project itself are not there. Hypothesis keeps its database in the run's cache, or for validation and grading in one per checkout in the data folder, never in the checkout. A project's known-flaky tests fail validation at random: leave them out of the verify command (`agentium task mine --verify "uv run pytest -q --deselect tests/test_x.py::test_flaky"`, say), or validate with `--repeat 3`, which marks a task whose runs disagree as flaky.

## Running and comparing

> [!WARNING]
> These commands start real Claude Code runs. They cost money, or use your plan's limits.

```sh
agentium run calibrate --snapshot trimmed     # optional: short checks (sandbox, large outputs, context size, tools); experiment run does it for any arm that lacks one
agentium run once <name> --snapshot trimmed   # one run, graded with the hidden tests
agentium experiment new lean --b trimmed      # a cost A/B: each task's own context against trimmed, on up to 16 valid tasks (method seq-v1, below)
agentium experiment plan lean                 # runs, estimated cost (calibrations included), detectable effects; what is missing
agentium experiment run lean                  # calibrates what is not calibrated, locks it, then runs interleaved pairs in stages, a look after each, within the budget; resumable; pauses before your plan's usage limit (--wait waits for the reset)
agentium experiment show lean                 # the lock and the progress per arm
agentium experiment report lean               # verdicts, intervals, per-task results (--markdown for a pull request, --json for everything)
```

Runs use `claude-sonnet-5-5` unless `--model` says otherwise (`experiment new`, `run once` and `run calibrate`); an experiment keeps the model it was made with. Estimates come only from earlier runs on the same model (and effort), so the first experiments on Sonnet 5.5 fall back to a default task run at list prices, and their default budgets and consent prompts look high until runs on it measure the project.

Each run stops at its cap (`--run-budget`, default $3). Claude Code checks the cap after each turn, so a run can pass it by the turn that crosses it (the `seq-v1` smoke check: $0.507 against $0.50). The budget therefore holds back an allowance beside the cap of every run in flight: 10% of the cap, at least $0.15 on a model whose output costs what Sonnet's does, a floor that scales with the model's output price ($0.30 on Opus 5.5, $0.375 on Opus 5, the dearest price in the table for a model without one). A judge other than the default model and effort holds the same allowance per call. The preview's worst case includes it, and a run's estimate is never above its cap. A capped run records how far it went past its cap; one that passed the allowance gets a warning in the run's progress and in the report.

The report marks runs cut short, capped at their cost cap or turn limit or stopped at the timeout, in the per-task table (`1/1, 1 capped`, and a mean cost of `≥$0.507`) and counts them in a note: such a run counts as it ended, graded and at the cost it reached, which is a lower bound of what it would have cost. That makes an arm cut short more often look cheaper, so a cost verdict that favours it says so in its headline.

### How a cost experiment decides

Cost experiments (`--goal cheaper`, the default) run method `seq-v1`, a group-sequential design: up to 16 tasks x 1 run per arm, in stages, with a look after 8, 12 and 16 tasks (one look after all of them with 8 to 11 tasks, two with 12 to 15).

- **Looks.** A look comes once its stage's runs are settled, retries included, and analyses exactly the tasks of the stages so far. No run of the next stage starts before it. Each look's cost interval is wider than a fixed design's (99.84%, 98.84% and 96.88% at 8, 12 and 16 tasks): together they spend a two-sided 3.5%, O'Brien–Fleming-type, which keeps false differences at or under 5% in the simulations of the [statistics note](research/2026-10-02-wave3-statistics-note.md).
- **Stops.** The experiment stops at the first look with a cost verdict (improved, regressed or equivalent), or for futility, when a verdict by the last look has become unlikely (below 10%); `experiment new --no-futility` turns futility stops off.
- **Spend.** `experiment plan` shows each look's runs and spend, the maximum (every stage; the default budget covers it), and the expected spend if nothing changed and at a 20% cut.
- **Stops between looks.** The budget, the usage limit or Ctrl-C keep the last look's verdict; `experiment run` resumes the stage, and its look comes once the stage is settled.
- **Reading it.** The report says where the experiment stopped ("stopped at look 1 of 3") and lists each look with its interval. An early stop overstates the effect's size on average: the true change is likely smaller than the estimate.

Success and time are exploratory in cost experiments. For success verdicts use `--goal better` with `--tier quick|confident` or `--task` and `--repeats` (method `phase1-v2`, one analysis at 95%). Experiments locked before keep the method they were locked under.

## Experiment templates

 `experiment new` has three: `context-ab` (the default; `--b` names the snapshot to compare with arm A's context), `aa` (one context in both arms, which must find no difference: it measures the noise) and `model-ab`, which compares two Claude Code profiles on the same tasks and one context:

```sh
agentium experiment new models --template model-ab --a claude-sonnet-5-5 --b claude-opus-5-5:high [--context trimmed]
```

`--a` and `--b` are `MODEL` or `MODEL:EFFORT` (low, medium, high, xhigh or max; without one, the CLI's default). The arms must differ in model or effort. Both run one context: the base's own, or `--context SNAPSHOT`. The plan estimates each arm from your earlier runs on its model (or from a default run at list prices), flags a model without a list price, and covers both arms in the budget; `--run-budget-a` and `--run-budget-b` give an arm its own run cap. Each arm's model needs its own calibration of the context: `experiment run` makes the ones that are missing, once, before the first pair (`run calibrate --model MODEL` does it ahead of time). The preview counts their cost, and a calibration that fails its checks stops the experiment before any task run. Reports of model experiments name each arm by profile (model and effort) in the headlines, the metric tables and the per-task rows, with a one-line verdict such as `B (claude-sonnet-5-5) costs 48% less; success: exploratory`; the noise note says it pools both models.

## The judge (second opinion)

 Tests decide pass and fail. `experiment new ... --judge` also asks an LLM judge about every graded run: does its change do what the task asks, as the task's reference solution does? The judge reads the instruction and both changes' code, never the tests; tasks whose reference solution has no code are skipped. It answers fixed, partly or no, with a one-line reason, and takes the majority of a few repeats. `--judge-model`, `--judge-effort` and `--judge-repeats` set it.

The report's Judge section shows, for each arm:
- the judge's verdicts among passing runs and among failing runs, with 95% intervals;
- the runs it did not judge, and why;
- how often its repeats agreed, and what it cost.

It then lists the passing runs the judge did not call fixed, each with its reason, for you to check.

Its limits:
- **It decides nothing.** Success, cost and every verdict stay the tests'.
- **Its accuracy is unmeasured.** In the [pilot](research/2026-10-01-judge-pilot-results.md), it judged 18 of 40 passing runs not fully fixed.
- **It costs extra.** Each call costs a few cents: about $0.065 a call (the preview's estimate; $0.063 per single judgement in the pilot). The calls count against the budget, but not toward an arm's cost.

## Scripting and automation

For hooks, schedulers and scripts. `--json` covers `init`, `context show|snapshot|list|diff|lint`, `task list|show|mine|validate|import|add|edit|rm`, `run once|show|list`, `pool update|status`, `start` and `experiment new|plan|show|list|run|rm`. `experiment report --json` exists already, with its own shape (the lock and every run); `run calibrate` has human output only.

**`--json`** prints exactly one JSON document on stdout and no other text there; progress, color and questions are off. Put it after the subcommand: `task list --json`. `task --json list` is deliberately not recognized.
- Top level: `"schema"` (1; raised only when a field is removed, renamed or changes meaning, never for added fields) and `"command"` (for example `"task list"`). Fields are snake_case. Lists are `[]`, never `null`, and every field is always present: one that can be unknown or not asked for is `null` (`solution_commit`, `unstated_requirements`, `passed`, `diff`, `patch`, the logs of `run show`).
- Failure: `{"schema": 1, "command": "...", "error": {"message": "...", "code": 1}}`. `code` is the exit code. `message` is the command's own error sentence, for example `task "nope": not found`, or `agentium task edit: give NAME and at least one of ...` for a usage error; when the arguments are wrong in a way that has no sentence, `invalid arguments: run the command with -h for its usage`. The usage text and earlier warnings are never part of it.
- A result that is bad rather than broken keeps its normal document and exit 1: an invalid or flaky task (`task validate`), `start` with too few tasks (`"status": "too_few_tasks"`) or with mined tasks waiting for your review (`"status": "awaiting_review"`), an `experiment run` that stopped (`"status": "stopped"`) or was refused for want of `--yes`.
- **Unstable human text:** `log`, `status_summary`, `summary`, `message`, `warnings`, `notes`, `problems`, `reason`, `status_note`, `note` (of a run result, a look or a metric), `verdict.summary` and `readiness[].text` are for people. Their wording changes; do not parse it. Branch on the other fields, and on `status` values.
- **`readiness[].status`** is one of `ok`, `missing` or `warning`, mapped from the labels the text shows, so a change of label does not change JSON.
- No ANSI escape ever, whatever `NO_COLOR`, `FORCE_COLOR` or the terminal say. Documents name no absolute path (repository, data folder, records, Claude Code), no user name and no secret, as reports do; repository files appear as relative paths. In free text (messages, logs, warnings, notes) the data folder is shown as `<data>`, the repository as `<repo>` and the home folder as `~`, at whole path names only. The setup and verification logs of `run show --log` are Agentium's own output and are redacted the same way; content that is yours (diffs, patches, instructions, file names) is never rewritten.
- Warnings for a person may still appear on stderr. `--json` with `-h`, `-help` or `--help` prints the usage as text. `context lint --hook` and `--print-hook` already print Claude Code's JSON and refuse `--json`.

**Exit codes**

| Code | Meaning |
|---|---|
| 0 | Success, including nothing to do: no tasks, no candidates, no differences, or `start` stopping at its preview. `context lint` exits 0 when it finds problems. |
| 1 | Runtime failure, or a bad result: an invalid or flaky task, `start` with too few tasks or with tasks awaiting review, `start --yes` that was not ready, an `experiment run` that stopped or was refused. A run that ended at its budget, at a usage pause or at a look is exit 0 (a result: read `status`). |
| 2 | Usage error: bad flags or arguments. |

**No prompts.** Agentium asks one question: `start`'s "Run it now?", and only when stdin and stdout are both terminals. With stdin from a pipe, a file or `/dev/null` it never reads stdin and stops at the preview; `--json` never asks, even at a terminal. Only consent spends money: `start --yes`, and for the experiment commands `experiment run NAME --json --yes`. `experiment run --json` without `--yes` opens nothing, runs nothing, prints `"status": "refused"` and exits 1. (`--yes` is accepted without `--json` and changes nothing there: a person's own `experiment run` is the consent.)

For `pool update`, `"tasks"` holds the validations of mined tasks (and candidates that failed to import), `"revalidated"` the stale tasks with their new `"status"` (`null` when not stored: `"problem"` says why, or when `"revalidations_skipped"` is true), `"kept"` the stale tasks experiments use, `"retired"` the tasks retired with their reasons, and `"health"` the counts `pool status` prints (`"last_pass"` and `"oldest_valid_base"` are `null` when there is none). With `--dry-run` the same fields say what it would do. Exit 0 includes a pass with invalid tasks; 1 is an interrupt or a failure, such as another pass of the project running.

For `start`, read `"status"`: `preview` (everything is in place; `"run_command"` starts the experiment and spends money), `not_ready` (`"readiness"` lists what is missing; exit 1 with `--yes`), `awaiting_review` (tasks start mined wait for a person's review before the experiment is made: `agentium task show NAME`, then `agentium task edit NAME --reviewed`), `too_few_tasks` (fewer than 8 valid tasks, and none waiting for a review), `finished` (the experiment is already done) or `ran` (`--yes` ran it; `"run"` is the result below, and the exit code follows its status). `"tasks_ready"` and `"tasks_awaiting_review"` count the tasks when `start` looked at them, and are `null` when the experiment already existed. `"nothing_was_run"` is true unless `--yes` ran the experiment. `"experiment"` carries the design's `"method"`, its planned `"looks"` and its `"spend"` (below).

**The experiment commands** share these objects:
- `experiment`: the design: `name`, `template` (`context-ab`, `aa`, `model-ab`), `goal`, `method` (`seq-v1` for a cost experiment), `arms[]` (`name`, `context`, `model`, `effort`), `tasks[]`, `repeats_per_arm`, `runs` (all, both arms), `budget_usd`, `run_budget_usd`, `concurrency`, `judge`.
- `spend` (in `experiment plan` and `start`'s preview): `known` (false when a task has no estimate yet, which makes the estimates `null`), `max_usd` (every look runs), `worst_case_usd` (every run and judgement at its cap, plus each cap's overshoot allowance: what the budget holds back for a run in flight), and for `seq-v1` `expected_usd` and `expected_tasks` (on average, if nothing changed) and `if_cut_usd` (at a 20% cut in B's cost). `looks[]` (seq-v1) lists each planned look: `tasks`, `runs`, `estimated_usd`, `worst_case_usd`, `efficacy_level`, `equivalence_level`; `sizes[]` does the same for a fixed design.
- `looks[]` in `experiment show` and `experiment run` are the looks made: `look`, `tasks_planned`, `tasks_counted`, `analysed`, `level` and `interval` (`estimate`, `low`, `high`, a ratio B / A for cost, at the look's level), `verdict`, `conditional_power`, `decision` (`continue`, `stop`, `futility`, `final`). `level`, `interval` and `verdict` are `null` for a look that had too few tasks to give a verdict.

`experiment new` prints the `experiment` and `plan_command`; `experiment plan` the design, `ready`, `readiness[]`, the calibrations it needs (`calibration_runs_needed`, `calibration_estimate_usd`), `eligible_tasks`, `ineligible_tasks[]` (`task`, `reason`), `spend`, and `looks` or `sizes`; `experiment show` the design, `status`, `lock` and `progress` (both `null` before the first run: slots settled, spend, per-arm counts, the looks, `ended_by`); `experiment list` `experiments[]`; `experiment rm` `removed`.

**`experiment run --json --yes`** prints one document when the run ends (no progress lines): `{"experiment": NAME, "run": {...}}`. Read `run.status`:

| `status` | Meaning | Exit |
|---|---|---|
| `done` | Every run settled, or a `seq-v1` experiment ended at a look: `ended_by` is `stop` (a decisive cost verdict, at `stopped_at_look`), `futility` or `final`. | 0 |
| `budget` | The next run would not fit the budget. `next_command` raises it; the looks so far stay. | 0 |
| `usage` | Paused before the subscription's usage limit (or the judge hit it); `resume_at` says when the window resets, when known. | 0 |
| `stopped` | Interrupted, repeated infrastructure failures, a changed environment. The spend and counts are in the document. | 1 |
| `refused` | `--yes` was missing: nothing ran. Only `status`, `note` and `next_command` (the command to run, with your `--budget`, `--usage-limit` and `--wait`) mean anything; `method`, `runs`, `spent_usd` and `budget_usd` are `null`. | 1 |

The result also has `note` and `method` (human text, and the experiment's method; an error after runs started is in `note` with the stored `status` kept, and the exit code is 1 whatever the status), `judge_paused`, `looks[]`, `runs` (`total` = `settled` + `pending` + `skipped` + `failed`: `pending` slots would run on a resume; `skipped` ones a `seq-v1` experiment chose not to run because it ended at a look, so `pending` is 0 once `ended_by` is set; `failed` ones ran out of attempts and a resume does not retry them), `spent_usd` (everything the budget counts, calibrations and the judge too) and `budget_usd`, `verdict` (`decisive`, a human `summary`, and `metrics[]` with `metric`, `role`, `verdict`, `decisive`, `tasks`, `a`, `b`, `interval`, `level`; `null` when no run exists or it could not be computed, in which case `note` says why; a number that is not finite is `null`), `north_star` (as in `start`) and `next_command` (for `usage` with no `resume_at`, it suggests `--usage-limit PCT`). A failure before any run (not ready, a lower `--budget`, an unreadable lock) is the error document, exit 1, or 2 for a usage mistake. In `start --json --yes` the same object is `"run"`.

Planned (part 2): a committed `agentium.toml` that Agentium reads and never writes, for budgets and consent to spend. See the [plan](../.agents/plans/2026-10-02-headless.md).

## Data folder and environment

Data lives in `~/.agentium`; set `AGENTIUM_HOME` to use another folder. Output is styled only on a terminal: `NO_COLOR=1` turns color off, and `FORCE_COLOR=1` keeps it through a pipe (for `less -R`).

| Variable | Effect |
|---|---|
| `AGENTIUM_HOME` | Data folder (default `~/.agentium`) |
| `NO_COLOR`, `FORCE_COLOR` | Turn color off; keep it through a pipe |
| `ANTHROPIC_API_KEY` | Claude Code credentials, as an alternative to signing in |
| `AGENTIUM_CLAUDE_TOKEN_FILE` | A token file for Claude Code |
