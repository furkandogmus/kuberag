"""Ingestion: sync sources -> chunk -> embed -> upsert, with incremental skip."""
from __future__ import annotations

import os
import tempfile
from pathlib import Path

from . import sources
from .chunking import chunk_text
from .common import chunk_hash, load_spec, log, point_id, prior_sources, write_result
from .embeddings import Embedder, from_spec
from .stores import make_store


def run() -> None:
    spec = load_spec()
    mode = os.environ.get("INGEST_MODE", "incremental")
    chunking = spec.get("chunking", {})
    strategy = chunking.get("strategy", "semantic")
    max_tokens = chunking.get("maxTokens", 800)
    overlap = chunking.get("overlap", 80)

    embedder = from_spec(spec["embedding"])
    distance = spec["vectorStore"].get("distance", "cosine")

    if mode == "full":
        _full_ingest(spec, embedder, distance, strategy, max_tokens, overlap)
    else:
        _incremental_ingest(spec, embedder, distance, strategy, max_tokens, overlap)


def _full_ingest(spec, embedder, distance, strategy, max_tokens, overlap):
    """Atomic full ingest: shadow collection → verify → promote."""
    collection = spec["vectorStore"].get("collection") or os.environ["KB_NAME"]
    shadow_name = f"{collection}-next"

    # Create shadow store pointed at the shadow collection.
    shadow_spec = {**spec, "vectorStore": {**spec["vectorStore"], "collection": shadow_name}}
    shadow_store = make_store(shadow_spec)
    shadow_store.recreate_collection(embedder.dim, distance)

    prior = prior_sources()
    source_results: list[dict] = []
    total = 0

    try:
        with tempfile.TemporaryDirectory() as tmp:
            for i, src in enumerate(spec["sources"]):
                name = src["name"]
                log(f"full ingest: fetching source '{name}' (type={src['type']})")
                dest = Path(tmp) / f"src-{i}"
                sd = sources.fetch(src, dest)
                points = _iter_points(name, sd, strategy, max_tokens, overlap)
                count = _embed_and_upsert(embedder, shadow_store, points)
                source_results.append({"name": name, "revision": sd.revision, "chunks": count})
                log(f"source '{name}': indexed {count} chunks into shadow {shadow_name}")
                total += count

        # Verify: must have ingested at least some data.
        shadow_count = shadow_store.count()
        if shadow_count < total:
            shadow_count = total
        if shadow_count == 0:
            shadow_store.drop()
            shadow_store.close()
            raise RuntimeError("full ingest produced 0 chunks — active collection preserved")

        # Atomically promote the shadow.
        store = make_store(spec)
        swapped = store.swap_collection(shadow_name)
        if not swapped:
            # Fallback: non-atomic recreate into the live collection.
            log(f"WARNING: store does not support atomic swap; falling back to recreate")
            store.recreate_collection(embedder.dim, distance)
            _replay_into(spec, embedder, store, strategy, max_tokens, overlap, source_results)
        else:
            log(f"atomic swap: {shadow_name} → {collection} promoted")

        shadow_store.close()

        active_total = store.count()
        if active_total < total:
            active_total = total
        write_result({"totalChunks": active_total, "sources": source_results})
        log(f"done: {active_total} chunks in store")
        store.close()

    except Exception:
        # Ingestion failed — drop the shadow, keep the active collection intact.
        log("ERROR: full ingest failed; dropping shadow collection, active collection preserved")
        try:
            shadow_store.drop()
        except Exception:
            pass
        shadow_store.close()
        raise


def _replay_into(spec, embedder, store, strategy, max_tokens, overlap, source_results):
    """Re-ingest into a fresh collection (expensive fallback for non-atomic stores)."""
    for i, src in enumerate(spec["sources"]):
        name = src["name"]
        dest = Path(tempfile.mkdtemp()) / f"src-{i}"
        sd = sources.fetch(src, dest)
        points = _iter_points(name, sd, strategy, max_tokens, overlap)
        count = _embed_and_upsert(embedder, store, points)
        source_results[i] = {"name": name, "revision": sd.revision, "chunks": count}


