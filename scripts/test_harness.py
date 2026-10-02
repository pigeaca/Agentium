"""Harness regressions; fixtures live in temporary directories and never touch the real checkout."""
from pathlib import Path
import re
import shutil
import contextlib
import fcntl
import json
import os
import subprocess
import tempfile
import threading
import unittest
import unittest.mock
from unittest.mock import patch

import harness


def fake(*parts):
    # Joined at runtime so this file never contains a credential-shaped literal itself.
    return "".join(parts)


class DocumentationChecks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="agentium-doc-check-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        patcher = patch.object(harness, "ROOT", self.root)
        patcher.start()
        self.addCleanup(patcher.stop)
        for entry in harness.DOC_ENTRYPOINTS:
            self.write(entry, "Shared instructions\n")
        self.write("CLAUDE.md", "\n".join("@" + entry for entry in harness.DOC_ENTRYPOINTS))
        for doc in ("README.md", ".agents/skills/README.md", ".agents/decisions/README.md", ".agents/plans/README.md"):
            self.write(doc, "Docs\n")
        self.write(".agents/plans/archive/INDEX.md", "# Completed Plans\n")
        metadata = "---\nname: demo\ndescription: Exercise a local workflow.\n---\n"
        self.write(".agents/skills/demo/SKILL.md", metadata + "Canonical steps.\n")
        self.adapter = ".claude/skills/demo/SKILL.md"
        self.write(self.adapter, metadata + "[canonical](../../../.agents/skills/demo/SKILL.md)\n")
        self.write(".agents/roles/checker.md", "---\nname: checker\ndescription: Review a local change.\n---\nCanonical role.\n")
        self.subagent = ".claude/agents/checker.md"
        self.write(self.subagent, "---\nname: checker\ndescription: Review a local change.\nmodel: opus\n---\n[role](../../.agents/roles/checker.md)\n")

    def write(self, relative, text):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def test_valid_tree_passes(self):
        with patch("builtins.print"):
            harness.check_docs()

    def test_prose_reference_does_not_replace_import(self):
        self.write("CLAUDE.md", "Please read AGENTS.md and .agents/README.md.\n")
        with self.assertRaisesRegex(ValueError, "explicitly import"):
            harness.check_agent_adapters()

    def test_fenced_imports_are_not_loaded(self):
        path = self.root / "CLAUDE.md"
        path.write_text("```markdown\n" + path.read_text() + "\n```\n")
        with self.assertRaisesRegex(ValueError, "explicitly import"):
            harness.check_agent_adapters()

    def test_missing_skill_adapter_and_metadata_drift(self):
        path = self.root / self.adapter
        path.write_text(path.read_text().replace("Exercise a local workflow.", "Different purpose."))
        with self.assertRaisesRegex(ValueError, "metadata differs"):
            harness.check_agent_adapters()
        path.unlink()
        with self.assertRaisesRegex(ValueError, "adapter mismatch"):
            harness.check_agent_adapters()

    def test_subagent_drift_and_foreign_links(self):
        path = self.root / self.subagent
        original = path.read_text()
        path.write_text(original.replace("Review a local change.", "Edit anything."))
        with self.assertRaisesRegex(ValueError, "differs from canonical role"):
            harness.check_agent_adapters()
        path.write_text(original.replace("roles/checker.md", "skills/demo/SKILL.md"))
        with self.assertRaisesRegex(ValueError, "subagent must link"):
            harness.check_agent_adapters()

    def test_broken_link_and_plan_archive_rules(self):
        self.write("docs/guide.md", "[missing](nowhere.md)\n")
        with patch("builtins.print"), self.assertRaisesRegex(ValueError, "broken link"):
            harness.check_docs()
        (self.root / "docs/guide.md").unlink()
        self.write(".agents/plans/2026-01-01-done.md", "# Done\n\n- Status: Completed\n")
        with patch("builtins.print"), self.assertRaisesRegex(ValueError, "must be archived"):
            harness.check_docs()
        (self.root / ".agents/plans/2026-01-01-done.md").rename(self.root / ".agents/plans/archive/2026-01-01-done.md")
        with patch("builtins.print"), self.assertRaisesRegex(ValueError, "Archive/index mismatch"):
            harness.check_docs()

    def test_context_budget(self):
        self.write(".agents/rules/core.md", "word " * 1801)
        with patch("builtins.print"), self.assertRaisesRegex(ValueError, "limit 1800"):
            harness.check_docs()


class CredentialScan(unittest.TestCase):
    def test_added_credentials_are_located_without_echoing_values(self):
        value = fake("sk-", "ant-", "a" * 24)
        diff = "\n".join(["+++ b/app/config.go", "@@ -3,0 +4,2 @@", "+ok := 1", f'+key := "{value}"',
                          "+++ b/notes.md", "@@ -1 +1 @@", f"-{value}", "+removed"])
        findings = harness.credential_findings(diff)
        self.assertEqual(findings, ["app/config.go:5: added line looks like a Anthropic API key"])
        self.assertNotIn(value, findings[0])

    def test_each_shape_marker_and_header_lookalike(self):
        samples = [fake("sk-", "proj-", "b" * 24), fake("AK", "IA", "ABCDEFGHIJKLMNOP"), fake("gh", "p_", "c" * 36),
                   fake("xo", "xb-", "1234567890ab"), fake("-----BEGIN ", "RSA PRIVATE KEY-----")]
        for sample in samples:
            with self.subTest(sample=sample[:6]):
                self.assertEqual(len(harness.credential_findings(f"+++ b/x\n@@ -0,0 +1 @@\n+{sample}")), 1)
        allowed = fake("sk-", "d" * 24) + "  // secret-scan: allow (redaction test)"
        self.assertEqual(harness.credential_findings(f"+++ b/x_test.go\n@@ -0,0 +1 @@\n+{allowed}"), [])
        diff = "\n".join(["+++ b/notes.md", "@@ -0,0 +7,2 @@", "+++ b/decoy", f"+{fake('sk-', 'f' * 24)}"])
        self.assertEqual(harness.credential_findings(diff), ["notes.md:8: added line looks like a OpenAI API key"])

    def test_sensitive_paths(self):
        for path in [".env", "config/.env.local", "certs/server.pem", "tls.key", "id_ed25519", "credentials.json", ".claude/settings.local.json"]:
            self.assertTrue(harness.sensitive_path(path), path)
        for path in [".env.example", "id_ed25519.pub", "docs/keys.md", "environment.ts"]:
            self.assertFalse(harness.sensitive_path(path), path)


