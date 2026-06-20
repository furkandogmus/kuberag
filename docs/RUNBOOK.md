# Operator Runbook

This is the on-call handbook for kuberag. It assumes the operator is
deployed via Helm (`deploy/helm/kuberag/`) and that you have `kubectl`
access to the cluster.

If the issue is **security-related**, see `SECURITY.md` for the
disclosure process before reading this.

## Quick links

- **Architecture**: `docs/ARCHITECTURE.md`
- **API reference**: `docs/API.md` (auto-generated)
- **Configuration / tuning knobs**: `docs/TUNING.md`
- **Provider matrix**: `docs/PROVIDERS.md`
- **Metrics & dashboards**: `docs/OBSERVABILITY.md`
- **Roadmap & known gaps**: `docs/ROADMAP.md`

## Glossary

| Term | Meaning |
|------|---------|
| `KB` | A `KnowledgeBase` CR — the source-of-truth desired state. |
| `VI` | A `VectorIndex` CR — controller-owned per-KB probe target. |
| `Rtr` | A `Retriever` CR — controller-owned FastAPI Deployment/Service. |
| `IR` | An `IngestionRun` CR — immutable per-run audit record. |
| `ActiveJob` | The name of the in-flight ingestion/eval/cleanup Job, tracked on `KB.status`. |
| `corpusHash` | SHA-256 prefix over sources + chunking + embedding + vector store. Triggers re-index. |
| `secretsHash` | SHA-256 prefix over referenced Secret *values*. Cancels in-flight Jobs only. |
| `TTLSecondsAfterFinished` | How long a finished Job (and its result ConfigMap) lingers. Default 300s. |
| `Phase` | `Pending / Ingesting / Ready / Degraded / Failed / Suspended` |

## Phase meanings

| `kb.status.phase` | What it means | What to do |
|-------------------|---------------|-------------|
| `Pending` | KB just created; no ingestion has run yet. | Wait. If it stays `Pending` for >5 min, the operator isn't running or the CRD isn't installed. |
| `Ingesting` | A Job is in flight. | Wait. Check `kb.status.activeJob`. If it's been `Ingesting` for >2h, see "Stuck Job" below. |
| `Ready` | Last ingestion succeeded; store is up to date. | Nothing. |
| `Degraded` | Eval ran but recall < target. Data is queryable but suboptimal. | Look at `kb.status.evaluation.recallPercent` vs `spec.retrievalQuality.minimumRecallPercent`. If `autoTune.enabled`, the operator already retried; if not, enable it. |
| `Failed` | Last ingestion Job exited non-zero. | `kubectl describe kb` → Events. `kubectl logs -l job-name=<active>`. |
| `Suspended` | `spec.suspend: true`. | Intentional. To resume: `kubectl patch kb <name> --type=merge -p '{"spec":{"suspend":false}}'`. |

## Reading status fields

```bash
kubectl get kb <name> -o yaml
```

Key fields under `status`:

- `observedSpecHash` — last spec that was successfully ingested. If
  this is older than the spec you expect, re-ingest hasn't run yet
  or has been failing.
- `observedSecretsHash` — last credential set used. Rotate secrets
  and observe; if this updates but `observedSpecHash` doesn't, your
  re-index is *not* triggered by the rotation (this is intentional).
- `activeJob` — empty means nothing is in flight. If non-empty but
  no Job exists, the operator's been restarted mid-reconcile; the
  next reconcile will detect the missing Job and clear it.
- `ingestRound` — monotonically increases per Job. Useful for
  correlating logs: `kubectl logs -l rag.furkan.dev/job-type=ingest`
  and look for the round number in the Job name.
- `sources[].revision` — last-synced revision per source. Empty
  string means never ingested.
- `indexedChunks` — last-known point count. `0` after a successful
  ingest usually means a `delete_by_source` cleaned up; an empty
  collection is `Degraded` until next eval.
- `evaluation.recallPercent` — last eval's recall@K.
- `autoTuneAttempts` — count of auto-tune iterations. If this is
  > 0 and Phase is `Ready`, the ladder converged to target. If
  Phase is `Degraded`, it exhausted `maxAttempts`.
- `conditions[]` — semantic status. Read
  `meta: True/False/Unknown` and the `reason` field:
  - `IngestionFailed` — Job exited non-zero. The full event is in
    `kubectl describe kb` Events.
  - `RecallBelowTarget` — eval < target.
  - `NoDataset` — eval Job ran but the dataset ConfigMap was
    empty. The eval is recorded but auto-tune and the recall gate
    are skipped; add queries to the dataset.
  - `AutoTuneSettling` — auto-tune exhausted and is re-indexing
    with the best-observed config.
  - `Suspended` — `spec.suspend: true`.

