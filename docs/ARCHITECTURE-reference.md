# Reference Architecture

A production deployment of kuberag. The local kind/k3d demo
(`make sample`) is a single-cluster, in-cluster, all-default
setup. This document is what a real install looks like.

## Goals

A production deployment must:

- Survive the loss of any single AZ.
- Authenticate and authorize all retriever traffic.
- Encrypt all data in transit (operator ↔ API server, worker ↔
  vector store, retriever ↔ clients).
- Encrypt all data at rest (vector store, operator's ConfigMap
  result data, secrets).
- Allow secrets to rotate without triggering re-ingestion.
- Be observable: metrics, traces, structured logs, alerts on
  SLO breach.
- Be operable: documented runbook, paging integration, on-call
  rotation.

## High-level diagram

```
                          ┌─────────────────────────────────────┐
                          │            Clients                  │
                          │   (apps, dashboards, agents)        │
                          └──────────────┬──────────────────────┘
                                         │ HTTPS + OIDC bearer
                                         ▼
                          ┌─────────────────────────────────────┐
                          │     Ingress / API gateway           │
                          │  (nginx, ambassador, oauth2-proxy)  │
                          │  TLS via cert-manager               │
                          └──────────────┬──────────────────────┘
                                         │
                                         ▼
   ┌────────────────────────────────────────────────────────────────┐
   │  kuberag-system namespace                                       │
   │                                                                 │
   │  ┌──────────────────┐  ┌──────────────────┐                    │
   │  │ Deployment        │  │ Deployment        │                    │
   │  │ kuberag-retriever │  │ kuberag-retriever │  (2+ replicas,    │
   │  │ (per Retriever)  │  │ ...               │   HPA when avail.) │
   │  └────────┬──────────┘  └──────────────────┘                    │
   │           │                                                   │
   │           │ in-cluster (NetworkPolicy-bound)                    │
   │           ▼                                                   │
   │  ┌──────────────────────────┐                                 │
   │  │ Service                   │                                 │
   │  │ kuberag-retriever         │                                 │
   │  └────────┬─────────────────┘                                 │
   │           │                                                   │
   │  ┌────────▼─────────────────┐                                 │
   │  │ Deployment                │                                 │
   │  │ kuberag-controller-manager│ (1-2 replicas, leader election) │
   │  └────────┬─────────────────┘                                 │
   │           │                                                   │
   │  ┌────────▼─────────────────┐  ┌──────────────────────────┐   │
   │  │ Job (per ingest / eval)  │  │ Job (cleanup on delete)  │   │
   │  │ kuberag-worker           │  │ kuberag-worker           │   │
   │  └──────────────────────────┘  └──────────────────────────┘   │
   │                                                                 │
   │  (worker/retriever share the same image)                       │
   └────────────────────────────────────────────────────────────────┘
                  │                       │                  │
                  │                       │                  │
        ┌─────────▼──────────┐    ┌───────▼─────────┐  ┌──────▼────────┐
        │  Qdrant cluster     │    │  PostgreSQL     │  │  Object store  │
        │  (HA, 3 nodes)      │    │  (managed or HA │  │  (S3, GCS,    │
        │  in dedicated       │    │   Patron/Citus) │  │   MinIO)      │
        │  namespace or       │    │  pgvector ext   │  │  source data  │
        │  external service   │    │  installed      │  │               │
        └────────────────────┘    └─────────────────┘  └───────────────┘
                                         ▲
                                         │ IRSA-bound
                                         │ credentials
```

## Component sizing

For a corpus of ~1M chunks, plan for:

| Component | CPU | Memory | Replicas | Notes |
|-----------|-----|--------|----------|-------|
| Operator | 1 | 256Mi | 1 (2 for HA) | Leader election. |
| Retriever | 2-4 | 4-8Gi | 3-5 | HPA on request rate. |
| Worker (ingest) | 4-8 | 16-32Gi | 1 (Job, not Deployment) | Scaled per-KB; bigger corpora need more. |
| Qdrant | 4-8 | 16-32Gi | 3 (HA mode) | Vector storage; disk for snapshots. |
| PostgreSQL | 4-8 | 16-32Gi | 2 (HA) | pgvector; backups via pg_basebackup. |
| Object store | n/a | n/a | managed | Source data, embedding model cache. |

