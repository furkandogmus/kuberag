"""Pure tests for vector-store query helpers."""
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.stores import MilvusStore, _milvus_literal  # noqa: E402


class TestStoreHelpers(unittest.TestCase):
    def test_milvus_literal_escapes_filter_values(self):
        self.assertEqual(_milvus_literal('docs" or source != "docs'), '"docs\\" or source != \\"docs"')

    def test_milvus_expr_uses_escaped_literals(self):
        store = MilvusStore.__new__(MilvusStore)

        expr = store._build_expr(source='docs" or source != "docs', doc_path_prefix="guide/")

        self.assertIn('source == "docs\\" or source != \\"docs"', expr)
        self.assertIn('doc_path like "guide/%"', expr)


if __name__ == "__main__":
    unittest.main()
