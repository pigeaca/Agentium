# ADR: An LLM judge alongside tests

## Status
Accepted (2026-10-01, the user).

## Context
- Tests decide whether a run fixed its task. Nothing else looks at the fix, so a pass on weak tests counts as a full fix, and a task without tests cannot be graded.
- The [judge pilot](../../docs/research/2026-10-01-judge-pilot-results.md) gave no GO under its own rules. Its human labels could not support two of the three comparisons.
- Without any labels, the pilot showed that the judge is:
  - **a little unsteady:** 87% of changes got the same verdict on 3 repeats, and 11% of pairs flipped when their order was swapped;
  - **much stricter than the tests:** it judged 18 of 40 passing runs not fully fixed, with concrete reasons;
  - **affordable:** about $0.06 per judgement.
- The user decided not to rely on the pilot, and to build the judge.

## Decision
- **Per run:** an opt-in, reference-guided judge gives each graded run a verdict (fixed, partly or no) and a one-line reason.
  - It reads the instruction, the reference solution's code diff and the agent's code diff.
  - Its verdict is the majority of a few repeats.
- **Shown alongside, never deciding:** tests still decide pass and fail, and the success and cost verdicts. Reports show the judge's verdicts and reasons next to the test results, as a secondary signal.
- **Opt-in per experiment:** the judge's cost appears in the preview and counts against the experiment's budget. It never counts toward an arm's cost metric.
- **Later, if wanted, each with its own plan:** arm-versus-arm quality comparison, and grading tasks without tests.

## Consequences
- **A second opinion on passes:** reports can show passing runs that may not really fix the task, with a reason the user can check.
- **Noise:** the judge is noisy and may anchor on the reference. Its verdicts carry that warning, and none of them changes a verdict.
- **Cost:** with judging on, each run costs about $0.06 more per repeat.
- **Revisit when** checking the judge's claims by running them, or careful labels, can measure its accuracy. Until then, it does not decide outcomes.