## Common operations

### Re-trigger an ingestion

To force a re-ingestion without changing the spec, clear the observed
hash:

```bash
kubectl patch kb <name> --type=merge --subresource=status \
  -p '{"status":{"observedSpecHash":""}}'
```

The operator will see `obs != desired` and start a new ingest Job.
**Warning**: this also clears `observedSecretsHash`, which forces a
full re-embed. To preserve it, just clear `observedSpecHash`.

### Rotate a Secret without re-indexing

Edit the referenced Secret in place:

```bash
kubectl edit secret <name>
```

The operator watches the Secret, recomputes `secretsHash`, and
cancels any in-flight ingestion (since the running Job has the old
credentials in its environment). The next ingest will pick up the
new credentials. **No re-embedding happens.**

### Roll back a generation

There's no way to "undo" an ingest. The vector store has the new
data. Options:

1. **Restore from backup** (your vector store's tooling —
   Qdrant snapshots, pgvector `pg_dump`, etc.).
2. **Delete and recreate** the KB. This triggers a `cleanup` Job
   that drops the remote collection, then a fresh ingest from
   scratch on the next reconcile.
3. **Re-ingest from a known revision** (e.g. an old git commit on
   the source). Set `spec.sources[].ref` and force re-trigger
   via the hash patch above.

### Drain and restart workers

The workers run as Kubernetes Jobs, not as long-lived Deployments,
so there's nothing to drain. If the operator is the component
acting up:

```bash
# Find the operator pod
kubectl -n kuberag-system get pod -l control-plane=controller-manager

# Cordon
kubectl -n kuberag-system cordon <pod>

# Drain (because leader election will hand off to a peer)
kubectl -n kuberag-system delete pod <pod>
```

The pod is recreated by the Deployment, leader election
re-elects, and reconciliation resumes. The only state in the
operator is the cache, which is rebuilt from the API server.

### Scale the retriever

Edit the CR:

```bash
kubectl patch rtr <name> --type=merge -p '{"spec":{"replicas":5}}'
```

The operator reconciles the Deployment. The retriever is stateless;
scaling is safe at any time.

**Note**: HPA is on `ROADMAP.md` as a future item. Today the
replicas are static.

### Inspect an in-flight Job

```bash
# Get the active Job name
JOB=$(kubectl get kb <name> -o jsonpath='{.status.activeJob}')

# Job details
kubectl get job "$JOB" -o yaml

# Pod logs
kubectl logs -l job-name="$JOB" -f
```

If the worker logs say `ERROR: full ingest failed; dropping shadow,
active collection preserved`, the ingestion failed but the
existing collection is untouched. The operator's `ActiveJob`
is cleared and the KB goes to `Phase=Failed` for retry.

### Recover from a stuck `ActiveJob`

If the operator restarts mid-reconcile, the in-flight Job may
have been GC'd by `TTLSecondsAfterFinished` (5 min default), but
the operator's `status.activeJob` still points at it. The next
reconcile will see `apierrors.IsNotFound` on the Job Get and
clear `activeJob`. No manual action needed.

