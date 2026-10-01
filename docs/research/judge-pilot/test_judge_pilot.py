"""Tests for judge_pilot.py: python3 -m unittest discover docs/research/judge-pilot"""

import json
import os
import sqlite3
import stat
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import judge_pilot as jp  # noqa: E402

DIFF = """diff --git a/value.txt b/value.txt
--- a/value.txt
+++ b/value.txt
@@ -1 +1 @@
-old
+new
diff --git a/tests/value_test.sh b/tests/value_test.sh
--- a/tests/value_test.sh
+++ b/tests/value_test.sh
@@ -0,0 +1 @@
+grep -q new value.txt
diff --git a/notes.md b/notes.md
--- a/notes.md
+++ b/notes.md
@@ -1 +1,2 @@
 a
+b
"""


class Rules(unittest.TestCase):
    def test_test_files_follow_agentium(self):
        for p, want in [("internal/x/x_test.go", True), ("tests/a.sh", True), ("pkg/testdata/f.json", True), ("a/test_b.py", True),
                        ("web/a.spec.ts", True), ("lib/a_spec.rb", True), ("conftest.py", True), ("internal/x/x.go", False),
                        ("docs/testing.md", False), ("latest.py", False), ("contest/a.go", False)]:
            self.assertEqual(jp.is_test_file(p), want, p)

    def test_diffs_split_and_lose_their_tests(self):
        self.assertEqual([p for p, _ in jp.split_diff(DIFF)], ["value.txt", "tests/value_test.sh", "notes.md"])
        kept = jp.without_tests(DIFF)
        self.assertNotIn("value_test", kept)
        self.assertIn("+new", kept)
        self.assertEqual(jp.changed_lines(kept), 3)

    def test_clip(self):
        self.assertEqual(jp.clip("x"), "x")
        self.assertIn("cut at", jp.clip("x" * (jp.MAX_DIFF_CHARS + 1)))


class Statistics(unittest.TestCase):
    def test_wilson(self):
        lo, hi = jp.wilson(40, 50)
        self.assertAlmostEqual(lo, 0.6696, places=3)
        self.assertAlmostEqual(hi, 0.8876, places=3)
        self.assertEqual(jp.wilson(0, 0), (0.0, 1.0))

    def test_binomial(self):
        self.assertAlmostEqual(jp.binomial_two_sided(0, 8), 2 / 256)
        self.assertEqual(jp.binomial_two_sided(4, 8), 1.0)
        self.assertEqual(jp.binomial_two_sided(0, 0), 1.0)

    def test_kappa_and_majority(self):
        self.assertAlmostEqual(jp.cohen_kappa([("a", "a"), ("b", "b")]), 1.0)
        self.assertIsNone(jp.cohen_kappa([]))
        self.assertEqual(jp.majority(["yes", "yes", "no"]), "yes")
        self.assertEqual(jp.majority(["yes", "partly", "no"]), "partly")
        self.assertIsNone(jp.majority([]))

    def test_baselines(self):
        ref = jp.without_tests(DIFF)  # value.txt and notes.md, as references hold no tests
        self.assertEqual(jp.baseline_fixed({"reference": ref, "candidate": jp.split_diff(DIFF)[0][1]}), "yes")  # 1 of 2 files
        self.assertEqual(jp.baseline_fixed({"reference": ref, "candidate": ""}), "no")
        item = {"reference": ref, "first": jp.split_diff(DIFF)[0][1], "second": DIFF}
        self.assertEqual(jp.baseline_prefer(item), "second")


class Replies(unittest.TestCase):
    def test_structured_output_text_and_errors(self):
        v, cost, err = jp.parse_reply(json.dumps({"structured_output": {"fixed": "yes", "reason": "r"}, "total_cost_usd": 0.1}), "fixed", jp.FIXED)
        self.assertEqual((v, cost, err), ({"fixed": "yes", "reason": "r"}, 0.1, ""))
        v, _, err = jp.parse_reply(json.dumps({"result": 'Sure: {"prefer": "tie", "reason": "same"}'}), "prefer", jp.PREFER)
        self.assertEqual(v["prefer"], "tie")
        self.assertIsNone(jp.parse_reply(json.dumps({"result": '{"fixed": "maybe"}'}), "fixed", jp.FIXED)[0])
        self.assertIn("not JSON", jp.parse_reply("crash", "fixed", jp.FIXED)[2])

    def test_command_has_no_tools_and_a_schema(self):
        cmd = jp.judge_command("claude", jp.SINGLE_SCHEMA)
        self.assertEqual(cmd[cmd.index("--tools") + 1], "")
        self.assertEqual(cmd[cmd.index("--model") + 1], jp.JUDGE_MODEL)
        self.assertIn('"enum": ["yes", "partly", "no"]', cmd[cmd.index("--json-schema") + 1])


