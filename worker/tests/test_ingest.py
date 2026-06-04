"""Tests for incremental ingestion bookkeeping."""
import os
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.common import prior_sources  # noqa: E402
from rag_worker.ingest import _carry_chunks, _prior_revision  # noqa: E402


class TestIngestBookkeeping(unittest.TestCase):
    def test_prior_sources_preserve_revision_and_chunk_counts(self):
        raw = '[{"name":"docs","revision":"abc123","chunks":42}]'
        with patch.dict(os.environ, {"PRIOR_SOURCES_JSON": raw}):
            prior = prior_sources()

        self.assertEqual(_prior_revision(prior, "docs"), "abc123")
        self.assertEqual(_carry_chunks(prior, "docs"), 42)

    def test_prior_helpers_tolerate_legacy_revision_only_shape(self):
        prior = {"docs": "abc123"}

        self.assertEqual(_prior_revision(prior, "docs"), "abc123")
        self.assertEqual(_carry_chunks(prior, "docs"), 0)


if __name__ == "__main__":
    unittest.main()
