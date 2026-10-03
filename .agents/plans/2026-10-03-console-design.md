# A designed console: a live dashboard and visual reports

- Date: 2026-10-03
- Status: Planned. The user asked (2026-10-03), showing an animated terminal dashboard ("agent stack": coloured panels, bars, a streaming log): "Is it possible to do beautification of our app in this way? Like some sandbox work, analyze etc". The user's choices:
  - a **live dashboard** for a running experiment, redrawn in place (not only a styled log);
  - all four screen groups: `experiment run`, `experiment report`, `run show`, and `start` / `experiment plan` / `pool status`.
- Scope: Agentium's human console output only. It follows the [no web UI decision](../decisions/2026-09-30-console-instead-of-web-ui.md): the console is the product's face.

## Outcome
- **A running experiment** shows a live panel. It redraws in place on a terminal, about four times a second:
  - the experiment, its method and the current look;
  - each arm (context or model, effort) with its tally;
  - bars for tasks, budget and the usage window, with its reset;
  - each look's result as it lands;
  - grading (sandbox, canary, flagged denials);
  - a spinner for runs in flight.
- **A log streams under it**, so scrollback keeps every line. The panel lives in a bottom-anchored region that the log scrolls above, as `docker build` does.
- **Reports, run details and previews** get the same visual language: boxed panels; bars; interval bars that show where zero falls; colour per arm and per outcome.
- **Nothing changes for machines.** With `--json`, a pipe, `NO_COLOR`, `TERM=dumb` or a narrow terminal (below 60 columns), output is today's plain text, byte for byte. Golden tests keep pinning it.

## Design rules
- **One palette, used everywhere:**
  - arm A and arm B each get their own colour;
  - outcomes: ok is green, failed red, infrastructure yellow, left out dim;
  - verdicts: improved green, regressed red, no loss blue, inconclusive dim;
  - money and the window warn as they near their limit.
- **Box drawing and block characters,** falling back to ASCII when the locale is not UTF-8. Widths are measured in display cells and fitted to the terminal; a resize redraws.
- **Live region rules:**
  - at most about 10 redraws a second, and only on change or for the spinner;
  - every write goes through one renderer (no interleaving);
  - the cursor and screen are restored on exit, on an error and on an interrupt (SIGINT is handled as today, and the region is cleared first);
  - a non-terminal never sees an escape.
- **No new modules:** the standard library, plus the existing `syscall` terminal size code in `internal/term`.

## Work
Step 1 comes first; steps 2–5 build on it and can run two at a time.
- [ ] **1. `internal/term` primitives.**
  - Panels (boxes with titles), bars (with partial blocks), interval bars, the palette, a spinner and width fitting.
  - A **live region** renderer (bottom-anchored, throttled, resize-aware, safe on exit).
  - Capability detection: terminal, colour, UTF-8 and width.
  - Tests: golden renders at several widths, ASCII fallback, plain fallback, the renderer's cleanup on cancel, and no escapes on a pipe.
  - Risk: medium (concurrency in the renderer).
- [ ] **2. `experiment run`:** the live dashboard, fed by the executor's events. The plain output when not on a terminal stays as today. Include calibration, usage pauses, `--wait`, budget stops, looks, judge pairs and sandbox lines. Risk: medium.
- [ ] **3. `experiment report`:**
  - a verdict panel with interval bars;
  - per-arm panels;
  - a per-task grid (each task's outcome per arm, coloured);
  - notes as dim lines.

  Markdown and JSON are unchanged. Risk: low.
- [ ] **4. `run show`:** the run as a pipeline: checkout → agent (its sandbox, tools, turns, cost) → grading (host or `sandbox-v1`, canary, denials) → record, with time per stage. Risk: low.
- [ ] **5. `start`, `experiment plan` and `pool status`:** the looks and spend as bars; the pool's health (valid, weak, flaky, invalid, awaiting review, retired) as bars. Risk: low.
- [ ] **6. Pictures:** re-record the README and gallery images, plus an animated SVG of the live dashboard for the README, with `scripts/readme_images`. Risk: low.

## Acceptance
1. On a terminal, each screen above shows its designed form. A real console sample (a screenshot or a recorded SVG) of each is in this plan.
2. Every existing golden and plain-output test passes unchanged. `--json` and piped output are byte for byte as before.
3. The live region never corrupts output:
   - a test drives it with a fake terminal (a pty or a writer capturing escapes) through updates, a resize and a cancel;
   - it leaves the screen clean;
   - nothing is written to a non-terminal except plain lines.
4. `NO_COLOR`, `TERM=dumb`, a non-UTF-8 locale and a 40-column terminal each give a readable result.
5. Docs: the guide's output section describes the dashboard and how to turn it off (`NO_COLOR`, `--plain` if added, or piping).

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
