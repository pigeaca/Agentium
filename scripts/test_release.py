"""Release tooling regressions; fixture repositories live in temporary directories and never touch the real checkout."""
from __future__ import annotations
from pathlib import Path
import contextlib
import io
import json
import os
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import harness
import release

GIT_ENV = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "t",
           "GIT_COMMITTER_EMAIL": "t@example.com", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null"}

CLI_GO = '''package cli

const (
	ExitOK = 0
	ExitUsage = 2
)

func dispatch(command string) {
	switch command {
	case "start":
	case "clean":
	}
}

func runClean() {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "remove")
	older := fs.String("older-than", "30d", "age")
}
'''
JSON_GO = '''package cli

type runJSON struct {
	ID string `json:"id"`
	Cost float64 `json:"cost_usd"`
}
'''
DESIGN_GO = '''package experiment

const (
	DesignVersion = 1
	DesignVersionSeq = 3
)

const MethodVersion = MethodV2
'''


class Fixture:
    """A repository whose main branch gets PR merges, shaped like GitHub's 'Merge pull request #N' commits."""

    def __init__(self, test: unittest.TestCase, files: dict[str, str] | None = None):
        self.tmp = tempfile.TemporaryDirectory()
        test.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.git("init", "-q", "-b", "main")
        self.write(files or {"README.md": "x\n"})
        self.git("add", "-A")
        self.git("commit", "-qm", "initial")
        self.number = 0

    def git(self, *args: str) -> str:
        return subprocess.run(["git", "-C", str(self.root), *args], env=GIT_ENV, check=True, capture_output=True, text=True).stdout.strip()

    def write(self, files: dict[str, str | None]) -> None:
        for name, text in files.items():
            path = self.root / name
            if text is None:
                path.unlink()
            else:
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text)

    def pr(self, title: str, files: dict[str, str | None] | None = None, body: str = "") -> int:
        self.number += 1
        branch = f"pr-{self.number}"
        self.git("checkout", "-q", "-b", branch)
        self.write(files or {f"notes/{self.number}.md": "n\n"})
        self.git("add", "-A")
        self.git("commit", "-qm", title, "--no-verify")
        self.git("checkout", "-q", "main")
        message = [f"Merge pull request #{self.number} from o/{branch}", title] + ([body] if body else [])
        self.git("merge", "-q", "--no-ff", branch, *sum((["-m", part] for part in message), []))
        return self.number

    def tag(self, name: str) -> None:
        self.git("tag", "-a", name, "-m", name)


class VersionComputation(unittest.TestCase):
    def test_before_one_point_zero_a_feature_or_breaking_change_bumps_minor_and_a_fix_bumps_patch(self):
        for level, want in [(0, None), (1, (0, 1, 1)), (2, (0, 2, 0)), (3, (0, 2, 0))]:
            self.assertEqual(release.next_version((0, 1, 0), level), want, level)
        self.assertEqual(release.next_version((0, 3, 4), 2), (0, 4, 0))
        self.assertEqual(release.next_version((0, 3, 4), 1), (0, 3, 5))

    def test_from_one_point_zero_the_levels_are_semver(self):
        for level, want in [(1, (1, 2, 4)), (2, (1, 3, 0)), (3, (2, 0, 0))]:
            self.assertEqual(release.next_version((1, 2, 3), level), want, level)

    def test_the_first_release_is_v0_1_0_whatever_the_level(self):
        for level in (1, 2, 3):
            self.assertEqual(release.next_version(None, level), (0, 1, 0))
        self.assertIsNone(release.next_version(None, 0))

    def test_tags_parse_and_the_highest_reachable_one_wins(self):
        self.assertEqual(release.parse_tag("v1.10.2"), (1, 10, 2))
        self.assertIsNone(release.parse_tag("v1.2"))
        self.assertIsNone(release.parse_tag("release-1"))


