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

GOLDEN = "internal/cli/testdata/contract.golden"


BASE = "\n".join(sorted([
    "command clean\t", 'flag clean --yes\t*flag.boolValue default="false"', "exit ExitUsage\t2",
    "json context lint .no_comparison\tstring", "migration 0001_a.sql\taa", "experiment MethodVersion\tMethodV2",
    "experiment DesignVersionSeq\t3"])) + "\n"


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

    def tag(self, name: str, marked: bool = False) -> None:
        """An annotated tag; `marked` ones carry the marker release cut writes (the tool's own tags)."""
        self.git("tag", "-a", name, "-m", name + (f"\n\n{release.TAG_MARKER}" if marked else ""))


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
        self.fx = Fixture(self, {GOLDEN: BASE, "internal/cli/x.go": "package cli\n"})
        self.fx.tag("v0.1.0")

    def detect(self, text, extra=None):
        self.make()  # each call starts from the tagged state
        self.fx.pr("feat: change", {GOLDEN: text, **(extra or {})})
        return {(c["kind"], c["text"]) for c in release.detect_contract(self.fx.root, "v0.1.0", "HEAD")}

    def test_an_unchanged_golden_detects_nothing_whatever_else_moved(self):
        # Renames and moved files change Go sources, not the golden.
        self.assertEqual(self.detect(BASE, {"internal/cli/y.go": "package cli\n", "internal/cli/x.go": None}), set())
        self.assertEqual(release.detect_contract(self.fx.root, None, "HEAD"), [])
        self.assertEqual(release.detect_contract(self.fx.root, "v0.1.0", "HEAD"), [])

    def test_a_removed_constant_named_flag_and_a_removed_lint_key_are_breaking(self):
        got = self.detect(BASE.replace('flag clean --yes\t*flag.boolValue default="false"\n', "")
                          .replace("json context lint .no_comparison\tstring\n", ""))
        self.assertEqual(got, {("breaking", "flag clean --yes removed"), ("breaking", "json context lint .no_comparison removed")})

    def test_changed_values_are_breaking_and_additions_additive(self):
        got = self.detect(BASE.replace("ExitUsage\t2", "ExitUsage\t3").replace("DesignVersionSeq\t3", "DesignVersionSeq\t7")
                          .replace("0001_a.sql\taa", "0001_a.sql\tbb") + "migration 0002_b.sql\tcc\ncommand wipe\t\n")
        self.assertEqual(got, {("breaking", "exit ExitUsage changed: 2 -> 3"), ("breaking", "experiment DesignVersionSeq changed: 3 -> 7"),
                               ("breaking", "migration 0001_a.sql changed: aa -> bb"), ("additive", "migration 0002_b.sql added"),
                               ("additive", "command wipe added")})

    def test_a_new_default_method_is_additive(self):
        got = self.detect(BASE.replace("MethodV2", "MethodSeq"))
        self.assertEqual(got, {("additive", "experiment MethodVersion changed: MethodV2 -> MethodSeq")})

    def test_a_missing_golden_is_breaking_and_a_base_without_one_detects_nothing(self):
        self.assertEqual({c["kind"] for c in release.detect_contract(self.fx.root, "v0.1.0", "HEAD")}, set())
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("feat: x", {GOLDEN: BASE})
        self.assertEqual(release.detect_contract(fx.root, "v0.1.0", "HEAD"), [])  # no golden at the tag
        self.make()
        self.fx.pr("feat: x", {GOLDEN: None})
        self.assertEqual([c["kind"] for c in release.detect_contract(self.fx.root, "v0.1.0", "HEAD")], ["breaking"])


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
        fx = Fixture(self, {GOLDEN: BASE})
        fx.tag("v1.0.0")
        fx.pr("fix(cli): tidy", {GOLDEN: BASE.replace("command clean\t\n", "")})  # a removed command under a fix title
        plan = self.plan(fx)
        self.assertEqual((plan["next_version"], plan["declared"], plan["detected"]), ("v2.0.0", "patch", "major"))
        self.assertIn("no PR declares", plan["warnings"][0])
        self.assertIn("## Contract changes\n- breaking: command clean removed", plan["notes"])
        fx2 = Fixture(self, {GOLDEN: BASE})
        fx2.tag("v1.0.0")
        fx2.pr("refactor: move", {GOLDEN: BASE + "migration 0002_b.sql\tcc\n"})  # additive under a refactor title: still a release
        plan = self.plan(fx2)
        self.assertEqual((plan["next_version"], plan["warnings"]), ("v1.1.0", []))

    def test_the_declared_breaking_pr_with_a_detected_breaking_change_has_no_warning(self):
        fx = Fixture(self, {GOLDEN: BASE})
        fx.tag("v1.0.0")
        fx.pr("feat(cli)!: drop clean", {GOLDEN: BASE.replace("command clean\t\n", "")}, body="Breaking: use wipe")
        plan = self.plan(fx)
        self.assertEqual((plan["next_version"], plan["warnings"]), ("v2.0.0", []))

    def test_a_contract_override_is_shown_in_the_notes(self):
        fx = Fixture(self, {GOLDEN: BASE})
        fx.tag("v1.0.0")
        fx.pr("fix: x", {GOLDEN: BASE.replace("command clean\t\n", "")}, body="Contract: none - the command was never released")
        plan = self.plan(fx)
        self.assertEqual(plan["overrides"][0]["reason"], "the command was never released")
        self.assertIn("## Contract overrides", plan["notes"])
        self.assertIn("never released", plan["notes"])

    def test_only_published_tags_count_as_released(self):
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("fix: a")
        fx.tag("v0.1.1")  # local only
        fx.pr("feat: b")
        with patch.object(harness, "github_repo", return_value="o/r"):
            plan = release.make_plan(fx.root, "HEAD", lookup={}, released={"v0.1.0"})
        self.assertEqual((plan["last_tag"], plan["next_version"]), ("v0.1.0", "v0.2.0"))
        self.assertEqual(len(plan["prs"]), 2)  # v0.1.1's change is not skipped

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
                patch.object(release, "github_prs", return_value={}), patch.object(release, "published_tags", return_value=None), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            release.release_command(["plan", "--json"])
        self.assertEqual(json.loads(out.getvalue())["next_version"], "v0.1.0")
        with self.assertRaisesRegex(ValueError, "Usage: release plan"):
            release.release_command(["plan", "--x"])
        with self.assertRaisesRegex(ValueError, "Usage: release plan"):
            release.release_command(["nope"])


