"""Pure tests for vector-store query helpers."""
import os
import sys
import unittest
from unittest.mock import MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.stores import MilvusStore, QdrantStore, _milvus_literal  # noqa: E402


class TestStoreHelpers(unittest.TestCase):
    def test_milvus_literal_escapes_filter_values(self):
        self.assertEqual(_milvus_literal('docs" or source != "docs'), '"docs\\" or source != \\"docs"')

    def test_milvus_expr_uses_escaped_literals(self):
        store = MilvusStore.__new__(MilvusStore)

        expr = store._build_expr(source='docs" or source != "docs', doc_path_prefix="guide/")

        self.assertIn('source == "docs\\" or source != \\"docs"', expr)
        self.assertIn('doc_path like "guide/%"', expr)

    def test_qdrant_swap_repoints_existing_alias_atomically(self):
        store = QdrantStore.__new__(QdrantStore)
        store.collection = "docs"
        store.client = MagicMock()
        alias = MagicMock(alias_name="docs")
        store.client.get_aliases.return_value.aliases = [alias]
        alias.collection_name = "docs-v1"
        store.client.collection_exists.return_value = True

        self.assertTrue(store.swap_collection("docs-v2"))

        operations = store.client.update_collection_aliases.call_args.kwargs["change_aliases_operations"]
        self.assertEqual(len(operations), 2)
        self.assertEqual(operations[0].delete_alias.alias_name, "docs")
        self.assertEqual(operations[1].create_alias.collection_name, "docs-v2")
        store.client.delete_collection.assert_called_once_with("docs-v1")

    def test_qdrant_staging_name_is_versioned(self):
        store = QdrantStore.__new__(QdrantStore)
        store.collection = "docs"

        self.assertEqual(store.staging_name(7), "docs-v7")

    def test_qdrant_drop_removes_alias_and_physical_target(self):
        store = QdrantStore.__new__(QdrantStore)
        store.collection = "docs"
        store.client = MagicMock()
        alias = MagicMock(alias_name="docs", collection_name="docs-v3")
        store.client.get_aliases.return_value.aliases = [alias]
        store.client.collection_exists.return_value = True

        store.drop()

        operations = store.client.update_collection_aliases.call_args.kwargs["change_aliases_operations"]
        self.assertEqual(operations[0].delete_alias.alias_name, "docs")
        store.client.delete_collection.assert_called_once_with("docs-v3")

    def test_milvus_swap_repoints_alias_and_removes_old_target(self):
        store = MilvusStore.__new__(MilvusStore)
        store.collection = "docs"
        store.client = MagicMock()
        store.client.describe_alias.return_value = {"collection_name": "docs-v1"}
        store.client.has_collection.return_value = True

        self.assertTrue(store.swap_collection("docs-v2"))

        store.client.alter_alias.assert_called_once_with(collection_name="docs-v2", alias="docs")
        store.client.drop_collection.assert_called_once_with("docs-v1")


if __name__ == "__main__":
    unittest.main()
