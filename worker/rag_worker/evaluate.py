"""Retrieval-quality evaluation: run a query dataset, compute recall@k + latency."""
from __future__ import annotations

import json
import os
import time

from .common import load_spec, log, write_result
from .embeddings import from_spec
from .stores import make_store


def _load_dataset() -> list[dict]:
    """Read the eval dataset from the mounted ConfigMap.

    The operator passes EVAL_DATASET_CONFIGMAP; the worker reads it via the API.
    Each line of key "dataset.jsonl" is {"query": str, "expectedSources": [str]}.
    """
    name = os.environ.get("EVAL_DATASET_CONFIGMAP")
    namespace = os.environ.get("KB_NAMESPACE")
    from kubernetes import client, config

    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    cm = client.CoreV1Api().read_namespaced_config_map(name=name, namespace=namespace)
    raw = cm.data.get("dataset.jsonl", "")
    rows = []
    for line in raw.splitlines():
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


def run() -> None:
    spec = load_spec()
    topk = int(os.environ.get("EVAL_TOPK", "8"))
    dataset = _load_dataset()
    if not dataset:
        write_result({"recallPercent": 0, "p95LatencyMillis": 0, "queries": 0})
        log("eval dataset empty")
        return

    embedder = from_spec(spec["embedding"])
    store = make_store(spec)

    hits = 0
    latencies: list[float] = []
    for row in dataset:
        expected = set(row.get("expectedSources", []))
        qv = embedder.embed_query(row["query"])
        t0 = time.perf_counter()
        results = store.search(qv, topk)
        latencies.append((time.perf_counter() - t0) * 1000.0)

        retrieved_paths = {r["payload"].get("doc_path", "") for r in results}
        # A query "hits" if any expected source appears in the retrieved doc paths
        # (substring match tolerates path prefixes like repo/owner/...).
        if _matches(expected, retrieved_paths):
            hits += 1

    recall = round(100 * hits / len(dataset))
    p95 = round(_percentile(latencies, 95))
    write_result({"recallPercent": recall, "p95LatencyMillis": p95, "queries": len(dataset)})
    log(f"eval: recall={recall}% p95={p95}ms over {len(dataset)} queries")


def _matches(expected: set[str], retrieved: set[str]) -> bool:
    if not expected:
        return True
    for exp in expected:
        for got in retrieved:
            if exp in got or got in exp:
                return True
    return False


def _percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = max(0, min(len(s) - 1, int(round((pct / 100.0) * (len(s) - 1)))))
    return s[k]
