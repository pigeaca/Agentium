# ADR: A clearer console instead of a web UI

## Status

Accepted by the user on 2026-09-30. It supersedes the UI part of [Go, React and SQLite](2026-09-28-stack-go-react-sqlite.md); Go and SQLite stay as decided there.

## Context

The stack decision planned a local React UI, built into the binary, for Phase 2. Phase 1 showed that the command line covers the whole workflow: `init`, context snapshots, tasks, calibration, experiments and reports. What it lacks is output that reads well. Previews, run progress and reports are plain text, and a long experiment prints one line per event. The owner decided that a web UI is not needed.

## Decision

- No web UI, local HTTP API or Node toolchain.
- Phase 2 makes the console output of every command clearer instead:
  - color and emphasis for verdicts, statuses and warnings, off when the output is not a terminal or `NO_COLOR` is set, so pipes and logs stay plain;
  - aligned tables and consistent headings;
  - a progress line during experiments (runs done, spend, usage) that updates in place on a terminal and stays plain lines otherwise;
  - reports that read well in the terminal, with the Markdown and JSON files kept for sharing and for other tools.
- Standard library first. A terminal styling library needs supply-chain approval when that work is planned.

## Consequences

- The binary stays Go and SQLite: no `check web`, browser tests or embedded assets.
- Evidence for an output change is a real console sample, in color and plain, instead of browser evidence.
- Results are browsed through commands (`experiment report`, `run show`) and the report files; the JSON reports are the interface for other tools.
- The feasibility study's UX section, written for a web UI, is history. Adding a UI later needs a new decision.
