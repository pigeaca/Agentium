#!/usr/bin/env python3
"""LLM judge pilot: blind human labels and a reference-guided judge over Agentium's stored agent diffs.

Commands (the work folder sits in the data folder, outside every repository):
  prepare   build the blind items from the acceptance data folder
  label     label the items in the terminal, resumably
  judge     ask the judge about every item, resumably (--limit N for a few calls; --judge-cmd for a fake)
  analyze   compute every number of the results document

protocol.md fixes the questions, prompts, settings and thresholds before any label or judge call. Standard library
only; Python 3.9.
"""

import argparse
import collections
import json
import math
import os
import random
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Dict, List, Optional, Tuple

SEED = 20261001
JUDGE_MODEL = "claude-opus-5-5"
JUDGE_EFFORT = "high"
SINGLE_REPEATS = 3
MAX_DIFF_CHARS = 40000
CALL_TIMEOUT_S = 600
FIXED = ("yes", "partly", "no")
# UNBLINDED singles had judge verdicts shown before labeling (the pricing calls); comparisons with your labels leave
# them out. Fixed before any label (protocol.md, Changes).
UNBLINDED = {"S01"}
PREFER = ("first", "second", "tie")

SYSTEM_PROMPT = (
    "You review code changes for a coding task against a reference change a developer made. You see diffs only: you "
    "cannot run code, and you do not know whether any change passes tests. Judge behavior, not style: a different "
    "approach is fine when it achieves the same result. Reply with the JSON the schema asks for, and nothing else."
)

SINGLE_PROMPT = """Task instruction:
<instruction>
{instruction}
</instruction>

Reference change (the developer's fix, test files left out):
<reference>
{reference}
</reference>

Candidate change (test files left out):
<candidate>
{candidate}
</candidate>

Does the candidate change do what the task asks, as the reference does? Answer "yes" if it fully does, "partly" if it
does only some of it or only for some inputs, and "no" if it does not, or only works around what checks it. Give the
reason in one sentence."""

PAIR_PROMPT = """Task instruction:
<instruction>
{instruction}
</instruction>

Reference change (the developer's fix, test files left out):
<reference>
{reference}
</reference>

Change 1 (test files left out):
<first>
{first}
</first>

Change 2 (test files left out):
<second>
{second}
</second>

Which change is the better fix for the task: closer to doing exactly what was asked, more likely correct in cases a
test might not cover, and no broader than needed? Answer "first", "second" or "tie", and give the reason in one
sentence."""

SINGLE_SCHEMA = {"type": "object", "properties": {"fixed": {"type": "string", "enum": list(FIXED)}, "reason": {"type": "string"}},
                 "required": ["fixed", "reason"], "additionalProperties": False}
PAIR_SCHEMA = {"type": "object", "properties": {"prefer": {"type": "string", "enum": list(PREFER)}, "reason": {"type": "string"}},
               "required": ["prefer", "reason"], "additionalProperties": False}

TEST_DIRS = {"test", "tests", "spec", "__tests__", "__mocks__", "testdata", "fixtures", "__fixtures__", "__snapshots__", "e2e"}
JS_TEST = re.compile(r"\.(test|spec)\.(js|jsx|ts|tsx|mjs|cjs|mts|cts)$")


def is_test_file(p: str) -> bool:
    """Agentium's rule for test files (internal/task.IsTestFile), so both diffs leave out what the agent never sees."""
    parts = p.split("/")
    if any(d in TEST_DIRS for d in parts[:-1]):
        return True
    base = parts[-1]
    if base.endswith("_test.go") or base.endswith("_spec.rb"):
        return True
    if base.endswith(".py"):
        return base.startswith("test_") or base.endswith("_test.py") or base in ("tests.py", "conftest.py")
    return bool(JS_TEST.search(base))


