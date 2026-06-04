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

The manager serves Prometheus metrics on `:8080/metrics`.

| Metric | Labels | Meaning |
|--------|--------|---------|
| `rag_knowledgebase_ingestions_total` | `knowledgebase`, `result` | Ingestion Jobs completed (`succeeded`/`failed`). |
| `rag_knowledgebase_indexed_chunks` | `knowledgebase` | Chunks currently indexed. |
| `rag_knowledgebase_recall_percent` | `knowledgebase` | Last measured recall@k. |
| `rag_knowledgebase_autotune_attempts` | `knowledgebase` | Auto-tune iterations applied. |
| `rag_knowledgebase_autotune_best_recall_percent` | `knowledgebase` | Best recall@k observed across auto-tune attempts (the config the KB reverts to if the target is never met). |

Plus the standard controller-runtime metrics (`controller_runtime_reconcile_total`,
`controller_runtime_reconcile_errors_total`, workqueue depth/latency, …).

### Wiring Prometheus + Grafana

```bash
kubectl apply -f config/observability/metrics-service.yaml
# With the Prometheus Operator installed:
kubectl apply -f config/observability/servicemonitor.yaml
# Import the dashboard:
config/observability/grafana-dashboard.json
```

The dashboard has a `knowledgebase` template variable and panels for indexed
chunks, recall, ingestion rate by result, auto-tune attempts, and reconcile
errors.

### Example alerts

```yaml
groups:
  - name: kuberag
    rules:
      - alert: KuberagRecallBelowTarget
        expr: rag_knowledgebase_recall_percent < 70
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: "Low retrieval recall for {{ $labels.knowledgebase }}"
      - alert: KuberagIngestionFailing
        expr: increase(rag_knowledgebase_ingestions_total{result="failed"}[1h]) > 0
        labels: { severity: warning }
        annotations:
          summary: "Ingestion failures for {{ $labels.knowledgebase }}"
```