class Cutting(unittest.TestCase):
    """release_cut against a fixture repository and a scripted remote: `remote` holds the published tags, `releases` the
    tags that have a GitHub Release, `calls` what was pushed or created."""

    def setUp(self):
        self.fx = Fixture(self)
        self.fx.tag("v0.1.0")
        self.fx.pr("feat(x): y")
        self.set_ref()
        self.ci = ("success", "CI passed: https://example/run")
        self.remote, self.releases, self.calls = {"v0.1.0"}, {"v0.1.0"}, []
        self.push_fails, self.create_error = False, None
        self.created = []

    def set_ref(self):
        self.fx.git("update-ref", "refs/remotes/origin/main", "HEAD")

    def cut(self, fetched=True, **kwargs):
        out = io.StringIO()
        real_run = subprocess.run

        def fake_run(command, *args, **kw):
            if command[:4] == ["git", "-C", str(self.fx.root), "push"]:
                self.calls.append(command)
                if self.push_fails:
                    return subprocess.CompletedProcess(command, 1, "", "denied")
                self.remote.add(command[-1].removeprefix("refs/tags/"))
                return subprocess.CompletedProcess(command, 0, "", "")
            return real_run(command, *args, **kw)

        def create(repo, tag, text):
            if self.create_error:
                raise ValueError(self.create_error)
            self.created.append((tag, text))
            self.releases.add(tag)
            return f"https://github.com/o/r/releases/tag/{tag}"

        with patch.object(harness, "github_repo", return_value="o/r"), patch.object(release.subprocess, "run", fake_run), \
                patch.object(release, "remote_names", return_value=("origin", "https://github.com/o/r.git")), \
                patch.object(harness, "fetch_default", return_value=("origin/main", fetched)), patch.object(release, "sync_tags"), \
                patch.object(release, "remote_tags", side_effect=lambda: {t: "x" for t in self.remote}), \
                patch.object(release, "has_github_release", side_effect=lambda repo, tag: tag in self.releases), \
                patch.object(release, "create_github_release", side_effect=create), \
                patch.object(harness, "ROOT", self.fx.root), patch.object(release, "github_prs", return_value={}), \
                patch.object(release, "commit_ci", side_effect=lambda *a: self.ci), contextlib.redirect_stdout(out):
            try:
                release.release_cut(**kwargs)
                error = None
            except ValueError as raised:
                error = str(raised)
        return error, out.getvalue()

    def test_dry_run_prints_every_step_and_changes_nothing(self):
        (self.fx.root / "dirty.txt").write_text("x")  # a dirty tree does not matter: the tag goes on the fetched commit
        error, out = self.cut(dry_run=True)
        self.assertIsNone(error)
        self.assertIn("Next version: v0.2.0", out)
        self.assertIn("git tag -a v0.2.0 --cleanup=verbatim -F <notes>", out)
        self.assertIn("git push https://github.com/o/r.git refs/tags/v0.2.0", out)
        self.assertIn("gh release create v0.2.0 --repo o/r", out)
        self.assertIn("Nothing was tagged, pushed or created.", out)
        self.assertEqual((self.fx.git("tag", "--list", "v0.2.0"), self.calls, self.created), ("", [], []))

    def test_refusals(self):
        self.assertIn("could not fetch origin/main", self.cut(fetched=False, dry_run=True)[0])
        self.ci = ("failure", "CI failure: https://example/run")
        self.assertIn("CI failure", self.cut(dry_run=True)[0])
        self.ci = ("success", "ok")
        self.remote.add("v0.2.0")  # published elsewhere, though this checkout has no such tag
        self.releases.add("v0.2.0")
        error, _ = self.cut(dry_run=True)
        self.assertIn("v0.2.0", error)  # the plan sees it as released: the next change is a new version or nothing

    def test_a_cancelled_main_run_refuses_the_release(self):
        self.ci = ("failure", "CI cancelled: https://example/run")
        error, _ = self.cut()
        self.assertIn("CI cancelled", error)
        self.assertEqual(self.calls, [])

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
        self.fx, self.remote, self.releases = fx, set(), set()
        error, _ = self.cut(dry_run=True)
        self.assertIn("must be cut explicitly with --first", error)
        self.assertIn("[ ] the sandbox real check passed", error)
        error, out = self.cut(dry_run=True, first=True)
        self.assertIsNone(error)
        self.assertIn("Next version: v0.1.0", out)
        self.assertIn("[ ] `agentium version` works", out)
        self.assertIn("is public", out)
        self.remote = {"v0.1.0"}
        fx.tag("v0.1.0")
        fx.pr("feat: z")
        fx.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.assertIn("--first is only for the first release", self.cut(dry_run=True, first=True)[0])

    def test_pending_ci_times_out_and_names_local_checks(self):
        self.ci = ("pending", "no CI run yet")
        clock, sleeps = iter(range(0, 100000, 600)).__next__, []
        error, _ = self.cut(timeout_minutes=20, sleep=sleeps.append, clock=clock)
        self.assertIn("still pending after 20 min", error)
        self.assertIn("--local-checks", error)
        self.assertTrue(sleeps)
        self.assertEqual(self.fx.git("tag", "--list", "v0.2.0"), "")

    def test_local_checks_are_neutral_read_ci_and_a_dry_run_runs_nothing(self):
        with patch.object(harness, "run") as run, patch.object(harness, "go_binary", return_value=Path("/go")):
            error, out = self.cut(dry_run=True, use_local_checks=True)
            self.assertIsNone(error)
            self.assertIn("instead of waiting for GitHub Actions", out)
            self.assertNotIn("currently runs no jobs", out)
            self.assertIn("check ci", out)
            self.assertIn("check vuln", out)
            self.ci = ("failure", "CI failure: https://example/run")  # a red run still refuses; no runs at all does not
            self.assertIn("CI failure", self.cut(use_local_checks=True)[0])
            self.ci = ("pending", "no CI run yet")
            self.assertIsNone(self.cut(dry_run=True, use_local_checks=True)[0])
        run.assert_not_called()

    def test_local_checks_run_in_a_detached_worktree_at_the_commit_and_remove_it_even_on_failure(self):
        seen = []

        def fake_run(*command, cwd=None):
            seen.append((command, Path(cwd), (Path(cwd) / "README.md").exists(),
                         self.fx.git("-C", str(cwd), "rev-parse", "HEAD")))
            if len(seen) == 2:
                raise subprocess.CalledProcessError(1, command)

        head = self.fx.git("rev-parse", "HEAD")
        branch_before = self.fx.git("branch", "--show-current")
        with patch.object(harness, "run", fake_run), patch.object(harness, "go_binary", return_value=Path("/go")):
            error, _ = self.cut(use_local_checks=True)
        self.assertIn("Refusing to release", error)
        self.assertEqual(len(seen), 2)
        self.assertTrue(all(tree != self.fx.root and exists and sha == head for _, tree, exists, sha in seen))
        self.assertFalse(seen[0][1].exists())  # removed
        self.assertNotIn(str(seen[0][1]), self.fx.git("worktree", "list"))
        self.assertEqual((self.fx.git("tag", "--list", "v0.2.0"), self.calls), ("", []))
        self.assertEqual((self.fx.git("rev-parse", "HEAD"), self.fx.git("branch", "--show-current")), (head, branch_before))

    def test_a_real_cut_tags_the_fetched_commit_keeps_headings_and_moves_nothing(self):
        # The checkout is on another branch with a dirty tree: nothing about it matters or moves.
        fetched = self.fx.git("rev-parse", "HEAD")
        self.fx.git("checkout", "-q", "-b", "task")
        (self.fx.root / "wip.txt").write_text("x")
        self.fx.git("add", "wip.txt")
        self.fx.git("commit", "-qm", "wip")
        head = self.fx.git("rev-parse", "HEAD")
        error, out = self.cut()
        self.assertIsNone(error)
        self.assertEqual(self.fx.git("rev-parse", "v0.2.0^{commit}"), fetched)
        self.assertEqual(self.fx.git("cat-file", "-t", "v0.2.0"), "tag")  # annotated
        self.assertIn("## Features", self.fx.git("tag", "--list", "--format=%(contents)", "v0.2.0"))  # headings kept
        self.assertEqual(self.calls[0][-2:], ["https://github.com/o/r.git", "refs/tags/v0.2.0"])
        self.assertEqual(self.created[0][0], "v0.2.0")
        self.assertIn("Released v0.2.0: https://github.com/o/r/releases/tag/v0.2.0", out)
        self.assertEqual((self.fx.git("rev-parse", "HEAD"), self.fx.git("branch", "--show-current")), (head, "task"))
        self.assertEqual(self.fx.git("rev-parse", "main"), fetched)  # no local branch moved

    def test_a_failed_push_deletes_the_local_tag_and_a_rerun_releases(self):
        self.push_fails = True
        error, _ = self.cut()
        self.assertIn("pushing the tag failed", error)
        self.assertEqual(self.fx.git("tag", "--list", "v0.2.0"), "")
        self.push_fails = False
        error, out = self.cut()
        self.assertIsNone(error)
        self.assertIn("Released v0.2.0", out)

    def test_a_leftover_local_tag_that_was_never_pushed_is_deleted_and_the_release_is_not_skipped(self):
        self.fx.tag("v0.2.0", marked=True)  # left by a failed push
        error, out = self.cut()
        self.assertIsNone(error)
        self.assertIn("deleted local tag v0.2.0: it was never published", out)
        self.assertEqual(self.created[0][0], "v0.2.0")
        self.assertIn("feat(x): y", self.created[0][1])  # v0.2.0's notes, not skipped into v0.2.1

    def test_a_pushed_tag_without_a_release_gets_one_first_and_the_notes_come_from_the_tag(self):
        self.remote.add("v0.2.0")
        self.fx.tag("v0.2.0")  # pushed, but `gh release create` failed
        self.fx.git("tag", "-d", "v0.2.0")
        (self.fx.root / ".notes").write_text("## Features\n- the notes\n")
        self.fx.git("tag", "-a", "v0.2.0", "--cleanup=verbatim", "-F", ".notes")
        self.fx.pr("docs: later")
        self.set_ref()
        with patch.object(release, "sync_tags"):
            error, out = self.cut()
        self.assertEqual([(t, text.strip()) for t, text in self.created], [("v0.2.0", "## Features\n- the notes")])
        self.assertIn("created the GitHub Release of v0.2.0", out)
        self.assertIn("Nothing to release", error)  # docs only since v0.2.0
        self.assertEqual(self.fx.git("tag", "--list", "v0.2.1"), "")

    def test_a_failed_github_release_leaves_the_pushed_tag_and_the_next_cut_finishes_it(self):
        self.create_error = "gh release create failed: HTTP 502"
        error, _ = self.cut()
        self.assertIn("Tag v0.2.0 is pushed but its GitHub Release was not created", error)
        self.assertIn("next `release cut`", error)
        self.assertIn("v0.2.0", self.remote)
        self.assertNotIn("v0.2.0", self.releases)
        self.create_error = None
        error, out = self.cut()
        self.assertIn("created the GitHub Release of v0.2.0", out)
        self.assertEqual([tag for tag, _ in self.created], ["v0.2.0"])
        self.assertIn("feat(x): y", self.created[0][1])  # the notes come from the tag message
        self.assertIn("Nothing to release", error)  # and v0.2.0 is not released twice or skipped into v0.2.1

    def test_a_hand_made_local_tag_is_never_deleted(self):
        self.fx.tag("v0.3.0")  # no marker: a person made it
        error, out = self.cut()
        self.assertIsNone(error)
        self.assertEqual(self.fx.git("tag", "--list", "v0.3.0"), "v0.3.0")
        self.assertNotIn("deleted local tag", out)

    def test_a_push_error_is_reported_even_when_the_tag_is_already_gone(self):
        self.push_fails = True
        real = release.git

        def vanishing(repo, *args, **kw):  # a concurrent cut removed the tag first
            if args[:2] == ("tag", "-d"):
                real(repo, *args, check=False)
                return real(repo, *args, **kw)
            return real(repo, *args, **kw)

        with patch.object(release, "git", vanishing):
            error, _ = self.cut()
        self.assertIn("pushing the tag failed (denied)", error)
        self.assertNotIn("not found", error)

    def test_an_existing_github_release_counts_as_created(self):
        with patch.object(harness, "gh", side_effect=ValueError("gh release create failed: Release.tag_name already exists")):
            url = release.create_github_release("o/r", "v0.2.0", "n")
        self.assertEqual(url, "https://github.com/o/r/releases/tag/v0.2.0")
        with patch.object(harness, "gh", side_effect=ValueError("gh release create failed: HTTP 502")), self.assertRaises(ValueError):
            release.create_github_release("o/r", "v0.2.0", "n")

    def test_a_dry_run_reconcile_changes_nothing(self):
        self.fx.tag("v0.3.0", marked=True)  # unpublished, made by the tool
        self.remote.add("v0.2.0")  # published without a release
        error, out = self.cut(dry_run=True)
        self.assertIn("dry run: would have deleted local tag v0.3.0", out)
        self.assertIn("dry run: would have created the GitHub Release of v0.2.0", out)
        self.assertEqual((self.fx.git("tag", "--list", "v0.3.0"), self.created), ("v0.3.0", []))

    def test_usage(self):
        with self.assertRaisesRegex(ValueError, "Usage: release cut"):
            release.cut_command(["--force"])