def split_diff(diff: str) -> List[Tuple[str, str]]:
    """Splits a git diff into (path, section) per file, by its "diff --git a/X b/Y" lines (path is Y)."""
    sections: List[Tuple[str, str]] = []
    path, lines = None, []  # type: Optional[str], List[str]
    for line in diff.splitlines(keepends=True):
        m = re.match(r"diff --git a/(.*) b/(.*)$", line.rstrip("\n"))
        if m:
            if path is not None:
                sections.append((path, "".join(lines)))
            path, lines = m.group(2), [line]
        elif path is not None:
            lines.append(line)
    if path is not None:
        sections.append((path, "".join(lines)))
    return sections


def without_tests(diff: str) -> str:
    return "".join(section for path, section in split_diff(diff) if not is_test_file(path))


def changed_lines(diff: str) -> int:
    return sum(1 for line in diff.splitlines() if (line.startswith("+") or line.startswith("-")) and not line.startswith(("+++", "---")))


def clip(diff: str) -> str:
    if len(diff) <= MAX_DIFF_CHARS:
        return diff
    return diff[:MAX_DIFF_CHARS] + "\n[... diff cut at %d characters ...]\n" % MAX_DIFF_CHARS


# ---- prepare ----

def copy_database(data: Path, into: Path) -> Path:
    """Copies the database with its write-ahead log, so reading it neither needs nor disturbs the original."""
    for suffix in ("", "-wal", "-shm"):
        src = data / ("agentium.db" + suffix)
        if src.exists():
            shutil.copy2(src, into / src.name)
    return into / "agentium.db"


def git(bare: Path, *args: str) -> str:
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    env.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"})
    out = subprocess.run(["git", "--git-dir", str(bare), "-c", "core.hooksPath=/dev/null", *args], capture_output=True, text=True, env=env)
    if out.returncode != 0:
        raise RuntimeError("git %s: %s" % (" ".join(args), out.stderr.strip()))
    return out.stdout