@unittest.skipUnless(shutil.which("git"), "git is required")
class StagedChecks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="agentium-staged-check-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        for name, value in [("ROOT", self.root), ("check_docs", lambda: None)]:
            patcher = patch.object(harness, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def stage(self, relative, text):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        subprocess.run(["git", "add", "--force", relative], cwd=self.root, check=True)

    def test_clean_index_passes(self):
        self.stage("notes.md", "Plain text\n")
        with patch("builtins.print"):
            harness.check_staged()

    def test_problems_are_reported_together(self):
        self.stage("README.md", "trailing space \n")
        self.stage(".env", "TOKEN=placeholder\n")
        self.stage("config.ts", f'export const key = "{fake("sk-", "e" * 24)}";\n')
        with patch("sys.stderr") as stderr, self.assertRaisesRegex(ValueError, "3 staged check"):
            harness.check_staged()
        report = "".join(call.args[0] for call in stderr.write.call_args_list)
        for expected in ["Whitespace errors", ".env: credential", "config.ts:1"]:
            self.assertIn(expected, report)

    @unittest.skipUnless(shutil.which("gofmt"), "gofmt is required")
    def test_staged_go_blob_is_checked_not_the_working_file(self):
        self.stage("main.go", "package main\nfunc main(){}\n")
        with patch("sys.stderr"), self.assertRaisesRegex(ValueError, "1 staged check"):
            harness.check_staged()
        self.stage("main.go", "package main\n\nfunc main() {}\n")
        (self.root / "main.go").write_text("package main\nfunc main(){}\n")
        with patch("builtins.print"):
            harness.check_staged()

    def test_staged_go_uses_the_pinned_toolchain_gofmt(self):
        gofmt = self.root / "fake-gofmt"
        gofmt.write_text('#!/bin/sh\necho "<standard input>"\n')
        gofmt.chmod(0o755)
        self.stage("pkg/x.go", "package pkg\n")
        with patch.object(harness, "go_tool", return_value=str(gofmt)) as go_tool, patch.object(harness, "check_docs"), \
                patch("builtins.print"), self.assertRaisesRegex(ValueError, "staged check"):
            harness.check_staged()
        go_tool.assert_called_once_with("gofmt")

    def test_missing_gofmt_warns_instead_of_blocking(self):
        self.stage("main.go", "package main\nfunc main(){}\n")
        with patch.object(harness.shutil, "which", return_value=None), patch("builtins.print") as output:
            harness.check_staged()
        warnings = [call.args[0] for call in output.call_args_list if call.kwargs.get("file") is harness.sys.stderr]
        self.assertTrue(any("not checked" in warning for warning in warnings), warnings)

    def test_hooks_do_not_replace_a_foreign_hook_path(self):
        subprocess.run(["git", "config", "core.hooksPath", "/elsewhere"], cwd=self.root, check=True)
        with self.assertRaisesRegex(ValueError, "already /elsewhere"):
            harness.enable_hooks()


class ChangedCheckSelection(unittest.TestCase):
    def commands(self, *paths):
        planned, suggestions = harness.plan_checks(list(paths))
        return [command for command, _ in planned], suggestions

    def test_docs_and_harness_changes(self):
        commands, suggestions = self.commands("docs/harness.md", "scripts/harness.py", ".githooks/pre-commit", "LICENSE")
        self.assertEqual(commands, [["check", "docs"], ["check", "harness"]])
        self.assertEqual(suggestions, [])

    def test_ci_and_unmapped_files_are_only_suggested(self):
        commands, suggestions = self.commands(".github/workflows/ci.yml", "web/src/App.tsx", "scripts/release.sh")
        self.assertEqual(commands, [])
        self.assertTrue(any("PR's CI run" in suggestion for suggestion in suggestions))
        self.assertTrue(any("2 file(s) have no mapped check" in suggestion for suggestion in suggestions), suggestions)

    def test_go_code_and_modules(self):
        commands, suggestions = self.commands("internal/cli/cli.go", "docs/harness.md")
        self.assertEqual(commands, [["check", "docs"], ["check", "go"]])
        self.assertEqual(suggestions, [])
        commands, _ = self.commands("go.sum")
        self.assertEqual(commands, [["check", "go"], ["check", "vuln"]])
        commands, suggestions = self.commands("internal/store/migrations/0002_tasks.sql")
        self.assertEqual((commands, suggestions), ([["check", "go"]], []))
        commands, _ = self.commands("docs/research/judge-pilot/judge_pilot.py")  # internal/judge's tests read it
        self.assertEqual(commands, [["check", "docs"], ["check", "go"]])


class RemoteUrls(unittest.TestCase):
    def test_ssh_github_remotes_get_an_https_fallback(self):
        for url in ["git@github.com:/pigeaca/Agentium.git", "git@github.com:pigeaca/Agentium.git", "ssh://git@github.com/pigeaca/Agentium.git"]:
            with self.subTest(url=url):
                self.assertEqual(harness.https_url(url), "https://github.com/pigeaca/Agentium.git")
        for url in ["https://github.com/pigeaca/Agentium.git", "git@gitlab.com:team/repo.git", "/tmp/remote.git"]:
            with self.subTest(url=url):
                self.assertIsNone(harness.https_url(url))


class OfflineInstall(unittest.TestCase):
    def test_lockfiles_are_installed_offline_and_nothing_else_runs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.assertEqual(harness.install_dependencies(root), "none configured for this stack yet")
            (root / "web").mkdir()
            (root / "web" / "pnpm-lock.yaml").write_text("lockfileVersion: 9\n")
            with patch.object(harness.shutil, "which", return_value="/usr/bin/corepack"), patch.object(harness, "run") as run:
                self.assertEqual(harness.install_dependencies(root), "installed offline in web")
            args, kwargs = run.call_args
            self.assertEqual(args, ("corepack", "pnpm", "install", "--frozen-lockfile", "--offline"))
            self.assertEqual(kwargs["cwd"], root / "web")
            self.assertEqual(kwargs["extra_env"]["COREPACK_ENABLE_NETWORK"], "0")

    def test_failure_explains_instead_of_downloading(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "pnpm-lock.yaml").write_text("lockfileVersion: 9\n")
            failure = subprocess.CalledProcessError(1, ["corepack"])
            with patch.object(harness.shutil, "which", return_value="/usr/bin/corepack"), patch.object(harness, "run", side_effect=failure):
                with self.assertRaisesRegex(ValueError, "Nothing was downloaded"):
                    harness.install_dependencies(root)
            with patch.object(harness.shutil, "which", return_value=None), self.assertRaisesRegex(ValueError, "corepack is missing"):
                harness.install_dependencies(root)


class PlanMetrics(unittest.TestCase):
    FILLED = ("# Plan\n\n## Metrics\n- Agent: Claude Code / claude-opus-5-5 / high\n- Elapsed: 95m\n"
              "- Check-fix loops: 2\n- User corrections: 1 (threshold)\n- Review: approve\n\n## Notes\n- Agent: ignored / outside / block\n")

    def test_filled_block_is_parsed_and_template_placeholders_are_skipped(self):
        self.assertEqual(harness.plan_metrics(self.FILLED),
                         {"agent": "Claude Code / claude-opus-5-5 / high", "minutes": 95, "loops": 2, "corrections": 1})
        template = (Path(harness.__file__).parents[1] / ".agents/templates/implementation-plan.md").read_text()
        self.assertIsNone(harness.plan_metrics(template))
        self.assertIsNone(harness.plan_metrics("# Old plan without metrics\n"))

    def test_report_groups_by_agent(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(harness, "ROOT", Path(directory)), patch("builtins.print") as output:
            archive = Path(directory, ".agents/plans/archive")
            archive.mkdir(parents=True)
            (archive / "a.md").write_text(self.FILLED)
            (archive / "b.md").write_text(self.FILLED.replace("User corrections: 1", "User corrections: 0"))
            harness.metrics_report()
        lines = [call.args[0] for call in output.call_args_list]
        self.assertTrue(any(line.startswith("Claude Code / claude-opus-5-5 / high") and " 50 " in line for line in lines), lines)


class CheckScopes(unittest.TestCase):
    def test_ci_runs_docs_harness_tests_and_go_when_there_is_a_module(self):
        with patch.object(harness, "check_docs") as docs, patch.object(harness, "run") as run, patch.object(harness, "check_go") as go:
            harness.main(["check", "ci"])
        docs.assert_called_once()
        self.assertIn("test_harness.py", run.call_args.args)
        go.assert_called_once()  # this repository has a go.mod
        with tempfile.TemporaryDirectory() as directory, patch.object(harness, "ROOT", Path(directory)), \
                patch.object(harness, "check_docs"), patch.object(harness, "run"), patch.object(harness, "check_go") as go:
            harness.main(["check", "ci"])
        go.assert_not_called()
        with self.assertRaisesRegex(ValueError, "Unknown check scope"):
            harness.main(["check", "everything"])


def fake_go(root, version, modcache="", gofmt_lists="", gofmt_code=0, run_out="", run_err="", run_code=0, version_code=0,
            list_out="", list_code=0):
    """A stand-in GOROOT at `root`: `go version`, `go env GOROOT|GOMODCACHE`, `go run`, `go list` (printing `list_out`)
    and a gofmt that records its arguments in <root>/gofmt.args and lists `gofmt_lists` as unformatted."""
    bin_dir = root / "bin"
    bin_dir.mkdir(parents=True)
    (root / "list.out").write_text(list_out)
    go = bin_dir / "go"
    go.write_text("#!/bin/sh\n"
                  f'case "$1" in\n'
                  f'  version) echo "go version go{version} test/arch"; exit {version_code};;\n'
                  f'  env) case "$2" in GOROOT) echo "{root}";; GOMODCACHE) echo "{modcache}";; esac;;\n'
                  f'  run) printf "{run_out}"; printf "{run_err}" >&2; exit {run_code};;\n'
                  f'  list) cat "{root}/list.out"; exit {list_code};;\n'
                  "esac\n")
    gofmt = bin_dir / "gofmt"
    gofmt.write_text(f'#!/bin/sh\necho "$@" > "{root}/gofmt.args"\nprintf "{gofmt_lists}"\nexit {gofmt_code}\n')
    for tool in (go, gofmt):
        tool.chmod(0o755)
    return go


class GoToolchain(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="agentium-go-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve()
        self.repo = self.base / "repo"
        self.repo.mkdir()
        (self.repo / "go.mod").write_text("module example.com/x\n\ngo 1.27.1\n")
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        (self.repo / "a.go").write_text("package x\n")
        (self.repo / ".gitignore").write_text("ignored.go\n")
        (self.repo / "ignored.go").write_text("package x\n")
        for name, value in [("ROOT", self.repo), ("run", unittest.mock.MagicMock())]:
            patcher = patch.object(harness, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        environ = patch.dict(harness.os.environ, {}, clear=True)  # not CI unless a test says so
        environ.start()
        self.addCleanup(environ.stop)

    def test_environment_drops_credentials_and_pins_go(self):
        env = harness.harness_env({"ANTHROPIC_API_KEY": "x", "OPENAI_API_KEY": "y", "CODEX_API_KEY": "z", "PATH": "/bin", "GOFLAGS": "-tags=dev"})
        self.assertEqual(env, {"PATH": "/bin", "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly"})
        self.assertFalse(set(harness.CREDENTIAL_ENV) & harness.ENV.keys())

    def test_go_mod_version_prefers_the_toolchain_line(self):
        self.assertEqual(harness.go_mod_version(), "1.27.1")
        (self.repo / "go.mod").write_text("module example.com/x\n\ngo 1.27\n\ntoolchain go1.27.3\n")
        self.assertEqual(harness.go_mod_version(), "1.27.3")
        (self.repo / "go.mod").write_text("module example.com/x\n")
        with self.assertRaisesRegex(ValueError, "no go version"):
            harness.go_mod_version()

    def test_prefers_matching_path_go_then_sdk_and_never_installs(self):
        old = fake_go(self.base / "old", "1.26.3")
        sdk = fake_go(self.base / "home/sdk/go1.27.1", "1.27.1")
        broken = fake_go(self.base / "broken", "1.27.1", version_code=1)
        self.assertIsNone(harness.toolchain_version(str(broken)))
        with patch.object(harness.shutil, "which", return_value=str(old)), patch.object(harness.Path, "home", return_value=self.base / "home"):
            self.assertEqual(harness.go_binary(), sdk)
        current = fake_go(self.base / "current", "1.27.1")
        with patch.object(harness.shutil, "which", return_value=str(current)):
            self.assertEqual(harness.go_binary(), current)
        with patch.object(harness.shutil, "which", return_value=str(old)), patch.object(harness.Path, "home", return_value=self.base / "nohome"), \
                self.assertRaisesRegex(ValueError, "Nothing was installed.*go install golang.org/dl/go1.27.1@latest"):
            harness.go_binary()

    def test_tools_come_from_goroot_even_through_a_symlinked_go(self):
        real = fake_go(self.base / "sdk/go1.27.1", "1.27.1")
        link = self.base / "linkbin/go"
        link.parent.mkdir()
        link.symlink_to(real)
        with patch.object(harness.shutil, "which", return_value=str(link)):
            self.assertEqual(harness.go_tool("gofmt"), str(self.base / "sdk/go1.27.1/bin/gofmt"))
        (self.repo / "go.mod").unlink()  # no pinned toolchain: fall back to PATH
        with patch.object(harness.shutil, "which", return_value="/usr/bin/gofmt"):
            self.assertEqual(harness.go_tool("gofmt"), "/usr/bin/gofmt")

    def test_check_go_formats_listed_files_then_vets_and_tests_offline(self):
        with patch.object(harness, "go_binary", return_value=fake_go(self.base / "bad", "1.27.1", gofmt_lists="a.go")), \
                self.assertRaisesRegex(ValueError, "Not gofmt-formatted: a.go"):
            harness.check_go()
        with patch.object(harness, "go_binary", return_value=fake_go(self.base / "err", "1.27.1", gofmt_code=2)), \
                self.assertRaisesRegex(ValueError, "gofmt failed"):
            harness.check_go()
        harness.run.assert_not_called()
        go = fake_go(self.base / "good", "1.27.1")
        with patch.object(harness, "go_binary", return_value=go):
            harness.check_go()
        self.assertEqual((self.base / "good/gofmt.args").read_text().split(), ["-l", "a.go"])  # ignored.go left out
        self.assertEqual([(call.args, call.kwargs) for call in harness.run.call_args_list],
                         [((str(go), "vet", "./..."), {"extra_env": {"GOPROXY": "off"}}),
                          ((str(go), "test", "-race", "-count=1", "./..."), {"extra_env": {"GOPROXY": "off"}})])
        harness.run.reset_mock()
        with patch.object(harness, "go_binary", return_value=go), patch.dict(harness.os.environ, {"CI": "true"}):
            harness.check_go()
        self.assertEqual(harness.run.call_args.kwargs, {"extra_env": {}})

    def test_changed_go_tests_only_the_packages_the_change_can_affect(self):
        module, repo = "example.com/x", self.repo
        for folder, files in {"internal/a": ["a.go", "testdata/x.txt"], "internal/b": ["b.go"], "internal/c": ["c.go"],
                              "internal/d": ["d.go"], "internal/store": ["store.go", "migrations/0001.sql"]}.items():
            for name in files:
                (repo / folder / name).parent.mkdir(parents=True, exist_ok=True)
                (repo / folder / name).write_text("package x\n")
        a, b, c, d, store = (f"{module}/internal/{name}" for name in ("a", "b", "c", "d", "store"))
        rows = [(module, repo, ""), (a, repo / "internal/a", ""), (f"{a} [{a}.test]", repo / "internal/a", ""),
                (f"{a}.test", repo / "internal/a", f"{a} [{a}.test] fmt"),
                (b, repo / "internal/b", a), (f"{b}.test", repo / "internal/b", f"{a} {b} [{b}.test]"),
                (c, repo / "internal/c", ""), (f"{c}.test", repo / "internal/c", f"{c} [{c}.test]"),
                (d, repo / "internal/d", a),  # no tests of its own
                (store, repo / "internal/store", ""), (f"{store}.test", repo / "internal/store", f"{store} [{store}.test]")]
        go = fake_go(self.base / "list", "1.27.1", list_out="".join(f"{name}\t{folder}\t{deps}\n" for name, folder, deps in rows))
        targets = lambda *paths: harness.go_test_targets(go, list(paths))
        self.assertEqual(targets("internal/a/a.go"), [a, b])  # b's tests import a
        self.assertEqual(targets("internal/a/testdata/x.txt"), [a, b])  # test data belongs to the folder's package
        self.assertEqual(targets("internal/store/migrations/0001.sql"), [store])  # embedded files too
        self.assertEqual(targets("internal/c/c.go", "internal/b/b.go"), [b, c])
        self.assertEqual(targets("internal/d/d.go"), [])  # nothing with tests depends on d
        self.assertIsNone(targets("internal/c/c.go", "go.sum"))  # module changes test everything
        self.assertIsNone(targets("internal/gone/gone.go"))  # a deleted package maps to nothing: test everything
        failing = fake_go(self.base / "listfail", "1.27.1", list_code=1)
        self.assertIsNone(harness.go_test_targets(failing, ["internal/a/a.go"]))
        with patch.object(harness, "go_binary", return_value=go), patch("builtins.print") as output:
            harness.check_go(["internal/c/c.go"])
            self.assertEqual(harness.run.call_args.args, (str(go), "test", "-race", "-count=1", c))
            harness.check_go(["internal/d/d.go"])
            self.assertEqual(harness.run.call_args.args, (str(go), "vet", "./..."))  # no tests to run
            self.assertIn("no package with tests is affected", output.call_args.args[0])
            harness.check_go()
            self.assertEqual(harness.run.call_args.args, (str(go), "test", "-race", "-count=1", "./..."))

    def test_vuln_skips_uncached_locally_and_downloads_only_in_ci(self):
        cache = self.base / "modcache"
        go = fake_go(self.base / "uncached", "1.27.1", modcache=cache)
        with patch.object(harness, "go_binary", return_value=go), patch("builtins.print") as output:
            harness.check_vuln()
        self.assertIn("skipped", output.call_args.args[0])
        (cache / "golang.org/x/vuln@v1.8.0").mkdir(parents=True)
        partial = fake_go(self.base / "partial", "1.27.1", modcache=cache, run_err="module lookup disabled by GOPROXY=off", run_code=1)
        with patch.object(harness, "go_binary", return_value=partial), patch("builtins.print") as output:
            harness.check_vuln()
        self.assertIn("skipped", output.call_args.args[0])
        found = fake_go(self.base / "found", "1.27.1", modcache=cache, run_out="Vulnerability #1", run_code=3)
        with patch.object(harness, "go_binary", return_value=found), patch("builtins.print"), self.assertRaises(subprocess.CalledProcessError):
            harness.check_vuln()
        clean = fake_go(self.base / "clean", "1.27.1", modcache=cache, run_out="No vulnerabilities found.")
        with patch.object(harness, "go_binary", return_value=clean), patch("builtins.print") as output:
            harness.check_vuln()
        self.assertIn("No vulnerabilities found.", "".join(str(call.args[0]) for call in output.call_args_list))
        harness.run.assert_not_called()
        with patch.object(harness, "go_binary", return_value=clean), patch.dict(harness.os.environ, {"CI": "true"}):
            harness.check_vuln()
        self.assertEqual(harness.run.call_args.args, (str(clean), "run", harness.GOVULNCHECK, "./..."))

    def test_doctor_and_dispatch(self):
        go = fake_go(self.base / "doc", "1.27.1")
        with patch.object(harness, "go_binary", return_value=go), patch("builtins.print") as output:
            harness.main(["doctor"])
        self.assertTrue(any("go1.27.1, pinned in go.mod" in str(call.args[0]) for call in output.call_args_list))
        with patch.object(harness, "go_binary", side_effect=ValueError("Go 1.27.1 missing")), patch("builtins.print") as output:
            harness.main(["doctor"])
        self.assertTrue(any("Go 1.27.1 missing" in str(call.args[0]) for call in output.call_args_list))
        with patch.object(harness, "check_go") as check_go, patch.object(harness, "check_vuln") as check_vuln:
            harness.main(["check", "go"])
            harness.main(["check", "vuln"])
        check_go.assert_called_once()
        check_vuln.assert_called_once()

    def test_ci_workflow_pins_the_same_tools(self):
        workflow = (Path(harness.__file__).resolve().parents[1] / ".github/workflows/ci.yml").read_text()
        self.assertIn("actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1", workflow)
        self.assertIn("actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e", workflow)
        # Every action is pinned to a full commit SHA, never a movable tag.
        for action in re.findall(r"uses:\s*(\S+)", workflow):
            self.assertRegex(action, r"@[0-9a-f]{40}$", action)
        self.assertIn("go-version-file: go.mod", workflow)
        self.assertIn("harness.py check vuln", workflow)


@unittest.skipUnless(shutil.which("git"), "git is required")
class WorktreeLifecycle(unittest.TestCase):
    """Real temporary repositories: a bare remote, a primary clone and task worktrees."""

    def git(self, *args, cwd=None):
        return subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.com", *args],
                              cwd=cwd or self.primary, check=True, capture_output=True, text=True).stdout

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="agentium-worktree-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve()
        self.primary = self.base / "repo"
        subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(self.base / "remote.git")], check=True)
        subprocess.run(["git", "init", "-q", "-b", "main", str(self.primary)], check=True)
        (self.primary / ".gitignore").write_text("node_modules/\n.idea/\n")
        self.git("add", ".gitignore")
        self.git("commit", "-q", "-m", "initial")
        self.git("remote", "add", "origin", str(self.base / "remote.git"))
        self.git("push", "-q", "origin", "main")
        self.git("remote", "set-head", "origin", "main")
        original_run = harness.run

        def contained_run(*args, cwd=None, extra_env=None):
            # Regression guard: harness commands must never reach the real checkout from these tests.
            target = Path(cwd or harness.ROOT).resolve()
            self.assertIn(self.base, (target, *target.parents), f"harness ran outside the test repositories: {target}")
            original_run(*args, cwd=cwd, extra_env=extra_env)

        for name, value in [("ROOT", self.primary), ("install_dependencies", lambda root: "none"), ("run", contained_run)]:
            patcher = patch.object(harness, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_new_rejects_bad_branch_names_before_touching_git(self):
        for branch in ["feature-x", "claude/feature/x", "Claude/fix/x", "claude/fix/"]:
            with self.subTest(branch=branch), self.assertRaisesRegex(ValueError, "agent>/<type>/<topic"):
                harness.worktree_new(branch, None)

    def test_new_then_remove_only_after_merge_and_when_clean(self):
        with patch("builtins.print"):
            harness.worktree_new("claude/fix/probe", None)
        path = self.base / "repo-worktrees" / "claude-fix-probe"
        self.assertTrue((path / ".gitignore").is_file())
        upstream = subprocess.run(["git", "config", "--get", "branch.claude/fix/probe.merge"], cwd=self.primary, capture_output=True, text=True)
        self.assertEqual(upstream.stdout.strip(), "", "task branches must not track the default branch")
        (path / "change.txt").write_text("work\n")
        self.git("add", "change.txt", cwd=path)
        self.git("commit", "-q", "-m", "work", cwd=path)
        with patch("builtins.print"), self.assertRaisesRegex(ValueError, "not merged"):
            harness.worktree_remove("claude/fix/probe")
        self.git("merge", "-q", "--no-ff", "-m", "merge", "claude/fix/probe")
        self.git("push", "-q", "origin", "main")
        (path / "stray.txt").write_text("unsaved\n")
        with patch("builtins.print"), self.assertRaisesRegex(ValueError, "uncommitted or untracked"):
            harness.worktree_remove("claude/fix/probe")
        (path / "stray.txt").unlink()
        (path / ".idea").mkdir()
        (path / ".idea" / "workspace.xml").write_text("personal\n")
        with patch("builtins.print"), self.assertRaisesRegex(ValueError, r"ignored files .*\.idea"):
            harness.worktree_remove("claude/fix/probe")
        shutil.rmtree(path / ".idea")
        (path / "web" / "node_modules").mkdir(parents=True)
        (path / "web" / "node_modules" / "package.js").write_text("regenerable\n")
        with patch("builtins.print"):
            harness.worktree_remove("claude/fix/probe")
        self.assertFalse(path.exists())
        self.assertNotIn("claude/fix/probe", self.git("branch", "--list", "claude/fix/probe"))

    def test_new_refuses_a_stale_base_unless_explicit(self):
        with patch.object(harness, "fetch_default", return_value=("origin/main", False)), self.assertRaisesRegex(ValueError, "--base origin/main"):
            harness.worktree_new("claude/fix/stale", None)
        self.assertFalse((self.base / "repo-worktrees" / "claude-fix-stale").exists())
        with patch.object(harness, "fetch_default", side_effect=AssertionError("explicit base must not fetch")), patch("builtins.print"):
            harness.worktree_new("claude/fix/stale", "origin/main")
        self.assertTrue((self.base / "repo-worktrees" / "claude-fix-stale").is_dir())

    def test_remove_refuses_default_branch_and_current_checkout(self):
        for branch in ["main", "master"]:
            with self.subTest(branch=branch), self.assertRaisesRegex(ValueError, "Only task branches"):
                harness.worktree_remove(branch)
        self.git("switch", "-q", "-c", "claude/chore/current")
        with patch("builtins.print"), self.assertRaisesRegex(ValueError, "current or primary checkout"):
            harness.worktree_remove("claude/chore/current")


class TestSlots(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="agentium-slots-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)

    def test_two_holders_at_once_and_a_third_waits_for_a_release(self):
        acquired, printed = threading.Event(), []
        with contextlib.ExitStack() as stack, patch("builtins.print", side_effect=lambda *a, **k: printed.append(a[0])):
            holders = {stack.enter_context(harness.test_slot(self.directory)): None for _ in range(2)}
            self.assertEqual(set(holders), {0, 1})
            self.assertEqual(printed, [])  # neither of the first two waited

            def third():
                with harness.test_slot(self.directory):
                    acquired.set()
            # The waiter blocks on the slot its pid picks; release exactly that one.
            waited_on = os.getpid() % harness.TEST_SLOTS
            thread = threading.Thread(target=third, daemon=True)
            thread.start()
            self.assertFalse(acquired.wait(0.3))
            self.assertEqual(printed, ["[harness] waiting for a test slot (another worktree is running tests)"])
            stack.close()  # releases both slots, including waited_on
            self.assertTrue(acquired.wait(5), f"waiter on slot {waited_on} never acquired it")
            thread.join(5)
        self.assertEqual(len(printed), 1)  # the wait is announced once

    def test_slot_is_released_on_exception(self):
        with self.assertRaises(RuntimeError), harness.test_slot(self.directory, slots=1):
            raise RuntimeError("test run interrupted")
        with open(self.directory / "agentium-test-slot-0.lock", "a") as handle:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)  # would raise BlockingIOError if still held

    def test_slots_live_in_the_shared_git_common_dir(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory).resolve()
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            with patch.object(harness, "ROOT", repo):
                self.assertEqual(harness.slot_directory(), repo / ".git")


class ExitLine(unittest.TestCase):
    def outcome(self, args, **patches):
        with contextlib.ExitStack() as stack:
            for name, value in patches.items():
                stack.enter_context(patch.object(harness, name, value))
            output = stack.enter_context(patch("builtins.print"))
            code = harness.exit_code(args)
        lines = [str(call.args[0]) for call in output.call_args_list if call.args]
        return code, lines

    def test_changed_and_ci_end_with_the_real_exit_code(self):
        code, lines = self.outcome(["check", "changed"], check_changed=unittest.mock.MagicMock())
        self.assertEqual((code, lines[-1]), (0, "[harness] exit=0"))
        failing = unittest.mock.MagicMock(side_effect=subprocess.CalledProcessError(3, ["go", "test"]))
        code, lines = self.outcome(["check", "changed"], check_changed=failing)
        self.assertEqual((code, lines[-1]), (3, "[harness] exit=3"))
        code, lines = self.outcome(["check", "ci"], check_docs=unittest.mock.MagicMock(side_effect=ValueError("broken link")))
        self.assertEqual((code, lines[-1]), (1, "[harness] exit=1"))
        self.assertIn("[harness] broken link", lines)
        code, lines = self.outcome(["check"], check_docs=unittest.mock.MagicMock(), run=unittest.mock.MagicMock(),
                                   check_go=unittest.mock.MagicMock(side_effect=KeyboardInterrupt))
        self.assertEqual((code, lines[-1]), (130, "[harness] exit=130"))

    def test_usage_error_also_reports_and_other_commands_do_not(self):
        code, lines = self.outcome(["check", "changed", "--bogus"])
        self.assertEqual(code, 1)
        self.assertEqual(lines[-2:], ["[harness] Usage: check changed [--dry-run] [base]", "[harness] exit=1"])
        code, lines = self.outcome(["check", "docs"], check_docs=unittest.mock.MagicMock())
        self.assertEqual((code, lines), (0, []))


def ci_run(sha, status="completed", conclusion="success", name="CI"):
    return {"name": name, "head_sha": sha, "status": status, "conclusion": conclusion if status == "completed" else None,
            "html_url": f"https://github.com/o/r/actions/runs/{sha}"}


class FakeGitHub:
    """Scripted `gh`: each `pr view` returns the next PR state (the last repeats), each runs listing the next list."""

    def __init__(self, prs, runs, head_sha_filter=True):
        self.prs, self.runs, self.head_sha_filter, self.calls = list(prs), list(runs), head_sha_filter, []

    def __call__(self, *args):
        self.calls.append(args)
        if args[:2] == ("pr", "view"):
            pr = self.prs.pop(0) if len(self.prs) > 1 else self.prs[0]
            return json.dumps({"number": 7, "url": "https://github.com/o/r/pull/7", "headRefName": "claude/fix/x",
                               "state": "OPEN", "isDraft": False, "mergeable": "MERGEABLE", **pr})
        if args[0] == "api":
            if "head_sha=" in args[1] and not self.head_sha_filter:
                raise ValueError("gh api failed: HTTP 422")
            listing = self.runs.pop(0) if len(self.runs) > 1 else self.runs[0]
            return json.dumps({"workflow_runs": listing})
        if args[:2] == ("pr", "merge"):
            return "Merged pull request #7\n"
        raise AssertionError(f"unexpected gh call {args}")

    def merges(self):
        return [call for call in self.calls if call[:2] == ("pr", "merge")]


class PullRequestLanding(unittest.TestCase):
    def land(self, fake, dry_run=False, timeout=40):
        sleeps = []
        clock = iter(range(0, 100000, 20)).__next__  # every reading advances one poll interval
        with patch.object(harness, "gh", fake), patch.object(harness, "github_repo", return_value="o/r"), patch("builtins.print") as output:
            try:
                harness.pr_land(7, dry_run=dry_run, timeout_minutes=timeout, sleep=sleeps.append, clock=clock)
                error = None
            except ValueError as raised:
                error = str(raised)
        return error, sleeps, " ".join(str(call.args[0]) for call in output.call_args_list if call.args)

    def test_success_waits_for_ci_then_merges_the_exact_head(self):
        fake = FakeGitHub([{"headRefOid": "a" * 40}], [[], [ci_run("a" * 40, "in_progress")],
                                                       [ci_run("a" * 40), ci_run("a" * 40, name="Other", conclusion="failure"), ci_run("b" * 40, conclusion="failure")]])
        error, sleeps, _ = self.land(fake)
        self.assertIsNone(error)
        self.assertEqual(sleeps, [20, 20])
        self.assertEqual(fake.merges(), [("pr", "merge", "7", "--repo", "o/r", "--merge", "--match-head-commit", "a" * 40)])
        self.assertTrue(all("--repo" in call or call[0] == "api" for call in fake.calls))
        self.assertIn(f"repos/o/r/actions/runs?head_sha={'a' * 40}&per_page=100", [call[1] for call in fake.calls if call[0] == "api"])

    def test_failure_and_cancellation_never_merge_and_name_the_run(self):
        for conclusion in ("failure", "cancelled"):
            with self.subTest(conclusion=conclusion):
                fake = FakeGitHub([{"headRefOid": "a" * 40}], [[ci_run("a" * 40, conclusion=conclusion)]])
                error, _, _ = self.land(fake)
                self.assertIn(f"CI {conclusion}: https://github.com/o/r/actions/runs/{'a' * 40}", error)
                self.assertEqual(fake.merges(), [])

    def test_draft_closed_and_conflicting_prs_are_refused_at_once(self):
        for state, reason in [({"isDraft": True}, "is a draft"), ({"mergeable": "CONFLICTING"}, "merge conflicts"),
                              ({"state": "MERGED"}, "is merged, not open")]:
            with self.subTest(reason=reason):
                fake = FakeGitHub([{"headRefOid": "a" * 40, **state}], [[ci_run("a" * 40)]])
                error, sleeps, _ = self.land(fake)
                self.assertIn(reason, error)
                self.assertEqual((fake.merges(), sleeps, [c for c in fake.calls if c[0] == "api"]), ([], [], []))

    def test_a_new_head_restarts_the_wait_on_the_new_commit(self):
        old, new = "a" * 40, "b" * 40
        # The old commit's CI gets cancelled by the push; that must not fail the landing.
        fake = FakeGitHub([{"headRefOid": old}, {"headRefOid": old}, {"headRefOid": new}],
                          [[ci_run(old, "in_progress")], [ci_run(new, "queued")], [ci_run(old, conclusion="cancelled"), ci_run(new)]])
        error, _, printed = self.land(fake)
        self.assertIsNone(error)
        self.assertIn("head changed to bbbbbbbbbbbb", printed)
        self.assertEqual(fake.merges()[0][-1], new)

    def test_timeout_never_merges(self):
        fake = FakeGitHub([{"headRefOid": "a" * 40}], [[ci_run("a" * 40, "in_progress")]])
        error, sleeps, _ = self.land(fake, timeout=1)
        self.assertIn("timed out after 1 min (CI in_progress: https://github.com/o/r/actions/runs/", error)
        self.assertEqual(fake.merges(), [])
        self.assertEqual(len(sleeps), 2)  # 60 s at a 20 s poll

    def test_dry_run_reports_without_waiting_or_merging(self):
        fake = FakeGitHub([{"headRefOid": "a" * 40}], [[ci_run("a" * 40, "in_progress")]])
        error, sleeps, printed = self.land(fake, dry_run=True)
        self.assertIsNone(error)
        self.assertEqual((sleeps, fake.merges()), ([], []))
        self.assertIn("would wait up to 40 min for CI, then merge only if it passes", printed)
        fake = FakeGitHub([{"headRefOid": "a" * 40}], [[ci_run("a" * 40, conclusion="failure")]])
        error, _, printed = self.land(fake, dry_run=True)
        self.assertIn("CI failure", error)
        self.assertIn("would refuse", printed)
        self.assertEqual(fake.merges(), [])

    def test_runs_fall_back_to_the_branch_filter(self):
        fake = FakeGitHub([{"headRefOid": "a" * 40}], [[ci_run("a" * 40)]], head_sha_filter=False)
        error, _, _ = self.land(fake)
        self.assertIsNone(error)
        self.assertIn("repos/o/r/actions/runs?branch=claude/fix/x&per_page=100", [call[1] for call in fake.calls if call[0] == "api"])

    def test_repository_from_origin_and_usage(self):
        for url in ("git@github.com:/pigeaca/Agentium.git", "git@github.com:pigeaca/Agentium.git",
                    "ssh://git@github.com/pigeaca/Agentium", "https://github.com/pigeaca/Agentium.git"):
            with self.subTest(url=url), patch.object(harness, "git_output", return_value=url + "\n"):
                self.assertEqual(harness.github_repo(), "pigeaca/Agentium")
        with patch.object(harness, "git_output", return_value="https://gitlab.com/a/b.git\n"), self.assertRaisesRegex(ValueError, "not a GitHub remote"):
            harness.github_repo()
        failed = subprocess.CompletedProcess(["gh"], 1, stdout="", stderr="HTTP 404")
        with patch.object(harness.subprocess, "run", return_value=failed), self.assertRaisesRegex(ValueError, "gh pr view failed: HTTP 404"):
            harness.gh("pr", "view", "7")
        for args in (["land"], ["land", "x"], ["merge", "7"], ["land", "7", "--timeout"], ["land", "7", "--timeout", "0"]):
            with self.subTest(args=args), self.assertRaisesRegex(ValueError, "Usage: pr land"):
                harness.main(["pr", *args])
        with patch.object(harness, "pr_land") as land:
            harness.main(["pr", "land", "12", "--timeout", "5", "--dry-run"])
        land.assert_called_once_with(12, dry_run=True, timeout_minutes=5.0)


if __name__ == "__main__":
    unittest.main()
