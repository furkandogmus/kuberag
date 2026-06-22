"""Small dependency-free Prometheus registry for the retriever process."""
from __future__ import annotations

import threading
from collections import defaultdict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


_BUCKETS = (0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60)
_QUERY_RESULTS = frozenset({"success", "generation_error", "error"})
_REJECTION_REASONS = frozenset({
    "authentication",
    "body_too_large",
    "invalid_content_length",
    "rate_limit",
    "rate_limit_backend",
    "concurrency",
})


def _bounded_label(value: str, allowed: frozenset[str]) -> str:
    return value if value in allowed else "other"


class RetrieverMetrics:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._queries: dict[str, int] = defaultdict(int)
        self._rejections: dict[str, int] = defaultdict(int)
        self._duration_count: dict[str, int] = defaultdict(int)
        self._duration_sum: dict[str, float] = defaultdict(float)
        self._duration_buckets: dict[str, list[int]] = defaultdict(
            lambda: [0] * len(_BUCKETS)
        )
        self._in_flight = 0
        self._capacity = 0

    def set_capacity(self, value: int) -> None:
        with self._lock:
            self._capacity = max(0, value)

    def query_started(self) -> None:
        with self._lock:
            self._in_flight += 1

    def query_finished(self, result: str, duration_seconds: float) -> None:
        result = _bounded_label(result, _QUERY_RESULTS)
        with self._lock:
            self._in_flight = max(0, self._in_flight - 1)
            self._queries[result] += 1
            self._duration_count[result] += 1
            self._duration_sum[result] += duration_seconds
            for index, boundary in enumerate(_BUCKETS):
                if duration_seconds <= boundary:
                    self._duration_buckets[result][index] += 1

    def rejected(self, reason: str) -> None:
        reason = _bounded_label(reason, _REJECTION_REASONS)
        with self._lock:
            self._rejections[reason] += 1

    def render(self) -> bytes:
        with self._lock:
            lines = [
                "# HELP kuberag_retriever_queries_total Completed query requests.",
                "# TYPE kuberag_retriever_queries_total counter",
            ]
            for result, value in sorted(self._queries.items()):
                lines.append(
                    f'kuberag_retriever_queries_total{{result="{result}"}} {value}'
                )

            lines.extend([
                "# HELP kuberag_retriever_query_duration_seconds Query latency.",
                "# TYPE kuberag_retriever_query_duration_seconds histogram",
            ])
            for result in sorted(self._duration_count):
                for boundary, value in zip(
                    _BUCKETS, self._duration_buckets[result], strict=True
                ):
                    lines.append(
                        "kuberag_retriever_query_duration_seconds_bucket"
                        f'{{result="{result}",le="{boundary:g}"}} {value}'
                    )
                count = self._duration_count[result]
                lines.append(
                    "kuberag_retriever_query_duration_seconds_bucket"
                    f'{{result="{result}",le="+Inf"}} {count}'
                )
                lines.append(
                    "kuberag_retriever_query_duration_seconds_sum"
                    f'{{result="{result}"}} {self._duration_sum[result]:.9f}'
                )
                lines.append(
                    "kuberag_retriever_query_duration_seconds_count"
                    f'{{result="{result}"}} {count}'
                )

            lines.extend([
                "# HELP kuberag_retriever_rejected_requests_total Requests rejected before query execution.",
                "# TYPE kuberag_retriever_rejected_requests_total counter",
            ])
            for reason, value in sorted(self._rejections.items()):
                lines.append(
                    "kuberag_retriever_rejected_requests_total"
                    f'{{reason="{reason}"}} {value}'
                )

            lines.extend([
                "# HELP kuberag_retriever_queries_in_flight Queries currently executing.",
                "# TYPE kuberag_retriever_queries_in_flight gauge",
                f"kuberag_retriever_queries_in_flight {self._in_flight}",
                "# HELP kuberag_retriever_concurrency_limit Configured per-pod query concurrency limit.",
                "# TYPE kuberag_retriever_concurrency_limit gauge",
                f"kuberag_retriever_concurrency_limit {self._capacity}",
                "# HELP kuberag_retriever_build_info Static retriever build information.",
                "# TYPE kuberag_retriever_build_info gauge",
                'kuberag_retriever_build_info{version="v1alpha1"} 1',
                "",
            ])
            return "\n".join(lines).encode()


metrics = RetrieverMetrics()


class _MetricsHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        if self.path not in ("/metrics", "/metrics/"):
            self.send_error(404)
            return
        body = metrics.render()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format: str, *_args) -> None:
        return


def start_metrics_server(port: int = 9090) -> ThreadingHTTPServer:
    server = ThreadingHTTPServer(("0.0.0.0", port), _MetricsHandler)
    thread = threading.Thread(
        target=server.serve_forever,
        name="retriever-prometheus",
        daemon=True,
    )
    thread.start()
    return server
