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
    store = make_store(spec)
    distance = spec["vectorStore"].get("distance", "cosine")

    if mode == "full":
        log(f"full ingest: recreating collection (dim={embedder.dim})")
        store.recreate_collection(embedder.dim, distance)
    else:
        store.ensure_collection(embedder.dim, distance)

    prior = prior_sources()
    source_results: list[dict] = []

    with tempfile.TemporaryDirectory() as tmp:
        for i, src in enumerate(spec["sources"]):
            name = src["name"]

            # Incremental skip: if the cheap revision probe matches what we last
            # ingested (and we're not doing a full rebuild), leave the source's
            # points untouched.
            if mode != "full":
                probe = sources.probe_revision(src)
                if probe is not None and probe == prior.get(name):
                    log(f"source '{name}' unchanged (rev {probe}); skipping")
                    source_results.append({"name": name, "revision": probe, "chunks": _carry_chunks(prior, name)})
                    continue

            log(f"fetching source '{name}' (type={src['type']})")
            dest = Path(tmp) / f"src-{i}"
            sd = sources.fetch(src, dest)

            if mode != "full" and sd.revision == prior.get(name):
                log(f"source '{name}' content unchanged after fetch; skipping embed")
                source_results.append({"name": name, "revision": sd.revision, "chunks": _carry_chunks(prior, name)})
                continue

            # Replace this source's chunks wholesale (idempotent for changed docs).
            if mode != "full":
                store.delete_by_source(name)

            points = _iter_points(name, sd, strategy, max_tokens, overlap)
            count = _embed_and_upsert(embedder, store, points)
            source_results.append({"name": name, "revision": sd.revision, "chunks": count})
            log(f"source '{name}': indexed {count} chunks")

    total = store.count()
    write_result({"totalChunks": total, "sources": source_results})
    log(f"done: {total} chunks in store")


def _carry_chunks(prior: dict, name: str) -> int:
    # Revision-only prior doesn't carry chunk counts; report 0 so the operator
    # relies on the store total. (Total is authoritative in status.indexedChunks.)
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
