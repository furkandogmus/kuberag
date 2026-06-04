"""Tests for gitignore-style glob matching (the ** semantics fnmatch lacks)."""
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.sources import _match_globs  # noqa: E402


class TestGlobs(unittest.TestCase):
    def test_doublestar_matches_zero_or_more_dirs(self):
        g = ["docs/**/*.md"]
        self.assertTrue(_match_globs("docs/intro.md", g))          # zero dirs
        self.assertTrue(_match_globs("docs/a/b/intro.md", g))      # nested
        self.assertFalse(_match_globs("docs/intro.txt", g))        # wrong ext
        self.assertFalse(_match_globs("other/intro.md", g))        # wrong root

    def test_single_star_stays_in_segment(self):
        self.assertTrue(_match_globs("a/file.md", ["a/*.md"]))
        self.assertFalse(_match_globs("a/b/file.md", ["a/*.md"]))  # * doesn't cross /

    def test_no_globs_falls_back_to_text_exts(self):
        self.assertTrue(_match_globs("README.md", []))
        self.assertTrue(_match_globs("notes/info.txt", []))
        self.assertFalse(_match_globs("image.png", []))

    def test_multiple_globs_any_match(self):
        g = ["docs/**/*.md", "guides/**/*.rst"]
        self.assertTrue(_match_globs("docs/x.md", g))
        self.assertTrue(_match_globs("guides/deep/y.rst", g))
        self.assertFalse(_match_globs("src/main.go", g))


if __name__ == "__main__":
    unittest.main()
