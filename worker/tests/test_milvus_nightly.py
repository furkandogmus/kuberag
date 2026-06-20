"""Nightly integration tests against a real Milvus instance.

Milvus is resource-heavy and not suitable for every PR. These tests run in a
separate nightly workflow. Set MILVUS_ENDPOINT to a running instance:

  MILVUS_ENDPOINT=http://localhost:19530

To run locally with Docker:

  docker run -d --rm --name milvus-standalone \
    -p 19530:19530 -p 9091:9091 \
    milvusdb/milvus:latest milvus run standalone
  MILVUS_ENDPOINT=http://localhost:19530 \
    python -m pytest worker/tests/test_milvus_nightly.py -v
"""
import os
import unittest

TEST_COLLECTION = "nightly_itest"


class TestMilvusIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        endpoint = os.environ.get("MILVUS_ENDPOINT")
        if not endpoint:
            raise unittest.SkipTest("MILVUS_ENDPOINT not set; skipping Milvus nightly test")
        from rag_worker.stores import MilvusStore

        cls.store = MilvusStore(endpoint, TEST_COLLECTION, "cosine")
        cls.store.recreate_collection(384, "cosine")

    def test_ensure_and_count(self):
        self.store.ensure_collection(384, "cosine")
        cnt = self.store.count()
        self.assertIsInstance(cnt, int)

    def test_upsert_and_search(self):
        points = [
            {"id": "m1", "vector": [1.0] + [0.0] * 383,
             "payload": {"source": "docs", "doc_path": "a.md", "text": "hello world"}},
            {"id": "m2", "vector": [0.0] * 383 + [1.0],
             "payload": {"source": "docs", "doc_path": "b.md", "text": "milvus test"}},
        ]
        self.store.upsert(points)
        import time
        time.sleep(2)  # Allow eventual consistency

        hits = self.store.search([1.0] + [0.0] * 383, topk=2, source="docs")
        self.assertGreaterEqual(len(hits), 1)
        self.assertEqual(hits[0]["payload"]["source"], "docs")

        cnt = self.store.count()
        self.assertGreaterEqual(cnt, 1)

    def test_search_text(self):
        hits = self.store.search_text("milvus", topk=1, source="docs")
        self.assertGreaterEqual(len(hits), 1)
        self.assertIn("milvus", hits[0]["payload"]["text"])

    def test_delete_by_source_and_recount(self):
        self.store.upsert([
            {"id": "mdel", "vector": [0.5] * 384,
             "payload": {"source": "tmp", "doc_path": "x.md", "text": "to delete"}},
        ])
        import time
        time.sleep(1)
        before = self.store.count()
        self.store.delete_by_source("tmp")
        time.sleep(1)
        after = self.store.count()
        self.assertLess(after, before)

    def test_staging_swap(self):
        stage = self.store.__class__(self.store.client._uri, self.store.staging_name(77), "cosine")
        try:
            stage.ensure_collection(384, "cosine")
            stage.upsert([
                {"id": "ms1", "vector": [1.0] * 384,
                 "payload": {"source": "s", "doc_path": "s.md", "text": "staged nightly"}},
            ])
            import time
            time.sleep(1)
            ok = self.store.swap_collection(stage.collection)
            self.assertTrue(ok)
            hits = self.store.search([1.0] * 384, topk=1)
            self.assertGreaterEqual(len(hits), 1)
            self.assertEqual(hits[0]["payload"]["text"], "staged nightly")
        finally:
            try:
                stage.drop()
                stage.close()
            except Exception:
                pass

    @classmethod
    def tearDownClass(cls):
        try:
            cls.store.drop()
            cls.store.close()
        except Exception:
            pass


if __name__ == "__main__":
    unittest.main()
