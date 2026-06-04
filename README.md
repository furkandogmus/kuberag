# rag-operator

A Kubernetes operator that manages RAG knowledge bases **declaratively**.

You describe the *desired knowledge state* — sources, chunking, embedding model,
vector store, freshness, retrieval-quality target — and the operator continuously
reconciles reality toward it: syncing sources, chunking, embedding, writing to the
vector DB, re-indexing on drift, evaluating retrieval quality and **auto-tuning
chunking** to hit a recall target, and serving low-latency retrieval.

```yaml
apiVersion: rag.furkan.dev/v1alpha1
kind: KnowledgeBase
metadata:
  name: company-docs
spec:
  sources:
    - name: docs-repo
      type: github
      github: { repo: qdrant/landing_page, ref: master, includeGlobs: ["**/*.md"] }
  chunking: { strategy: semantic, maxTokens: 800, overlap: 80 }
  embedding: { model: bge-small, provider: local }   # change model -> full re-embed
  vectorStore: { type: qdrant, endpoint: http://qdrant:6333, collection: company-docs }
  ingestion: { mode: incremental }                   # skip unchanged sources
  freshness: { schedule: "0 */6 * * *" }
  retrievalQuality:
    enabled: true
    evalSchedule: "0 * * * *"
    datasetRef: { name: company-docs-eval }
    minimumRecallPercent: 80
    autoTune: { enabled: true, maxAttempts: 3 }       # below target -> tune & re-index
```

## Custom Resources

| Kind | Short | Purpose |
|------|-------|---------|
| `KnowledgeBase` | `kb` | The control surface: sources → chunk → embed → store, freshness, quality, auto-tune. |
| `Retriever` | `rtr` | A serving endpoint (Deployment + Service) for low-latency querying over a KnowledgeBase, with optional reranking. |
| `VectorIndex` | `vi` | Auto-created per KnowledgeBase; tracks collection health, point count and dimension. |

## Architecture

Two planes, intentionally separated:

| Plane | What | Tech |
|-------|------|------|
| **Control** | Decides *when* to ingest/evaluate/tune, manages Job + Deployment lifecycle, reports status, emits events & metrics | Go + controller-runtime |
| **Data** | Does the work: clone/list/crawl → chunk → embed → upsert; evaluate; serve | Python (`worker/`) |

```
KnowledgeBase ──watch──▶ Reconciler ──creates──▶ Job (ingest|eval|cleanup)
      ▲                      │  │                      │
      │                      │  └─creates─▶ VectorIndex │  result ConfigMap
      └──── status ◀─────────┘                          ▼
                                          sources → chunk → embed → vector store
Retriever ──watch──▶ Reconciler ──creates──▶ Deployment + Service (FastAPI /query)
```

The KnowledgeBase reconciler:

1. Computes a `specHash` over re-ingest-relevant fields (sources, effective chunking, model, store).
2. Decides work: **ingest** (first run, spec drift, model change, or freshness cron due) takes priority, then **evaluate** (eval cron due).
3. Creates a single in-flight **Job** (tracked in `status.activeJob`), injecting secrets as env and passing the spec as JSON.
4. On completion reads the worker's **result ConfigMap** and writes `status` (phase, conditions, `indexedChunks`, per-source revisions, evaluation).
5. **Incremental ingest:** the worker probes each source's revision (`git ls-remote`, S3 ETags, crawl hash) and skips unchanged sources.
6. **Auto-tune:** if measured recall < `minimumRecallPercent` and auto-tune is enabled, it adjusts effective chunking (grow overlap, then shrink chunk size), clears the spec hash to force a re-index, and retries up to `maxAttempts`.
7. **Deletion:** a finalizer runs a `cleanup` Job that drops the remote collection before the object is removed.

## Supported backends

- **Sources:** `github` (public/private via token), `s3` (incl. S3-compatible/MinIO), `web` (depth-bounded crawl).
- **Embeddings:** `local` via fastembed (`bge-small`, `bge-large`), `openai` (`text-embedding-3-small/large`).
- **Vector stores:** `qdrant`, `pgvector`, `milvus`.

## Project layout

```
api/v1alpha1/          CRD Go types (+ generated DeepCopy)
internal/controller/   reconcilers: knowledgebase, retriever, vectorindex
                       + jobs / scheduling (cron) / metrics helpers + tests
cmd/main.go            manager entrypoint (3 controllers, leader election)
worker/rag_worker/     Python data plane: sources, stores, chunking,
                       embeddings, ingest, evaluate, cleanup
worker/retriever/      FastAPI retrieval server
worker/tests/          Python unit tests
config/crd/            generated CRD manifests (3 kinds)
config/rbac/           operator ClusterRole, leader-election Role, worker RBAC
config/manager/        operator Deployment + ServiceAccount + binding
config/samples/        KnowledgeBase, Retriever, eval dataset, local Qdrant
```

## Quick start (local)

```bash
make test          # Go unit tests
make test-py       # Python unit tests
make install       # CRDs
make run           # run the operator against your kubeconfig

# In another shell: Qdrant + worker RBAC + eval dataset + KnowledgeBase + Retriever
make sample

kubectl get kb,vi,rtr
kubectl describe kb company-docs
```

Build & deploy in-cluster:

```bash
make docker-build-all        # operator + worker + retriever images
make deploy                  # CRDs + RBAC + manager
make worker-rbac             # worker ServiceAccount/RBAC (per KB namespace)
```

## Status & observability

```bash
$ kubectl get kb
NAME           PHASE   MODEL       CHUNKS   RECALL   LASTINDEXED   AGE
company-docs   Ready   bge-small   742      86       2m            5m

$ kubectl get vi
NAME                 HEALTH    POINTS   DIM   AGE
company-docs-index   Healthy   742      384   5m
```

- `status.conditions`: `Ready`, `Ingesting`, `Evaluated` (KB); `Ready` (VectorIndex); `Available` (Retriever).
- **Events** on every transition (`IngestionStarted/Complete/Failed`, `Evaluating`, `RecallMet`, `AutoTuning`, `RecallBelowTarget`, `Cleanup`).
- **Prometheus metrics** (`/metrics` on `:8080`): `rag_knowledgebase_ingestions_total`, `rag_knowledgebase_indexed_chunks`, `rag_knowledgebase_recall_percent`, `rag_knowledgebase_autotune_attempts`.

## Querying a Retriever

```bash
kubectl port-forward svc/company-docs-retriever 8000:8000
curl -s localhost:8000/query -d '{"query":"how do I create a collection?"}' | jq
```

## Design notes / limitations

- One in-flight Job per KnowledgeBase (ingest *or* eval) keeps reconciliation deterministic.
- Auto-tune adjusts chunking only; it does not change the embedding model.
- VectorIndex health probing is implemented for Qdrant (HTTP); other stores report `Unknown` and rely on ingestion success.
- Recall is computed as recall@TopK over a user-provided query dataset (expected source paths).

> Status: `v1alpha1`. APIs may still change before `v1`.