class Classification(unittest.TestCase):
    def test_titles_and_bodies(self):
        cases = [("feat(cli): add x", "", "feature"), ("fix: y", "", "fix"), ("perf(stats): z", "", "fix"),
                 ("feat(cli)!: drop x", "", "breaking"), ("fix!: y", "", "breaking"),
                 ("fix: y", "Why.\nBreaking: run `agentium migrate`", "breaking"), ("fix: y", "Not Breaking: here", "fix"),
                 ("docs: x", "", "other"), ("refactor(cli): x", "", "other"), ("chore: x", "", "other"),
                 ("test: x", "", "other"), ("ci: x", "", "other"), ("Update things", "", "other"),
                 ("docs: x", "Breaking: yes", "breaking")]
        for title, body, want in cases:
            self.assertEqual(release.classify(title, body), want, (title, body))


class ContractDetection(unittest.TestCase):
    def setUp(self):
        self.make()

    def make(self):
        self.fx = Fixture(self, {"internal/store/migrations/0001_a.sql": "create table a(x);\n", "internal/cli/cli.go": CLI_GO,
                                 "internal/cli/json_run.go": JSON_GO, "internal/experiment/design.go": DESIGN_GO,
                                 "internal/report/testdata/r.json": json.dumps({"verdict": "x", "arms": [{"name": "a"}]})})
        self.fx.tag("v0.1.0")

    def detect(self, files):
        self.make()  # each call starts from the tagged state
        self.fx.pr("feat: change", files)
        return {(c["kind"], c["text"]) for c in release.detect_contract(self.fx.root, "v0.1.0", "HEAD")}

    def test_nothing_changed_nothing_detected(self):
        self.assertEqual(self.detect({"docs/a.md": "hi\n"}), set())
        self.assertEqual(release.detect_contract(self.fx.root, None, "HEAD"), [])

    def test_a_new_migration_is_additive_and_a_changed_or_deleted_one_is_breaking(self):
        self.assertEqual(self.detect({"internal/store/migrations/0002_b.sql": "alter table a add y;\n"}),
                         {("additive", "new migration 0002_b.sql (applied automatically, forward-only)")})
        self.assertEqual(self.detect({"internal/store/migrations/0001_a.sql": "create table a(x, y);\n"}),
                         {("breaking", "changed migration 0001_a.sql")})
        self.assertEqual(self.detect({"internal/store/migrations/0001_a.sql": None}), {("breaking", "deleted migration 0001_a.sql")})

    def test_json_keys_in_goldens_and_struct_tags(self):
        got = self.detect({"internal/report/testdata/r.json": json.dumps({"verdict": "x", "arms": [{"name": "a", "cost": 1}], "extra": 1})})
        self.assertEqual(got, {("additive", "JSON key .arms[].cost added (internal/report/testdata/r.json)"),
                               ("additive", "JSON key .extra added (internal/report/testdata/r.json)")})
        got = self.detect({"internal/report/testdata/r.json": json.dumps({"verdict": "y", "arms": [{}], "extra": 1})})
        self.assertEqual(got, {("breaking", "JSON key .arms[].name removed (internal/report/testdata/r.json)"),
                               ("additive", "JSON key .extra added (internal/report/testdata/r.json)")})
        got = self.detect({"internal/cli/json_run.go": JSON_GO.replace('`json:"cost_usd"`', '`json:"cost"`')})
        self.assertEqual(got, {("breaking", "json runJSON.cost_usd removed"), ("additive", "json runJSON.cost added")})

    def test_removed_flags_and_commands_are_breaking_and_added_ones_additive(self):
        got = self.detect({"internal/cli/cli.go": CLI_GO.replace('\tyes := fs.Bool("yes", false, "remove")\n', "")
                           .replace('\tcase "clean":\n', '\tcase "wipe":\n')})
        self.assertEqual(got, {("breaking", "flag internal/cli/cli.go:clean --yes removed"),
                               ("breaking", "command internal/cli/cli.go: clean removed"),
                               ("additive", "command internal/cli/cli.go: wipe added")})
        got = self.detect({"internal/cli/cli.go": CLI_GO.replace('\tolder :=', '\tkeep := fs.Int("keep", 1, "n")\n\tolder :=')})
        self.assertEqual(got, {("additive", "flag internal/cli/cli.go:clean --keep added")})

    def test_design_method_and_exit_constants(self):
        got = self.detect({"internal/experiment/design.go": DESIGN_GO.replace("\tDesignVersionSeq = 3\n", "\tDesignVersionSeq = 3\n\tDesignVersionX = 6\n")
                           .replace("MethodV2", "MethodSeq")})
        self.assertEqual(got, {("additive", "constant DesignVersionX added"), ("additive", "constant MethodVersion changed: MethodV2 -> MethodSeq")})
        got = self.detect({"internal/experiment/design.go": DESIGN_GO.replace("DesignVersionSeq = 3", "DesignVersionSeq = 7")})
        self.assertEqual(got, {("breaking", "constant DesignVersionSeq changed: 3 -> 7")})
        got = self.detect({"internal/cli/cli.go": CLI_GO.replace("ExitUsage = 2", "ExitUsage = 3")})
        self.assertEqual(got, {("breaking", "constant ExitUsage changed: 2 -> 3")})
        # Tests and unrelated Go files are not the contract.
        self.assertEqual(self.detect({"internal/cli/cli_test.go": CLI_GO, "internal/other/x.go": 'package x\nconst DesignVersion = 9\n'}), set())