def git(repo, *args):
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    env.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"})
    return subprocess.run(["git", "-C", str(repo), "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", *args],
                          check=True, capture_output=True, text=True, env=env).stdout.strip()


def data_folder(root: Path) -> Path:
    """A data folder with one A/B and one A/A experiment: six graded runs, one of them failed, and one ungraded."""
    data = root / "data"
    user = root / "user"
    user.mkdir()
    git(user, "init", "-q")
    (user / "value.txt").write_text("old\n")
    (user / "notes.md").write_text("a\n")
    git(user, "add", "-A")
    git(user, "commit", "-q", "-m", "base")
    base = git(user, "rev-parse", "HEAD")
    (user / "value.txt").write_text("new\n")
    (user / "tests").mkdir()
    (user / "tests" / "value_test.sh").write_text("grep -q new value.txt\n")
    git(user, "add", "-A")
    git(user, "commit", "-q", "-m", "solution")
    solution = git(user, "rev-parse", "HEAD")
    bare = data / "projects" / "1" / "repo.git"
    bare.parent.mkdir(parents=True)
    subprocess.run(["git", "clone", "-q", "--bare", str(user), str(bare)], check=True)
    db = sqlite3.connect(str(data / "agentium.db"))
    db.execute("CREATE TABLE experiments (id INTEGER, project_id INTEGER, name TEXT, lock TEXT)")
    db.execute("CREATE TABLE runs (id TEXT, experiment_id INTEGER, task_name TEXT, arm TEXT, passed INTEGER, slot INTEGER, record TEXT, kind TEXT)")
    task = {"name": "value", "instruction": "Make the value new.", "base": base, "solution": solution, "reference": ["value.txt"],
            "hidden_tests": ["tests/value_test.sh"]}
    for eid, name, template in [(1, "ab", "context-ab"), (2, "aa", "aa")]:
        db.execute("INSERT INTO experiments VALUES (?, 1, ?, ?)", (eid, name, json.dumps({"design": {"template": template}, "tasks": [task]})))
    runs = [("r1", 1, "A", 1, 0), ("r2", 1, "B", 1, 1), ("r3", 1, "A", 0, 2), ("r4", 1, "B", None, 3),
            ("r5", 2, "A", 1, 0), ("r6", 2, "B", 1, 1), ("r7", 2, "A", 1, 2)]
    for n, (rid, eid, arm, passed, slot) in enumerate(runs):
        db.execute("INSERT INTO runs VALUES (?, ?, 'value', ?, ?, ?, ?, 'task')", (rid, eid, arm, passed, slot, json.dumps({"records": "/gone"})))
        (data / "records" / rid).mkdir(parents=True)
        (data / "records" / rid / "agent.diff").write_text(DIFF.replace("+new", "+new variant %d" % n))
    db.commit()
    db.close()
    return data


def fake_claude(root: Path, garbage_first: bool = False) -> str:
    """A fake Claude Code: answers yes (singles) or first (pairs) as structured output, at $0.01; optionally garbage first."""
    path = root / "claude"
    counter = root / "calls"
    path.write_text(textwrap.dedent("""\
        #!%s
        import json, sys, pathlib
        counter = pathlib.Path(%r)
        n = int(counter.read_text()) + 1 if counter.exists() else 1
        counter.write_text(str(n))
        prompt = sys.stdin.read()
        assert "--tools" in sys.argv and pathlib.Path.cwd().iterdir() is not None
        if %r and n == 1:
            print("garbage"); sys.exit(1)
        out = {"prefer": "first", "reason": "r"} if "<first>" in prompt else {"fixed": "yes", "reason": "r"}
        print(json.dumps({"structured_output": out, "total_cost_usd": 0.01}))
        """) % (sys.executable, str(counter), garbage_first))
    path.chmod(path.stat().st_mode | stat.S_IEXEC)
    return str(path)