class AfterMerge(unittest.TestCase):
    def run_after(self, fx, remote):
        """after_merge with origin/main where the caller put it; asserts that HEAD and the branch did not move."""
        head, branch = fx.git("rev-parse", "HEAD"), fx.git("branch", "--show-current")
        with patch.object(harness, "ROOT", fx.root), patch.object(harness, "github_repo", return_value="o/r"), \
                patch.object(harness, "fetch_default", return_value=("origin/main", True)), patch.object(release, "sync_tags"), \
                patch.object(release, "remote_tags", return_value={t: "x" for t in remote}), \
                patch.object(release, "has_github_release", return_value=True), patch.object(release, "github_prs", return_value={}), \
                patch.object(release, "release_cut", return_value=("v0.1.1", "https://x/v0.1.1")) as cut_mock, \
                contextlib.redirect_stdout(io.StringIO()):
            outcome = release.after_merge(40)
        self.assertEqual((fx.git("rev-parse", "HEAD"), fx.git("branch", "--show-current")), (head, branch))
        return cut_mock, outcome

    def test_cuts_when_due_from_a_task_worktree_and_moves_no_branch(self):
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("fix: y")
        fx.git("update-ref", "refs/remotes/origin/main", "main")
        fx.git("checkout", "-q", "-b", "task")  # HEAD is a task branch, not the default branch
        fx.git("branch", "-f", "main", "main~1")  # and the local default branch is stale
        main_before = fx.git("rev-parse", "main")
        cut, outcome = self.run_after(fx, {"v0.1.0"})
        cut.assert_called_once_with(timeout_minutes=40)
        self.assertEqual(outcome, "v0.1.1 published https://x/v0.1.1")
        self.assertEqual(fx.git("rev-parse", "main"), main_before)  # no fast-forward

    def test_never_cuts_the_first_release_and_says_nothing_to_release_when_nothing_is_due(self):
        fx = Fixture(self)
        fx.pr("feat: y")
        fx.git("update-ref", "refs/remotes/origin/main", "HEAD")
        cut, outcome = self.run_after(fx, set())
        cut.assert_not_called()
        self.assertIn("first release is cut explicitly", outcome)
        fx = Fixture(self)
        fx.tag("v0.1.0")
        fx.pr("docs: y")
        fx.git("update-ref", "refs/remotes/origin/main", "HEAD")
        cut, outcome = self.run_after(fx, {"v0.1.0"})
        cut.assert_not_called()
        self.assertEqual(outcome, "nothing to release")


