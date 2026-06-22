import os
import sys
import unittest
from unittest.mock import MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.backup import _export_milvus
from rag_worker.restore import _restore_points
from rag_worker.stores import MilvusStore


class TestBackupRestore(unittest.TestCase):
    def test_milvus_backup_requests_and_preserves_all_point_fields(self):
        store = MagicMock()
        store.collection = "docs"
        store.client.query.side_effect = [
            [{
                "id": "point-1",
                "vector": [0.1, 0.2],
                "source": "docs",
                "doc_path": "guide.md",
                "text": "hello",
                "chunk_hash": "abc123",
            }],
            [],
        ]

        points = _export_milvus(store)

        self.assertEqual(points, [{
            "id": "point-1",
            "vector": [0.1, 0.2],
            "payload": {
                "source": "docs",
                "doc_path": "guide.md",
                "text": "hello",
                "chunk_hash": "abc123",
            },
        }])
        self.assertEqual(
            store.client.query.call_args_list[0].kwargs["output_fields"],
            ["id", "vector", "source", "doc_path", "text", "chunk_hash"],
        )

    def test_milvus_upsert_keeps_chunk_hash(self):
        store = MilvusStore.__new__(MilvusStore)
        store.collection = "docs"
        store.client = MagicMock()

        store.upsert([{
            "id": "point-1",
            "vector": [0.1],
            "payload": {
                "source": "docs",
                "doc_path": "guide.md",
                "text": "hello",
                "chunk_hash": "abc123",
            },
        }])

        row = store.client.upsert.call_args.args[1][0]
        self.assertEqual(row["chunk_hash"], "abc123")

    def test_restore_promotes_verified_staging_collection(self):
        active = MagicMock()
        active.staging_name.return_value = "docs-v42"
        active.swap_collection.return_value = True
        shadow = MagicMock()
        shadow.count.return_value = 2
        spec = {
            "vectorStore": {
                "type": "qdrant",
                "endpoint": "http://qdrant:6333",
                "collection": "docs",
            },
        }
        points = [
            {"id": "1", "vector": [0.1], "payload": {}},
            {"id": "2", "vector": [0.2], "payload": {}},
        ]

        with patch(
            "rag_worker.restore.make_store",
            side_effect=[active, shadow],
        ) as make_store:
            total = _restore_points(
                spec,
                points,
                dim=1,
                distance="cosine",
                restore_round=42,
            )

        self.assertEqual(total, 2)
        shadow.recreate_collection.assert_called_once_with(1, "cosine")
        shadow.upsert.assert_called_once_with(points)
        active.swap_collection.assert_called_once_with("docs-v42")
        rendered_shadow_spec = make_store.call_args_list[1].args[0]
        self.assertEqual(
            rendered_shadow_spec["vectorStore"]["collection"],
            "docs-v42",
        )
        shadow.drop.assert_not_called()
        shadow.close.assert_called_once()
        active.close.assert_called_once()

    def test_restore_failure_preserves_active_collection(self):
        active = MagicMock()
        active.staging_name.return_value = "docs-v7"
        shadow = MagicMock()
        shadow.count.return_value = 1
        spec = {
            "vectorStore": {
                "type": "qdrant",
                "endpoint": "http://qdrant:6333",
                "collection": "docs",
            },
        }
        points = [
            {"id": "1", "vector": [0.1], "payload": {}},
            {"id": "2", "vector": [0.2], "payload": {}},
        ]

        with patch(
            "rag_worker.restore.make_store",
            side_effect=[active, shadow],
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "expected 2 points, got 1",
            ):
                _restore_points(
                    spec,
                    points,
                    dim=1,
                    distance="cosine",
                    restore_round=7,
                )

        active.swap_collection.assert_not_called()
        shadow.drop.assert_called_once()
        shadow.close.assert_called_once()
        active.close.assert_called_once()


if __name__ == "__main__":
    unittest.main()
