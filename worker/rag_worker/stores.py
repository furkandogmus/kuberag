"""Vector store backends behind a small common interface.

A "point" is a dict: {"id": str, "vector": list[float], "payload": {...}}.
Payload always carries: source (name), doc_path, text, chunk_hash.
"""
from __future__ import annotations

import json
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
    def search(
        self,
        vector: list[float],
        topk: int,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ) -> list[dict]: ...

    @abstractmethod
    def search_text(
        self,
        query: str,
        topk: int,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ) -> list[dict]: ...

    @abstractmethod
    def drop(self) -> None: ...

    def swap_collection(self, shadow_name: str) -> bool:
        """Atomically promote a shadow collection to the active name.

        The current collection is replaced by the shadow. Returns True if the
        store supports atomic swaps (Qdrant aliases, pgvector table rename,
        Milvus aliases). If False, the caller must fall back to a non-atomic
        recreate.

        The shadow collection must already exist with the correct dimension.
        """
        return False  # default: not supported

    def close(self) -> None:
        pass


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
        self._ensure_payload_indexes()

    def recreate_collection(self, dim: int, distance: str) -> None:
        from qdrant_client import models

        self.client.recreate_collection(
            collection_name=self.collection,
            vectors_config=models.VectorParams(size=dim, distance=self._distance()),
        )
        self._ensure_payload_indexes()

    def _ensure_payload_indexes(self) -> None:
        from qdrant_client import models

        self._create_payload_index("source", models.PayloadSchemaType.KEYWORD)
        self._create_payload_index("doc_path", models.PayloadSchemaType.KEYWORD)
        self._create_payload_index("text", models.PayloadSchemaType.TEXT)

    def _create_payload_index(self, field_name: str, field_schema) -> None:
        try:
            self.client.create_payload_index(
                self.collection,
                field_name=field_name,
                field_schema=field_schema,
            )
        except Exception as e:
            if "already exists" not in str(e).lower():
                raise

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

    def _build_filter(
        self,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ):
        from qdrant_client import models
        import re

        must = []
        if source is not None:
            must.append(models.FieldCondition(key="source", match=models.MatchValue(value=source)))
        if doc_path is not None:
            must.append(models.FieldCondition(key="doc_path", match=models.MatchValue(value=doc_path)))
        if doc_path_prefix is not None:
            must.append(models.FieldCondition(key="doc_path", match=models.MatchRegex(regex="^" + re.escape(doc_path_prefix))))
        
        return models.Filter(must=must) if must else None

    def search(
        self,
        vector: list[float],
        topk: int,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ) -> list[dict]:
        query_filter = self._build_filter(source, doc_path, doc_path_prefix)
        hits = self.client.search(
            self.collection,
            query_vector=vector,
            limit=topk,
            query_filter=query_filter,
            with_payload=True,
        )
        return [{"score": h.score, "payload": h.payload} for h in hits]

    def search_text(
        self,
        query: str,
        topk: int,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ) -> list[dict]:
        from qdrant_client import models
        import re

        must = []
        if source is not None:
            must.append(models.FieldCondition(key="source", match=models.MatchValue(value=source)))
        if doc_path is not None:
            must.append(models.FieldCondition(key="doc_path", match=models.MatchValue(value=doc_path)))
        if doc_path_prefix is not None:
            must.append(models.FieldCondition(key="doc_path", match=models.MatchRegex(regex="^" + re.escape(doc_path_prefix))))
        
        must.append(models.FieldCondition(key="text", match=models.MatchText(text=query)))

        scroll_filter = models.Filter(must=must)
        res, _ = self.client.scroll(
            collection_name=self.collection,
            scroll_filter=scroll_filter,
            limit=topk,
            with_payload=True,
        )
        return [{"score": 1.0, "payload": r.payload} for r in res]

    def drop(self) -> None:
        if self._exists():
            self.client.delete_collection(self.collection)

    def swap_collection(self, shadow_name: str) -> bool:
        """Qdrant: promote shadow via alias. Drops old physical collection first
        (the shadow already contains all verified data, so this is lossless)."""
        from qdrant_client import models

        # If a physical collection with the active name exists, drop it so the
        # alias can be created. The shadow already holds the verified data.
        if self._exists():
            self.client.delete_collection(self.collection)
        self.client.update_collection_aliases(
            change_aliases_operations=[
                models.CreateAliasOperation(
                    create_alias=models.CreateAlias(
                        collection_name=shadow_name,
                        alias_name=self.collection,
                    )
                )
            ]
        )
        return True

    def close(self) -> None:
        if hasattr(self, "client") and self.client:
            try:
                self.client.close()
            except Exception:
                pass


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

    def _build_where(
        self,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
        text_query: str | None = None,
    ) -> tuple[str, list]:
        clauses = []
        params = []
        if source is not None:
            clauses.append("source = %s")
            params.append(source)
        if doc_path is not None:
            clauses.append("doc_path = %s")
            params.append(doc_path)
        if doc_path_prefix is not None:
            clauses.append("doc_path LIKE %s")
            params.append(doc_path_prefix + "%")
        if text_query is not None:
            clauses.append("text ILIKE %s")
            params.append("%" + text_query + "%")
        
        where_clause = ""
        if clauses:
            where_clause = "WHERE " + " AND ".join(clauses)
        return where_clause, params

    def search(
        self,
        vector: list[float],
        topk: int,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ) -> list[dict]:
        op = self._ORDER[self.distance]
        vec = "[" + ",".join(map(str, vector)) + "]"
        where_clause, params = self._build_where(source, doc_path, doc_path_prefix)
        query_params = [vec] + params + [topk]
        rows = self.conn.execute(
            f"SELECT source, doc_path, text, embedding {op} %s AS dist "
            f"FROM {self.table} {where_clause} ORDER BY dist ASC LIMIT %s",
            query_params,
        ).fetchall()
        out = []
        for src, doc_path_val, text, dist in rows:
            out.append({"score": 1.0 - float(dist), "payload": {"source": src, "doc_path": doc_path_val, "text": text}})
        return out

    def search_text(
        self,
        query: str,
        topk: int,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ) -> list[dict]:
        where_clause, params = self._build_where(source, doc_path, doc_path_prefix, text_query=query)
        query_params = params + [topk]
        rows = self.conn.execute(
            f"SELECT source, doc_path, text "
            f"FROM {self.table} {where_clause} LIMIT %s",
            query_params,
        ).fetchall()
        out = []
        for src, doc_path_val, text in rows:
            out.append({"score": 1.0, "payload": {"source": src, "doc_path": doc_path_val, "text": text}})
        return out

    def drop(self) -> None:
        self.conn.execute(f"DROP TABLE IF EXISTS {self.table}")

    def swap_collection(self, shadow_name: str) -> bool:
        """pgvector: transactional table rename for atomic promotion."""
        import psycopg

        old_table = self.table
        shadow_table = _sanitize(shadow_name)
        try:
            with self.conn.transaction():
                self.conn.execute(f"ALTER TABLE IF EXISTS {old_table} RENAME TO {old_table}_old")
                self.conn.execute(f"ALTER TABLE {shadow_table} RENAME TO {old_table}")
                self.conn.execute(f"DROP TABLE IF EXISTS {old_table}_old")
        except psycopg.Error:
            return False
        return True

    def close(self) -> None:
        if hasattr(self, "conn") and self.conn:
            self.conn.close()


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

    def _build_expr(
        self,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
        text_query: str | None = None,
    ) -> str | None:
        exprs = []
        if source is not None:
            exprs.append(f"source == {_milvus_literal(source)}")
        if doc_path is not None:
            exprs.append(f"doc_path == {_milvus_literal(doc_path)}")
        if doc_path_prefix is not None:
            exprs.append(f"doc_path like {_milvus_literal(doc_path_prefix + '%')}")
        if text_query is not None:
            exprs.append(f"text like {_milvus_literal('%' + text_query + '%')}")

        return " and ".join(exprs) if exprs else None

    def search(
        self,
        vector: list[float],
        topk: int,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ) -> list[dict]:
        expr = self._build_expr(source, doc_path, doc_path_prefix)
        res = self.client.search(
            self.collection, data=[vector], limit=topk,
            filter=expr,
            output_fields=["source", "doc_path", "text"],
        )[0]
        return [{"score": h["distance"], "payload": h["entity"]} for h in res]

    def search_text(
        self,
        query: str,
        topk: int,
        source: str | None = None,
        doc_path: str | None = None,
        doc_path_prefix: str | None = None,
    ) -> list[dict]:
        expr = self._build_expr(source, doc_path, doc_path_prefix, text_query=query)
        res = self.client.query(
            self.collection,
            filter=expr,
            limit=topk,
            output_fields=["source", "doc_path", "text"],
        )
        return [{"score": 1.0, "payload": h} for h in res]

    def drop(self) -> None:
        if self.client.has_collection(self.collection):
            self.client.drop_collection(self.collection)

    def swap_collection(self, shadow_name: str) -> bool:
        """Milvus: promote shadow via alter_alias."""
        try:
            self.client.alter_alias(
                collection_name=shadow_name,
                alias=self.collection,
            )
            return True
        except Exception:
            return False

    def close(self) -> None:
        if hasattr(self, "client") and self.client:
            try:
                self.client.close()
            except Exception:
                pass


def _sanitize(name: str) -> str:
    return "".join(c if c.isalnum() or c == "_" else "_" for c in name)


def _milvus_literal(value: str) -> str:
    return json.dumps(value)
