# Roadmap

Product direction: an AI development lab for coding agents. The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) is accepted as a hybrid strategy ([decision](decisions/2026-09-27-hybrid-strategy.md)). Vision: a clear answer on what your AI setup changes, within a day, for tens of dollars. Since 2026-10-05 it is its owner's personal tool, not a commercial product ([decision](decisions/2026-10-05-personal-tool.md)).

History: [completed plans](plans/archive/INDEX.md) and [decisions](decisions/README.md).

## North star
Time and dollars to the first decisive verdict (inconclusive doesn't count): reached 2026-10-02: 65 min, $3.34 ([report](../docs/examples/model-ab-report.md)); target within a day, $40 or less. Guard: A/A false verdicts at most 5%.

## Next, in waves ([coordinating plan](plans/2026-10-01-next-chapter.md))
1. Done: task mining, judge reports, temp isolation, [refactor](plans/archive/2026-10-01-refactor-round.md).
2. First decisive verdict: [quick start](plans/archive/2026-10-02-quick-start.md), [model A/B](plans/archive/2026-10-01-model-ab.md); [Java and Rust](plans/archive/2026-09-30-java-rust.md).
3. Done: [cheaper verdicts](plans/archive/2026-10-02-cheaper-verdicts.md): the [statistics note](../docs/research/2026-10-02-wave3-statistics-note.md), then sequential stopping (`seq-v1`); run reuse deferred.
4. [Automation](plans/2026-10-01-automation.md): task pool, PR cost screen (parked by the user, 2026-10-05), as commands that you, hooks or an AI call (the scheduled watch was cancelled by the user; nothing runs in the background).
5. [Codex](plans/2026-10-04-codex.md) (approved; steps 1–3 done, step 4 next): Codex runs and experiments, then Claude Code against Codex; Codex in containers after the container track.
6. [Container mode](plans/2026-10-04-containers.md) (parked by the user, 2026-10-05; steps 1–2 merged): grading in Docker, beside the macOS sandbox.
7. [A quieter console and tasks that can answer](plans/2026-10-05-quiet-console-useful-tasks.md) (approved 2026-10-05; the four screens, rule checks and drafts are merged; the proof that hidden tests ran and the cleanup are in review; the README is in progress): the report, preview, task list and a quiet run view; drafted task text; proof that the hidden tests ran.

[Judge](plans/archive/2026-10-01-llm-judge.md) (a second opinion): done, with a real check; pairs and ticket grading are merged, labelled unvalidated; the next steps are parked (the user, 2026-10-05).
