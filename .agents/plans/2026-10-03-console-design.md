# A designed console: a live dashboard and visual reports

- Date: 2026-10-03
- Status: Planned. The user asked (2026-10-03), showing an animated terminal dashboard ("agent stack": coloured panels, bars, a streaming log): "Is it possible to do beautification of our app in this way? Like some sandbox work, analyze etc". The user's choices:
  - a **live dashboard** for a running experiment, redrawn in place;
  - **and a log format too** ("But also should be dashboard and just log format"): a styled, append-only log, chosen per call or once;
  - all four screen groups: `experiment run`, `experiment report`, `run show`, and `start` / `experiment plan` / `pool status`.
- Scope: Agentium's human console output only. It follows the [no web UI decision](../decisions/2026-09-30-console-instead-of-web-ui.md): the console is the product's face.

## Outcome
- **A running experiment** shows a live **data-flow picture** (the user, 2026-10-03, showing a picture of connected boxes), redrawn in place on a terminal:
  - the experiment's box (runs and spend as bars, the time elapsed) **splits** into one box per arm, side by side (its tally, and a spinner on the run in flight);
  - the arms **merge** into grading (sandbox, canary, flagged denials);
  - an arrow leads to the current look (its interval bar and verdict so far), and the usage window shows as a bar when it matters.

  See the [sample](../../docs/images/console-flow.svg). Each box holds two or three lines; the rest is in the log.
- **A log streams under it**, so scrollback keeps every line. The panel lives in a bottom-anchored region that the log scrolls above, as `docker build` does.
- **Reports, run details and previews** get the same visual language: boxed panels; bars; interval bars that show where zero falls; colour per arm and per outcome. `run show` is a vertical **chain**: checkout → agent → grading → record, with the time of each stage.
- **Two views for a running experiment.** The dashboard is the default on a terminal. `--view log` (or `AGENTIUM_VIEW=log`, set once) gives the styled, append-only log instead:
  - a coloured line per run;
  - a boxed panel with bars at each look and at the end;
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
  - Samples (`AGENTIUM_TERM_DEMO`, then `ansi2svg.py`): a still of the live view, the log above the experiment flow, and `run show` as a chain; and every shape.

    ![A still of the live view: the log above the experiment flow; then run show as a chain](../../docs/images/console-flow.svg)

    ![The console's shapes](../../docs/images/console-shapes.svg)
- [ ] **2. `experiment run`** (and `start --yes`): the live dashboard (the experiment flow above: `Shapes.Flow` in a `Display` frame) and the log view, both fed by the executor's events, with `--view dashboard|log` and `AGENTIUM_VIEW`. The plain output when not on a terminal stays as today. Include calibration, usage pauses, `--wait`, budget stops, looks, judge pairs and sandbox lines. Risk: medium.
- [ ] **3. `experiment report`:**
  - a verdict panel with interval bars;
  - per-arm panels;
  - a per-task grid (each task's outcome per arm, coloured);
  - notes as dim lines.

  Markdown and JSON are unchanged. Risk: low.
- [ ] **4. `run show`:** the run as a vertical data-flow chain (`Shapes.Flow`, one box per row): checkout → agent (its sandbox, tools, turns, cost) → grading (host or `sandbox-v1`, canary, denials) → record, each box two lines at most, with its time on the right. Risk: low.
- [ ] **5. `start`, `experiment plan` and `pool status`:** the looks and spend as bars; the pool's health (valid, weak, flaky, invalid, awaiting review, retired) as bars. Risk: low.
- [ ] **6. Pictures:** re-record the README and gallery images, plus an animated SVG of the live dashboard for the README, with `scripts/readme_images`. Risk: low.

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
