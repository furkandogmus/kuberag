"""Vector store backends behind a small common interface.

A "point" is a dict: {"id": str, "vector": list[float], "payload": {...}}.
Payload always carries: source (name), doc_path, text, chunk_hash.
"""
from __future__ import annotations

import os
from abc import ABC, abstractmethod


class VectorStore(ABC):
    @abstractmethod
    def ensure_collection(self, dim: int, distance: str) -> None: ...

    @abstractmethod
    def recreate_collection(self, dim: int, distance: str) -> None: ...

    @abstractmethod
    def delete_by_source(self, source_name: str) -> None: ...

    @abstractmethod
    def upsert(self, points: list[dict]) -> None: ...

    @abstractmethod
    def count(self) -> int: ...

    @abstractmethod
    def search(self, vector: list[float], topk: int) -> list[dict]: ...

    @abstractmethod
    def drop(self) -> None: ...


def make_store(spec: dict) -> "VectorStore":
    vs = spec["vectorStore"]
    t = vs["type"]
    collection = vs.get("collection") or os.environ["KB_NAME"]
    distance = vs.get("distance", "cosine")
    if t == "qdrant":
        return QdrantStore(vs["endpoint"], collection, distance)
    if t == "pgvector":
        return PgVectorStore(vs["endpoint"], collection, distance)
    if t == "milvus":
        return MilvusStore(vs["endpoint"], collection, distance)
    raise ValueError(f"unsupported vector store: {t}")


# --------------------------------------------------------------------------- #
# Qdrant
# --------------------------------------------------------------------------- #
class QdrantStore(VectorStore):
    def __init__(self, endpoint: str, collection: str, distance: str):
        from qdrant_client import QdrantClient

        self.collection = collection
        self.distance = distance
        api_key = os.environ.get("VECTORSTORE_CREDENTIAL")
        self.client = QdrantClient(url=endpoint, api_key=api_key)

    def _distance(self):
        from qdrant_client import models

        return {
            "cosine": models.Distance.COSINE,
            "dot": models.Distance.DOT,
            "euclid": models.Distance.EUCLID,
        }[self.distance]

    def _exists(self) -> bool:
        return self.client.collection_exists(self.collection)

    def ensure_collection(self, dim: int, distance: str) -> None:
        from qdrant_client import models

        if not self._exists():
            self.client.create_collection(
                collection_name=self.collection,
                vectors_config=models.VectorParams(size=dim, distance=self._distance()),
            )
            self.client.create_payload_index(
                self.collection, field_name="source",
                field_schema=models.PayloadSchemaType.KEYWORD,
            )

    def recreate_collection(self, dim: int, distance: str) -> None:
        from qdrant_client import models

        self.client.recreate_collection(
            collection_name=self.collection,
            vectors_config=models.VectorParams(size=dim, distance=self._distance()),
        )
        self.client.create_payload_index(
            self.collection, field_name="source",
            field_schema=models.PayloadSchemaType.KEYWORD,
        )

    def delete_by_source(self, source_name: str) -> None:
        from qdrant_client import models

        self.client.delete(
            collection_name=self.collection,
            points_selector=models.FilterSelector(
                filter=models.Filter(must=[
                    models.FieldCondition(key="source", match=models.MatchValue(value=source_name))
                ])
            ),
        )

    def upsert(self, points: list[dict]) -> None:
        from qdrant_client import models

        self.client.upsert(
            collection_name=self.collection,
            points=[
                models.PointStruct(id=p["id"], vector=p["vector"], payload=p["payload"])
                for p in points
            ],
        )

    def count(self) -> int:
        return self.client.count(self.collection, exact=True).count

    def search(self, vector: list[float], topk: int) -> list[dict]:
        hits = self.client.search(self.collection, query_vector=vector, limit=topk, with_payload=True)
        return [{"score": h.score, "payload": h.payload} for h in hits]

    def drop(self) -> None:
        if self._exists():
            self.client.delete_collection(self.collection)


