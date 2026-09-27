"""Hidden acceptance test: titled and angle-bracket Markdown links."""
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import harness


class HiddenMarkdownLinks(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        patcher = patch.object(harness, "ROOT", self.root)
        patcher.start()
        self.addCleanup(patcher.stop)

    def doc(self, text):
        path = self.root / "doc.md"
        path.write_text(text)
        return path

    def test_forms_are_extracted(self):
        path = self.doc('[a](guide.md "Guide") [b](<my notes.md>) [c](plain.md#part) [d](#top) '
                        "[e](other.md 'Other') ![img](pic.png \"Pic\") [f](<spaced dir/x.md#y>)\n")
        self.assertEqual(harness.markdown_links(path), ["guide.md", "my notes.md", "plain.md", "other.md", "pic.png", "spaced dir/x.md"])

    def test_broken_titled_or_bracketed_links_are_reported(self):
        (self.root / "exists.md").write_text("x\n")
        (self.root / "my notes.md").write_text("x\n")
        self.doc('[ok](exists.md "Title") [ok2](<my notes.md>)\n')
        harness.check_doc_links([self.root / "doc.md"])
        for text in ('[bad](missing.md "Title")\n', "[bad](<missing file.md>)\n"):
            with self.subTest(text=text), self.assertRaisesRegex(ValueError, "broken link"):
                self.doc(text)
                harness.check_doc_links([self.root / "doc.md"])

    def test_urls_are_still_allowed(self):
        self.doc('[site](https://example.com/a "Example") [s2](<https://example.com/b c>)\n')
        harness.check_doc_links([self.root / "doc.md"])