If a real Job is stuck (e.g. `BackoffLimit=2` reached but the
operator isn't noticing), patch the status:

```bash
kubectl patch kb <name> --type=merge --subresource=status \
  -p '{"status":{"activeJob":""}}'
```

This forces the operator to start a new reconcile. The old Job
needs to be deleted manually:

```bash
kubectl delete job <active-job-name>
```

## Specific failure modes

### "All ingestions fail with `IngestionFailed` condition"

1. `kubectl describe kb <name>` — read the Events.
2. If the event says `vector store connection refused`:
   - Qdrant: `kubectl get pod -l app=qdrant` and check
     `kubectl logs <qdrant-pod>`.
   - pgvector: `kubectl get pod -l app=postgres` and
     `kubectl exec -it <pg-pod> -- psql -U postgres -c '\l'`.
   - Milvus: the standalone deployment needs etcd + MinIO;
     check all three pods.
3. If the event says `embedding API 401/403`:
   - The Secret referenced by `spec.embedding.apiKeySecretRef`
     has been rotated or revoked. Update it.
4. If the event says `OOMKilled`:
   - `spec.ingestion.resources.memory` is too low. The default
     (`4Gi`) is for `bge-small`. `bge-large` or larger corpora
     need more.
5. If the event says `BackoffLimit exceeded`:
   - The Job retried 3 times and failed each time. Look at the
     pod logs for the actual error.

### "Phase=Degraded with RecallBelowTarget"

1. `kubectl get kb <name> -o jsonpath='{.status.evaluation}'` —
   read the recall percent.
2. If `spec.retrievalQuality.autoTune.enabled: true`, the operator
   already retried up to `maxAttempts` times. Check
   `status.autoTuneAttempts` and `status.bestRecallPercent`.
3. If `autoTune.enabled: false`, enable it:
   ```bash
   kubectl patch kb <name> --type=merge -p '{
     "spec":{"retrievalQuality":{"autoTune":{"enabled":true}}}
   }'
   ```
4. If the dataset is too small (only a handful of queries),
   recall is statistically meaningless. Add more queries to
   `spec.retrievalQuality.datasetRef`.
5. If the corpus itself is the problem, consider switching
   `spec.chunking.strategy` from `semantic` to `recursive`.

### "Retriever returns 5xx on every request"

1. `kubectl get pods -l app.kubernetes.io/name=<retriever-name>`.
2. `kubectl logs <retriever-pod>` — look for embedding errors or
   vector store timeouts.
3. The retriever caches the embedder and store on first request
   (lifespan startup). If the embedder fails to init, the
   `/healthz` probe is still 200 but every `/query` will 500.
   Restart the pod:
   ```bash
   kubectl rollout restart deploy/<retriever-name>
   ```
4. If the retriever is healthy but slow, see
   `docs/TUNING.md` for embedding batch size and vector store
   timeout tuning.

### "VectorIndex stuck in `Degraded`"

1. `kubectl get vi <name> -o yaml` — read `status.conditions[]`.
2. If the message is `Collection not found`:
   - The store exists but the collection was dropped externally.
     Trigger an ingestion to recreate it.
3. If the message is `points=0`:
   - The collection is empty. An ingestion has been queued;
     wait for it.
4. If the message is `store unreachable`:
   - The store's service is down. Check the store itself.

### "Operator logs are spammy with `Reconciler error`"

1. The operator uses `controller-runtime` which retries on errors.
   Some errors are non-actionable (e.g. an old Job's
   `metadata.labels` exceeded the K8s limit). The controller
   already truncates hashes to 8 hex chars; if you see
   `Invalid value: ... must be no more than 63 characters` on
   a label, that's a bug — file an issue.
2. For transient errors (network blips to the vector store),
   the controller will retry. Watch for the error rate to
   flatten.

## Useful queries

```bash
# All KBs not in Ready
kubectl get kb -A -o json | jq -r '
  .items[] | select(.status.phase != "Ready") |
  "\(.metadata.namespace)/\(.metadata.name) \(.status.phase)"
'

# All in-flight Jobs
kubectl get jobs -A -l app.kubernetes.io/managed-by=kuberag \
  -o json | jq -r '
  .items[] | select(.status.active != null) |
  "\(.metadata.namespace)/\(.metadata.name) active=\(.status.active)"
'

# Last indexed time per KB
kubectl get kb -A -o json | jq -r '
  .items[] |
  "\(.metadata.namespace)/\(.metadata.name) \(.status.lastIndexedTime // "never")"
'

# Auto-tune attempts
kubectl get kb -A -o json | jq -r '
  .items[] |
  "\(.metadata.namespace)/\(.metadata.name) attempts=\(.status.autoTuneAttempts // 0) best=\(.status.bestRecallPercent // 0)%"
'

# Retriever pod readiness
kubectl get pods -A -l app.kubernetes.io/managed-by=kuberag \
  -o json | jq -r '
  .items[] | select(.metadata.labels["app.kubernetes.io/component"] == "retriever") |
  "\(.metadata.namespace)/\(.metadata.name) ready=\(.status.containerStatuses[0].ready // false)"
'
```

## Escalation

For issues that aren't resolvable by the steps above:

1. File an issue at <https://github.com/furkandogmus/kuberag/issues>
   with the output of `kubectl get kb <name> -o yaml`,
   `kubectl logs <operator-pod>`, and any worker pod logs.
2. For suspected security issues, follow `SECURITY.md` instead.
3. For data loss or corruption, **stop the operator first** to
   prevent further in-flight changes:
   ```bash
   kubectl -n kuberag-system scale deploy kuberag-controller-manager --replicas=0
   ```
   Then take a snapshot of the vector store before recovery.