class Planning(unittest.TestCase):
    def plan(self, fx):
        with patch.object(harness, "github_repo", return_value="o/r"):
            return release.make_plan(fx.root, "HEAD", lookup={})

    def test_without_a_tag_the_first_release_is_v0_1_0_and_lists_everything(self):
        fx = Fixture(self)
        fx.pr("docs: a")
        number = fx.pr("feat(cli): add x")
        plan = self.plan(fx)
        self.assertEqual((plan["next_version"], plan["first"], plan["last_tag"]), ("v0.1.0", True, None))
        self.assertEqual([pr["kind"] for pr in plan["prs"]], ["feature", "other", "other"])  # newest first; the initial commit is the last
        self.assertIn(f"- feat(cli): add x ([#{number}](https://github.com/o/r/pull/{number}))", plan["notes"])

    def test_declared_bumps_and_the_notes_groups(self):
        fx = Fixture(self)
        fx.tag("v1.2.3")
        for title, body, want in [("docs: a", "", None), ("fix(x): y", "", "v1.2.4"), ("feat(x): y", "", "v1.3.0"),
                                  ("feat(x)!: y", "Breaking: do z", "v2.0.0")]:
            fx.pr(title, body=body)
            plan = self.plan(fx)
            self.assertEqual(plan["next_version"], want, title)
        self.assertEqual(plan["warnings"], [])
        for heading in ("## Breaking", "## Features", "## Fixes"):
            self.assertIn(heading, plan["notes"])
        self.assertIn("1 other change(s)", plan["notes"])
        self.assertNotIn("## Contract changes", plan["notes"])

    def test_pre_one_point_zero_rules_apply_to_the_computed_bump(self):
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("fix: y")
        self.assertEqual(self.plan(fx)["next_version"], "v0.1.1")
        fx.pr("feat(x)!: y", body="Breaking: z")
        self.assertEqual(self.plan(fx)["next_version"], "v0.2.0")

    def test_nothing_releasable(self):
        fx = Fixture(self)
        fx.tag("v0.1.0")
        for title in ("docs: a", "test: b", "chore: c", "ci: d", "refactor: e"):
            fx.pr(title)
        plan = self.plan(fx)
        self.assertEqual((plan["due"], plan["next_version"]), (False, None))
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            release.print_plan(plan)
        self.assertIn("Nothing releasable", out.getvalue())

    def test_the_bump_is_the_larger_of_declared_and_detected_never_lower(self):
        files = {"internal/cli/cli.go": CLI_GO}
        fx = Fixture(self, files)
        fx.tag("v1.0.0")
        fx.pr("fix(cli): tidy", {"internal/cli/cli.go": CLI_GO.replace('\tcase "clean":\n', "")})  # a removed command under a fix title
        plan = self.plan(fx)
        self.assertEqual((plan["next_version"], plan["declared"], plan["detected"]), ("v2.0.0", "patch", "major"))
        self.assertIn("no PR declares", plan["warnings"][0])
        self.assertIn("## Contract changes\n- breaking: command internal/cli/cli.go: clean removed", plan["notes"])
        fx2 = Fixture(self, {"internal/store/migrations/0001_a.sql": "x\n"})
        fx2.tag("v1.0.0")
        fx2.pr("refactor: move", {"internal/store/migrations/0002_b.sql": "y\n"})  # additive under a refactor title: still a release
        plan = self.plan(fx2)
        self.assertEqual((plan["next_version"], plan["warnings"]), ("v1.1.0", []))

    def test_the_declared_breaking_pr_with_a_detected_breaking_change_has_no_warning(self):
        fx = Fixture(self, {"internal/cli/cli.go": CLI_GO})
        fx.tag("v1.0.0")
        fx.pr("feat(cli)!: drop clean", {"internal/cli/cli.go": CLI_GO.replace('\tcase "clean":\n', "")}, body="Breaking: use wipe")
        plan = self.plan(fx)
        self.assertEqual((plan["next_version"], plan["warnings"]), ("v2.0.0", []))

    def test_github_titles_win_and_commit_text_is_the_fallback(self):
        fx = Fixture(self)
        fx.tag("v0.1.0")
        number = fx.pr("docs: from the commit")
        lookup = {number: {"number": number, "title": "fix(x): from github", "body": "", "url": "https://example.com/pr"}}
        with patch.object(harness, "github_repo", return_value="o/r"):
            plan = release.make_plan(fx.root, "HEAD", lookup=lookup)
        self.assertEqual((plan["prs"][0]["title"], plan["prs"][0]["url"], plan["next_version"]), ("fix(x): from github", "https://example.com/pr", "v0.1.1"))
        failed = subprocess.CompletedProcess(["gh"], 1, stdout="", stderr="no auth")
        with patch.object(harness.subprocess, "run", return_value=failed), patch.object(harness, "github_repo", return_value="o/r"):
            self.assertEqual(release.github_prs(), {})

    def test_plan_json_is_valid(self):
        fx = Fixture(self)
        fx.pr("feat: x")
        with patch.object(harness, "ROOT", fx.root), patch.object(harness, "github_repo", return_value="o/r"), \
                patch.object(release, "github_prs", return_value={}), contextlib.redirect_stdout(io.StringIO()) as out:
            release.release_command(["plan", "--json"])
        self.assertEqual(json.loads(out.getvalue())["next_version"], "v0.1.0")
        with self.assertRaisesRegex(ValueError, "Usage: release plan"):
            release.release_command(["plan", "--x"])
        with self.assertRaisesRegex(ValueError, "Usage: release plan"):
            release.release_command(["nope"])