def prepare(data: Path, work: Path) -> Dict[str, int]:
    work.mkdir(mode=0o700, parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as tmp:
        db = sqlite3.connect(str(copy_database(data, Path(tmp))))
        experiments = {row[0]: {"project": row[1], "name": row[2], "lock": json.loads(row[3])}
                       for row in db.execute("SELECT id, project_id, name, lock FROM experiments WHERE lock IS NOT NULL")}
        runs = db.execute("SELECT id, experiment_id, task_name, arm, passed, slot, record FROM runs "
                          "WHERE kind = 'task' AND experiment_id IS NOT NULL AND passed IS NOT NULL ORDER BY experiment_id, slot, id").fetchall()
        db.close()
    references: Dict[Tuple[int, str], Tuple[str, str]] = {}
    singles, by_task = [], collections.defaultdict(lambda: collections.defaultdict(list))
    for run_id, exp_id, task_name, arm, passed, slot, record in runs:
        e = experiments[exp_id]
        task = next(t for t in e["lock"]["tasks"] if t["name"] == task_name)
        if (exp_id, task_name) not in references:
            bare = data / "projects" / str(e["project"]) / "repo.git"
            files = [f for f in task.get("reference", []) if not is_test_file(f)]
            ref = git(bare, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", task["base"], task["solution"], "--", *files)
            references[(exp_id, task_name)] = (task["instruction"], ref)
        instruction, ref = references[(exp_id, task_name)]
        diff_path = data / "records" / run_id / "agent.diff"
        if not diff_path.exists():
            diff_path = Path(json.loads(record).get("records", "")) / "agent.diff"
        if not diff_path.exists():
            print("no diff for run %s: left out" % run_id, file=sys.stderr)
            continue
        candidate = without_tests(diff_path.read_text(errors="replace"))
        single = {"run": run_id, "experiment": e["name"], "template": e["lock"]["design"]["template"], "task": task_name, "arm": arm,
                  "passed": bool(passed), "instruction": instruction, "reference": ref, "candidate": candidate}
        singles.append(single)
        by_task[(e["name"], task_name)][arm].append(single)
    pairs = []
    for (_, _), arms in sorted(by_task.items()):
        for a, b in zip(arms.get("A", []), arms.get("B", [])):
            if a["passed"] and b["passed"]:
                pairs.append((a, b))
    rng = random.Random(SEED)
    rng.shuffle(singles)
    rng.shuffle(pairs)
    items: Dict[str, list] = {"singles": [], "pairs": []}
    key: Dict[str, dict] = {"singles": {}, "pairs": {}}
    for i, s in enumerate(singles, 1):
        sid = "S%02d" % i
        items["singles"].append({"id": sid, "instruction": s["instruction"], "reference": s["reference"], "candidate": s["candidate"]})
        key["singles"][sid] = {k: s[k] for k in ("run", "experiment", "template", "task", "arm", "passed")}
    for i, (a, b) in enumerate(pairs, 1):
        pid = "P%02d" % i
        first, second = (a, b) if rng.random() < 0.5 else (b, a)
        items["pairs"].append({"id": pid, "instruction": a["instruction"], "reference": a["reference"], "first": first["candidate"],
                               "second": second["candidate"]})
        key["pairs"][pid] = {"experiment": a["experiment"], "template": a["template"], "task": a["task"],
                             "first": {"run": first["run"], "arm": first["arm"]}, "second": {"run": second["run"], "arm": second["arm"]}}
    write_json(work / "items.json", items)
    write_json(work / "key.json", key)
    return {"singles": len(items["singles"]), "pairs": len(items["pairs"]),
            "aa_pairs": sum(1 for p in key["pairs"].values() if p["template"] == "aa")}


def write_json(path: Path, value) -> None:
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(value, indent=1, sort_keys=True) + "\n")
    os.chmod(tmp, 0o600)
    tmp.replace(path)


def read_json(path: Path, default=None):
    return json.loads(path.read_text()) if path.exists() else default


# ---- label ----

def single_text(item: dict) -> str:
    return SINGLE_PROMPT.format(instruction=item["instruction"], reference=clip(item["reference"]), candidate=clip(item["candidate"]))


def pair_text(item: dict, swapped: bool = False) -> str:
    first, second = (item["second"], item["first"]) if swapped else (item["first"], item["second"])
    return PAIR_PROMPT.format(instruction=item["instruction"], reference=clip(item["reference"]), first=clip(first), second=clip(second))


def show(text: str) -> None:
    if sys.stdout.isatty() and shutil.which("less"):
        subprocess.run(["less", "-R"], input=text, text=True)
    else:
        print(text)


def label(work: Path, ask=input, show=show) -> int:
    """Labels unlabeled items, singles then pairs, saving after each; returns how many are left."""
    items = read_json(work / "items.json")
    labels = read_json(work / "labels.json", {"singles": {}, "pairs": {}})
    answers = {"y": "yes", "p": "partly", "n": "no", "1": "first", "2": "second", "t": "tie"}
    queue = [("singles", i) for i in items["singles"] if i["id"] not in labels["singles"]] + \
            [("pairs", i) for i in items["pairs"] if i["id"] not in labels["pairs"]]
    for done, (kind, item) in enumerate(queue):
        show("%s (%d left)\n\n%s" % (item["id"], len(queue) - done, single_text(item) if kind == "singles" else pair_text(item)))
        choices = "y/p/n" if kind == "singles" else "1/2/t"
        while True:
            reply = ask("%s %s [%s, then an optional note; q quits]: " % (item["id"], "fixed?" if kind == "singles" else "better?", choices)).strip()
            if reply == "q":
                return len(queue) - done
            code, _, note = reply.partition(" ")
            value = answers.get(code)
            if value in (FIXED if kind == "singles" else PREFER):
                field = "fixed" if kind == "singles" else "prefer"
                labels[kind][item["id"]] = {field: value, "note": note.strip()}
                write_json(work / "labels.json", labels)
                break
    return 0


# ---- judge ----

def judge_command(cmd: str, schema: dict) -> List[str]:
    return [cmd, "-p", "--model", JUDGE_MODEL, "--effort", JUDGE_EFFORT, "--tools", "", "--system-prompt", SYSTEM_PROMPT,
            "--json-schema", json.dumps(schema), "--output-format", "json", "--no-session-persistence",
            "--setting-sources", "project", "--strict-mcp-config"]


def parse_reply(stdout: str, field: str, allowed) -> Tuple[Optional[dict], float, str]:
    """Reads Claude Code's JSON result: the structured verdict (or one written as JSON in the text), and the cost."""
    try:
        out = json.loads(stdout)
    except ValueError:
        return None, 0.0, "not JSON: %s" % stdout[:200]
    cost = float(out.get("total_cost_usd") or 0)
    verdict = out.get("structured_output")
    if not isinstance(verdict, dict):
        m = re.search(r"\{.*\}", str(out.get("result", "")), re.S)
        try:
            verdict = json.loads(m.group(0)) if m else None
        except ValueError:
            verdict = None
    if not isinstance(verdict, dict) or verdict.get(field) not in allowed:
        return None, cost, "no valid %s in: %s" % (field, str(out.get("result", ""))[:200])
    return {field: verdict[field], "reason": str(verdict.get("reason", ""))}, cost, ""


def jobs(items: dict) -> List[Tuple[str, str, dict, str]]:
    """Every judgement: each single SINGLE_REPEATS times, each pair in both orders."""
    out = []
    for item in items["singles"]:
        for r in range(1, SINGLE_REPEATS + 1):
            out.append(("%s#%d" % (item["id"], r), "single", item, single_text(item)))
    for item in items["pairs"]:
        out.append((item["id"] + "#fs", "pair", item, pair_text(item)))
        out.append((item["id"] + "#sf", "pair", item, pair_text(item, swapped=True)))
    return out


def judge(work: Path, cmd: str = "claude", limit: Optional[int] = None, budget: Optional[float] = None, log=print) -> Dict[str, float]:
    items = read_json(work / "items.json")
    path = work / "verdicts.jsonl"
    rows = [json.loads(line) for line in path.read_text().splitlines() if line.strip()] if path.exists() else []
    done = {r["job"] for r in latest(rows).values() if "error" not in r}  # a job that ended in an error is tried again
    spent = sum(r.get("cost_usd", 0) for r in rows)
    env = dict(os.environ, CLAUDE_CODE_DISABLE_AUTO_MEMORY="1", ENABLE_CLAUDEAI_MCP_SERVERS="false", DISABLE_AUTOUPDATER="1")
    calls = 0
    for job, kind, item, prompt in jobs(items):
        if job in done:
            continue
        if limit is not None and calls >= limit:
            break
        if budget is not None and spent >= budget:
            log("budget reached: $%.2f of $%.2f" % (spent, budget))
            break
        field, allowed, schema = ("fixed", FIXED, SINGLE_SCHEMA) if kind == "single" else ("prefer", PREFER, PAIR_SCHEMA)
        verdict, cost, error = None, 0.0, ""
        for attempt in (1, 2):  # a malformed reply is asked once more
            with tempfile.TemporaryDirectory() as empty:
                started = time.time()
                proc = subprocess.run(judge_command(cmd, schema), input=prompt, capture_output=True, text=True, cwd=empty, env=env,
                                      timeout=CALL_TIMEOUT_S)
            verdict, c, error = parse_reply(proc.stdout, field, allowed)
            cost += c
            if proc.returncode != 0 and not error:
                error = "exit %d: %s" % (proc.returncode, proc.stderr.strip()[:200])
            if verdict and not error:
                break
        calls += 1
        spent += cost
        row = {"job": job, "item": item["id"], "kind": kind, "cost_usd": round(cost, 6), "seconds": round(time.time() - started, 1),
               "model": JUDGE_MODEL, "effort": JUDGE_EFFORT}
        if verdict and not error:
            row.update(verdict)
        else:
            row["error"] = error
        with path.open("a") as f:
            f.write(json.dumps(row, sort_keys=True) + "\n")
        os.chmod(path, 0o600)
        log("%-8s %-7s %s  $%.4f  (spent $%.2f)" % (job, row.get("fixed") or row.get("prefer") or "ERROR", row.get("reason", error)[:70], cost, spent))
    return {"calls": calls, "spent": spent}


def latest(rows: List[dict]) -> Dict[str, dict]:
    """Each job's last row: a job retried after an error keeps only its later answer."""
    out: Dict[str, dict] = {}
    for r in rows:
        out[r["job"]] = r
    return out


# ---- analyze ----

def wilson(k: int, n: int, z: float = 1.959964) -> Tuple[float, float]:
    if n == 0:
        return (0.0, 1.0)
    p = k / n
    d = 1 + z * z / n
    c = p + z * z / (2 * n)
    r = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n))
    return ((c - r) / d, (c + r) / d)


