# Guide

The full manual flow. For a first run, use `agentium start` from the [README](../README.md) quick start; this guide covers each step by hand, and the details behind it.

## Contents

- [Registering a repository](#registering-a-repository)
- [Versioning context](#versioning-context) and [context lint](#context-lint)
- [Turning commits into tasks](#turning-commits-into-tasks)
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
- mines and validates tasks until 8 are ready, the cost floor;
- creates the experiment `quick-...` at the floor, 8 tasks x 1 run per arm: an A/A calibration of your context, or with `--b SNAPSHOT` a comparison of the context with that snapshot;
- prints the preview: runs, estimated cost, detectable effect, and what is missing;
- stops there. `--yes` (or answering `y` on a terminal) runs the experiment, within its budget (`--budget USD` raises it). The run first calibrates each context that lacks a calibration (a short paid run, about $0.1 to $0.2, counted in the budget and shown in the preview).

Mined instructions need your review for solution leaks (`agentium task show NAME`, then `agentium task edit NAME --reviewed`), so a first `start` stops there. `start --accept-mined` accepts the tasks it mined without your review: it checks only solution headings, reference-file names and unstated test requirements, so an instruction that explains the fix passes. The default A/A calibration never counts toward the first decisive verdict.

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

### Build tools and offline dependencies

Mined tasks verify with your build tool's test command (`go test ./...`, `./mvnw -q test` or `mvn -q test`, `./gradlew test` or `gradle test`, `cargo test`); `--verify` changes it. Dependencies for the agent's offline builds are fetched by a run's setup, once per base commit and tool set, into the `deps` folder of your data folder (`~/.agentium`, or `AGENTIUM_HOME`). Build files are detected only at the repository root: a Maven or Cargo build in a subfolder is not detected, so its agent gets no offline dependencies, and your caches stay denied to it. Plugins that fetch their own tools when a task runs (Spotless, say) are not warmed, so those tasks fail offline in the agent's sandbox. Gradle tools that run in a separate worker process (Checkstyle, PMD) also fail in the agent's sandbox: Claude Code's sandbox allows only IPv4 localhost connections, and Gradle starts those workers without the option that keeps Java on IPv4. Your machine's grading is unaffected; only the agent can't run them itself. Gradle's file-lock service needs the sandbox's local binding, which lets the agent bind any local port and reach localhost services (outbound network to other hosts stays blocked): agent runs on a Gradle project refuse to start until you run `agentium init --allow-local-binding`. Validation builds and runs tests on your machine, two tasks at a time by default: `--jobs` above 1 assumes your tests can run side by side (no fixed ports, shared `/tmp` paths or databases), so use `--jobs 1` if they cannot.

## Running and comparing

> [!WARNING]
> These commands start real Claude Code runs. They cost money, or use your plan's limits.

```sh
agentium run calibrate --snapshot trimmed     # optional: short checks (sandbox, large outputs, context size, tools); experiment run does it for any arm that lacks one
agentium run once <name> --snapshot trimmed   # one run, graded with the hidden tests
agentium experiment new lean --b trimmed      # an A/B: each task's own context against trimmed, on a sample of valid tasks
agentium experiment plan lean                 # runs, estimated cost (calibrations included), detectable effects; what is missing
agentium experiment run lean                  # calibrates what is not calibrated, locks it, then runs interleaved pairs within the budget; resumable; pauses before your plan's usage limit (--wait waits for the reset)
agentium experiment show lean                 # the lock and the progress per arm
agentium experiment report lean               # verdicts, intervals, per-task results (--markdown for a pull request, --json for everything)
```

## Experiment templates

 `experiment new` has three: `context-ab` (the default; `--b` names the snapshot to compare with arm A's context), `aa` (one context in both arms, which must find no difference: it measures the noise) and `model-ab`, which compares two Claude Code profiles on the same tasks and one context:

```sh
agentium experiment new models --template model-ab --a claude-sonnet-5 --b claude-opus-5-5:high [--context trimmed]
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

For hooks, schedulers and scripts. Part 1 covers `init`, `context show|snapshot|list|diff|lint`, `task list|show|mine|validate|import|add|edit|rm`, `run once|show|list` and `start`. The experiment commands follow in part 1b (`experiment report --json` exists already, with its own shape).

**`--json`** prints exactly one JSON document on stdout and no other text there; progress, color and questions are off.
- Top level: `"schema"` (1; raised only when a field is removed, renamed or changes meaning, never for added fields) and `"command"` (for example `"task list"`). Fields are snake_case, and lists are `[]`, never `null`. A field that can be unknown is `null`.
- Failure: `{"schema": 1, "command": "...", "error": {"message": "...", "code": 1}}`. `code` is the exit code. Usage errors say only that the arguments are invalid. Messages show the home folder as `~`, the working folder as `<repo>` and the data folder as `<data>`.
- A result that is bad rather than broken keeps its normal document and exit 1: an invalid or flaky task (`task validate`), `start` with too few tasks (`"status": "too_few_tasks"`).
- No ANSI escape ever, whatever `NO_COLOR`, `FORCE_COLOR` or the terminal say. Documents name no absolute path (repository, data folder, records, Claude Code), no user name and no secret, as reports do; repository files appear as relative paths.
- Warnings for a person may still appear on stderr. `--json` with `-h` prints the usage as text. `context lint --hook` and `--print-hook` already print Claude Code's JSON and refuse `--json`.

**Exit codes**

| Code | Meaning |
|---|---|
| 0 | Success, including nothing to do: no tasks, no candidates, no differences, or `start` stopping at its preview. `context lint` exits 0 when it finds problems. |
| 1 | Runtime failure, or a bad result: an invalid or flaky task, `start` with too few tasks, `start --yes` that was not ready. |
| 2 | Usage error: bad flags or arguments. |

**No prompts.** Agentium asks one question: `start`'s "Run it now?", and only when stdin and stdout are both terminals. With stdin from a pipe, a file or `/dev/null` it never reads stdin and stops at the preview; `start --json` never asks, even at a terminal. Only `--yes` spends money, and `start --json --yes` is refused (usage error) until `experiment run` has JSON.

For `start`, read `"status"`: `preview` (everything is in place; `"run_command"` starts the experiment and spends money), `not_ready` (`"readiness"` lists what is missing, for example mined tasks that wait for a person's review), `too_few_tasks`, or `finished`. `"nothing_was_run"` is always true.

Planned (part 2): a committed `agentium.toml` that Agentium reads and never writes, for budgets and consent to spend. See the [plan](../.agents/plans/2026-10-02-headless.md).

## Data folder and environment

Data lives in `~/.agentium`; set `AGENTIUM_HOME` to use another folder. Output is styled only on a terminal: `NO_COLOR=1` turns color off, and `FORCE_COLOR=1` keeps it through a pipe (for `less -R`).

| Variable | Effect |
|---|---|
| `AGENTIUM_HOME` | Data folder (default `~/.agentium`) |
| `NO_COLOR`, `FORCE_COLOR` | Turn color off; keep it through a pipe |
| `ANTHROPIC_API_KEY` | Claude Code credentials, as an alternative to signing in |
| `AGENTIUM_CLAUDE_TOKEN_FILE` | A token file for Claude Code |