For embedding model cache: mount a `PersistentVolumeClaim` at
`/scratch/.cache` so the model isn't re-downloaded on every Job.
Default is `EmptyDir` (re-download every Job). Set
`spec.ingestion.modelCacheSizeLimit: "10Gi"` to give the model
cache room for one large model.

## Identity and secrets

### IRSA (AWS) or Workload Identity (GCP)

The worker Jobs need credentials for:

- **Vector store** (Qdrant API key, Postgres password, etc.)
- **Source backends** (GitHub token, S3 access/secret key, etc.)
- **Embedding API** (OpenAI, Gemini, etc.)

Use IRSA / Workload Identity, not long-lived secrets. Map an IAM
role to the `kuberag-worker` ServiceAccount per namespace, and
have the role allow only the resources the KB needs. Example
trust policy for a KB that ingests from an S3 bucket:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "arn:aws:iam::ACCOUNT:oidc-provider/..."},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "oidc.eks.region.amazonaws.com/id/...:sub": [
          "system:serviceaccount:NAMESPACE:kuberag-worker"
        ]
      }
    }
  }]
}
```

Per-KB scoping is in `ROADMAP.md` ("Per-KB worker ServiceAccount").

### External Secrets Operator (ESO)

For non-AWS deployments, sync secrets from Vault / Doppler /
1Password via
[External Secrets Operator](https://external-secrets.io/) into
Kubernetes `Secret`s. The kuberag `SecretKeyRef` references then
stay the same; only the source-of-truth moves.

## Networking

### Ingress

The retriever Service is HTTP by default. For production:

1. Install [cert-manager](https://cert-manager.io/) for automatic
   TLS cert provisioning.
2. Use an Ingress (nginx, Traefik) or Gateway API resource to
   expose the retriever Service on HTTPS.
3. Front the Ingress with an auth proxy that validates OIDC
   tokens (e.g. [oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/),
   [ambassador](https://www.getambassador.io/)) and forwards the
   authenticated user's identity as a header.

Example Ingress (nginx + cert-manager + oauth2-proxy):

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: kuberag-retriever
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/auth-url: https://oauth2-proxy.kuberag-system.svc/oauth2/auth
    nginx.ingress.kubernetes.io/auth-signin: https://oauth2-proxy.kuberag-system.svc/oauth2/start
spec:
  tls:
  - hosts: [rag.example.com]
    secretName: rag-tls
  rules:
  - host: rag.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: kuberag-retriever
            port:
              number: 8000
```

### NetworkPolicy

The default `config/rbac/network-policy.yaml` is a starting point.
Production needs tighter rules:

