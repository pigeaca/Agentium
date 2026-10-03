# README pictures

Standard-library Python tools that turn real console output into the SVGs in `docs/images/`. Paths into the home
folder are shortened to `~` and `…` marks lines left out; check every SVG with
`grep -c "/Users/\|<your name>" docs/images/*.svg` before committing.

- `ptyrun.py CMD ARGS...` runs a command on a pseudo-terminal (width from `COLS`, default 160) and prints its styled
  output. Set `FORCE_COLOR=1` and `AGENTIUM_HOME` as needed. Run read-only commands against a copy of a data folder.
- `ansi2svg.py TITLE COLS < output.ans > picture.svg` renders it as a terminal window. The output's first line is
  usually `\x1b[36m$\x1b[39m agentium ...` to show the command. It knows 8, 256 and 24-bit colors.
- `AGENTIUM_TERM_DEMO=/tmp/shapes.ans COLS=100 go test ./internal/term -run TestDemo -count=1` writes the console's
  shapes and a still of a live dashboard (`console-shapes.svg`); `AGENTIUM_TERM_DEMO=tty` draws them, and a live
  region, on your terminal.
- `cast2svg.py TITLE COLS PROMPT < rec.json > animation.svg` animates a timed recording, JSON `[[seconds, text], ...]`.
- The recorder for that JSON is `TestRecordLiveRun` in `internal/cli/record_live_test.go` (skipped unless asked):
  `AGENTIUM_RECORD_LIVE=/tmp/live.json go test ./internal/cli -run TestRecordLiveRun -count=1`. It runs
  `experiment run` as on a terminal with the test stand-in for Claude Code, at its real speed, no paid runs.
- `run-ab-events.ansi` holds the event lines recorded during the real `ab` run on 2026-09-29, which cannot be
  regenerated without paid runs. `console-run.svg` shows them above today's `experiment show ab`.