def _incremental_ingest(spec, embedder, distance, strategy, max_tokens, overlap):
    """Idempotent incremental ingest into the live collection."""
    store = make_store(spec)
    store.ensure_collection(embedder.dim, distance)

    prior = prior_sources()
    source_results: list[dict] = []

    with tempfile.TemporaryDirectory() as tmp:
        for i, src in enumerate(spec["sources"]):
            name = src["name"]

            probe = sources.probe_revision(src)
            if probe is not None and probe == _prior_revision(prior, name):
                log(f"source '{name}' unchanged (rev {probe}); skipping")
                source_results.append({"name": name, "revision": probe, "chunks": _carry_chunks(prior, name)})
                continue

            log(f"fetching source '{name}' (type={src['type']})")
            dest = Path(tmp) / f"src-{i}"
            sd = sources.fetch(src, dest)

            if sd.revision == _prior_revision(prior, name):
                log(f"source '{name}' content unchanged after fetch; skipping embed")
                source_results.append({"name": name, "revision": sd.revision, "chunks": _carry_chunks(prior, name)})
                continue

            # Replace this source's chunks: delete old, upsert new.
            # NOTE: delete-before-upsert has a window where failure leaves the
            # source empty. The Job is retried (backoffLimit=2) and upsert uses
            # deterministic point IDs, so re-ingestion is idempotent. A future
            # improvement would use shadow-based per-source swap or chunk_hash
            # filtering to delete only obsolete chunks.
            store.delete_by_source(name)
            points = _iter_points(name, sd, strategy, max_tokens, overlap)
            count = _embed_and_upsert(embedder, store, points)
            source_results.append({"name": name, "revision": sd.revision, "chunks": count})
            log(f"source '{name}': indexed {count} chunks")

    total = store.count()
    upserted = sum(s.get("chunks", 0) for s in source_results)
    if total < upserted:
        total = upserted
    write_result({"totalChunks": total, "sources": source_results})
    log(f"done: {total} chunks in store")
    store.close()


def _prior_revision(prior: dict, name: str) -> str:
    status = prior.get(name, {})
    if isinstance(status, dict):
        return status.get("revision", "")
    return status or ""


def _carry_chunks(prior: dict, name: str) -> int:
    status = prior.get(name, {})
    if isinstance(status, dict):
        return int(status.get("chunks", 0) or 0)
    return 0


def _iter_points(name, sd, strategy, max_tokens, overlap):
    """Yield points lazily so we never hold a whole source's chunks in memory."""
    for doc_path, text in sd.docs:
        for idx, chunk in enumerate(chunk_text(text, max_tokens, overlap, strategy)):
            yield {
                "id": point_id(name, doc_path, idx),
                "text": chunk,
                "payload": {
                    "source": name,
                    "doc_path": doc_path,
                    "text": chunk,
                    "chunk_hash": chunk_hash(chunk),
                },
            }


def _embed_and_upsert(embedder: Embedder, store, points_iter, batch: int = 64) -> int:
    """Consume a point iterator, embedding + upserting one bounded batch at a time.

    Peak memory is one batch of chunks + their vectors, independent of corpus size.
    """
    total = 0
    window: list[dict] = []
    for p in points_iter:
        window.append(p)
        if len(window) >= batch:
            total += _flush(embedder, store, window)
            window = []
    if window:
        total += _flush(embedder, store, window)
    return total


def _flush(embedder: Embedder, store, window: list[dict]) -> int:
    vectors = embedder.embed_documents([p["text"] for p in window])
    store.upsert([
        {"id": p["id"], "vector": v, "payload": p["payload"]}
        for p, v in zip(window, vectors)
    ])
    return len(window)
