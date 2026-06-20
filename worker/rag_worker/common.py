"""Shared helpers: spec loading, result reporting, point ids, logging, tracing."""
from __future__ import annotations

import hashlib
import json
import os
import sys
import time
import uuid


def _noop_tracer():
    """Return a tracer-like object that silently discards all span operations."""
    class _Span:
        def __enter__(self): return self
        def __exit__(self, *a): pass
        def set_attribute(self, *a): pass
        def set_attributes(self, *a): pass
        def add_event(self, *a): pass
        def record_exception(self, *a): pass
        def set_status(self, *a): pass
    class _Tracer:
        def start_as_current_span(self, *a, **kw):
            return _Span()
        def start_span(self, *a, **kw):
            return _Span()
    return _Tracer()

try:
    from opentelemetry import trace
    tracer = trace.get_tracer("kuberag-worker")
except ImportError:
    tracer = _noop_tracer()


def init_tracing() -> None:
    """Initialise OpenTelemetry if OTEL_EXPORTER_OTLP_ENDPOINT is set."""
    endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
    if not endpoint:
        return
    try:
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.resources import SERVICE_NAME, Resource
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
        from opentelemetry.sdk.trace.export import BatchSpanProcessor

        resource = Resource(attributes={SERVICE_NAME: "kuberag-worker"})
        provider = TracerProvider(resource=resource)
        exporter = OTLPSpanExporter(endpoint=endpoint)
        provider.add_span_processor(BatchSpanProcessor(exporter))
        trace.set_tracer_provider(provider)
    except Exception:
        pass


_log_burst = 30
_log_interval = 10.0
_log_tokens = _log_burst
_log_last = time.monotonic()


def log(msg: str) -> None:
    """Rate-limited logging to avoid overloading kubelet/journald."""
    global _log_tokens, _log_last
    now = time.monotonic()
    elapsed = now - _log_last
    _log_tokens = min(_log_burst, _log_tokens + elapsed / _log_interval * _log_burst)
    _log_last = now
    if _log_tokens >= 1:
        _log_tokens -= 1
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


def _kubernetes_api():
    """Lazy-import and configure a CoreV1Api client. Returns (api, client) tuple."""
    from kubernetes import client, config

    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    return client.CoreV1Api(), client


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
        api, k8s_client = _kubernetes_api()
        body = k8s_client.V1ConfigMap(
            metadata=k8s_client.V1ObjectMeta(name=name, namespace=namespace),
            data={"result.json": json.dumps(data)},
        )
        try:
            # The operator pre-creates this ConfigMap and grants the per-KB
            # ServiceAccount update access only to this exact resource name.
            api.replace_namespaced_config_map(name=name, namespace=namespace, body=body)
        except k8s_client.ApiException as e:
            # Backward compatibility for manually-run workers using the legacy
            # shared RBAC manifest.
            if e.status == 404:
                api.create_namespaced_config_map(namespace=namespace, body=body)
            else:
                raise
    except Exception as e:  # never fail the Job solely because reporting failed
        log(f"WARNING: could not write result ConfigMap: {e}")


def write_checkpoint(data: dict) -> None:
    """Write checkpoint progress to a ConfigMap so the next Job can resume.

    Best-effort: never fail the job on checkpoint write failure.
    """
    name = os.environ.get("CHECKPOINT_CONFIGMAP")
    namespace = os.environ.get("KB_NAMESPACE")
    if not name or not namespace:
        return
    try:
        api, k8s_client = _kubernetes_api()
        body = k8s_client.V1ConfigMap(
            metadata=k8s_client.V1ObjectMeta(name=name, namespace=namespace),
            data={"checkpoint.json": json.dumps(data)},
        )
        api.replace_namespaced_config_map(name=name, namespace=namespace, body=body)
    except Exception as e:
        log(f"WARNING: could not write checkpoint ConfigMap: {e}")


def read_checkpoint() -> dict | None:
    """Read checkpoint progress from the ConfigMap, if any.

    Returns the parsed dict or None.
    """
    name = os.environ.get("CHECKPOINT_CONFIGMAP")
    namespace = os.environ.get("KB_NAMESPACE")
    if not name or not namespace:
        return None
    try:
        api, _ = _kubernetes_api()
        cm = api.read_namespaced_config_map(name=name, namespace=namespace)
        raw = cm.data.get("checkpoint.json", "") if cm.data else ""
        if raw:
            return json.loads(raw)
    except Exception as e:
        log(f"WARNING: could not read checkpoint ConfigMap: {e}")
    return None
