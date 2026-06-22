# Observability

kuberag surfaces state three ways: **status/conditions** (for `kubectl`),
**events** (for timelines), and **Prometheus metrics** (for dashboards/alerts).

## Status & conditions

```bash
kubectl get kb        # PHASE, MODEL, CHUNKS, RECALL, LASTINDEXED
kubectl get vi        # HEALTH, POINTS, DIM
kubectl get rtr       # KNOWLEDGEBASE, PHASE, ENDPOINT
kubectl describe kb <name>
```

Conditions follow Kubernetes conventions:

| Resource | Condition types |
|----------|-----------------|
| KnowledgeBase | `Ready`, `Ingesting`, `Evaluated` |
| VectorIndex | `Ready` |
| Retriever | `Available` |

## Events

Emitted on every meaningful transition — tail them with:

```bash
kubectl get events --field-selector involvedObject.name=<kb-name> --watch
```

`IngestionStarted` · `IngestionComplete` · `IngestionFailed` · `EvaluationStarted`
· `RecallMet` · `AutoTuning` · `RecallBelowTarget` · `EvalFailed` · `Cleanup`.

## Metrics

The manager serves Prometheus metrics on `:8080/metrics`. Every Retriever pod
also serves dependency-free Prometheus metrics on a dedicated `:9090/metrics`
port through an owned `<retriever>-retriever-metrics` ClusterIP Service. Keeping
metrics on a separate port prevents API authentication and OIDC routing from
interfering with scraping.

| Metric | Labels | Meaning |
|--------|--------|---------|
| `rag_knowledgebase_ingestions_total` | `namespace`, `result` | Ingestion Jobs completed (`succeeded`/`failed`). |
| `rag_knowledgebase_indexed_chunks` | `namespace` | Chunks currently indexed. |
| `rag_knowledgebase_last_successful_ingestion_timestamp_seconds` | `namespace` | Latest successful ingestion Unix timestamp in the namespace. |
| `rag_knowledgebase_recall_percent` | `namespace` | Last measured recall@k. |
| `rag_knowledgebase_autotune_attempts` | `namespace` | Auto-tune iterations applied. |
| `rag_knowledgebase_autotune_best_recall_percent` | `namespace` | Best recall@k observed across auto-tune attempts. |
| `kuberag_retriever_queries_total` | `result` | Completed queries (`success`, `generation_error`, `error`). |
| `kuberag_retriever_query_duration_seconds` | `result` | Retriever latency histogram for p50/p95/p99 SLOs. |
| `kuberag_retriever_rejected_requests_total` | `reason` | Requests rejected by auth, rate, body, or concurrency guards. |
| `kuberag_retriever_queries_in_flight` | — | Queries currently executing in one pod. |
| `kuberag_retriever_concurrency_limit` | — | Configured per-pod concurrency capacity. |

Plus the standard controller-runtime metrics (`controller_runtime_reconcile_total`,
`controller_runtime_reconcile_errors_total`, workqueue depth/latency, …).

### Cardinality budget

Application metrics must not use KnowledgeBase names, source names, URLs,
document paths, client identifiers/IPs, query text, model names, or other
user-controlled/unbounded values as labels.

- Operator metrics use only Kubernetes `namespace` plus bounded lifecycle
  results. Their budget is at most 52 application series per namespace with
  the current histogram buckets.
- Retriever metrics use three query results, six rejection reasons, and a
  single `other` fallback for unexpected internal values. Their budget is at
  most 74 application series per pod.
- High-cardinality context belongs in structured logs and traces, not metric
  labels.

Go and Python regression tests enforce the declared label sets and collapse
unknown Retriever label values to `other`.

### Wiring Prometheus + Grafana

```bash
kubectl apply -f config/observability/metrics-service.yaml
# With the Prometheus Operator installed:
kubectl apply -f config/observability/servicemonitor.yaml
kubectl apply -f config/observability/retriever-servicemonitor.yaml
kubectl apply -f config/observability/prometheusrule.yaml
# Import the dashboard:
config/observability/grafana-dashboard.json
```

This is the single canonical dashboard artifact. Keep downstream imports pinned
to that path; obsolete dashboard copies under `docs/` were removed to prevent
stale PromQL from surviving after metric schema changes.

The dashboard uses a bounded `namespace` variable and includes indexed chunks,
recall, ingestion rate, auto-tune,
reconcile errors, Retriever request rate, p50/p95/p99 latency, saturation, and
rejected traffic.

With Helm and Prometheus Operator CRDs installed:

```yaml
metrics:
  serviceMonitor:
    enabled: true
  retrieverServiceMonitor:
    enabled: true
  prometheusRule:
    enabled: true
```

The bundled alerts fire on error rate above 5%, p99 above two seconds,
concurrency above 90%, sustained rejected traffic, controller reconcile errors,
and no successful namespace ingestion within the configured freshness
objective (24 hours by default).

## Load validation

Run a repeatable concurrent query test against a port-forwarded or public
Retriever:

```bash
URL=http://localhost:8000/query REQUESTS=500 CONCURRENCY=25 make load-test
```

The harness prints JSON with throughput, status counts, error rate, and
mean/p50/p95/p99/max latency. It exits non-zero when p99 exceeds two seconds or
the error rate exceeds 1%; override with `--max-p99-ms` and
`--max-error-rate` when calling `hack/load-test.py` directly.
