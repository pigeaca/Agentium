"""Tests for judge_pilot.py: python3 -m unittest discover docs/research/judge-pilot"""

import json
import os
import sqlite3
import stat
import subprocess
import sys
import tempfile
import textwrap
import threading
import unittest
import urllib.error
import urllib.request
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
diff --git a/notes.txt b/notes.txt
--- a/notes.txt
+++ b/notes.txt
@@ -1 +1,2 @@
 a
+b
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-x
+y
"""


class Rules(unittest.TestCase):
    def test_test_files_follow_agentium(self):
        for p, want in [("internal/x/x_test.go", True), ("tests/a.sh", True), ("pkg/testdata/f.json", True), ("a/test_b.py", True),
                        ("web/a.spec.ts", True), ("lib/a_spec.rb", True), ("conftest.py", True), ("internal/x/x.go", False),
                        ("docs/testing.md", False), ("latest.py", False), ("contest/a.go", False)]:
            self.assertEqual(jp.is_test_file(p), want, p)

    def test_diffs_split_and_lose_their_tests_and_documents(self):
        self.assertEqual([p for p, _ in jp.split_diff(DIFF)], ["value.txt", "tests/value_test.sh", "notes.txt", "README.md"])
        kept = jp.code_only(DIFF)
        self.assertEqual([p for p, _ in jp.split_diff(kept)], ["value.txt", "notes.txt"])
        self.assertEqual(jp.changed_lines(kept), 3)
        for p, want in [("README.md", True), ("docs/a.rst", True), (".agents/plans/x.md", True), ("testdata/golden.md", False),
                        ("notes.txt", False), ("a/b.adoc", True)]:
            self.assertEqual(jp.is_document(p), want, p)

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
        ref = jp.code_only(DIFF)  # value.txt and notes.txt, as references hold no tests or documents
        self.assertEqual(jp.baseline_fixed({"reference": ref, "candidate": jp.split_diff(DIFF)[0][1]}), "yes")  # 1 of 2 files
        self.assertEqual(jp.baseline_fixed({"reference": ref, "candidate": ""}), "no")
        item = {"reference": ref, "first": jp.split_diff(DIFF)[0][1], "second": jp.code_only(DIFF)}
        self.assertEqual(jp.baseline_prefer(item), "second")


class Replies(unittest.TestCase):
    def test_structured_output_text_and_errors(self):
        reply = jp.parse_reply(json.dumps({"structured_output": {"fixed": "yes", "reason": "r"}, "total_cost_usd": 0.1}), "fixed", jp.FIXED)
        self.assertEqual(reply, ({"fixed": "yes", "reason": "r"}, 0.1, "", ""))
        v, _, err, _ = jp.parse_reply(json.dumps({"result": 'Sure: {"prefer": "tie", "reason": "same"}'}), "prefer", jp.PREFER)
        self.assertEqual(v["prefer"], "tie")
        self.assertEqual(jp.parse_reply(json.dumps({"result": '{"fixed": "maybe"}'}), "fixed", jp.FIXED)[3], "malformed")
        self.assertEqual(jp.parse_reply("crash", "fixed", jp.FIXED)[3], "infra")
        refused = jp.parse_reply(json.dumps({"is_error": True, "result": "API Error: 400 needs a newer version", "total_cost_usd": 0}), "fixed", jp.FIXED)
        self.assertEqual((refused[0], refused[3]), (None, "infra"))

    def test_the_judge_keeps_an_allowlisted_environment(self):
        env = jp.judge_env({"PATH": "/bin", "HOME": "/h", "LC_ALL": "C", "CLAUDE_CONFIG_DIR": "/h/.claude", "ANTHROPIC_BASE_URL": "x",
                            "CLAUDECODE": "1", "CLAUDE_EFFORT": "low", "ANTHROPIC_API_KEY": "secret", "GITHUB_TOKEN": "t"})
        self.assertEqual(sorted(env), ["CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CONFIG_DIR", "DISABLE_AUTOUPDATER", "ENABLE_CLAUDEAI_MCP_SERVERS",
                                       "HOME", "LC_ALL", "PATH"])

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
    (user / "notes.txt").write_text("a\n")
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
        self.assertEqual((rows[0]["error_kind"], rows[1]["fixed"]), ("infra", "yes"))  # no answer came: a row of its own
        self.assertAlmostEqual(first["spent"], 0.01)
        rest = jp.judge(self.work, cmd, log=quiet)
        self.assertEqual(first["calls"] + rest["calls"], 6 * jp.SINGLE_REPEATS + 2 * 2 + 1)  # the infrastructure error is tried again
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

    def test_malformed_answers_are_asked_twice_then_kept_and_infra_errors_stop_at_max_tries(self):
        jp.prepare(self.data, self.work)
        quiet = lambda *a: None
        malformed = self.root / "malformed"
        malformed.write_text("#!/bin/sh\necho '{\"result\": \"I cannot say\", \"total_cost_usd\": 0.01}'\n")
        malformed.chmod(0o755)
        self.assertEqual(jp.judge(self.work, str(malformed), limit=1, log=quiet)["calls"], 1)
        rows = [json.loads(l) for l in (self.work / "verdicts.jsonl").read_text().splitlines()]
        self.assertEqual((rows[0]["error_kind"], rows[0]["cost_usd"]), ("malformed", 0.02))  # asked twice, both paid
        broken = self.root / "broken"
        broken.write_text("#!/bin/sh\nexit 1\n")
        broken.chmod(0o755)
        for _ in range(jp.MAX_TRIES + 1):
            jp.judge(self.work, str(broken), limit=2, log=quiet)
        rows = [json.loads(l) for l in (self.work / "verdicts.jsonl").read_text().splitlines()]
        self.assertEqual(sum(r["job"] == "S01#1" for r in rows), 1)  # the malformed answer is final
        self.assertEqual(sum(r["job"] == "S01#2" for r in rows), jp.MAX_TRIES)

    def test_a_timeout_is_a_row_not_a_crash(self):
        jp.prepare(self.data, self.work)
        slow = self.root / "slow"
        slow.write_text("#!/bin/sh\nsleep 5\n")
        slow.chmod(0o755)
        saved, jp.CALL_TIMEOUT_S = jp.CALL_TIMEOUT_S, 1
        try:
            self.assertEqual(jp.judge(self.work, str(slow), limit=1, log=lambda *a: None)["calls"], 1)
        finally:
            jp.CALL_TIMEOUT_S = saved
        row = json.loads((self.work / "verdicts.jsonl").read_text().splitlines()[0])
        self.assertEqual(row["error_kind"], "infra")
        self.assertIn("timed out", row["error"])

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

    def test_browser_form_is_blind_saves_answers_and_refuses_other_callers(self):
        jp.prepare(self.data, self.work)
        server = jp.label_server(self.work, 0)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        base = "http://127.0.0.1:%d" % server.server_address[1]
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # never through a configured proxy

        def call(path, body=None, content_type="application/json", host=None):
            req = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode())
            if body is not None:
                req.add_header("Content-Type", content_type)
            if host:
                req.add_header("Host", host)
            try:
                with opener.open(req) as resp:
                    return resp.status, resp.read()
            except urllib.error.HTTPError as e:
                return e.code, e.read()

        status, page = call("/")
        self.assertEqual(status, 200)
        self.assertIn(b"<title>Judge pilot labels</title>", page)
        status, raw = call("/state")
        state = json.loads(raw)
        self.assertEqual((len(state["items"]["singles"]), len(state["items"]["pairs"])), (6, 2))
        self.assertEqual(state["rubric"], jp.SYSTEM_PROMPT)
        self.assertTrue(state["questions"]["singles"].startswith("Does the candidate change do what the task asks"))
        for hidden in (b"r1", b"\"arm\"", b"passed", b"tests/value_test.sh"):
            self.assertNotIn(hidden, raw)

        self.assertEqual(call("/label", {"kind": "singles", "id": "S02", "code": "p", "note": " misses one case "})[0], 200)
        self.assertEqual(call("/label", {"kind": "pairs", "id": "P01", "code": "t"})[0], 200)
        status, raw = call("/label", {"kind": "singles", "id": "S02", "code": "n"})  # an answer can be changed
        self.assertEqual(json.loads(raw)["labels"]["singles"]["S02"], {"fixed": "no", "note": ""})
        labels = json.loads((self.work / "labels.json").read_text())
        self.assertEqual(labels, {"singles": {"S02": {"fixed": "no", "note": ""}}, "pairs": {"P01": {"prefer": "tie", "note": ""}}})

        refused = [
            call("/label", {"kind": "singles", "id": "S02", "code": "1"}),  # a pair's answer for a single
            call("/label", {"kind": "singles", "id": "S99", "code": "y"}),
            call("/label", {"kind": "key", "id": "S01", "code": "y"}),
            call("/label", ["S01", "y"]),
            call("/label", {"kind": "singles", "id": "S01", "code": "y"}, content_type="text/plain"),  # a cross-site form
            call("/state", host="attacker.example:%d" % server.server_address[1]),  # DNS rebinding
            call("/label", {"kind": "singles", "id": "S01", "code": "y"}, host="attacker.example"),
        ]
        self.assertEqual([s for s, _ in refused], [400, 400, 400, 400, 400, 403, 403])
        self.assertEqual(json.loads((self.work / "labels.json").read_text()), labels)
        self.assertEqual(call("/key.json")[0], 404)

    def test_analyze_on_the_prepared_items(self):
        jp.prepare(self.data, self.work)
        jp.judge(self.work, fake_claude(self.root), log=lambda *a: None)
        key = json.loads((self.work / "key.json").read_text())
        labels = {"singles": {s: {"fixed": "yes", "note": ""} for s in key["singles"]}, "pairs": {p: {"prefer": "first", "note": ""} for p in key["pairs"]}}
        jp.write_json(self.work / "labels.json", labels)
        text = jp.analyze(self.work, unblinded=set())
        self.assertIn("The judge agrees with you, fixed or not: 5 of 5, 100%", text)  # five passing runs
        self.assertIn("Same verdict on all 3 repeats: 6 of 6", text)
        # The fake always says "first", so the two orders disagree once mapped back: every pair flips, and counts as a tie.
        self.assertIn("Pair order flips the preference: 2 of 2", text)
        self.assertIn("**Secondary score for false passes: INCONCLUSIVE** (you found 0 false passes", text)
        self.assertIn("**Secondary score for quality: INCONCLUSIVE**", text)
        self.assertIn("**Grading tasks without tests (promising, not proven): INCONCLUSIVE**", text)
        # The pair holding an unblinded single's diff leaves the comparison with your pair labels too.
        s_r1 = next(s for s, k in key["singles"].items() if k["run"] == "r1")
        self.assertIn("Pairs where you preferred one change: 1 of 1.", jp.analyze(self.work, unblinded={s_r1}))


def synthetic(work: Path, singles, pairs) -> None:
    """A work folder by hand. singles: (passed, label, [judge answers], empty); pairs: (template, label, fs answer, sf answer)."""
    work.mkdir(parents=True, exist_ok=True)
    items, key, labels, rows = {"singles": [], "pairs": []}, {"singles": {}, "pairs": {}}, {"singles": {}, "pairs": {}}, []
    for n, (passed, label, answers, empty) in enumerate(singles, 1):
        sid = "S%02d" % n
        items["singles"].append({"id": sid, "instruction": "i", "reference": DIFF, "candidate": "" if empty else DIFF})
        key["singles"][sid] = {"run": "r%d" % n, "experiment": "e", "template": "context-ab", "task": "t", "arm": "A", "passed": passed}
        labels["singles"][sid] = {"fixed": label, "note": ""}
        rows += [{"job": "%s#%d" % (sid, r), "item": sid, "kind": "single", "fixed": a, "reason": "", "cost_usd": 0.04}
                 for r, a in enumerate(answers, 1)]
    for n, (template, label, fs, sf) in enumerate(pairs, 1):
        pid = "P%02d" % n
        items["pairs"].append({"id": pid, "instruction": "i", "reference": DIFF, "first": DIFF, "second": DIFF})
        key["pairs"][pid] = {"experiment": "e", "template": template, "task": "t", "first": {"run": "p%da" % n, "arm": "A"},
                             "second": {"run": "p%db" % n, "arm": "B"}}
        labels["pairs"][pid] = {"prefer": label, "note": ""}
        rows += [{"job": pid + "#fs", "item": pid, "kind": "pair", "prefer": fs, "reason": "", "cost_usd": 0.06},
                 {"job": pid + "#sf", "item": pid, "kind": "pair", "prefer": sf, "reason": "", "cost_usd": 0.06}]
    jp.write_json(work / "items.json", items)
    jp.write_json(work / "key.json", key)
    jp.write_json(work / "labels.json", labels)
    (work / "verdicts.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows))


class Verdicts(unittest.TestCase):
    """The go/no-go rules: a judge that adds nothing gets no GO; one that matches your labels does."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.work = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_a_judge_that_approves_everything_gets_no_go(self):
        singles = [(True, "yes", ["yes"] * 3, False)] * 16 + [(True, "no", ["yes"] * 3, False)] * 4 + \
                  [(False, "no", ["yes"] * 3, False)] * 4 + [(False, "no", ["no"] * 3, True)] * 2
        pairs = [("context-ab", "first", "tie", "tie")] * 6 + [("aa", "tie", "tie", "tie")] * 2
        synthetic(self.work, singles, pairs)
        text = jp.analyze(self.work, unblinded=set())
        self.assertIn("- NOT met: catches >= 2/3 of the false passes you found", text)
        self.assertIn("- NOT met: agrees with you more often than \"always fixed\"", text)
        self.assertIn("**Secondary score for false passes: NO-GO**", text)
        self.assertIn("**Secondary score for quality: NO-GO**", text)
        self.assertIn("- NOT met: judges >= 3 of the failing runs with a change not fixed", text)
        self.assertIn("**Grading tasks without tests (promising, not proven): NO-GO**", text)

    def test_a_judge_that_matches_your_labels_gets_go(self):
        singles = [(True, "yes", ["yes"] * 3, False)] * 16 + [(True, "no", ["no"] * 3, False)] * 4 + \
                  [(False, "no", ["no"] * 3, False)] * 4 + [(False, "no", ["no"] * 3, True)] * 2
        pairs = [("context-ab", "first", "first", "second")] * 3 + [("context-ab", "second", "second", "first")] * 3 + \
                [("aa", "tie", "tie", "tie")] * 2
        synthetic(self.work, singles, pairs)
        text = jp.analyze(self.work, unblinded=set())
        self.assertIn("**Secondary score for false passes: GO**", text)
        self.assertIn("**Secondary score for quality: GO**", text)
        self.assertIn("**Grading tasks without tests (promising, not proven): GO**", text)
        self.assertIn("A/A pairs (the same context in both arms): 2 judged; arm A preferred in 0 of the 0 with a preference", text)

    def test_question_3_leaves_out_runs_where_you_and_the_tests_disagree_either_way(self):
        # 10 passing runs judged fixed; 3 failing runs judged not fixed; 1 failing run you call fixed, judged fixed.
        singles = [(True, "yes", ["yes"] * 3, False)] * 10 + [(False, "no", ["no"] * 3, False)] * 3 + [(False, "yes", ["yes"] * 3, False)]
        synthetic(self.work, singles, [])
        text = jp.analyze(self.work, unblinded=set())
        self.assertIn("without the 1 where your label and the tests disagree: 13 of 13, 100%", text)
        self.assertIn("**Grading tasks without tests (promising, not proven): GO**", text)
        # An unblinded single's label does not count: S14 stays in, and the judge is counted wrong on it.
        self.assertIn("without the 0 where your label and the tests disagree: 13 of 14", jp.analyze(self.work, unblinded={"S14"}))

    def test_a_judge_that_prefers_one_arm_fails_the_a_a_check(self):
        pairs = [("context-ab", "first", "first", "second")] * 6 + [("aa", "tie", "first", "second")] * 7
        synthetic(self.work, [(True, "yes", ["yes"] * 3, False)], pairs)
        text = jp.analyze(self.work, unblinded=set())
        self.assertIn("arm A preferred in 7 of the 7 with a preference", text)
        self.assertIn("- NOT met: no significant arm preference on A/A pairs (p >= 0.05)", text)


if __name__ == "__main__":
    unittest.main()