class RemoteState(unittest.TestCase):
    def test_remote_tags_peel_annotated_tags_and_ignore_other_refs(self):
        listing = "\n".join(["aaa\trefs/tags/v0.1.0", "bbb\trefs/tags/v0.1.0^{}", "ccc\trefs/tags/v0.2.0", "ddd\trefs/tags/vnext"])
        with patch.object(release, "git_remote", return_value=listing):
            self.assertEqual(release.remote_tags(), {"v0.1.0": "bbb", "v0.2.0": "ccc"})

    def test_git_remote_falls_back_to_https_and_reports_both_failing(self):
        calls = []

        def fake_run(command, **kw):
            calls.append(command[-1])
            return subprocess.CompletedProcess(command, 0 if command[-1].startswith("https") else 1, "ok\n", "no ssh key")

        with patch.object(release, "remote_names", return_value=("origin", "https://github.com/o/r")), \
                patch.object(release.subprocess, "run", fake_run):
            self.assertEqual(release.git_remote("ls-remote", "<remote>"), "ok\n")
        self.assertEqual(calls, ["origin", "https://github.com/o/r"])
        with patch.object(release, "remote_names", return_value=("origin", "origin")), \
                patch.object(release.subprocess, "run", lambda c, **k: subprocess.CompletedProcess(c, 1, "", "boom")), \
                self.assertRaisesRegex(ValueError, "SSH and HTTPS.*boom"):
            release.git_remote("ls-remote", "<remote>")

    def test_a_missing_release_is_false_and_other_gh_errors_propagate(self):
        with patch.object(harness, "gh", side_effect=ValueError("gh release view failed: release not found")):
            self.assertFalse(release.has_github_release("o/r", "v0.1.0"))
        with patch.object(harness, "gh", side_effect=ValueError("gh release view failed: HTTP 502")), self.assertRaises(ValueError):
            release.has_github_release("o/r", "v0.1.0")
        with patch.object(harness, "gh", return_value="title: v0.1.0"):
            self.assertTrue(release.has_github_release("o/r", "v0.1.0"))


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
        # The escape hatch needs a reason.
        override = {**pr, "title": "fix: x", "body": "Contract: none - a type was renamed, the JSON is identical"}
        self.assertIsNone(release.contract_refusal(override, breaking))
        self.assertIn("`Contract: none - <reason>`", release.contract_refusal({**pr, "title": "fix: x", "body": "Contract: none"}, breaking))


if __name__ == "__main__":
    unittest.main()