class Cutting(unittest.TestCase):
    def setUp(self):
        self.fx = Fixture(self)
        self.fx.tag("v0.1.0")
        self.fx.pr("feat(x): y")
        self.set_ref()
        self.ci = ("success", "CI passed: https://example/run")

    def set_ref(self):
        self.fx.git("update-ref", "refs/remotes/origin/main", "HEAD")

    def cut(self, fetched=True, **kwargs):
        out = io.StringIO()
        with patch.object(harness, "ROOT", self.fx.root), patch.object(harness, "github_repo", return_value="o/r"), \
                patch.object(harness, "fetch_default", return_value=("origin/main", fetched)), patch.object(release, "sync_tags"), \
                patch.object(release, "github_prs", return_value={}), patch.object(release, "commit_ci", side_effect=lambda *a: self.ci), \
                contextlib.redirect_stdout(out):
            try:
                release.release_cut(**kwargs)
                error = None
            except ValueError as raised:
                error = str(raised)
        return error, out.getvalue()

    def test_dry_run_prints_every_step_and_changes_nothing(self):
        error, out = self.cut(dry_run=True)
        self.assertIsNone(error)
        self.assertIn("Next version: v0.2.0", out)
        self.assertIn("git tag -a v0.2.0 -F <notes>", out)
        self.assertIn("git push https://github.com/o/r.git refs/tags/v0.2.0", out)
        self.assertIn("gh release create v0.2.0 --repo o/r", out)
        self.assertIn("Nothing was tagged, pushed or created.", out)
        self.assertEqual(self.fx.git("tag", "--list", "v0.2.0"), "")

    def test_refusals(self):
        (self.fx.root / "dirty.txt").write_text("x")
        error, _ = self.cut(dry_run=True)
        self.assertIn("working tree is not clean", error)
        (self.fx.root / "dirty.txt").unlink()
        self.assertIn("could not fetch origin/main", self.cut(fetched=False, dry_run=True)[0])
        self.fx.pr("fix: later")  # HEAD is ahead of the fetched ref
        self.assertIn("HEAD is not the freshly fetched origin/main", self.cut(dry_run=True)[0])
        self.set_ref()
        self.ci = ("failure", "CI failure: https://example/run")
        self.assertIn("CI failure", self.cut(dry_run=True)[0])
        self.ci = ("success", "ok")
        # A v0.2.0 tag that is not an ancestor of HEAD does not count as a release, so the plan still proposes it.
        self.fx.git("checkout", "-q", "--orphan", "other")
        self.fx.git("commit", "-q", "--allow-empty", "-m", "orphan")
        self.fx.tag("v0.2.0")
        self.fx.git("checkout", "-q", "-f", "main")
        self.assertIn("tag v0.2.0 already exists", self.cut(dry_run=True)[0])

    def test_nothing_to_release(self):
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("docs: x")
        fx.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.fx = fx
        self.assertIn("Nothing to release", self.cut(dry_run=True)[0])

    def test_the_first_release_needs_first_and_prints_the_checklist(self):
        fx = Fixture(self)
        fx.pr("feat: x")
        fx.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.fx = fx
        error, _ = self.cut(dry_run=True)
        self.assertIn("must be cut explicitly with --first", error)
        self.assertIn("[ ] the sandbox real check passed", error)
        error, out = self.cut(dry_run=True, first=True)
        self.assertIsNone(error)
        self.assertIn("Next version: v0.1.0", out)
        self.assertIn("[ ] `agentium version` works", out)
        self.assertIn("--first is only for the first release", self.cut_with_tag_first())

    def cut_with_tag_first(self):
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("feat: x")
        fx.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.fx = fx
        return self.cut(dry_run=True, first=True)[0]

    def test_pending_ci_times_out_and_names_local_checks(self):
        self.ci = ("pending", "no CI run yet")
        clock, sleeps = iter(range(0, 100000, 600)).__next__, []
        error, _ = self.cut(timeout_minutes=20, sleep=sleeps.append, clock=clock)
        self.assertIn("still pending after 20 min", error)
        self.assertIn("--local-checks", error)
        self.assertTrue(sleeps)
        self.assertEqual(self.fx.git("tag", "--list", "v0.2.0"), "")

    def test_local_checks_say_why_and_a_dry_run_runs_nothing(self):
        with patch.object(harness, "run") as run, patch.object(harness, "go_binary", return_value=Path("/go")):
            error, out = self.cut(dry_run=True, use_local_checks=True)
        self.assertIsNone(error)
        self.assertIn("GitHub Actions currently runs no jobs", out)
        self.assertIn("check ci", out)
        self.assertIn("check vuln", out)
        run.assert_not_called()

    def test_a_failing_local_check_stops_before_tagging(self):
        with patch.object(harness, "run", side_effect=subprocess.CalledProcessError(1, ["x"])), \
                patch.object(harness, "go_binary", return_value=Path("/go")):
            error, _ = self.cut(use_local_checks=True)
        self.assertIn("Refusing to release", error)
        self.assertEqual(self.fx.git("tag", "--list", "v0.2.0"), "")

    def test_a_real_cut_tags_pushes_and_creates_the_release(self):
        calls = []
        real_run = subprocess.run

        def fake_run(command, *args, **kwargs):
            if command[:4] == ["git", "-C", str(self.fx.root), "push"]:
                calls.append(command)
                return subprocess.CompletedProcess(command, 0, b"", b"")
            return real_run(command, *args, **kwargs)

        def fake_gh(*args):
            calls.append(("gh", *args))
            return "https://github.com/o/r/releases/tag/v0.2.0\n"

        with patch.object(release.subprocess, "run", fake_run), patch.object(harness, "gh", fake_gh):
            error, out = self.cut()
        self.assertIsNone(error)
        self.assertEqual(self.fx.git("tag", "--list", "v0.2.0"), "v0.2.0")
        self.assertIn("feat(x): y", self.fx.git("tag", "-n99", "--list", "v0.2.0"))
        self.assertEqual(self.fx.git("cat-file", "-t", "v0.2.0"), "tag")  # annotated
        self.assertEqual(calls[0][-2:], ["https://github.com/o/r.git", "refs/tags/v0.2.0"])
        self.assertEqual(calls[1][:5], ("gh", "release", "create", "v0.2.0", "--repo"))
        self.assertIn("--notes-file", calls[1])
        self.assertIn("Released v0.2.0: https://github.com/o/r/releases/tag/v0.2.0", out)
        self.assertEqual(self.fx.git("rev-list", "--count", "HEAD"), self.fx.git("rev-list", "--count", "origin/main"))  # no commit to main

    def test_usage(self):
        with self.assertRaisesRegex(ValueError, "Usage: release cut"):
            release.cut_command(["--force"])


