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
  `AGENTIUM_RECORD_VIEW` picks the dashboard (default, the quiet view: a seq-v1 experiment on 16 tasks, 80 by 34), `flow` (the step boxes), `log`, or `plain`
  (the status line); `AGENTIUM_RECORD_PACE` is the stand-in's seconds a run, `AGENTIUM_RECORD_COST` the second
  context's cost a run. The flow view's old animation (recorded as `flow`, before the quiet view became the default, no longer in `docs/images`) was
  `AGENTIUM_RECORD_PACE=2 AGENTIUM_RECORD_COST=0.25`, then `frames2svg.py "…" 80 34 "…" --from 31 --fps 8` (the checks
  and the sandbox's revalidation, shown as "getting ready", take the first 30 seconds); its still is `--still 64.435`, and `console-run-log.svg` is
  the log view's `--still last` at 80 by 40.
- `AGENTIUM_RUN_DEMO=/tmp/demo go test ./internal/cli -run TestDashboardMomentsGoldens -count=1` writes `sandbox-news.ans`
  (a row whose grading sandbox could not start, and one left out for flagged denials, from the test scenes: the stand-in
  cannot make the real sandbox fail); `console-run-sandbox-news.svg` is it through `ansi2svg.py`, at 80 columns.
- `AGENTIUM_RUN_DEMO=/tmp/demo go test ./internal/cli -run TestDashboardJudgeGraded -count=1` writes `judge-graded.ans` (two
  judge-graded runs, from the test scenes); `console-run-judge-graded.svg` is it, after a `$ agentium experiment run`
  line, through `ansi2svg.py` at 80 columns. `console-report-judge-graded.svg` is `report-judge-graded.ans` from
  `TestReportViewPreview` (`AGENTIUM_REPORT_DEMO`), the same way.
- `AGENTIUM_RECORD_SCREENS=/tmp/screens go test ./internal/cli -run TestRecordConsoleScreens -count=1` (into an existing
  folder) runs `start --accept-mined` on a small fixture repository, and `experiment plan`, `pool status` and
  `run show` on a 16-task seq-v1 experiment, as on a 120-column terminal at 256 colors with the test stand-in (no paid
  runs), and writes `start.ans`, `plan.ans`, `pool-status.ans` and `run-show.ans`; temporary paths become `~/…`.
  `console-start.svg` (with the settings block replaced by a `…` line), `console-plan.svg` and `console-run-show.svg`
  are those through `ansi2svg.py` at 120 columns.
- `AGENTIUM_RUN_DEMO=/tmp/demo go test ./internal/cli -run TestRunShowPreview -count=1` and
  `AGENTIUM_PLAN_DEMO=/tmp/demo go test ./internal/cli -run TestPlanViewPreview -count=1` write each scene's view
  (five runs, three plans, three pools) at 80 columns, for looking at the variants by eye.

## Pictures recorded on 2026-10-05 (the four screens)
- `console-report-model-ab.svg` and `console-task-list.svg`: the real model A/B on samber/lo, from a copy of its data folder
  (`cp -R` of `agentium.db`, `projects` and `records` into a `chmod 700` folder, a binary built from the checkout, never
  the original): `cd` into the repository, then `AGENTIUM_HOME=COPY FORCE_COLOR=1 COLS=100 ptyrun.py BIN experiment report
  opus-vs-sonnet` and `... task list`, each after a `$ agentium ...` line, through `ansi2svg.py TITLE 100`.
- `console-plan-not-sure.svg`: `experiment plan ab16` from a copy of the 16-run context A/B's data folder, at 120 columns
  with `TERM=xterm-256color`; `console-plan.svg` and `console-start.svg` come from `TestRecordConsoleScreens`.
  `console-report-*.svg` (the test scenes) are `report-NAME.ans` from `TestReportViewPreview`, at 80 columns.
- `console-run-dashboard.svg` and `console-run-dashboard-still.svg`: the quiet view, graded in the sandbox,
  `AGENTIUM_RECORD_PACE=2`, then `frames2svg.py "agentium experiment run lean-vs-base" 80 34 PROMPT --from 36 --fps 8`
  (the checks and the sandbox's revalidation take the first 35 seconds; the still: `--still 63.6`).
  `AGENTIUM_RECORD_GRADER=host` records where the sandbox grader reports itself unavailable (its probe's denial did
  not reach the unified log in time, which also happens on a heavily loaded machine); the run then carries the host
  grader's warning line, so prefer a recording without it. `console-run-flow.svg` is the earlier flow picture, kept and renamed (it is the step boxes' view).
