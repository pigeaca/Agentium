<div align="center">

# Agentium

**A local-first lab that measures what really makes AI coding agents better on your code.**

[![CI](https://github.com/pigeaca/Agentium/actions/workflows/ci.yml/badge.svg)](https://github.com/pigeaca/Agentium/actions/workflows/ci.yml) ![Go 1.27.1](https://img.shields.io/badge/Go-1.27.1-00ADD8?logo=go&logoColor=white) ![Status: Phase 1 done](https://img.shields.io/badge/status-Phase%201%20done-orange) [![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

</div>

Agentium runs coding agents such as Claude Code (Codex comes later) on tasks from your own repository. It measures how the agent, the model or your project's AI context (`AGENTS.md`, `CLAUDE.md`, rules, skills) changes correctness, cost and speed, and it reports the results with honest statistics.

- **Your tasks, not a benchmark.** A past commit becomes a task: its parent is the starting point, and its test changes become hidden tests. You can also write tasks by hand.
- **Context versions as experiment arms.** Snapshot `CLAUDE.md`, rules and skills, change them, and compare the versions head to head.
- **Fair, isolated runs.** Every run starts from a fresh checkout, and its commands run in a sandbox without network. It can't see the hidden tests, the solution, other runs or your credentials.
- **Plain verdicts.** Paired runs with repeats give one of four verdicts: improved, regressed, no loss beyond the margin, or inconclusive. Each comes with its intervals and honesty notes.

> [!NOTE]
> Agentium is early. Phase 1, context A/B for Claude Code from the command line, is done; Phase 2 makes the console output clearer and adds Codex and agent comparison; no web UI is planned. See the [roadmap](.agents/ROADMAP.md) and the [feasibility study](docs/research/2026-09-27-ai-development-lab.md).

## Requirements

- Go 1.27.1 and a C compiler (SQLite is built with cgo)
- Git
- Your project's own build tool, on the machine that runs Agentium: Go, Maven or Gradle (`mvnw` and `gradlew` preferred; Java and Kotlin, with a JDK) or Cargo. Go is proven in real runs; the Maven, Gradle and Cargo profiles are built from recipes proved in real Claude Code sessions, and their pilot on real repositories is still to come.
- [Claude Code](https://claude.com/claude-code), signed in. `ANTHROPIC_API_KEY` or a token file in `AGENTIUM_CLAUDE_TOKEN_FILE` works too.

## Quick start

```sh
go install ./cmd/agentium          # from a clone of this repository; puts agentium in your Go bin folder
cd /path/to/your/repo
agentium start                     # registers, snapshots, mines and validates 8 tasks, creates an experiment, previews its cost; stops at your review of the tasks
```

`agentium start` never writes to your repository and makes no paid run on its own. It does the steps below for you, skipping those already done, so run it again to resume:

- registers the repository (`init`) and, if the project has no snapshot, saves the committed context as `baseline` (arm A);
- mines and validates tasks until 8 are ready, the cost floor;
- creates the experiment `quick-...` at the floor, 8 tasks × 1 run per arm: an A/A calibration of your context, or with `--b SNAPSHOT` a comparison of the context with that snapshot;
- prints the preview: runs, estimated cost, detectable effect, and what is missing;
- stops there. `--yes` (or answering `y` on a terminal) runs the experiment, within its budget (`--budget USD` raises it). The run first calibrates each context that lacks a calibration (a short paid run, about $0.1 to $0.2, counted in the budget and shown in the preview).

Mined instructions need your review for solution leaks (`agentium task show NAME`, then `agentium task edit NAME --reviewed`), so a first `start` stops there. `start --accept-mined` accepts the tasks it mined without your review: it checks only solution headings, reference-file names and unstated test requirements, so an instruction that explains the fix passes. The default A/A calibration never counts toward the first decisive verdict.

It also shows how long it took, and what was spent, to your first decisive verdict (improved, regressed or no loss; inconclusive does not count), here and in `experiment report`, from finished experiments only. The manual commands follow.

```sh
agentium init /path/to/your/repo   # registers it; Agentium never writes to your repository
cd /path/to/your/repo
```

**1. Version your context**

```sh
agentium context show                              # what Claude Code loads at start, and on demand
agentium context snapshot baseline                 # save the committed context (HEAD) as a version
# edit CLAUDE.md, rules or skills, then:
agentium context snapshot trimmed --working-tree   # --include-linked adds linked docs
agentium context diff baseline trimmed --patch
```

**Context lint (free):** `agentium context lint [--ref REF]` checks the working tree (or a commit) and reports the size change against your latest snapshot, broken `@` imports, `AGENTS.md` files that Codex would cut off (it reads only the first 32 KiB of the chain from the root down) and the warnings `show` gives. It runs no agent and exits 0 even when it finds problems. To see it after every edit of a context file in Claude Code, run `agentium context lint --print-hook` and merge the printed `PostToolUse` hook into your own `~/.claude/settings.json` (Agentium never writes your settings, and its runs load project settings only, so the hook never fires inside them).

**2. Turn past commits into tasks**

```sh
agentium task mine --dry-run                       # commits that would make good tasks (tests and code changed, small, a clear message), and why others don't
agentium task mine --limit 10                      # import the best 10 and validate them: base: the parent; hidden tests: the test-file changes
agentium task edit <name> --reviewed               # once the instruction doesn't give the solution away (--accept-gaps: hidden tests need texts or names nothing states)
agentium task validate --all --snapshot trimmed    # tests fail on the base and pass with the reference, in each arm (--jobs N at a time)
agentium task import --commit <sha>                # one commit by hand (mining skips commits that are already tasks)
agentium task validate <name> --repeat 3           # run every stage 3 times: a task whose runs disagree is flaky, and experiments reject it
agentium task validate <name> --weak-tests          # which parts of the reference the hidden tests do not need (a warning, not a gate; a later validate without the flag drops the list)
```

Mined tasks verify with your build tool's test command (`go test ./...`, `./mvnw -q test` or `mvn -q test`, `./gradlew test` or `gradle test`, `cargo test`); `--verify` changes it. Dependencies for the agent's offline builds are fetched by a run's setup, once per base commit and tool set, into the `deps` folder of your data folder (`~/.agentium`, or `AGENTIUM_HOME`). Build files are detected only at the repository root: a Maven or Cargo build in a subfolder is not detected, so its agent gets no offline dependencies, and your caches stay denied to it. Plugins that fetch their own tools when a task runs (Spotless, say) are not warmed, so those tasks fail offline in the agent's sandbox. Gradle tools that run in a separate worker process (Checkstyle, PMD) also fail in the agent's sandbox: Claude Code's sandbox allows only IPv4 localhost connections, and Gradle starts those workers without the option that keeps Java on IPv4. Your machine's grading is unaffected; only the agent can't run them itself. Gradle's file-lock service needs the sandbox's local binding, which lets the agent bind any local port and reach localhost services (outbound network to other hosts stays blocked): agent runs on a Gradle project refuse to start until you run `agentium init --allow-local-binding`. Validation builds and runs tests on your machine, two tasks at a time by default: `--jobs` above 1 assumes your tests can run side by side (no fixed ports, shared `/tmp` paths or databases), so use `--jobs 1` if they cannot.

**3. Run and compare**

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

**Templates.** `experiment new` has three: `context-ab` (the default; `--b` names the snapshot to compare with arm A's context), `aa` (one context in both arms, which must find no difference: it measures the noise) and `model-ab`, which compares two Claude Code profiles on the same tasks and one context:

```sh
agentium experiment new models --template model-ab --a claude-sonnet-5 --b claude-opus-5-5:high [--context trimmed]
```

`--a` and `--b` are `MODEL` or `MODEL:EFFORT` (low, medium, high, xhigh or max; without one, the CLI's default). The arms must differ in model or effort. Both run one context: the base's own, or `--context SNAPSHOT`. The plan estimates each arm from your earlier runs on its model (or from a default run at list prices), flags a model without a list price, and covers both arms in the budget; `--run-budget-a` and `--run-budget-b` give an arm its own run cap. Each arm's model needs its own calibration of the context: `experiment run` makes the ones that are missing, once, before the first pair (`run calibrate --model MODEL` does it ahead of time). The preview counts their cost, and a calibration that fails its checks stops the experiment before any task run. Reports of model experiments name each arm by profile (model and effort) in the headlines, the metric tables and the per-task rows, with a one-line verdict such as `B (claude-sonnet-5-5) costs 48% less; success: exploratory`; the noise note says it pools both models.

**Judge (second opinion).** Tests decide pass and fail. `experiment new ... --judge` also asks an LLM judge about every graded run: does its change do what the task asks, as the task's reference solution does? The judge reads the instruction and both changes' code, never the tests; tasks whose reference solution has no code are skipped. It answers fixed, partly or no, with a one-line reason, and takes the majority of a few repeats. `--judge-model`, `--judge-effort` and `--judge-repeats` set it.

The report's Judge section shows, for each arm:
- the judge's verdicts among passing runs and among failing runs, with 95% intervals;
- the runs it did not judge, and why;
- how often its repeats agreed, and what it cost.

It then lists the passing runs the judge did not call fixed, each with its reason, for you to check.

Its limits:
- **It decides nothing.** Success, cost and every verdict stay the tests'.
- **Its accuracy is unmeasured.** In the [pilot](docs/research/2026-10-01-judge-pilot-results.md), it judged 18 of 40 passing runs not fully fixed.
- **It costs extra.** Each call costs a few cents: about $0.065 a call (the preview's estimate; $0.063 per single judgement in the pilot). The calls count against the budget, but not toward an arm's cost.

Data lives in `~/.agentium`; set `AGENTIUM_HOME` to use another folder. Output is styled only on a terminal: `NO_COLOR=1` turns color off, and `FORCE_COLOR=1` keeps it through a pipe (for `less -R`).

## Example results

This is real output from Phase 1's acceptance runs: Claude Code 2.1.281 with claude-sonnet-5, on tasks taken from this repository. The pictures show it as today's `agentium` prints it in a 120-column terminal. Paths into the home folder are shortened to `~`, and `…` marks lines left out. The experiment compared today's docs (`full`) with a minimal version (`minimal`). It was stopped after 4 complete pairs to fit one usage window, so every verdict is "exploratory": the report says the data is too thin instead of naming a winner.

**1. Plan it:** what it costs and what it can detect, before anything runs.

<img src="docs/images/console-plan.svg" alt="agentium experiment plan ab: the arms, checks marked ok, and a table of sizes with their cost and detectable effects">

**2. Run it:** pairs of runs, interleaved, within the budget. It was stopped here with Ctrl-C, and `experiment run ab` would resume it. The lines are the ones recorded during the run, in today's colors; the summary under them is today's `experiment show ab`.

<img src="docs/images/console-run.svg" alt="agentium experiment run ab: checks marked ok, twelve runs started and finished in pairs with their cost, two cancelled, and a summary per arm">

While runs go, a status line under the events shows the progress and redraws in place. This animation is a short run with a stand-in agent (Agentium's test double for Claude Code), at its real speed:

<img src="docs/images/console-live.svg" alt="An animation of agentium experiment run: event lines appear while a status line below them counts runs settled and in flight, spend and usage, then the summary">

**3. Report it:** verdicts in words, then the metrics with both intervals, noise, context and cost per arm, what the runs used of their context (files read on demand, project skills, subagents), behavior (tests run, files changed, denials), per-task results and notes. On a terminal it looks like this; piped, with `--out FILE` or with `--markdown`, it is Markdown you can paste into a pull request, like the [full report](docs/examples/context-ab-report.md) (`--json` for everything).

<img src="docs/images/console-report.svg" alt="agentium experiment report ab on a terminal: exploratory verdicts in yellow, the metrics table, and per-task results">

**4. Look at one run:** the minimal-docs run that failed its hidden tests (`--diff` and `--log` show the agent's changes and the grading output).

<img src="docs/images/console-run-show.svg" alt="agentium run show: outcome ok in green, verification failed in red, then cost, changes, behavior, environment and records">

The minimal docs cut Claude Code's first request by about 4.6k tokens, but cost did not fall, and one task failed. With four pairs the intervals are far too wide to call either a result, and the report says so in words.

The [A/A report](docs/examples/aa-report.md) is the sanity check: the same context in both arms. It reported no difference (cost −6%, 95%: −31% to +26%), and every run passed.

A larger A/B, 8 tasks × 1 run per arm ([report](docs/examples/context-ab-16-report.md)), found the same: every run passed in both arms, and cost was inconclusive (+5%, 95%: −11% to +25%), with about 55 tasks needed to settle it.

The first decisive verdict came from a model A/B on [samber/lo](https://github.com/samber/lo), 8 mined tasks × 1 run per arm ([report](docs/examples/model-ab-report.md)). Sonnet 5.5 cost 63% less than Opus 5.5 per run (95%: −70% to −53%), and the whole experiment cost $3.10. Success (75% against 50%) was exploratory at that size.

## Development

Agentium is built by AI coding agents (Claude Code and Codex) under shared rules in [`.agents/`](.agents/README.md):

- every task gets a plan with acceptance criteria, and its own worktree;
- a pre-commit guard checks each commit, and an independent reviewer reads each change;
- work lands as a pull request with green CI.

A standard-library Python harness is the single entrypoint:

```sh
python3 scripts/harness.py help
python3 scripts/harness.py hooks                               # once per clone: enable the pre-commit guard
python3 scripts/harness.py worktree new claude/feat/<topic>
python3 scripts/harness.py check changed
```

See the [harness reference](docs/harness.md) and [agent setup](.agents/reference/agent-setup.md).

## Contributing

Read [CONTRIBUTING](.github/CONTRIBUTING.md) first. Report vulnerabilities privately, as the [security policy](.github/SECURITY.md) describes.

## License

[Apache 2.0](LICENSE)
