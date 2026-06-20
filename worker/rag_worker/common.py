"""Shared helpers: spec loading, result reporting, point ids, logging."""
from __future__ import annotations

import hashlib
import json
import os
import sys
import uuid


def log(msg: str) -> None:
    print(f"[rag-worker] {msg}", flush=True)


def load_spec() -> dict:
    """Load the KnowledgeBase spec from the mounted ConfigMap file.

    The operator mounts a ConfigMap at /etc/kuberag/spec.json. Falls back to
    the legacy KB_SPEC_JSON env var for backward compatibility.
    """
    path = os.environ.get("KB_SPEC_PATH", "")
    if path and os.path.isfile(path):
        try:
            with open(path, "r", encoding="utf-8") as f:
                return json.loads(f.read())
        except (json.JSONDecodeError, OSError):
            pass
    raw = os.environ.get("KB_SPEC_JSON")
    if not raw:
        sys.exit("KB_SPEC_PATH is not set and no KB_SPEC_JSON fallback")
    return json.loads(raw)


def prior_sources() -> dict[str, dict]:
    """Map source name -> last-synced status, from the operator."""
    raw = os.environ.get("PRIOR_SOURCES_JSON", "") or "[]"
    try:
        items = json.loads(raw) or []
    except json.JSONDecodeError:
        return {}
    return {
        s["name"]: {
            "revision": s.get("revision", ""),
            "chunks": int(s.get("chunks", 0) or 0),
        }
        for s in items
        if "name" in s
    }


def chunk_hash(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()[:16]


def point_id(source_name: str, doc_path: str, index: int) -> str:
    """Deterministic id so re-ingesting a doc overwrites rather than duplicates."""
    return str(uuid.uuid5(uuid.NAMESPACE_URL, f"{source_name}|{doc_path}|{index}"))


def write_result(data: dict) -> None:
    """Publish the worker result to a ConfigMap the operator reads.

    Also prints it so it is visible in pod logs.
    """
    log(f"RESULT {json.dumps(data)}")
    name = os.environ.get("RESULT_CONFIGMAP")
    namespace = os.environ.get("KB_NAMESPACE")
    if not name or not namespace:
        return
    try:
        from kubernetes import client, config

        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()
        api = client.CoreV1Api()
        body = client.V1ConfigMap(
            metadata=client.V1ObjectMeta(name=name, namespace=namespace),
            data={"result.json": json.dumps(data)},
        )
        try:
            api.create_namespaced_config_map(namespace=namespace, body=body)
        except client.ApiException as e:
            if e.status == 409:
                api.replace_namespaced_config_map(name=name, namespace=namespace, body=body)
            else:
                raise
    except Exception as e:  # never fail the Job solely because reporting failed
        log(f"WARNING: could not write result ConfigMap: {e}")
