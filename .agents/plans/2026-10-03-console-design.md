# A designed console: a live dashboard and visual reports

- Date: 2026-10-03
- Status: In Progress (2026-10-04): step 1 merged (#133), the design settled with the user over previews (#134), step 2 merged (#136, #137), step 3 merged, steps 4–6 done on `claude/feat/console-steps`. The user asked (2026-10-03), showing an animated terminal dashboard ("agent stack": coloured panels, bars, a streaming log): "Is it possible to do beautification of our app in this way? Like some sandbox work, analyze etc". The user's choices:
  - a **live dashboard** for a running experiment, redrawn in place;
  - **and a log format too** ("But also should be dashboard and just log format"): a styled, append-only log, chosen per call or once;
  - all four screen groups: `experiment run`, `experiment report`, `run show`, and `start` / `experiment plan` / `pool status`.
- Scope: Agentium's human console output only. It follows the [no web UI decision](../decisions/2026-09-30-console-instead-of-web-ui.md): the console is the product's face.

## Outcome
- **A running experiment** shows **each arm's current task as a row of step boxes joined by dotted lines**, redrawn in place on a terminal. The user settled this on 2026-10-03 after a series of previews: one panel was "too difficult to understand… too many strange words"; tracks were rejected for "keep boxes but with lines"; then "dot line like on the screenshot and dot that move"; then "remove arrow at the end, just dot line".
  - **Header:** the question in plain words, coloured by arm ("BASELINE vs TRIMMED · does trimmed save money?"). Under it, one line with the money spent against the budget, the share of the Claude plan used and each arm's progress ("baseline 11/16 · trimmed 10/16"). A legend explains the sandbox once ("sandbox: no internet, your secrets hidden").
  - **Each arm:** its name and current task, then four step boxes: **fresh copy → Claude works → hidden tests → result**.
    - **Done steps:** a green border, and ✓ with the time.
    - **The current step:** the arm's colour, with a spinner and its time.
    - **Later steps:** grey.
    - **The result box:** ✓ passed or ✗ failed. The run then drops into the log, and the arm's next task starts again at fresh copy.
  - **The sandbox:** Claude works and hidden tests each sit inside a dashed purple outline labelled "sandbox". It reads "docker" in the container mode to come; with `--grader host` there is no outline around grading, and a warning instead.
  - **Connectors:** thin grey dotted lines (`╌`) between the boxes, with no arrowheads; they never change. When a step finishes, **a dot in the arm's colour travels along the dotted line** to the next box, passing through the sandbox's edge.
  - **The answer so far:** a green box in plain words ("about the same cost (+4%) · not sure yet · next check after 16 tasks"). At the end it becomes "the answer", e.g. "no clear difference in cost · stopped early · more tasks would not help".
  - **Plain words throughout:** no "look", "futility", "interval", "window", "arm A/B" or bracketed numbers on this screen. Those stay in `experiment report` and `--json`.
  - **Colours** (from the user's screenshot): baseline blue, trimmed orange, the sandbox purple, done and the answer green, failures red, everything else grey.
  - **Animation:** up to about 10 frames a second; the dot slides between boxes, spinners turn, and bars and counts update as runs finish. Nothing blinks.
  - **Previews** (`AGENTIUM_TERM_DEMO`-style frames turned into an animated SVG) are shown to the user before any of this merges.
  - The agreed preview, animated: ![The running experiment: step boxes joined by dotted lines, a dot moving from box to box through the sandbox outlines](../../docs/images/console-run-dashboard.svg) (recorded from the real code; the design previews from the rounds above are not kept).
- **The connected-box flow** for one run (`run show`) uses the same boxes, dotted lines and sandbox outlines: fresh copy → Claude works → hidden tests → result, with each step's time and cost. A one-time "how this experiment works" flow when an experiment is created is optional, to be previewed first.
- **The log sits inside the frame while it runs**, as the approved preview has it: a fixed area of the last 4 runs' results under the answer, blank until filled, so the frame never jumps (the user, 2026-10-03: "it spams and goes somewhere, like last 3-4 only"). Nothing prints above the region during the run: the checks, calibrations, pauses, retries and warnings show as one status line in the frame. **When the screen closes**, on every exit path (the end, an error, a panic, Ctrl-C), the region clears and the full log goes to the scrollback once: what was printed before the first run, the question, every run's line and the answer.
- **Reports, run details and previews** get the same visual language: boxes with coloured borders, dotted connectors, plain words, colour per arm and per outcome. Interval bars stay in `experiment report`, explained in words beside them.
- **Two views for a running experiment.** The dashboard is the default on a terminal. `--view log` (or `AGENTIUM_VIEW=log`, set once) gives the styled, append-only log instead:
  - a coloured line per run;
  - a small box at each check of the answer and at the end;
  - nothing redrawn, so it suits SSH, tmux, recordings and slow terminals.

  `--view dashboard` forces the dashboard (`AGENTIUM_VIEW=dashboard`). Off a terminal, both give today's plain text.
- **Nothing changes for machines.** With `--json`, a pipe, `NO_COLOR`, `TERM=dumb` or a narrow terminal (below 60 columns), output is today's plain text, byte for byte. Golden tests keep pinning it.

## Design rules
- **Restraint over decoration** (the user, 2026-10-03: "keep all UI not so overloaded"):
  - each box holds only what matters now, two or three lines; details belong in the log view, `run show` or the report;
  - no number shown twice, and no label that restates the obvious;
  - breathing room: gaps between side-by-side boxes, a blank line between flow rows only where it helps, no nested boxes;
  - at most one spinner per box; nothing blinks and no colour animates;
  - wide terminals get more space, not more content (boxes keep their width; the flow is at most 100 columns).
- **One small palette, used everywhere** (no rainbow):
  - arm A and arm B each get their own colour;
  - outcomes: ok is green, failed red, infrastructure yellow, left out dim;
  - verdicts: improved and no loss green, regressed red, inconclusive dim;
  - money and the window turn yellow, then red, as they near their limit;
  - borders, connectors, spinners and secondary text in one muted grey.
- **Box drawing and block characters,** falling back to ASCII when the locale is not UTF-8. Widths are measured in display cells and fitted to the terminal; a resize redraws.
- **Live region rules:**
  - at most about 10 redraws a second, and only on change or for the spinner;
  - every write goes through one renderer (no interleaving);
  - the cursor and screen are restored on exit, on an error and on an interrupt (SIGINT is handled as today, and the region is cleared first);
  - a non-terminal never sees an escape.
- **No new modules:** the standard library, plus the existing `syscall` terminal size code in `internal/term`.

## Work
Step 1 comes first; steps 2–5 build on it and can run two at a time.
- [x] **1. `internal/term` primitives.**
  - Panels (boxes with titles), bars (with partial blocks), interval bars, the palette, a spinner and width fitting.
  - A **live region** renderer (bottom-anchored, throttled, resize-aware, safe on exit).
  - Capability detection: terminal, colour, UTF-8 and width.
  - Tests: golden renders at several widths, ASCII fallback, plain fallback, the renderer's cleanup on cancel, and no escapes on a pipe.
  - Risk: medium (concurrency in the renderer).
  - Done: `Capabilities`/`DetectCapabilities`, `Role` and `Style.Paint` (8, 256 and 24-bit), `Width` in cells (wide characters), `Truncate`/`Pad`/`Wrap`/`Sanitize`, `Shapes` (`Panel`, `Bar`, `IntervalBar`, `Legend`, `Spinner`), `Display`/`NewDisplay`. Goldens in `internal/term/testdata` (40, 80 and 120 columns; color, ASCII, plain); the live region is driven through a fake terminal (`vt_test.go`) and on a real pseudo-terminal. No screen uses them yet.
  - Added for the data-flow layout: `Row` (boxes side by side, equal or weighted widths, padded to the tallest, stacked when too narrow), connectors at the boxes' middles (an arrow, a split, a merge, ┬/┴ on the borders, ASCII `+ | - v`) and `Flow` (rows top to bottom, joined). Goldens at 40, 60, 80 and 120 columns.
  - Review fixes (PR #133): `Frame` is `func(width, height, tick int)`, so a frame compacts to the rows it gets instead of being cut; `Panel.MaxWidth` and `Flow.MaxWidth` default to `MaxContentWidth` (100); `Accent` is gone (it clashed with arm A); `Log` waits while `MaxPending` (1000) lines are queued; every styled line in the region ends with a reset and `Close` resets and shows the cursor; a failing `Over` stream drops only its own lines; `Sanitize` also drops raw C1 bytes, invalid UTF-8 and bidirectional overrides.
  - Limits:
    - A stacked row (three boxes under about 66 columns, two under about 43) reads top to bottom: arrows enter its first box and leave its last, so the split and merge lose their meaning there.
    - Shrinking the terminal's height leaves a stale copy of the region in the scrollback; a narrowing resize assumes the terminal reflows lines (Terminal.app, iTerm2, kitty), and on one that does not (xterm) it can erase a few log lines from the screen.
    - A frame must not call the display's methods (`Flush`, or a `Log` that waits for room, would deadlock); a killed process (SIGKILL, `os.Exit` without `Close`) leaves the cursor hidden.
    - The plain display passes text through as today; screens `Sanitize` text from outside Agentium.
  - Samples (`AGENTIUM_TERM_DEMO`, then `ansi2svg.py`): a still of the live view, the log above the experiment flow, and `run show` as a chain; and every shape.
- [x] **2. `experiment run`** (and `start --yes`): the live dashboard (the step boxes, dotted connectors and moving dot above, in a `Display` frame) and the log view, both fed by the executor's events, with `--view dashboard|log` and `AGENTIUM_VIEW`. The plain output when not on a terminal stays as today. Include calibration, usage pauses, `--wait`, budget stops, looks, judge pairs and sandbox lines. Risk: medium.
  - Done:
    - **Events:** a run's steps reach the observer as `Event{Kind: "step"}` (`run.StepPreparing`, `StepAgent`, `StepGrading`, `StepJudging`, the words `Env.Step` already sent), only when an observer takes events. `Result.Passed` carries the grade, and `Observer.Finish` gets the `Summary`. Records, JSON and the plain lines are unchanged.
    - **The screens** (`internal/cli`): `runstate.go` keeps the state behind a lock and copies it for each frame; `rundash.go` draws the dashboard on a `term.Canvas`; `runview.go` holds the words; `runscreen.go` wires either view to the observer.
    - **Compaction:** the dashboard drops the legend, then draws boxes on one line, then each arm on one line. On a terminal narrower than 74 columns it uses 10-cell boxes with short names.
    - **The log:** a fixed area of the last 4 runs' results in the frame. What is printed during the run (checks, calibrations, revalidation) is held and shown as the status line ("getting ready · …" before the first run). Pauses, retries, warnings and the pair judge's comparisons also take the status line, under the header (a comparison or a warning for 10 s, a retry until its run starts, a pause until it ends), in place of how the runs are run ("2 at a time · each run up to $3 and 20 min · Ctrl-C stops; run again to go on", which replaces the plain lines' "Running up to …").
    - **Notices that must not wait:** a run recovered from a dead Agentium, and a start file that cannot be read, print above the region at once, so a kill before the end loses neither.
    - **The end:** the screen closes on every exit path, an error, a panic or Ctrl-C included. The region clears, and the held lines, the question, every run's line and the answer go to the scrollback once.
    - **Steps are opt-in:** only an observer that asks for them gets steps (`Observer.Steps`). The plain and JSON observers never do, so a stalled terminal cannot hold a run at a step boundary.
    - **Plain words:** the answer's wording is one table (`answerWords`, tested case by case). A fixed design's answer is read from its analysis once every run is done.
    - **Palette:** the arms are blue (75) and orange (215), the sandbox purple (141) and green 114. At 8 colors, warnings and infrastructure failures are magenta (arm B is yellow there) and the sandbox blue. The term color goldens were rewritten for it.
    - **Choosing the view:** `chooseView` gives the plain lines off a terminal, on `TERM=dumb`, below 60 columns, with `--json`, and with `NO_COLOR` unless a view is asked for.
  - Samples, recorded from the real code with the test stand-in for Claude Code (2 s a run, no paid runs): the dashboard, animated ([`console-run-dashboard.svg`](../../docs/images/console-run-dashboard.svg)) and still ([`console-run-dashboard-still.svg`](../../docs/images/console-run-dashboard-still.svg)), and the log view ([`console-run-log.svg`](../../docs/images/console-run-log.svg)), with `scripts/readme_images/frames2svg.py`.
  - Limits:
    - The summary printed after the run (`WriteProgress`, shared with `experiment show`) still says "Looks" and "look 1 of 3": that is step 3's and step 5's.
    - A run that is never graded leaves its tests box at "–".
    - With concurrency above 2, each arm shows its latest run.
    - Before the first run, the status line shows only the latest line printed. The checks' warnings reach the screen at the end, in the scrollback.
  - Follow-up, approved by the user on 2026-10-03 ("Add follow ups"):
    - **Moments inside the boxes:** no new boxes. A box in progress names its slow moment: copying, fetching dependencies, setup, starting the sandbox, running the tests, cleaning up, judging.
      - Where room is short, the words shorten ("downloads", "starting", "testing", "cleanup") and the time is left out.
      - They come from finer run steps (`run.StepDependencies`, `StepSetup`, `StepSandbox`, `StepTests`, `StepCleanup`), still opt-in (`Observer.Steps`).
    - **The sandbox's news:**
      - A sandbox that cannot start (`run.StepSandboxDown`) turns the row's outline the caution color, shows "! no sandbox" and "not graded", and says "sandbox unavailable · retrying" on the row's name line.
      - A grade left out for flagged denials says "blocked: <operation classes in words>" there; its log line reads "left out (sandbox)".
      - A folder quarantined by a grade's cleanup (`run.StepQuarantined`) gets a fading status-line note.
      - The row notes sit on the name line rather than under the row, so the frame never changes height.
    - **Samples:**
      - The animated dashboard and its still, re-recorded: the still shows "starting" and "testing".
      - [`console-run-sandbox-news.svg`](../../docs/images/console-run-sandbox-news.svg), drawn from the test scenes: the stand-in cannot make the real sandbox fail.
    - **Limits:**
      - At 80 columns the moments' words fit only without their time.
      - Moments under a second flash by, or never show between two redraws.
      - `run once` and `run calibrate` status lines now name these moments too (terminal only; their plain output is unchanged).
- [x] **3. `experiment report`:**
  - a verdict panel with interval bars;
  - per-arm panels;
  - a per-task grid (each task's outcome per arm, coloured);
  - notes as dim lines.

  Markdown and JSON are unchanged. Risk: low.
  - Done (2026-10-04), the user approved the prototype's four previews ("Ok"):
    - **The view** (`internal/cli/reportview.go`), on a terminal that shows the designed console (`Capabilities.Designed`):
      - the dashboard's question line, then the tasks (run of planned, for a stopped seq-v1), runs, spend and time;
      - the answer in a green box, worded by the dashboard's `answerWords`, with the guard in words (an A/A: how much the same setup's cost varies), and a "cheaper ◀ │ ▶ costlier" interval bar with "same" under the line of no change and the range in words ("likely 6% to 35% less");
      - each arm's box in its color, side by side (stacked below 74 columns): passes as a bar and a count, typical cost and time, runs cut short or left out by the sandbox;
      - "where they differ": only the tasks the arms ended differently (✓/✗ per run, mean cost), the rest counted in one line; tasks a stopped seq-v1 never ran are left out;
      - the judges' opinion, labelled as an AI's: passing fixes the judge thinks right per arm, and the pair judge's preference by task ("may be chance" when the binomial test cannot tell it from an even split, "too few to say" below 5);
      - dim notes that matter, wrapped (a wrapped line keeps its grey), ending with the `--details` command.
    - **`--details`** shows the full terminal report as before. Piped, `--markdown`, `--out`, `--json`, `NO_COLOR`, `TERM=dumb` and below 60 columns are unchanged.
    - **Shared wording** (approved): the dashboard's final answer now says "sure enough" when decisive and "not sure yet · about N tasks in all could settle it" otherwise (`answerState.Settle`, from `TasksToResolve`; not in an A/A).
    - **Judge pairs:** step 2 of the [judge pairs plan](2026-10-01-judge-pairs.md) (`Report.PairJudge`, `pair_judge` in the JSON, a Judge pairs section in the Markdown and `--details`), only for experiments made with `--judge-pairs`.
    - **Fixtures:** the report's test fixtures moved to `internal/report/reporttest` (with `Seq`, `JudgedPairs` and `FewPairs`), so the view's tests build real reports. The A/A fixture's design now names the lock's second arm (an evaluation-order slip); no golden changed, as every rendering names contexts from the lock's arms.
    - **Proof:** with a scratch test writing the Markdown, JSON and `--details` text (plain and colored) of ten fixtures without `--judge-pairs` (the A/B, one-run A/B under both methods, the A/A, the judged one and five seq-v1 states), the 40 files from this branch and from `origin/main` (`4b7eb9b`) are byte for byte identical.
    - **Tests:** goldens of seven scenes at 60, 80, 100 and 120 columns (`internal/cli/testdata/report-*.golden`), the decisive one in 256 colors and ASCII; the answer's words are `answerWords`' in every scene; the judge lines and the floor; the collapsed grid; sanitized names; wrapped colors; the fallbacks (`TERM=dumb`, 50 columns, `NO_COLOR`).
    - **Samples** (`AGENTIUM_REPORT_DEMO=dir go test ./internal/cli -run TestReportViewPreview`, then `ansi2svg.py` at 80 columns): [decisive](../../docs/images/console-report-decisive.svg), [not sure yet](../../docs/images/console-report-unsure.svg), [A/A](../../docs/images/console-report-aa.svg), [both judges](../../docs/images/console-report-judged.svg), [too few pairs](../../docs/images/console-report-few-pairs.svg), [seq-v1 stopped early](../../docs/images/console-report-seq-stopped.svg), [seq-v1 futility](../../docs/images/console-report-seq-futility.svg).
    - **Review of #139:** an early seq-v1 stop gets the dim note "it stopped early: the true saving is likely smaller than 50%"; the answer box always says the other side of the question ("whether lean passes as many tasks: too few to tell", or the guard's verdict in words); "about N tasks in all could settle it" on both screens; a pair reason loses its line breaks and escape codes; "≥" is ">=" in ASCII; the `--details` command never breaks; on a narrow dashboard the answer's status drops its least important parts (`fitParts`) instead of being cut, so the frame keeps its height. Mutation checks killed every mutant of these paths.
    - **Limits:**
      - `experiment show` and the summary after a run (`WriteProgress`) keep their words: step 5's.
      - The time is from the first run's start to the last one's end, pauses included.
      - The grid's costs are the arms' means per task; a task's reason for the pair judge's vote is only in `--details` and the Markdown.
- [x] **4. `run show`:** the run as a vertical data-flow chain (`Shapes.Flow`, one box per row): checkout → agent (its sandbox, tools, turns, cost) → grading (host or `sandbox-v1`, canary, denials; for a judge-graded run the judge and its votes) → record, each box two lines at most, with its time on the right. Risk: low.
  - Done (2026-10-04):
    - **The view** (`internal/cli/runshowview.go`), on a terminal that shows the designed console:
      - A heading (task, arm) and four boxes in plain words: **fresh copy** (the arm's context, the setup), **Claude works** (turns, cost, the tools used most; a dashed purple outline, as the agent always runs in a sandbox), **hidden tests** (the sandbox's check and blocked actions, or "run on your machine, outside the sandbox"; dashed purple in the sandbox) or **the judge** (its votes, model and cost, "unvalidated"), and **result** (green ✓ passed, red ✗ failed, yellow no fair attempt, grey not graded; the whole run's time on the right).
      - A box's time is on its right: setup's, the agent's, the verification commands' and the whole run's (the record keeps no time for the copy itself, so none is shown for it).
      - Dim lines under the chain: which experiment's slot, Claude Code's version and model, drift, notes, the records folder (its end, cut from the left) and the `--details` command.
    - **`--details`** (new flag of `run show`) prints every line as before. Piped, `NO_COLOR`, `TERM=dumb`, below 60 columns and `--json` are byte for byte as before; `--diff` and `--log` still follow the picture.
    - **Primitives** (`internal/term`): the connector is a thin grey dotted cell without an arrowhead (`Flow.Dotted`: a dotted cell in place of each arrow, no ┬ on the box above), and `Panel.Dashed` draws the sandbox's outline (the dashboard's glyphs, `. : '` in ASCII). Neither changes an existing render. `Shapes.Flow` is used as the plan asked, with `Dotted`, since its arrows would break the rule of no arrowheads.
    - **Tests:** goldens of five runs (passed in the sandbox, failed on the host, judge-graded, no fair attempt, a failed sandbox check) at 60, 80 and 120 columns, the first also in 256 colors and ASCII (`internal/cli/testdata/runshow-*.golden`); the words per scene, no arrowheads, escapes from names, two lines per box; the term test of the dotted flow and dashed panels; an end-to-end test on a real run (terminal, `--details`, `NO_COLOR`, `TERM=dumb`, 40 columns, `--json`, `--diff`).
- [x] **5. `start`, `experiment plan` and `pool status`:** the looks and spend as bars; the pool's health (valid, weak, flaky, invalid, awaiting review, retired) as bars. Risk: low.
  - Done (2026-10-04):
    - **`experiment plan` and `start`'s preview** (`internal/cli/planview.go`): the dashboard's question line, a facts line (experiment, tasks, runs, model, concurrency), then boxes: **before it runs** (one green line when everything is in place, else each check that is not ok, two lines at most), **what it may spend** (bars: likely, at most or worst case, and the budget, with the money's colour turning yellow and red near the budget; the calibration's share and the cap's worst case as dim lines), and, for a seq-v1 design with more than one check, **when it checks the answer** (a bar per check: its tasks and the spend by then). Dim lines: the plan's share (a subscription's), a spend that assumes every run hits its cap, "not ready to run", and the `--details` command. The sizes, detectable effects, floors and long notes are in `experiment plan NAME --details` (new flag); `--json`, a pipe, `NO_COLOR`, `TERM=dumb` and below 60 columns are as before.
    - **`pool status`** (`internal/cli/poolview.go`): a box of bars, one per state that has tasks (valid always; weak tests under it), green for valid, yellow for weak and flaky, red for invalid and grey for the rest, then the last pass and the oldest valid base. `pool update`'s closing health lines are unchanged.
    - **Rendering only:** one call-site change each in `experiment.go`, `start.go` and `pool.go`; the mining and pool logic are untouched.
    - **Tests:** goldens of three plans (a ready seq-v1, a fixed design that is not ready, unknown costs) and three pools at 60, 80 and 120 columns, with colour and ASCII; the words, no jargon of `--details`, escapes from names; an end-to-end test (terminal, `--details`, `NO_COLOR`, `TERM=dumb`, 40 columns, `--json`) for plan and pool status.
- [x] **6. Pictures:** re-record the README and gallery images that show these screens, with `scripts/readme_images`. Risk: low.
  - Done (2026-10-04): `console-start.svg`, `console-plan.svg` and `console-run-show.svg` re-recorded from the real code with the test stand-in (`TestRecordConsoleScreens`, see the script README); the gallery's text for items 1, 3 and 7 and its sources note say so. The data of the old pictures (the 2026-09-29 `ab` experiment, the semver clone) is no longer on this machine, so the new ones are from a small fixture repository, not from real runs. The README's hero (the dashboard still and its animation link) is unchanged, as its output is. `console-mine.svg`, the report and the run-screen images show unchanged output. `pool status` has no picture in the gallery; its samples are in the previews below.
  - Verification (steps 4–6): `go test -race -count=1` of `internal/cli`, `internal/term` and `internal/report/...` and `python3 scripts/harness.py check changed` (exit 0; vet and the nine affected packages); every existing golden unchanged. Three mutants (the grading box's sandbox test flipped, the dotted flow's tee restored, `pool status`'s designed-terminal gate removed) were each killed by the named tests.
  - Samples (the screens' variants, from the test scenes): `AGENTIUM_RUN_DEMO` / `AGENTIUM_PLAN_DEMO` with `TestRunShowPreview` / `TestPlanViewPreview`.
  - Limits:
    - The run's checkout has no time of its own in the record, so the fresh-copy box shows only the setup's time, when there is a setup.
    - A designed `run show` leaves out the records' file list, the permission mode, the tool and skill counts and the first request's size; they are in `--details` and the JSON.
    - `start` keeps its other lines as they were: only the preview is a picture.

## Acceptance
1. On a terminal, each screen above shows its designed form; `experiment run` in both views. A real console sample (a screenshot or a recorded SVG) of each is in this plan.
2. Every existing golden and plain-output test passes unchanged. `--json` and piped output are byte for byte as before.
3. The live region never corrupts output:
   - a test drives it with a fake terminal (a pty or a writer capturing escapes) through updates, a resize and a cancel;
   - it leaves the screen clean;
   - nothing is written to a non-terminal except plain lines.
4. `NO_COLOR`, `TERM=dumb`, a non-UTF-8 locale and a 40-column terminal each give a readable result.
5. Docs: the guide's output section describes both views, `--view` and `AGENTIUM_VIEW`, and plain output (`NO_COLOR` or piping).

## Boundaries
- No new modules, no paid runs, no change to what is computed: only how it is shown.
- Isolation step 3 (#131) and `agentium clean` are in flight. Steps 2–5 touch `internal/cli` and `internal/report`. Merge main before each and keep to the presentation files.

## Verification
`python3 scripts/harness.py check changed`; the term and screen tests; a review per step; real console samples.

## Metrics
- Agent: <client> / <exact model id> / <effort>
- Elapsed: <minutes>m
- Check-fix loops: <n>
- User corrections: <n>
- Review: <verdict>