class AfterMerge(unittest.TestCase):
    def run_after(self, fx, fetched=True):
        fx.git("update-ref", "refs/remotes/origin/main", "HEAD")
        out = io.StringIO()
        with patch.object(harness, "ROOT", fx.root), patch.object(harness, "github_repo", return_value="o/r"), \
                patch.object(harness, "fetch_default", return_value=("origin/main", fetched)), patch.object(release, "sync_tags"), \
                patch.object(release, "github_prs", return_value={}), patch.object(release, "release_cut") as cut, \
                contextlib.redirect_stdout(out):
            release.after_merge(40)
        return cut, out.getvalue()

    def test_cuts_when_due_after_a_first_release(self):
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("fix: y")
        cut, out = self.run_after(fx)
        cut.assert_called_once_with(timeout_minutes=40)
        self.assertIn("v0.1.1 is due", out)

    def test_never_cuts_the_first_release_and_skips_when_nothing_is_due(self):
        fx = Fixture(self)
        fx.pr("feat: y")
        cut, out = self.run_after(fx)
        cut.assert_not_called()
        self.assertIn("first release is cut explicitly", out)
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("docs: y")
        cut, out = self.run_after(fx)
        cut.assert_not_called()
        self.assertIn("nothing releasable", out)


class ContractRefusal(unittest.TestCase):
    def test_declarations(self):
        pr = {"number": 3, "title": "feat(cli)!: x", "body": "Breaking: do y"}
        breaking, additive = [{"kind": "breaking", "text": "b"}], [{"kind": "additive", "text": "a"}]
        self.assertIsNone(release.contract_refusal(pr, breaking))
        self.assertIsNone(release.contract_refusal({**pr, "title": "docs: x"}, []))
        self.assertIn("`Breaking:` line", release.contract_refusal({**pr, "body": ""}, breaking))
        self.assertIn("need a `!`", release.contract_refusal({**pr, "title": "feat: x"}, breaking))
        self.assertIsNone(release.contract_refusal({**pr, "title": "fix: x", "body": ""}, additive))
        self.assertIn("feat or fix title", release.contract_refusal({**pr, "title": "chore: x", "body": ""}, additive))


if __name__ == "__main__":
    unittest.main()
