"""Pure-Python tests for chunking (no third-party deps).

Run: python -m pytest worker/tests  (or) python -m unittest discover worker/tests
"""
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.chunking import chunk_text  # noqa: E402
from rag_worker.common import chunk_hash, point_id  # noqa: E402


class TestChunking(unittest.TestCase):
    def test_respects_max_tokens(self):
        text = " ".join(f"w{i}" for i in range(1000))
        chunks = chunk_text(text, max_tokens=100, overlap=10, strategy="fixed")
        self.assertGreater(len(chunks), 1)
        # max_words = int(100/1.3) ~= 76; allow a small margin.
        for c in chunks:
            self.assertLessEqual(len(c.split()), 80)

    def test_overlap_creates_shared_words(self):
        text = " ".join(f"w{i}" for i in range(300))
        chunks = chunk_text(text, max_tokens=100, overlap=40, strategy="fixed")
        self.assertGreater(len(chunks), 1)
        # With overlap>0 adjacent windows must share some words.
        shared = set(chunks[0].split()) & set(chunks[1].split())
        self.assertTrue(shared, "adjacent chunks should overlap")
        # And without overlap they must not.
        no_ov = chunk_text(text, max_tokens=100, overlap=0, strategy="fixed")
        self.assertFalse(set(no_ov[0].split()) & set(no_ov[1].split()))

    def test_semantic_splits_on_headings(self):
        text = "# Title\n\nIntro paragraph.\n\n## Section\n\nBody text here."
        chunks = chunk_text(text, max_tokens=800, overlap=0, strategy="semantic")
        self.assertGreaterEqual(len(chunks), 2)

    def test_empty_text(self):
        self.assertEqual(chunk_text("", 800, 80, "semantic"), [])

    def test_point_id_stable_and_unique(self):
        a = point_id("docs", "a/b.md", 0)
        self.assertEqual(a, point_id("docs", "a/b.md", 0))
        self.assertNotEqual(a, point_id("docs", "a/b.md", 1))
        self.assertNotEqual(a, point_id("other", "a/b.md", 0))

    def test_chunk_hash_changes_with_content(self):
        self.assertNotEqual(chunk_hash("hello"), chunk_hash("world"))
        self.assertEqual(chunk_hash("hello"), chunk_hash("hello"))


if __name__ == "__main__":
    unittest.main()
