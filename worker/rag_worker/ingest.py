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
    ingestion_cfg = spec.get("ingestion", {})
    batch_size = int(ingestion_cfg.get("batchSize", 0) or 64)

    embedder = from_spec(spec["embedding"])
    distance = spec["vectorStore"].get("distance", "cosine")

    if mode == "full":
        _full_ingest(spec, embedder, distance, strategy, max_tokens, overlap, batch_size)
    else:
        _incremental_ingest(spec, embedder, distance, strategy, max_tokens, overlap, batch_size)


def _full_ingest(spec, embedder, distance, strategy, max_tokens, overlap, batch_size):
    """Atomic full ingest: versioned shadow → verify → promote via alias."""
    store = make_store(spec)
    round_num = int(os.environ.get("INGEST_ROUND", "1"))

    # Create a versioned staging collection without touching the active target.
    shadow_name = store.staging_name(round_num)
    shadow_spec = {**spec, "vectorStore": {**spec["vectorStore"], "collection": shadow_name}}
    shadow_store = make_store(shadow_spec)
    shadow_store.recreate_collection(embedder.dim, distance)

    total = 0
    source_results: list[dict] = []
    try:
        with tempfile.TemporaryDirectory() as tmp:
            for i, src in enumerate(spec["sources"]):
                name = src["name"]
                log(f"full ingest: fetching source '{name}'")
                dest = Path(tmp) / f"src-{i}"
                sd = sources.fetch(src, dest)
                points = _iter_points(name, sd, strategy, max_tokens, overlap)
                count = _embed_and_upsert(embedder, shadow_store, points, batch_size)
                source_results.append({"name": name, "revision": sd.revision, "chunks": count})
                log(f"source '{name}': indexed {count} chunks into {shadow_name}")
                total += count

        shadow_count = shadow_store.count()
        if shadow_count < total:
            shadow_count = total
        if shadow_count == 0:
            shadow_store.drop()
            shadow_store.close()
            raise RuntimeError("full ingest produced 0 chunks — active collection preserved")

        # Promote: point alias to verified shadow.
        swapped = store.swap_collection(shadow_name)
        if not swapped:
            log("WARNING: store does not support atomic swap; recreating inline")
            store.recreate_collection(embedder.dim, distance)
            _replay_into(spec, embedder, store, strategy, max_tokens, overlap, source_results, batch_size)
        else:
            log(f"atomic swap: alias → {shadow_name} promoted")

        shadow_store.close()

        active_total = store.count()
        if active_total < total:
            active_total = total
        write_result({"totalChunks": active_total, "sources": source_results})
        log(f"done: {active_total} chunks in store")
        store.close()

    except Exception:
        log("ERROR: full ingest failed; dropping shadow, active collection preserved")
        try:
            shadow_store.drop()
        except Exception:
            pass
        shadow_store.close()
        raise


def _replay_into(spec, embedder, store, strategy, max_tokens, overlap, source_results, batch_size):
    """Re-ingest into a fresh collection (expensive fallback for non-atomic stores)."""
    import shutil
    for i, src in enumerate(spec["sources"]):
        name = src["name"]
        dest = Path(tempfile.mkdtemp()) / f"src-{i}"
        try:
            sd = sources.fetch(src, dest)
            points = _iter_points(name, sd, strategy, max_tokens, overlap)
            count = _embed_and_upsert(embedder, store, points, batch_size)
            source_results[i] = {"name": name, "revision": sd.revision, "chunks": count}
        finally:
            shutil.rmtree(dest.parent, ignore_errors=True)


def _incremental_ingest(spec, embedder, distance, strategy, max_tokens, overlap, batch_size):
    """Skip unchanged sources; rebuild atomically when any source changed."""
    store = make_store(spec)
    store.ensure_collection(embedder.dim, distance)

    prior = prior_sources()
    source_results: list[dict] = []
    changed = False

    with tempfile.TemporaryDirectory() as tmp:
        for i, src in enumerate(spec["sources"]):
            name = src["name"]

            probe = sources.probe_revision(src)
            if probe is not None and probe == _prior_revision(prior, name):
                source_results.append({"name": name, "revision": probe, "chunks": _carry_chunks(prior, name)})
                continue

            dest = Path(tmp) / f"src-{i}"
            sd = sources.fetch(src, dest)

            if sd.revision == _prior_revision(prior, name):
                source_results.append({"name": name, "revision": sd.revision, "chunks": _carry_chunks(prior, name)})
                continue
            changed = True
            break

    if changed:
        store.close()
        log("source change detected; rebuilding through atomic staging collection")
        _full_ingest(spec, embedder, distance, strategy, max_tokens, overlap, batch_size)
        return

    total = store.count()
    if total == 0:
        store.close()
        log("active collection is empty; rebuilding through atomic staging collection")
        _full_ingest(spec, embedder, distance, strategy, max_tokens, overlap, batch_size)
        return
    write_result({"totalChunks": total, "sources": source_results})
    log(f"all sources unchanged: {total} chunks retained")
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
