"""Integration tests against real vector stores.

These tests require running store instances. Set environment variables to
configure connections, or they skip gracefully:

  QDRANT_ENDPOINT    – Qdrant REST endpoint (default: http://localhost:6333)
  PGVECTOR_DSN       – PostgreSQL DSN (default: postgresql://postgres:postgres@localhost:5432/postgres)

To run locally with Docker:

  docker run -d --rm -p 6333:6333 qdrant/qdrant
  docker run -d --rm -p 5432:5432 -e POSTGRES_PASSWORD=postgres pgvector/pgvector:pg17
  python -m pytest worker/tests/test_stores_integration.py -v
"""
import json
import os
import unittest

TEST_COLLECTION = "itest_kb"


def _store_available(env_var: str, default_endpoint: str) -> str:
    endpoint = os.environ.get(env_var, default_endpoint)
    if not endpoint:
        raise unittest.SkipTest(f"{env_var} not configured; set it to run this test")
    return endpoint


# --------------------------------------------------------------------------- #
# Qdrant
# --------------------------------------------------------------------------- #
class TestQdrantIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        endpoint = _store_available("QDRANT_ENDPOINT", "http://localhost:6333")
        from rag_worker.stores import QdrantStore

        cls.store = QdrantStore(endpoint, TEST_COLLECTION, "cosine")
        cls.store.recreate_collection(384, "cosine")

    def test_ensure_and_count(self):
        self.store.ensure_collection(384, "cosine")
        cnt = self.store.count()
        self.assertIsInstance(cnt, int)

    def test_upsert_and_search(self):
        points = [
            {"id": "p1", "vector": [1.0] + [0.0] * 383,
             "payload": {"source": "docs", "doc_path": "a.md", "text": "hello world"}},
            {"id": "p2", "vector": [0.0] * 383 + [1.0],
             "payload": {"source": "docs", "doc_path": "b.md", "text": "machine learning"}},
        ]
        self.store.upsert(points)

        hits = self.store.search([1.0] + [0.0] * 383, topk=2, source="docs")
        self.assertGreaterEqual(len(hits), 1)
        self.assertEqual(hits[0]["payload"]["source"], "docs")

    def test_search_text(self):
        hits = self.store.search_text("machine", topk=1, source="docs")
        self.assertGreaterEqual(len(hits), 1)
        self.assertIn("machine", hits[0]["payload"]["text"])

    def test_delete_by_source(self):
        self.store.upsert([
            {"id": "p-del", "vector": [0.5] * 384,
             "payload": {"source": "tmp", "doc_path": "x.md", "text": "temp"}},
        ])
        before = self.store.count()
        self.store.delete_by_source("tmp")
        after = self.store.count()
        self.assertLess(after, before)

    def test_staging_swap(self):
        stage_name = self.store.staging_name(99)
        stage = self.store.__class__(self.store.client._client._base_url, stage_name, "cosine")
        try:
            stage.ensure_collection(384, "cosine")
            stage.upsert([
                {"id": "stage-1", "vector": [1.0] * 384,
                 "payload": {"source": "s", "doc_path": "s.md", "text": "staged"}},
            ])
            ok = self.store.swap_collection(stage_name)
            self.assertTrue(ok)
            hits = self.store.search([1.0] * 384, topk=1)
            self.assertGreaterEqual(len(hits), 1)
            self.assertEqual(hits[0]["payload"]["text"], "staged")
        finally:
            try:
                stage.drop()
            except Exception:
                pass

    @classmethod
    def tearDownClass(cls):
        try:
            cls.store.drop()
            cls.store.close()
        except Exception:
            pass


# --------------------------------------------------------------------------- #
# pgvector
# --------------------------------------------------------------------------- #
class TestPgVectorIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        dsn = _store_available("PGVECTOR_DSN", "postgresql://postgres:postgres@localhost:5432/postgres")
        from rag_worker.stores import PgVectorStore

        cls.store = PgVectorStore(dsn, TEST_COLLECTION, "cosine")
        cls.store.recreate_collection(384, "cosine")

    def test_ensure_and_count(self):
        self.store.ensure_collection(384, "cosine")
        cnt = self.store.count()
        self.assertIsInstance(cnt, int)

    def test_upsert_and_search(self):
        points = [
            {"id": "p1", "vector": [1.0] + [0.0] * 383,
             "payload": {"source": "docs", "doc_path": "a.md", "text": "hello world", "chunk_hash": "abc123"}},
            {"id": "p2", "vector": [0.0] * 383 + [1.0],
             "payload": {"source": "docs", "doc_path": "b.md", "text": "machine learning", "chunk_hash": "def456"}},
        ]
        self.store.upsert(points)

        hits = self.store.search([1.0] + [0.0] * 383, topk=2, source="docs")
        self.assertGreaterEqual(len(hits), 1)
        self.assertEqual(hits[0]["payload"]["source"], "docs")

        cnt = self.store.count()
        self.assertGreaterEqual(cnt, 2)

    def test_on_conflict_update(self):
        points = [
            {"id": "p1", "vector": [0.0] * 384,
             "payload": {"source": "docs", "doc_path": "a.md", "text": "updated text", "chunk_hash": "xyz"}},
        ]
        self.store.upsert(points)
        hits = self.store.search([0.0] * 384, topk=1, source="docs")
        self.assertGreaterEqual(len(hits), 1)
        self.assertEqual(hits[0]["payload"]["text"], "updated text")

    def test_search_text(self):
        hits = self.store.search_text("learning", topk=1, source="docs")
        self.assertGreaterEqual(len(hits), 1)
        self.assertIn("learning", hits[0]["payload"]["text"])

    def test_delete_by_source(self):
        self.store.upsert([
            {"id": "p-del", "vector": [0.5] * 384,
             "payload": {"source": "tmp", "doc_path": "x.md", "text": "temp", "chunk_hash": "del"}},
        ])
        before = self.store.count()
        self.store.delete_by_source("tmp")
        after = self.store.count()
        self.assertLess(after, before)

    def test_swap_collection(self):
        stage = self.store.__class__(self.store.conn.info.dsn, self.store.staging_name(42), "cosine")
        try:
            stage.ensure_collection(384, "cosine")
            stage.upsert([
                {"id": "s1", "vector": [1.0] * 384,
                 "payload": {"source": "s", "doc_path": "s.md", "text": "staged", "chunk_hash": "s1"}},
            ])
            ok = self.store.swap_collection(stage.table)
            self.assertTrue(ok)
            hits = self.store.search([1.0] * 384, topk=1)
            self.assertGreaterEqual(len(hits), 1)
            self.assertEqual(hits[0]["payload"]["text"], "staged")
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