def binomial_two_sided(k: int, n: int) -> float:
    """Exact two-sided p-value of k successes in n at p = 0.5."""
    if n == 0:
        return 1.0
    probs = [math.comb(n, i) / 2 ** n for i in range(n + 1)]
    return min(1.0, sum(p for p in probs if p <= probs[k] + 1e-12))


def cohen_kappa(pairs: List[Tuple[str, str]]) -> Optional[float]:
    n = len(pairs)
    if n == 0:
        return None
    observed = sum(a == b for a, b in pairs) / n
    ca, cb = collections.Counter(a for a, _ in pairs), collections.Counter(b for _, b in pairs)
    expected = sum(ca[x] * cb[x] for x in set(ca) | set(cb)) / (n * n)
    return None if expected == 1 else (observed - expected) / (1 - expected)


def majority(values: List[str]) -> Optional[str]:
    """The most common verdict of a single's repeats; with no majority (yes, partly, no), partly."""
    if not values:
        return None
    (top, count), = collections.Counter(values).most_common(1)
    return top if count * 2 > len(values) else "partly"


def file_set(diff: str) -> set:
    return {p for p, _ in split_diff(diff)}


def baseline_fixed(item: dict) -> str:
    """The deterministic baseline: fixed when the candidate touches at least half of the reference's files."""
    ref = file_set(item["reference"])
    if not ref:
        return "yes"
    return "yes" if len(ref & file_set(item["candidate"])) / len(ref) >= 0.5 else "no"


