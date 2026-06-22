"""Restore: import vector store collection data from S3-compatible object storage."""
from __future__ import annotations

import gzip
import json
import os
import tempfile

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
    location = os.environ["RESTORE_LOCATION"]
    endpoint = os.environ["RESTORE_S3_ENDPOINT"]
    region = os.environ["RESTORE_S3_REGION"]
    bucket = os.environ["RESTORE_S3_BUCKET"]
    access_key = os.environ.get("RESTORE_S3_ACCESS_KEY", "")
    secret_key = os.environ.get("RESTORE_S3_SECRET_KEY", "")

    s3 = _s3_client(endpoint, region, access_key, secret_key)

    key = location[len(f"s3://{bucket}/"):]

    tmp = tempfile.NamedTemporaryFile(suffix=".jsonl.gz", delete=False)
    try:
        s3.download_file(bucket, key, tmp.name)
        tmp.close()

        points = []
        with gzip.open(tmp.name, "r") as gz:
            for line in gz:
                points.append(json.loads(line.decode()))

        log(f"downloaded {len(points)} points from {location}")

        vs = spec["vectorStore"]
        dim = spec.get("embedding", {}).get("dimension", 768)
        distance = vs.get("distance", "cosine")
        restore_round = int(os.environ.get("RESTORE_ROUND", "1"))
        total = _restore_points(
            spec,
            points,
            dim=dim,
            distance=distance,
            restore_round=restore_round,
        )

        log(f"restore complete: {total} points")
        write_result({"restoredPoints": total})
    finally:
        try:
            os.unlink(tmp.name)
        except OSError:
            pass


def _restore_points(
    spec: dict,
    points: list[dict],
    *,
    dim: int,
    distance: str,
    restore_round: int,
) -> int:
    """Restore into a staging collection and atomically promote on success."""
    store = make_store(spec)
    shadow_name = store.staging_name(restore_round)
    shadow_spec = {
        **spec,
        "vectorStore": {
            **spec["vectorStore"],
            "collection": shadow_name,
        },
    }
    shadow_store = make_store(shadow_spec)
    total = len(points)
    try:
        shadow_store.recreate_collection(dim, distance)
        batch_size = 64
        for i in range(0, total, batch_size):
            batch = points[i:i + batch_size]
            shadow_store.upsert(batch)
            log(f"restored {min(i + batch_size, total)}/{total} points")

        restored = shadow_store.count()
        if restored != total:
            raise RuntimeError(
                f"restore verification failed: expected {total} points, got {restored}"
            )
        if not store.swap_collection(shadow_name):
            raise RuntimeError(
                "vector store does not support atomic restore promotion"
            )
        return total
    except Exception:
        log("ERROR: restore failed; dropping staging collection, active data preserved")
        try:
            shadow_store.drop()
        except Exception:
            pass
        raise
    finally:
        shadow_store.close()
        store.close()
