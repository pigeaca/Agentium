<div align="center">

# Agentium

**Find out what really makes AI coding agents better on your code.**

[![CI](https://github.com/pigeaca/Agentium/actions/workflows/ci.yml/badge.svg)](https://github.com/pigeaca/Agentium/actions/workflows/ci.yml) ![Go 1.27.1](https://img.shields.io/badge/Go-1.27.1-00ADD8?logo=go&logoColor=white) ![Status: early](https://img.shields.io/badge/status-early-orange) [![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

</div>

Does a shorter `CLAUDE.md` save money? Is a cheaper model good enough for your project? Agentium answers questions like these with real Claude Code runs on tasks from **your own repository**, and tells you the answer in plain words, with honest statistics behind it.

<img src="docs/images/console-run-dashboard-still.svg" alt="agentium experiment run: the question at the top; for each version, the current task moving through fresh copy, Claude works and hidden tests (both in a sandbox) to the result, a dot travelling between the steps; the answer so far in a green box; the last four results below">

*A running experiment (shown in the step-box view, `--view flow`; the default dashboard is quieter, with progress, spend, what runs now, the answer so far and the last results): each task gets a fresh copy, Claude works on it in a sandbox, hidden tests check the result in a sandbox, and the answer updates as results come in. [See it running](docs/images/console-run-dashboard.svg).*

## How it works

<img src="docs/images/how-it-works.svg" alt="How Agentium works: tasks from your history, two versions to compare, runs in a sandbox, a plain answer">

- **Your tasks, not a benchmark.** A past commit becomes a task: Claude gets the code from before it, and the commit's own tests become hidden tests.
- **Two versions, side by side.** Your current AI context against a changed one (`CLAUDE.md`, rules, skills), or one model against another.
- **Fair and locked in.** Every run starts from the same fresh copy. Claude and the hidden tests each run in a sandbox: no internet, your secrets hidden.
- **A plain answer.** "trimmed is cheaper: 18% less", "about the same", or "not sure yet". It checks after 8, 12 and 16 tasks and stops as soon as the answer is clear.

The first clear answer, on [samber/lo](https://github.com/samber/lo): Sonnet cost 63% less than Opus per task, reached 65 minutes and $3.34 after setup ([report](docs/examples/model-ab-report.md)).

## Quick start

You need Go 1.27.1 with a C compiler, Git, [Claude Code](https://claude.com/claude-code) signed in (or `ANTHROPIC_API_KEY`), and your project's own build tool (Go, Maven, Gradle, Cargo or Python).

```sh
go install github.com/pigeaca/agentium/cmd/agentium@latest   # the latest release, once one exists
agentium version
cd /path/to/your/repo
agentium start
```

`start` never writes to your repository. It finds up to 16 tasks in your history, checks them, and shows what an experiment would cost. Then it stops: **nothing paid runs until you say so** (`agentium start --yes`, or `agentium experiment run NAME`).

> [!WARNING]
> Running an experiment starts real Claude Code runs. They cost money or use your Claude plan's limits; the preview shows the most it can spend first.

Before tasks are used, read them once for hints that give the answer away: `agentium task show NAME`, then `agentium task edit NAME --reviewed`.

Releases and what each version means are on the [releases page](https://github.com/pigeaca/Agentium/releases); the [policy](.agents/rules/releases.md) is SemVer for the commands, flags, `--json` keys and data folder. From a clone, `go install ./cmd/agentium` builds the working tree.

## Known limits

- macOS first: sandboxed grading needs `sandbox-exec`; Linux has no sandbox yet and containers are planned.
- Experiments run Claude Code only. Codex runs one at a time (`run once --agent codex`, a preview; [guide](docs/guide.md#advanced-flags)); Codex experiments come later.
- The judge features (judge reports, pairs, graded tasks without tests) are experimental and unvalidated.
- No TypeScript yet: Go, Maven, Gradle, Cargo and Python.
- A grade can leave data for later grades through `/mp-` POSIX semaphores and the macOS unified log. This is accepted for the first sandbox version; containers will close it.

## Everyday commands

| Command | What it does |
|---|---|
| `agentium start` | From a repository to a ready experiment, with its cost shown first |
| `agentium experiment new NAME --b trimmed` | Compare your context with a saved version called `trimmed` |
| `agentium experiment new NAME --b claude-opus-5-5` | Compare two models on the same tasks |
| `agentium experiment run NAME` | Run it, with the live screen above (`--view flow` for step boxes, `--view log` for a plain log) |
| `agentium experiment report NAME` | The answer and the details per task |
| `agentium context snapshot NAME` | Save the current context as a version to compare |
| `agentium task add NAME --ticket-file FILE --base REF --solution REF` | A task from a ticket; when its fix has no tests, the judge grades it (unvalidated, reported apart) |
| `agentium pool update` | Find new tasks in your history and keep the old ones fresh |
| `agentium clean` | Show the space unused caches take; `--yes` frees it |

Everything else is in the [guide](docs/guide.md), and more screens are in the [gallery](docs/gallery.md).

## Safety

- **Claude is locked in.** It works in a fresh copy of your code, without the hidden tests or the answer. Its commands have no internet, and your keychain, SSH keys, cloud and Git credentials and other runs are out of its reach.
- **The tests are locked in too.** On macOS, the hidden tests run in their own sandbox by default, and every leftover process is stopped afterwards.
- **Your repository is never written.** Nothing paid runs without your consent, and Agentium starts nothing in the background: it only runs when you, a hook or an AI calls it.
- **Not yet:** other people's repositories. A container mode through Docker is planned for that.

## Development

Agentium is built by AI coding agents under shared rules in [`.agents/`](.agents/README.md): every change is planned, built in its own worktree, reviewed independently and merged with green CI.

<img src="docs/images/dev-loop.svg" alt="How Agentium is built: plan, build, review, merge; a review that requests changes sends the work back to build">

```sh
python3 scripts/harness.py help
python3 scripts/harness.py hooks                               # once per clone: enable the pre-commit guard
python3 scripts/harness.py worktree new claude/feat/<topic>
python3 scripts/harness.py check changed
```

See the [harness reference](docs/harness.md) and [agent setup](.agents/reference/agent-setup.md). Read [CONTRIBUTING](.github/CONTRIBUTING.md) first, and report vulnerabilities privately as the [security policy](.github/SECURITY.md) describes.

## License

[Apache 2.0](LICENSE)
