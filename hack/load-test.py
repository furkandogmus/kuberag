#!/usr/bin/env python3
"""Dependency-free concurrent load test for a kuberag Retriever."""
from __future__ import annotations

import argparse
import concurrent.futures
import json
import math
import statistics
import time
import urllib.error
import urllib.request


def percentile(values: list[float], percentile_value: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = min(
        len(ordered) - 1,
        max(0, math.ceil(percentile_value / 100 * len(ordered)) - 1),
    )
    return ordered[index]


def request_once(
    url: str,
    query: str,
    api_key: str,
    timeout: float,
) -> tuple[int, float]:
    body = json.dumps({"query": query}).encode()
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    request = urllib.request.Request(url, data=body, headers=headers, method="POST")
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            response.read()
            status = response.status
    except urllib.error.HTTPError as exc:
        exc.read()
        status = exc.code
    except (OSError, TimeoutError):
        status = 0
    return status, time.perf_counter() - started


def run_load(
    url: str,
    query: str,
    requests: int,
    concurrency: int,
    api_key: str = "",
    timeout: float = 30,
) -> dict:
    started = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
        results = list(pool.map(
            lambda _: request_once(url, query, api_key, timeout),
            range(requests),
        ))
    elapsed = time.perf_counter() - started
    statuses: dict[str, int] = {}
    latencies = []
    for status, latency in results:
        statuses[str(status)] = statuses.get(str(status), 0) + 1
        latencies.append(latency)
    errors = sum(count for status, count in statuses.items() if not status.startswith("2"))
    return {
        "requests": requests,
        "concurrency": concurrency,
        "elapsedSeconds": round(elapsed, 3),
        "requestsPerSecond": round(requests / elapsed, 2) if elapsed else 0,
        "errorRate": round(errors / requests, 4) if requests else 0,
        "statusCounts": statuses,
        "latencyMillis": {
            "mean": round(statistics.fmean(latencies) * 1000, 2) if latencies else 0,
            "p50": round(percentile(latencies, 50) * 1000, 2),
            "p95": round(percentile(latencies, 95) * 1000, 2),
            "p99": round(percentile(latencies, 99) * 1000, 2),
            "max": round(max(latencies) * 1000, 2) if latencies else 0,
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True, help="Full /query endpoint URL")
    parser.add_argument("--query", default="What is kuberag?")
    parser.add_argument("--requests", type=int, default=100)
    parser.add_argument("--concurrency", type=int, default=10)
    parser.add_argument("--api-key", default="")
    parser.add_argument("--timeout", type=float, default=30)
    parser.add_argument("--max-p99-ms", type=float, default=2000)
    parser.add_argument("--max-error-rate", type=float, default=0.01)
    args = parser.parse_args()
    if args.requests < 1 or args.concurrency < 1:
        parser.error("--requests and --concurrency must be positive")

    result = run_load(
        args.url,
        args.query,
        args.requests,
        args.concurrency,
        args.api_key,
        args.timeout,
    )
    print(json.dumps(result, indent=2, sort_keys=True))
    if result["errorRate"] > args.max_error_rate:
        return 1
    if result["latencyMillis"]["p99"] > args.max_p99_ms:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
