# Clearer console output

- Date: 2026-09-30
- Status: In Progress (plan awaiting approval by merge)
- Scope: the user dropped the web UI ([decision](../decisions/2026-09-30-console-instead-of-web-ui.md)) and moved this work ahead of the hardening plan's paid A/B ("swap the order, console first", 2026-09-30), so the A/B runs with it. Free: no real Claude Code runs.

## Why

Agentium is used only from the console, and its output is plain text sized by hand:
- **Tables break.** Column widths are fixed in format strings. `task list` runs SOURCE into TESTS, and `context list` misaligns on a branch name longer than its column.
- **Nothing stands out.** Verdicts, `MISSING`, `WARNING`, failed runs and the command to type next all look the same.
- **Long waits are silent.** An experiment prints one line per finished run and nothing between. `task validate` can take minutes per task with no sign of life.
- **Reports are Markdown.** `experiment report` prints Markdown markup to the terminal.

## Acceptance

Each item maps to evidence. Changing any of them needs the user's agreement.

1. **Color only where it belongs.**
   - Color and emphasis are on when stdout is a terminal, `NO_COLOR` is unset or empty, and `TERM` is not `dumb`. A non-empty `FORCE_COLOR` (other than `0`) turns them on for pipes, such as `less -R`.
   - Otherwise the output has no escape codes. With color off, every command prints the same words, lines and order as before; only table spacing changes.
   - *Evidence:* a detection test over those inputs, and a test that runs each command's main path with color off and finds no escape code.
2. **One set of styles, used everywhere.** They're defined once in a new `internal/term` package:
   - green: `ok`, `valid`, passed, improved, no loss;
   - yellow: `WARNING`, exploratory, inconclusive, cancelled, paused, retrying, stopped;
   - red: `MISSING`, failed and error outcomes, regressed, `invalid`;
   - bold: section headings and table headers;
   - dim: notes and footnotes;
   - cyan: commands to type next (`agentium experiment run ab`).
   - *Evidence:* unit tests of the mapping; a real console sample of `experiment plan`, `run list` and `experiment run`.
3. **Tables sized to their content.**
   - One table helper sizes columns to the widest cell, measured in characters with escape codes ignored. Numbers and money are right-aligned.
   - Every hand-sized table uses it: the `experiment`, `task`, `context` and `run` lists; the preview's sizes table; the per-arm summary after a run.
   - *Evidence:* table tests (escape codes, `×`/`–`, long cells); the lists above show aligned columns on the acceptance data.
4. **Progress while waiting**, on a terminal only.
   - `experiment run` keeps its event lines and adds a status line at the bottom, redrawn in place: a spinner, runs settled of the total, runs in flight, spend of the budget, the latest usage reading, and time elapsed.
     - Event lines print above it.
     - It is cut to the terminal's width and cleared when the command ends.
     - During a usage pause it counts down to the reset.
   - `run once`, `run calibrate` and `task validate` show a spinner, the step in progress and its elapsed time.
   - Not on a terminal: the output is exactly today's, with no status line or spinner.
   - *Evidence:* an end-to-end test with the fake `claude` and a fake terminal: the line appears, is cleared before each event line and at the end, and nothing is left over. The non-terminal tests stay unchanged. A real console sample of an experiment with the fake `claude`.
5. **Reports for the terminal.**
   - On a terminal, `experiment report` renders the report's own data rather than parsing its Markdown. It has the same content without markup: bold headings, aligned tables and colored verdicts.
   - Piped, or with `--markdown`, it prints today's Markdown byte for byte; `--json` is unchanged.
   - *Evidence:* a golden file for the terminal rendering (color off); the Markdown and JSON golden files are unchanged.
6. **Docs.** The README's example results show the new output, and a note covers `NO_COLOR` and `FORCE_COLOR`. The architecture lists `internal/term`.

## Work
- [x] **1. Styles and tables** (acceptance 1–3): `internal/term` with detection, styles and the table helper. Wire it through `cli.Env`: `main` reports whether stdout is a terminal. Apply it to every command's statuses, headings, hints and tables. Done on 2026-09-30, approved in review after small fixes:
  - Terminal detection asks for the window size (`TIOCGWINSZ`), so `/dev/null` is not a terminal; `term.Columns` is there for step 2.
  - Beyond the listed scope: `task.Validator` and `run.Env` got a `Style` field, because their progress lines carry the verdicts.
  - Tables can print a note line between rows, so notes stay under their row. Styles end at each line, and `Heading` and `Note` must not nest (they share a reset code).
  - Evidence: plain and styled output compared on 28 commands by the reviewer (same words, lines and order); real output captured through a pseudo-terminal on the acceptance data.
- [ ] **2. Progress** (acceptance 4): the status line and spinner in `internal/term`; terminal width from `TIOCGWINSZ` through `syscall`, then `COLUMNS`, then 80. Wire it into `experiment run`, `run once`, `run calibrate` and `task validate`.
- [ ] **3. Terminal report** (acceptance 5): a terminal renderer in `internal/report` beside the Markdown one; `--markdown` on `experiment report`.
- [ ] **4. Docs and samples** (acceptance 6): README and architecture; real console samples in each PR.

Then the hardening plan's step 6, the paid A/B, runs with this output.

## Boundaries
- No new dependencies: the standard library, with `syscall` for the terminal size.
- The Markdown and JSON reports, stored data and exit codes do not change. Errors on stderr stay plain `agentium: …`.
- Words change only where a message needs a heading or style; the pipe output stays readable on its own.
- No full-screen interface, prompts or mouse input.
- No real Claude Code runs; samples use the acceptance data and the fake `claude`.

## Verification
- Every step: `harness.py check changed`, CI, and a review.
- Tests as listed under each acceptance item; the existing non-terminal tests keep passing unchanged, except for table spacing.
- Real console samples: captured through a pseudo-terminal (`script`) in color, and piped plain, attached to each PR.

## Parallel ownership
Step 1 comes first; it fixes the `internal/term` API the others use. Steps 2 and 3 then run in parallel from step 1's merge, one PR each; the coordinator integrates them. Worktrees live under `/Users/pigeaca/GolandProjects/Agentium-worktrees/`. At most two agents run at once.

| Step | Owner | Branch / worktree | Editable scope | Must not touch |
|---|---|---|---|---|
| 1 | coordinator (Opus) | `claude/feat/console-styles` / `claude-feat-console-styles` | new `internal/term`; `internal/cli` output; `cmd/agentium/main.go` (the terminal flag); added: a `Style` field in `internal/task` and `internal/run` | `internal/report`, `internal/experiment`, `internal/store` |
| 2 | `implementer` (Sonnet, medium) | `claude/feat/console-progress` / `claude-feat-console-progress` | a new progress file in `internal/term`; `internal/cli/experiment_run.go`, `run.go` (once, calibrate), `task.go` (validate) | `internal/report`, `internal/experiment` (events are read, not changed), `internal/term`'s existing files |
| 3 | `implementer` (Sonnet, medium) | `claude/feat/console-report` / `claude-feat-console-report` | `internal/report` (a new terminal renderer and golden file); `internal/cli/experiment_report.go` | `internal/term` (uses it only), the Markdown and JSON renderers' output |

Each implementer pushes its branch and returns a handoff; the coordinator runs the reviewer, opens the PR and integrates. Step 4 is the coordinator's, after steps 2 and 3.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