- **Egress**: per-KB allowlist. Each `KnowledgeBase.Spec.Ingestion`
  or `KB.Spec.Sources` should declare the endpoints it needs to
  reach, and the operator should reconcile the egress allowlist
  per-KB. (Tracked in `ROADMAP.md` as "Network policy egress
  allowlist per store".)
- **Ingress to retriever**: only the auth proxy's IP range, not
  `0.0.0.0/0`.
- **Cross-namespace**: the retriever's egress to the vector store
  should be limited to the vector store's namespace IP range.

## Observability

### Metrics

The operator exposes Prometheus metrics on `:8080/metrics`. The
`ServiceMonitor` at `config/observability/servicemonitor.yaml`
configures scraping. A Grafana dashboard is at
`config/observability/grafana-dashboard.json`.

Production should add:

- **SLO burn rate alerts** on:
  - Ingestion freshness: `time() - kb.status.lastIndexedTime >
    threshold` (paged).
  - Retrieval latency: histogram p99 from the retriever.
  - Eval drift: `kb.status.evaluation.recallPercent` dropping
    below `minRecallPercent` for N consecutive evals.
- **Capacity alerts**:
  - Qdrant memory / disk usage.
  - Worker Job OOMKilled count.
- **Operator health**:
  - Reconcile error rate.
  - ActiveJob > 0 for > 1h (stuck job).
  - Auto-tune loop duration p99.

### Traces

W3C trace context now propagates from controller reconciles into worker Jobs,
and both workers and Retrievers can export OTLP/gRPC spans to Tempo, Jaeger, or
another collector. Individual vector-store HTTP/SQL calls are not yet wrapped
in child spans.

### Logs

The operator emits structured `logr` records. Python workers emit prefixed
single-line records through a bounded burst limiter. Forward stdout/stderr to
Loki or Elasticsearch via a node agent such as Fluent Bit or Vector; parse the
worker prefix when structured fields are required.

## Backup and disaster recovery

### Vector store backups

| Store | Backup mechanism |
|-------|------------------|
| Qdrant | [Snapshot API](https://qdrant.tech/documentation/guides/snapshots/) to S3. Run on a cron. |
| pgvector | `pg_dump` or managed-snapshot service (RDS automated snapshots, Cloud SQL). |
| Milvus | Milvus backup utility or `etcd` snapshot for standalone. |

For Qdrant specifically, the operator's `swap_collection` design
means an in-flight ingestion is atomic; an interrupted Job leaves
the previous alias untouched. Backups can therefore be point-in-
time and don't need to coordinate with ingestions.

### Disaster recovery procedure

1. **Vector store** is rebuilt from sources. There is no
   incremental state in kuberag itself; the only state is the
   CRDs (which live in etcd and are backed up via standard
   Kubernetes mechanisms).
2. **Source data** lives in GitHub, S3, or web; the operator
   can re-ingest from the same sources after a vector store
   loss.
3. **Operator + retriever** are stateless and are recreated
   by their Deployments.

For a full cluster loss:

```bash
# 1. Provision a new cluster with the same networking.
# 2. Reinstall kuberag (Helm or kustomize).
helm install kuberag deploy/helm/kuberag/ \
  --namespace kuberag-system --create-namespace

# 3. Restore CRDs from the etcd backup.
kubectl apply -f kb-backup.yaml
kubectl apply -f vi-backup.yaml
kubectl apply -f rtr-backup.yaml

# 4. Restore the vector store from its backup.
# (Qdrant: restore from S3 snapshot; pgvector: pg_restore)
# 5. Re-apply secrets. (Or rely on ESO to re-sync.)
# 6. The operator detects the rebuilt state and may re-ingest
#    any KB whose ObservedSpecHash doesn't match. To force a
#    clean re-ingest, see RUNBOOK.md "Re-trigger an ingestion".
```

## Multi-tenancy

For shared clusters with multiple teams:

- **Namespace per team.** Each team gets their own namespace,
  with their own `KnowledgeBase`s, `Retriever`s, and a per-namespace
  worker ServiceAccount. The operator is cluster-scoped today;
  namespace-scoped mode is in `ROADMAP.md`.
- **Vector store per team or shared with row-level isolation.**
  Qdrant and Milvus support per-collection auth tokens. pgvector
  is naturally multi-tenant via schemas.
- **Resource quotas** on the worker namespace so a single team
  can't starve the operator's reconcile loop.
- **Per-team retriever auth** — the OAuth2 proxy should be
  configured with one auth provider per team, or one provider
  with team claims extracted from the token.

## Example manifests

A production baseline is provided as:

- `config/samples/production-values.yaml` — namespace-scoped Helm install,
  two leader-elected replicas, PDB, NetworkPolicies, and Prometheus resources.
- `config/samples/production-reference.yaml` — restricted tenant namespace,
  quota/limits, external authenticated Qdrant, scheduled KnowledgeBase, and an
  autoscaled OIDC/TLS Retriever.

Replace every `sha-REPLACE_ME` with a signed immutable release tag and create
the referenced Secrets before applying.
