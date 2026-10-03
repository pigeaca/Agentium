# README pictures

Standard-library Python tools that turn real console output into the SVGs in `docs/images/`. Paths into the home
folder are shortened to `~` and `…` marks lines left out; check every SVG with
`grep -c "/Users/\|<your name>" docs/images/*.svg` before committing.

- `ptyrun.py CMD ARGS...` runs a command on a pseudo-terminal (width from `COLS`, default 160) and prints its styled
  output. Set `FORCE_COLOR=1` and `AGENTIUM_HOME` as needed. Run read-only commands against a copy of a data folder.
- `ansi2svg.py TITLE COLS < output.ans > picture.svg` renders it as a terminal window. The output's first line is
  usually `\x1b[36m$\x1b[39m agentium ...` to show the command. It knows 8, 256 and 24-bit colors.
- `AGENTIUM_TERM_DEMO=/tmp/demo COLS=100 go test ./internal/term -run TestDemo -count=1` writes `shapes.ans` (every
  shape) and `flow.ans` (a flow of boxes) into an existing folder, for checking the primitives by eye;
  `AGENTIUM_TERM_DEMO=tty` draws them, and a live flow, on your terminal.
- `cast2svg.py TITLE COLS PROMPT < rec.json > animation.svg` animates a timed recording, JSON `[[seconds, text], ...]`.
- `frames2svg.py TITLE COLS ROWS PROMPT [--from S] [--fps N] [--still S|last] < rec.json > out.svg` replays a live
  display's recording (cursor movement and erasing included) as a terminal of COLS by ROWS and animates each change as a
  frame, or draws one moment with `--still`. Lines that repeat across frames are defined once, so the dashboard's
  40-second loop stays near 400 KB.
- The recorder for that JSON is `TestRecordLiveRun` in `internal/cli/record_live_test.go` (skipped unless asked):
  `AGENTIUM_RECORD_LIVE=/tmp/live.json go test ./internal/cli -run TestRecordLiveRun -count=1`. It runs
  `experiment run` as on a terminal with the test stand-in for Claude Code, at its real speed, no paid runs.
  `AGENTIUM_RECORD_VIEW` picks the dashboard (default: a seq-v1 experiment on 16 tasks, 80 by 34), `log`, or `plain`
  (the status line); `AGENTIUM_RECORD_PACE` is the stand-in's seconds a run, `AGENTIUM_RECORD_COST` the second
  context's cost a run. `console-run-dashboard.svg` is
  `AGENTIUM_RECORD_PACE=2 AGENTIUM_RECORD_COST=0.25`, then `frames2svg.py "…" 80 34 "…" --from 31 --fps 8` (the checks
  and the sandbox's revalidation, shown as "getting ready", take the first 30 seconds); its still is `--still 64.435`, and `console-run-log.svg` is
  the log view's `--still last` at 80 by 40.
- `AGENTIUM_RUN_DEMO=/tmp/demo go test ./internal/cli -run TestDashboardMomentsGoldens -count=1` writes `sandbox-news.ans`
  (a row whose grading sandbox could not start, and one left out for flagged denials, from the test scenes: the stand-in
  cannot make the real sandbox fail); `console-run-sandbox-news.svg` is it through `ansi2svg.py`, at 80 columns.