class Pipeline(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.data = data_folder(self.root)
        self.work = self.root / "work"

    def tearDown(self):
        self.tmp.cleanup()

    def test_prepare_is_blind_and_deterministic(self):
        counts = jp.prepare(self.data, self.work)
        self.assertEqual(counts, {"singles": 6, "pairs": 2, "aa_pairs": 1})  # r4 is ungraded; r3 failed, so A/B has one pair
        items = (self.work / "items.json").read_text()
        for hidden in ("r1", "\"arm\"", "passed", "tests/value_test.sh", "grep -q new"):
            self.assertNotIn(hidden, items)
        key = json.loads((self.work / "key.json").read_text())
        self.assertEqual(sorted(k["run"] for k in key["singles"].values()), ["r1", "r2", "r3", "r5", "r6", "r7"])
        self.assertIn("+new", json.loads(items)["singles"][0]["reference"])
        first = (self.work / "items.json").read_text(), (self.work / "key.json").read_text()
        jp.prepare(self.data, self.work)
        self.assertEqual(first, ((self.work / "items.json").read_text(), (self.work / "key.json").read_text()))
        self.assertEqual(stat.S_IMODE(os.stat(self.work / "key.json").st_mode), 0o600)

    def test_judge_resumes_retries_and_stops_at_its_budget(self):
        jp.prepare(self.data, self.work)
        cmd = fake_claude(self.root, garbage_first=True)
        quiet = lambda *a: None
        first = jp.judge(self.work, cmd, limit=2, log=quiet)
        self.assertEqual(first["calls"], 2)
        rows = [json.loads(l) for l in (self.work / "verdicts.jsonl").read_text().splitlines()]
        self.assertEqual(rows[0]["fixed"], "yes")  # the garbage reply was asked again
        self.assertAlmostEqual(first["spent"], 0.02)
        rest = jp.judge(self.work, cmd, log=quiet)
        self.assertEqual(first["calls"] + rest["calls"], 6 * jp.SINGLE_REPEATS + 2 * 2)
        self.assertEqual(jp.judge(self.work, cmd, log=quiet)["calls"], 0)
        other = self.root / "other"
        jp.prepare(self.data, other)
        self.assertEqual(jp.judge(other, fake_claude(self.root), budget=0.025, log=quiet)["calls"], 3)

    def test_jobs_that_ended_in_an_error_are_tried_again(self):
        jp.prepare(self.data, self.work)
        broken = self.root / "broken"
        broken.write_text("#!/bin/sh\necho 'API Error: 400 this version does not support the model'\nexit 1\n")
        broken.chmod(0o755)
        self.assertEqual(jp.judge(self.work, str(broken), limit=2, log=lambda *a: None)["calls"], 2)
        again = jp.judge(self.work, fake_claude(self.root), log=lambda *a: None)
        self.assertEqual(again["calls"], 6 * jp.SINGLE_REPEATS + 2 * 2)  # the two errors are judged again
        latest = jp.latest([json.loads(l) for l in (self.work / "verdicts.jsonl").read_text().splitlines()])
        self.assertTrue(all("error" not in r for r in latest.values()))

    def test_label_saves_each_answer_and_resumes(self):
        jp.prepare(self.data, self.work)
        answers = iter(["maybe", "y", "p wrong field", "q"])
        left = jp.label(self.work, ask=lambda _: next(answers), show=lambda _: None)
        self.assertEqual(left, 6)
        labels = json.loads((self.work / "labels.json").read_text())
        self.assertEqual([v["fixed"] for v in labels["singles"].values()], ["yes", "partly"])
        self.assertEqual(list(labels["singles"].values())[1]["note"], "wrong field")
        answers = iter(["n"] * 4 + ["1", "t"])
        self.assertEqual(jp.label(self.work, ask=lambda _: next(answers), show=lambda _: None), 0)
        self.assertEqual(len(json.loads((self.work / "labels.json").read_text())["pairs"]), 2)

    def test_analyze_counts_what_it_should(self):
        jp.prepare(self.data, self.work)
        jp.judge(self.work, fake_claude(self.root), log=lambda *a: None)
        key = json.loads((self.work / "key.json").read_text())
        labels = {"singles": {s: {"fixed": "yes", "note": ""} for s in key["singles"]}, "pairs": {p: {"prefer": "first", "note": ""} for p in key["pairs"]}}
        jp.write_json(self.work / "labels.json", labels)
        text = jp.analyze(self.work, unblinded=set())
        self.assertIn("4 of 4", jp.analyze(self.work, unblinded={next(s for s in key["singles"] if key["singles"][s]["passed"])}))
        self.assertIn("Judge agrees with you (fixed or not): 5 of 5, 100%", text)  # five passing runs
        self.assertIn("Runs that failed, judged not fixed: 0 of 1", text)
        self.assertIn("Same verdict on all 3 repeats: 6 of 6", text)
        # The fake always says "first", so both orders disagree once mapped back: every pair flips, and counts as a tie.
        self.assertIn("Pair order flips the preference: 2 of 2", text)
        self.assertIn("Judge agrees with you (first, second or tie): 0 of 2", text)
        self.assertIn("NOT met: order flips <= 10% of pairs", text)
        self.assertIn("Secondary judge score (false passes and quality): NO-GO", text)


if __name__ == "__main__":
    unittest.main()