def jaccard(a: set, b: set) -> float:
    return len(a & b) / len(a | b) if a | b else 1.0


def baseline_prefer(item: dict) -> str:
    ref = file_set(item["reference"])
    j1, j2 = jaccard(file_set(item["first"]), ref), jaccard(file_set(item["second"]), ref)
    if abs(j1 - j2) < 1e-9:
        r = max(changed_lines(item["reference"]), 1)
        d1, d2 = abs(changed_lines(item["first"]) - r), abs(changed_lines(item["second"]) - r)  # closer in size to the reference
        return "tie" if d1 == d2 else ("first" if d1 < d2 else "second")
    return "first" if j1 > j2 else "second"


def pct(k: int, n: int) -> str:
    if n == 0:
        return "n/a (no cases)"
    lo, hi = wilson(k, n)
    return "%d of %d, %.0f%% (95%%: %.0f–%.0f%%)" % (k, n, 100 * k / n, 100 * lo, 100 * hi)


def analyze(work: Path, unblinded=frozenset(UNBLINDED)) -> str:
    items = read_json(work / "items.json")
    key = read_json(work / "key.json")
    labels = read_json(work / "labels.json", {"singles": {}, "pairs": {}})
    rows = [json.loads(line) for line in (work / "verdicts.jsonl").read_text().splitlines() if line.strip()] \
        if (work / "verdicts.jsonl").exists() else []
    verdicts = list(latest(rows).values())
    by_item = collections.defaultdict(list)
    for v in verdicts:
        by_item[v["item"]].append(v)
    singles = {i["id"]: i for i in items["singles"]}
    pairs = {i["id"]: i for i in items["pairs"]}
    out = []
    w = out.append

    # Singles: the judge's majority of its repeats, and whether all repeats agreed.
    judge_fixed, consistent, errors = {}, [], 0
    for sid in singles:
        vs = [v["fixed"] for v in by_item.get(sid, []) if "fixed" in v]
        errors += sum(1 for v in by_item.get(sid, []) if "error" in v)
        if vs:
            judge_fixed[sid] = majority(vs)
            if len(vs) == SINGLE_REPEATS:
                consistent.append(len(set(vs)) == 1)
    # Pairs: each order's preference mapped to the item's own order; a flip between the orders counts as a tie.
    judge_prefer, flips = {}, []
    for pid in pairs:
        fs = [v["prefer"] for v in by_item.get(pid, []) if v["job"].endswith("#fs") and "prefer" in v]
        sf = [v["prefer"] for v in by_item.get(pid, []) if v["job"].endswith("#sf") and "prefer" in v]
        errors += sum(1 for v in by_item.get(pid, []) if "error" in v)
        if fs and sf:
            back = {"first": "second", "second": "first", "tie": "tie"}[sf[0]]
            flips.append(fs[0] != back)
            judge_prefer[pid] = fs[0] if fs[0] == back else "tie"

    binary = lambda v: "fixed" if v == "yes" else "not fixed"
    w("## Q1. False passes: on runs that passed their tests, does the judge agree with you on \"fixed\"?\n")
    passing = [s for s in singles if key["singles"][s]["passed"] and s in labels["singles"] and s in judge_fixed and s not in unblinded]
    agree = sum(binary(judge_fixed[s]) == binary(labels["singles"][s]["fixed"]) for s in passing)
    base_agree = sum(binary(baseline_fixed(singles[s])) == binary(labels["singles"][s]["fixed"]) for s in passing)
    exact = sum(judge_fixed[s] == labels["singles"][s]["fixed"] for s in passing)
    kappa = cohen_kappa([(binary(judge_fixed[s]), binary(labels["singles"][s]["fixed"])) for s in passing])
    human_not = sum(binary(labels["singles"][s]["fixed"]) == "not fixed" for s in passing)
    w("- Judge agrees with you (fixed or not): %s; Cohen's kappa %s." % (pct(agree, len(passing)), "n/a" if kappa is None else "%.2f" % kappa))
    w("- Same three-way answer (yes, partly, no): %s." % pct(exact, len(passing)))
    w("- Baseline (touches at least half of the reference's files) agrees with you: %s." % pct(base_agree, len(passing)))
    w("- Passing runs you judged not fixed (false passes): %s.\n" % pct(human_not, len(passing)))

    w("## Q2. Quality: when both arms passed, does the judge prefer the same change as you?\n")
    rated = [p for p in pairs if p in labels["pairs"] and p in judge_prefer]
    agree2 = sum(judge_prefer[p] == labels["pairs"][p]["prefer"] for p in rated)
    base2 = sum(baseline_prefer(pairs[p]) == labels["pairs"][p]["prefer"] for p in rated)
    w("- Judge agrees with you (first, second or tie): %s." % pct(agree2, len(rated)))
    w("- Baseline (file overlap, then size closest to the reference) agrees with you: %s.\n" % pct(base2, len(rated)))

    w("## Q3. Tasks without tests: without seeing results, does the judge's verdict match the tests?\n")
    judged = [s for s in singles if s in judge_fixed]
    match = sum((binary(judge_fixed[s]) == "fixed") == key["singles"][s]["passed"] for s in judged)
    on_pass = [s for s in judged if key["singles"][s]["passed"]]
    on_fail = [s for s in judged if not key["singles"][s]["passed"]]
    w("- All runs: %s." % pct(match, len(judged)))
    w("- Runs that passed, judged fixed: %s." % pct(sum(binary(judge_fixed[s]) == "fixed" for s in on_pass), len(on_pass)))
    w("- Runs that failed, judged not fixed: %s.\n" % pct(sum(binary(judge_fixed[s]) != "fixed" for s in on_fail), len(on_fail)))

    w("## Q4–Q5. Reliability and cost\n")
    w("- Same verdict on all %d repeats: %s." % (SINGLE_REPEATS, pct(sum(consistent), len(consistent))))
    w("- Pair order flips the preference: %s." % pct(sum(flips), len(flips)))
    aa = [p for p in judge_prefer if key["pairs"][p]["template"] == "aa" and judge_prefer[p] != "tie"]
    arm_a = sum(key["pairs"][p][judge_prefer[p]]["arm"] == "A" for p in aa)
    w("- A/A pairs (same context in both arms) with a preference: arm A preferred in %d of %d (two-sided p = %.2f; no "
      "difference expected)." % (arm_a, len(aa), binomial_two_sided(arm_a, len(aa))))
    costs = [r["cost_usd"] for r in rows]  # retried calls cost too
    w("- Judgements: %d (%d errors); cost $%.2f in all, $%.3f each on average (%s, effort %s).\n" %
      (len(verdicts), errors, sum(costs), sum(costs) / len(costs) if costs else 0, JUDGE_MODEL, JUDGE_EFFORT))

    w("## Go/no-go (thresholds fixed in protocol.md)\n")
    checks = [
        ("agrees with you on fixed, passing runs >= 80%", len(passing) > 0 and agree / len(passing) >= 0.80),
        ("same verdict on all repeats >= 90%", len(consistent) > 0 and sum(consistent) / len(consistent) >= 0.90),
        ("order flips <= 10% of pairs", len(flips) > 0 and sum(flips) / len(flips) <= 0.10),
        ("no significant arm preference on A/A pairs (p >= 0.05)", binomial_two_sided(arm_a, len(aa)) >= 0.05),
        ("agrees with you more often than the baseline", agree > base_agree),
    ]
    for name, ok in checks:
        w("- %s: %s" % ("met" if ok else "NOT met", name))
    secondary = all(ok for _, ok in checks)
    w("\nSecondary judge score (false passes and quality): %s." % ("GO: worth building" if secondary else "NO-GO"))
    no_tests = len(judged) > 0 and match / len(judged) >= 0.90
    w("Grading tasks without tests: %s." % ("promising (at least 90%% of runs match their tests)" if no_tests else "NO-GO"))
    return "\n".join(out) + "\n"


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--data", default=os.environ.get("AGENTIUM_HOME") or str(Path.home() / ".agentium-acceptance"))
    parser.add_argument("--work", help="the pilot's folder (default: DATA/judge-pilot)")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("prepare")
    sub.add_parser("label")
    j = sub.add_parser("judge")
    j.add_argument("--judge-cmd", default="claude")
    j.add_argument("--limit", type=int)
    j.add_argument("--budget", type=float, help="stop once the judgements so far cost this many USD")
    a = sub.add_parser("analyze")
    a.add_argument("--out")
    args = parser.parse_args(argv)
    data = Path(args.data).expanduser()
    work = Path(args.work).expanduser() if args.work else data / "judge-pilot"
    if args.command == "prepare":
        counts = prepare(data, work)
        print("Prepared %(singles)d diffs and %(pairs)d pairs (%(aa_pairs)d from A/A experiments) in " % counts + str(work))
    elif args.command == "label":
        left = label(work)
        print("All labeled." if left == 0 else "%d left; run label again to go on." % left)
    elif args.command == "judge":
        result = judge(work, args.judge_cmd, args.limit, args.budget)
        print("%d call(s); $%.2f spent so far." % (result["calls"], result["spent"]))
    else:
        text = analyze(work)
        if args.out:
            Path(args.out).write_text(text)
        else:
            sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
