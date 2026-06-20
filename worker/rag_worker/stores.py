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

    def staging_name(self, round_num: int) -> str:
        """Return a versioned staging collection name."""
        return f"{self.collection}-v{round_num}"

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
        # Also resolve aliases — collection_exists returns True for aliases.
        return self.client.collection_exists(self.collection)

    def staging_name(self, version: int) -> str:
        return f"{self.collection}-v{version}"

    def _ensure_alias(self, physical: str) -> None:
        """Create alias self.collection → physical if it doesn't already exist."""
        from qdrant_client import models
        try:
            self.client.update_collection_aliases(
                change_aliases_operations=[
                    models.CreateAliasOperation(
                        create_alias=models.CreateAlias(
                            collection_name=physical,
                            alias_name=self.collection,
                        )
                    )
                ]
            )
        except Exception as e:
            if "already exists" not in str(e).lower():
                raise

    def ensure_collection(self, dim: int, distance: str) -> None:
        from qdrant_client import models

        if self._exists():
            # Alias or physical exists — verify dimension matches.
            try:
                info = self.client.get_collection(self.collection)
                if info.config.params.vectors.size != dim:
                    self.recreate_collection(dim, distance)
                    return
            except Exception:
                pass
            self._ensure_payload_indexes()
            return

        # Nothing exists. Create v1 physical + alias.
        v1 = self.staging_name(1)
        self.client.create_collection(
            collection_name=v1,
            vectors_config=models.VectorParams(size=dim, distance=self._distance()),
        )
        self._ensure_alias(v1)
        self._ensure_payload_indexes()

    def recreate_collection(self, dim: int, distance: str) -> None:
        """Recreate this exact physical collection."""
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
        aliases = self.client.get_aliases().aliases
        target = next(
            (alias.collection_name for alias in aliases if alias.alias_name == self.collection),
            None,
        )
        if target is not None:
            from qdrant_client import models

            self.client.update_collection_aliases(
                change_aliases_operations=[
                    models.DeleteAliasOperation(
                        delete_alias=models.DeleteAlias(alias_name=self.collection)
                    )
                ]
            )
            if self.client.collection_exists(target):
                self.client.delete_collection(target)
            return
        if self.client.collection_exists(self.collection):
            self.client.delete_collection(self.collection)

    def swap_collection(self, shadow_name: str) -> bool:
        """Qdrant: atomically repoint an existing alias to the staging collection."""
        from qdrant_client import models

        aliases = self.client.get_aliases().aliases
        current_target = next(
            (alias.collection_name for alias in aliases if alias.alias_name == self.collection),
            None,
        )
        alias_exists = current_target is not None
        operations = []
        if alias_exists:
            operations.append(
                models.DeleteAliasOperation(
                    delete_alias=models.DeleteAlias(alias_name=self.collection)
                )
            )
        elif self.client.collection_exists(self.collection):
            # One-time migration from the legacy physical-name layout. Qdrant
            # cannot use a collection and alias with the same name.
            self.client.delete_collection(self.collection)
        operations.append(
            models.CreateAliasOperation(
                create_alias=models.CreateAlias(
                    collection_name=shadow_name,
                    alias_name=self.collection,
                )
            )
        )
        self.client.update_collection_aliases(
            change_aliases_operations=operations
        )
        if current_target and current_target != shadow_name and self.client.collection_exists(current_target):
            self.client.delete_collection(current_target)
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
        import psycopg.sql

        self.table = _sanitize(collection)
        self._tbl = psycopg.sql.Identifier(self.table)
        self._src_idx = psycopg.sql.Identifier(f"{self.table}_source_idx")
        self.distance = distance
        cred = os.environ.get("VECTORSTORE_CREDENTIAL")
        if cred and "@" not in endpoint:
            endpoint = endpoint.replace("postgresql://", f"postgresql://postgres:{cred}@")
        self.conn = psycopg.connect(endpoint, autocommit=True)
        self.conn.execute("CREATE EXTENSION IF NOT EXISTS vector")

    def ensure_collection(self, dim: int, distance: str) -> None:
        import psycopg.sql
        self.conn.execute(
            psycopg.sql.SQL("CREATE TABLE IF NOT EXISTS {} ("
            "id text PRIMARY KEY, source text, doc_path text, text text, "
            "chunk_hash text, embedding vector({}))").format(self._tbl, psycopg.sql.Literal(dim))
        )
        self.conn.execute(
            psycopg.sql.SQL("CREATE INDEX IF NOT EXISTS {} ON {} (source)").format(self._src_idx, self._tbl)
        )

    def recreate_collection(self, dim: int, distance: str) -> None:
        import psycopg.sql
        self.conn.execute(psycopg.sql.SQL("DROP TABLE IF EXISTS {}").format(self._tbl))
        self.ensure_collection(dim, distance)

    def delete_by_source(self, source_name: str) -> None:
        import psycopg.sql
        self.conn.execute(
            psycopg.sql.SQL("DELETE FROM {} WHERE source = %s").format(self._tbl), (source_name,)
        )

    def upsert(self, points: list[dict]) -> None:
        import psycopg.sql
        with self.conn.cursor() as cur:
            cur.executemany(
                psycopg.sql.SQL(
                    "INSERT INTO {} (id, source, doc_path, text, chunk_hash, embedding) "
                    "VALUES (%s, %s, %s, %s, %s, %s) "
                    "ON CONFLICT (id) DO UPDATE SET "
                    "text = EXCLUDED.text, chunk_hash = EXCLUDED.chunk_hash, embedding = EXCLUDED.embedding"
                ).format(self._tbl),
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
        import psycopg.sql
        return self.conn.execute(psycopg.sql.SQL("SELECT count(*) FROM {}").format(self._tbl)).fetchone()[0]

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

    def _sql(self, template: str, *args) -> "psycopg.sql.Composed":
        import psycopg.sql
        return psycopg.sql.SQL(template).format(self._tbl, *args)

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
            self._sql(
                f"SELECT source, doc_path, text, embedding {op} %s AS dist "
                f"FROM {{}} {where_clause} ORDER BY dist ASC LIMIT %s"
            ),
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
            self._sql(
                f"SELECT source, doc_path, text "
                f"FROM {{}} {where_clause} LIMIT %s"
            ),
            query_params,
        ).fetchall()
        out = []
        for src, doc_path_val, text in rows:
            out.append({"score": 1.0, "payload": {"source": src, "doc_path": doc_path_val, "text": text}})
        return out

    def drop(self) -> None:
        import psycopg.sql
        self.conn.execute(psycopg.sql.SQL("DROP TABLE IF EXISTS {}").format(self._tbl))

    def swap_collection(self, shadow_name: str) -> bool:
        """pgvector: transactional table rename for atomic promotion."""
        import psycopg
        import psycopg.sql

        old_tbl = self._tbl
        old_old = psycopg.sql.Identifier(f"{self.table}_old")
        shadow_tbl = psycopg.sql.Identifier(_sanitize(shadow_name))
        try:
            with self.conn.transaction():
                self.conn.execute(
                    psycopg.sql.SQL("ALTER TABLE IF EXISTS {} RENAME TO {}").format(old_tbl, old_old)
                )
                self.conn.execute(
                    psycopg.sql.SQL("ALTER TABLE {} RENAME TO {}").format(shadow_tbl, old_tbl)
                )
                self.conn.execute(
                    psycopg.sql.SQL("DROP TABLE IF EXISTS {}").format(old_old)
                )
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
        try:
            self.client.describe_alias(self.collection)
            return
        except Exception:
            pass
        if self.client.has_collection(self.collection):
            return
        physical = self.staging_name(1)
        self._create_collection(physical, dim)
        self.client.create_alias(collection_name=physical, alias=self.collection)

    def _create_collection(self, collection: str, dim: int) -> None:
        self.client.create_collection(
            collection_name=collection,
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
        self._create_collection(self.collection, dim)

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
        try:
            description = self.client.describe_alias(self.collection)
            target = description.get("collection_name")
            self.client.drop_alias(self.collection)
            if target and self.client.has_collection(target):
                self.client.drop_collection(target)
            return
        except Exception:
            pass
        if self.client.has_collection(self.collection):
            self.client.drop_collection(self.collection)

    def swap_collection(self, shadow_name: str) -> bool:
        """Milvus: promote shadow via alter_alias."""
        try:
            try:
                current = self.client.describe_alias(self.collection).get("collection_name")
                self.client.alter_alias(collection_name=shadow_name, alias=self.collection)
                if current and current != shadow_name and self.client.has_collection(current):
                    self.client.drop_collection(current)
            except Exception:
                if self.client.has_collection(self.collection):
                    self.client.drop_collection(self.collection)
                self.client.create_alias(collection_name=shadow_name, alias=self.collection)
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
