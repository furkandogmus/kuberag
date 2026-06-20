"""Backup: export vector store collection data to S3-compatible object storage."""
from __future__ import annotations

import gzip
import json
import os
import tempfile
import time

from .common import load_spec, log, write_result
from .stores import make_store


def _s3_client(endpoint: str, region: str, access_key: str, secret_key: str):
    import boto3

    return boto3.client(
        "s3",
        endpoint_url=endpoint,
        region_name=region,
        aws_access_key_id=access_key,
        aws_secret_access_key=secret_key,
    )


def run() -> None:
    spec = load_spec()
    store = make_store(spec)
    backup_id = os.environ.get("BACKUP_ID", str(int(time.time())))
    endpoint = os.environ["BACKUP_S3_ENDPOINT"]
    region = os.environ["BACKUP_S3_REGION"]
    bucket = os.environ["BACKUP_S3_BUCKET"]
    prefix = os.environ.get("BACKUP_S3_PREFIX", "kuberag-backups")
    access_key = os.environ.get("BACKUP_S3_ACCESS_KEY", "")
    secret_key = os.environ.get("BACKUP_S3_SECRET_KEY", "")
    kb_name = os.environ["KB_NAME"]

    s3 = _s3_client(endpoint, region, access_key, secret_key)
    vs = spec["vectorStore"]
    store_type = vs["type"]

    points = _export_points(store, store_type)

    tmp = tempfile.NamedTemporaryFile(mode="w", suffix=".jsonl.gz", delete=False)
    try:
        with gzip.GzipFile(fileobj=tmp, mode="w") as gz:
            for pt in points:
                gz.write((json.dumps(pt) + "\n").encode())
        tmp.close()

        key = f"{prefix.rstrip('/')}/{kb_name}/{backup_id}/points.jsonl.gz"
        file_size = os.path.getsize(tmp.name)
        s3.upload_file(tmp.name, bucket, key)

        location = f"s3://{bucket}/{key}"
        log(f"uploaded {len(points)} points ({file_size} bytes) to {location}")

        write_result({
            "backupID": backup_id,
            "totalPoints": len(points),
            "sizeBytes": file_size,
            "location": location,
        })
    finally:
        try:
            os.unlink(tmp.name)
        except OSError:
            pass
        store.close()


def _export_points(store, store_type: str) -> list[dict]:
    """Export all points from the vector store as a list of dicts.

    Each point dict has: id, vector, payload (source, doc_path, text, chunk_hash).
    """
    if store_type == "qdrant":
        return _export_qdrant(store)
    elif store_type == "pgvector":
        return _export_pgvector(store)
    elif store_type == "milvus":
        return _export_milvus(store)
    else:
        raise ValueError(f"unsupported store type for backup: {store_type}")


def _export_qdrant(store) -> list[dict]:
    points = []
    offset = None
    while True:
        res, next_offset = store.client.scroll(
            collection_name=store.collection,
            limit=500,
            offset=offset,
            with_payload=True,
            with_vectors=True,
        )
        for r in res:
            points.append({
                "id": str(r.id),
                "vector": r.vector.get("", []) if isinstance(r.vector, dict) else (r.vector or []),
                "payload": {
                    "source": r.payload.get("source", ""),
                    "doc_path": r.payload.get("doc_path", ""),
                    "text": r.payload.get("text", ""),
                    "chunk_hash": r.payload.get("chunk_hash", ""),
                },
            })
        offset = next_offset
        if next_offset is None:
            break
    return points


def _export_pgvector(store) -> list[dict]:
    import psycopg.sql

    rows = store.conn.execute(
        psycopg.sql.SQL(
            "SELECT id, source, doc_path, text, chunk_hash, embedding::text FROM {}"
        ).format(store._tbl)
    ).fetchall()
    points = []
    for row in rows:
        vec_str = row[5]
        vector = json.loads(vec_str) if vec_str else []
        points.append({
            "id": row[0],
            "vector": vector,
            "payload": {
                "source": row[1] or "",
                "doc_path": row[2] or "",
                "text": row[3] or "",
                "chunk_hash": row[4] or "",
            },
        })
    return points


def _export_milvus(store) -> list[dict]:
    try:
        store.client.load_collection(store.collection)
    except Exception:
        pass
    time.sleep(2)
    # Milvus query has a default limit; iterate with offset.
    points = []
    offset = 0
    batch = 500
    while True:
        res = store.client.query(
            store.collection,
            filter="",
            output_fields=["source", "doc_path", "text"],
            limit=batch,
            offset=offset,
        )
        if not res:
            break
        for entity in res:
            points.append({
                "id": str(entity.get("id", "")),
                "vector": entity.get("vector", []),
                "payload": {
                    "source": entity.get("source", ""),
                    "doc_path": entity.get("doc_path", ""),
                    "text": entity.get("text", ""),
                    "chunk_hash": entity.get("chunk_hash", ""),
                },
            })
        if len(res) < batch:
            break
        offset += batch
    return points