# --------------------------------------------------------------------------- #
# pgvector
# --------------------------------------------------------------------------- #
class PgVectorStore(VectorStore):
    """Endpoint is a libpq DSN (postgresql://user:pass@host/db)."""

    _OPS = {"cosine": "vector_cosine_ops", "dot": "vector_ip_ops", "euclid": "vector_l2_ops"}
    _ORDER = {"cosine": "<=>", "dot": "<#>", "euclid": "<->"}

    def __init__(self, endpoint: str, collection: str, distance: str):
        import psycopg

        self.table = _sanitize(collection)
        self.distance = distance
        cred = os.environ.get("VECTORSTORE_CREDENTIAL")
        if cred and "@" not in endpoint:
            endpoint = endpoint.replace("postgresql://", f"postgresql://postgres:{cred}@")
        self.conn = psycopg.connect(endpoint, autocommit=True)
        self.conn.execute("CREATE EXTENSION IF NOT EXISTS vector")

    def ensure_collection(self, dim: int, distance: str) -> None:
        self.conn.execute(
            f"CREATE TABLE IF NOT EXISTS {self.table} ("
            f"id text PRIMARY KEY, source text, doc_path text, text text, "
            f"chunk_hash text, embedding vector({dim}))"
        )
        self.conn.execute(f"CREATE INDEX IF NOT EXISTS {self.table}_source_idx ON {self.table} (source)")

    def recreate_collection(self, dim: int, distance: str) -> None:
        self.conn.execute(f"DROP TABLE IF EXISTS {self.table}")
        self.ensure_collection(dim, distance)

    def delete_by_source(self, source_name: str) -> None:
        self.conn.execute(f"DELETE FROM {self.table} WHERE source = %s", (source_name,))

    def upsert(self, points: list[dict]) -> None:
        with self.conn.cursor() as cur:
            cur.executemany(
                f"INSERT INTO {self.table} (id, source, doc_path, text, chunk_hash, embedding) "
                f"VALUES (%s, %s, %s, %s, %s, %s) "
                f"ON CONFLICT (id) DO UPDATE SET "
                f"text = EXCLUDED.text, chunk_hash = EXCLUDED.chunk_hash, embedding = EXCLUDED.embedding",
                [
                    (
                        p["id"], p["payload"]["source"], p["payload"]["doc_path"],
                        p["payload"]["text"], p["payload"].get("chunk_hash", ""),
                        "[" + ",".join(map(str, p["vector"])) + "]",
                    )
                    for p in points
                ],
            )

    def count(self) -> int:
        return self.conn.execute(f"SELECT count(*) FROM {self.table}").fetchone()[0]

    def search(self, vector: list[float], topk: int) -> list[dict]:
        op = self._ORDER[self.distance]
        vec = "[" + ",".join(map(str, vector)) + "]"
        rows = self.conn.execute(
            f"SELECT source, doc_path, text, embedding {op} %s AS dist "
            f"FROM {self.table} ORDER BY dist ASC LIMIT %s",
            (vec, topk),
        ).fetchall()
        out = []
        for source, doc_path, text, dist in rows:
            out.append({"score": 1.0 - float(dist), "payload": {"source": source, "doc_path": doc_path, "text": text}})
        return out

    def drop(self) -> None:
        self.conn.execute(f"DROP TABLE IF EXISTS {self.table}")


# --------------------------------------------------------------------------- #
# Milvus
# --------------------------------------------------------------------------- #
class MilvusStore(VectorStore):
    _METRIC = {"cosine": "COSINE", "dot": "IP", "euclid": "L2"}

    def __init__(self, endpoint: str, collection: str, distance: str):
        from pymilvus import MilvusClient

        self.collection = _sanitize(collection)
        self.distance = distance
        token = os.environ.get("VECTORSTORE_CREDENTIAL", "")
        self.client = MilvusClient(uri=endpoint, token=token)

    def ensure_collection(self, dim: int, distance: str) -> None:
        if not self.client.has_collection(self.collection):
            self.client.create_collection(
                collection_name=self.collection,
                dimension=dim,
                metric_type=self._METRIC[self.distance],
                auto_id=False,
                primary_field_name="id",
                id_type="string",
                max_length=128,
            )

    def recreate_collection(self, dim: int, distance: str) -> None:
        if self.client.has_collection(self.collection):
            self.client.drop_collection(self.collection)
        self.ensure_collection(dim, distance)

    def delete_by_source(self, source_name: str) -> None:
        self.client.delete(self.collection, filter=f'source == "{source_name}"')

    def upsert(self, points: list[dict]) -> None:
        rows = [
            {
                "id": p["id"], "vector": p["vector"], "source": p["payload"]["source"],
                "doc_path": p["payload"]["doc_path"], "text": p["payload"]["text"],
            }
            for p in points
        ]
        self.client.upsert(self.collection, rows)

    def count(self) -> int:
        # MilvusClient has no flush(); load then count(*). Counts are eventually
        # consistent right after an upsert, so retry briefly for a stable value.
        import time

        try:
            self.client.load_collection(self.collection)
        except Exception:
            pass
        n = 0
        for _ in range(10):
            res = self.client.query(self.collection, filter="", output_fields=["count(*)"])
            n = int(res[0]["count(*)"]) if res else 0
            if n > 0:
                break
            time.sleep(1)
        return n

    def search(self, vector: list[float], topk: int) -> list[dict]:
        res = self.client.search(
            self.collection, data=[vector], limit=topk,
            output_fields=["source", "doc_path", "text"],
        )[0]
        return [{"score": h["distance"], "payload": h["entity"]} for h in res]

    def drop(self) -> None:
        if self.client.has_collection(self.collection):
            self.client.drop_collection(self.collection)


def _sanitize(name: str) -> str:
    return "".join(c if c.isalnum() or c == "_" else "_" for c in name)
