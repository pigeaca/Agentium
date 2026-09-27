"""Paired statistics for the Phase 0 spike (standard library only).

The unit of analysis is the task: each task contributes the difference between arm means, so task difficulty cancels
out. Intervals come from a two-stage cluster bootstrap (resample tasks, then runs within each task and arm).
"""
from __future__ import annotations

import math
import random
from statistics import mean, stdev, variance

Z_TWO_SIDED_80 = (1.96 + 0.8416) ** 2  # alpha 0.05 two-sided, power 0.8
Z_ONE_SIDED_80 = (1.645 + 0.8416) ** 2  # alpha 0.05 one-sided (non-inferiority), power 0.8


def cells(runs: list[dict], value) -> dict[str, dict[str, list[float]]]:
    """{task: {arm: [value per run]}} for runs where value(run) is not None."""
    table: dict[str, dict[str, list[float]]] = {}
    for run in runs:
        observed = value(run)
        if observed is not None:
            table.setdefault(run["task"], {}).setdefault(run["arm"], []).append(observed)
    return table


def paired(table: dict[str, dict[str, list[float]]], a: str, b: str, transform=lambda x: x) -> list[float]:
    """Per-task difference of arm means (b − a) after transform; tasks missing an arm are skipped."""
    diffs = []
    for arms in table.values():
        if arms.get(a) and arms.get(b):
            diffs.append(mean(map(transform, arms[b])) - mean(map(transform, arms[a])))
    return diffs


def bootstrap(table, a, b, transform=lambda x: x, draws=10000, seed=7) -> tuple[float, float, float]:
    """Estimate and 95% percentile interval of the mean paired difference, via the two-stage cluster bootstrap."""
    rng = random.Random(seed)
    tasks = [arms for arms in table.values() if arms.get(a) and arms.get(b)]
    if not tasks:
        raise ValueError("no task has runs in both arms")
    estimate = mean(paired(dict(enumerate(tasks)), a, b, transform))
    stats = []
    for _ in range(draws):
        sample = [rng.choice(tasks) for _ in tasks]
        diffs = [mean(transform(rng.choice(arms[b])) for _ in arms[b]) - mean(transform(rng.choice(arms[a])) for _ in arms[a])
                 for arms in sample]
        stats.append(mean(diffs))
    stats.sort()
    return estimate, stats[int(0.025 * draws)], stats[int(0.975 * draws) - 1]


def within_variance(table, transform=lambda x: x) -> float | None:
    """Pooled within-cell variance of one run (cells = task × arm), weighted by degrees of freedom."""
    total, dof = 0.0, 0
    for arms in table.values():
        for values in arms.values():
            if len(values) > 1:
                total += variance(list(map(transform, values))) * (len(values) - 1)
                dof += len(values) - 1
    return total / dof if dof else None


def heterogeneity(diffs: list[float], within: float, repeats: float) -> float:
    """Method-of-moments τ²: observed variance of paired differences minus their sampling part, floored at 0."""
    if len(diffs) < 2:
        return 0.0
    return max(0.0, variance(diffs) - 2 * within / repeats)


def mde(tau2: float, within: float, repeats: int, tasks: int) -> float:
    """Minimum detectable paired difference (80% power, two-sided 5%)."""
    return math.sqrt(Z_TWO_SIDED_80 * (tau2 + 2 * within / repeats) / tasks)


def noninferiority_tasks(tau2: float, within: float, repeats: int, margin: float) -> int:
    """Tasks needed to show 'no loss beyond margin' with 80% power when there is no true difference."""
    return math.ceil(Z_ONE_SIDED_80 * (tau2 + 2 * within / repeats) / margin ** 2)


# Two-sided 95% t quantiles; t_quantile uses the nearest listed df at or below df, which errs wide.
T975 = {1: 12.706, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571, 6: 2.447, 7: 2.365, 8: 2.306, 9: 2.262, 10: 2.228,
        12: 2.179, 15: 2.131, 20: 2.086, 25: 2.060, 30: 2.042, 40: 2.021, 60: 2.000, 120: 1.980}


def t_quantile(df: int) -> float:
    return T975[max(k for k in T975 if k <= df)] if df >= 1 else math.inf


def t_interval(diffs: list[float]) -> tuple[float, float, float]:
    """Mean and 95% t-interval of per-task paired differences: a cross-check on the bootstrap. With few tasks the
    percentile bootstrap runs narrow; the two-stage bootstrap counts within-cell noise twice and runs wide."""
    if len(diffs) < 2:
        raise ValueError("need at least two tasks")
    center = mean(diffs)
    half = t_quantile(len(diffs) - 1) * stdev(diffs) / math.sqrt(len(diffs))
    return center, center - half, center + half
