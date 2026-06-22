# kuberag

[![ci](https://github.com/furkandogmus/kuberag/actions/workflows/ci.yaml/badge.svg)](https://github.com/furkandogmus/kuberag/actions/workflows/ci.yaml)

Kubernetes-native operator for declarative RAG knowledge bases.

`kuberag` is a **RAG lifecycle operator** (managing synchronization, chunking, embedding, vector store ingestion, evaluation, auto-tuning, and serving) rather than an application-level framework (like LangChain or LlamaIndex). It automates and manages the infrastructure and operations layer of RAG.

You describe the *desired knowledge state* — sources, chunking, embedding model,
vector store, freshness, retrieval-quality target — and the operator continuously
reconciles reality toward it: syncing sources, chunking, embedding, writing to the
vector DB, re-indexing on drift, evaluating retrieval quality and **auto-tuning
chunking** to hit a recall target, and serving low-latency retrieval (optionally
with LLM answer generation — full RAG).

```yaml
apiVersion: rag.furkan.dev/v1alpha1
kind: KnowledgeBase
metadata:
  name: company-docs
spec:
  sources:
    - name: docs
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

## Status

`v1alpha1`, early but functional. Validated **end-to-end on a live cluster** (k3d):
GitHub / S3 (MinIO) / web sources → Qdrant **and** pgvector; Gemini and Ollama
embeddings; Ollama answer generation; incremental ingest; the eval + auto-tune
loop; finalizer cleanup; in-cluster deployment; blobless+sparse clone.

## Documentation

- [Architecture](docs/ARCHITECTURE.md) — control/data planes, reconcile state machine, ingest/eval/auto-tune, hashing.
- [Configuration & tuning](docs/TUNING.md) — what every knob does and which to pick: chunking strategies, embedding/store choices, retrieval & auto-tune tuning, ready-made profiles.
- [API reference](docs/API.md) — every CRD field (`KnowledgeBase`, `Retriever`, `VectorIndex`).
- [Providers & backends](docs/PROVIDERS.md) — sources, stores, embeddings, generation (+ fully-local Ollama).
- [Observability](docs/OBSERVABILITY.md) — status/conditions, events, Prometheus metrics, Grafana dashboard.
- [Runbook](docs/RUNBOOK.md) — on-call handbook: phase meanings, re-trigger, rollback, scaling, drain/restart.
- [Reference architecture](docs/ARCHITECTURE-reference.md) — production deployment: HA, IRSA, ingress, auth, backups.
- [Versioning & deprecation policy](docs/VERSIONING.md) — SemVer + Kubernetes API conventions.
- [Roadmap & production readiness](docs/ROADMAP.md) · [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [Changelog](docs/CHANGELOG.md)

## Custom Resources

| Kind | Short | Purpose |
|------|-------|---------|
| `KnowledgeBase` | `kb` | The control surface: sources → chunk → embed → store, freshness, quality, auto-tune. |
| `Retriever` | `rtr` | A serving endpoint (Deployment + Service) over a KnowledgeBase: vector search, optional reranking, and optional LLM answer generation. |
| `VectorIndex` | `vi` | Auto-created per KnowledgeBase; tracks collection health, point count and dimension. |

## Supported backends

- **Sources:** `github` (public/private via token, blobless+sparse clone), `s3` (incl. MinIO and other S3-compatible stores), `web` (depth-bounded crawl).
- **Vector stores:** `qdrant`, `pgvector`, `milvus`.
- **Embeddings:** `local` (fastembed: `bge-small`/`bge-large`) or any OpenAI-compatible API via `openai` / `gemini` / `openai-compatible` (OpenAI, Gemini, **Ollama**, vLLM, LM Studio, TEI, …). Dimension is taken from a built-in table or auto-detected.
- **Generation** (optional, on `Retriever`): OpenAI-compatible chat — `openai` / `openrouter` / `groq` / `gemini` / `openai-compatible` (incl. **Ollama**). `/query` then returns `{answer, sources}`.

## Architecture

```mermaid
%%{init: {'theme':'base','themeVariables':{
  'fontFamily':'ui-sans-serif, system-ui, sans-serif',
  'fontSize':'14px',
  'lineColor':'#94a3b8',
  'clusterBorder':'#cbd5e1',
  'clusterBkg':'#ffffff00'
}}}%%
flowchart LR
  classDef crd      fill:#fef3c7,stroke:#f59e0b,stroke-width:1.5px,color:#0f172a;
  classDef control  fill:#eef2ff,stroke:#6366f1,stroke-width:1.5px,color:#0f172a;
  classDef data     fill:#ecfdf5,stroke:#10b981,stroke-width:1.5px,color:#0f172a;
  classDef store    fill:#fae8ff,stroke:#a855f7,stroke-width:1.5px,color:#0f172a;
  classDef ext      fill:#f1f5f9,stroke:#64748b,stroke-width:1.5px,color:#0f172a;
  classDef user     fill:#0f172a,stroke:#0f172a,stroke-width:1.5px,color:#f8fafc;

  User(["👤 Client"]):::user

  subgraph Sources["📥 Sources"]
    direction TB
    Git[("Git repo")]:::ext
    S3[("S3 / MinIO")]:::ext
    Web[("Web crawl")]:::ext
  end

  subgraph K8s["☸️ Kubernetes Cluster"]
    direction TB

    subgraph CP["Control Plane · Go operator"]
      direction TB
      KB["KnowledgeBase<br/><i>kb</i>"]:::crd
      RTR["Retriever<br/><i>rtr</i>"]:::crd
      VI["VectorIndex<br/><i>vi</i>"]:::crd
      REC{{"Reconciler<br/>specHash · schedule<br/>ingest / eval / auto-tune"}}:::control
      KB -. watch .-> REC
      RTR -. watch .-> REC
      REC -->|"owns / health"| VI
    end

    subgraph DP["Data Plane · Python worker"]
      direction TB
      JOB["Ingest / Eval / Cleanup<br/>Job (rag_worker)"]:::data
      RES["Result ConfigMap"]:::data
      SRV["Retriever API<br/>(FastAPI Deployment)"]:::data
      JOB --> RES
    end

    REC ==>|"spawns one Job"| JOB
    RES -. "status · conditions · metrics" .-> REC
    REC ==>|"deploys"| SRV
  end

  subgraph Stores["🗄️ Vector Stores"]
    direction TB
    QD[("Qdrant")]:::store
    PG[("pgvector")]:::store
    MV[("Milvus")]:::store
  end

  subgraph AI["🧠 AI APIs"]
    direction TB
    EMB["Embeddings<br/>FastEmbed · OpenAI · Gemini · Ollama"]:::ext
    GEN["Generation<br/>OpenAI · Gemini · Groq · Ollama"]:::ext
  end

  Sources -->|"1 · pull"| JOB
  JOB -->|"2 · chunk + embed"| EMB
  JOB -->|"3 · upsert"| Stores

  User -->|"/query"| SRV
  SRV -->|"a · vector + lexical search (RRF)"| Stores
  SRV -->|"b · ground answer"| GEN
  SRV -->|"c · answer + sources"| User
```

> Diagram source: [`docs/images/architecture.mermaid`](docs/images/architecture.mermaid). GitHub renders the block above natively — no build step.

Two planes, intentionally separated:

| Plane | What | Tech |
|-------|------|------|
| **Control** | Decides *when* to ingest/evaluate/tune, manages Job + Deployment lifecycle, reports status, emits events & metrics | Go + controller-runtime |
| **Data** | Does the work: clone/list/crawl → chunk → embed → upsert; evaluate; serve | Python (`worker/`) |


The KnowledgeBase reconciler:

1. Computes a `specHash` over re-ingest-relevant fields (sources, effective chunking, model, store).
2. Decides work: **ingest** (first run, spec drift, model change, or freshness cron) takes priority, then **evaluate** (eval cron).
3. Creates a single in-flight **Job** (tracked in `status.activeJob`), injecting secrets as env and passing the spec as JSON.
4. On completion reads the worker's **result ConfigMap** and writes `status` (phase, conditions, `indexedChunks`, per-source revisions, evaluation).
5. **Incremental ingest:** the worker probes each source's revision (`git ls-remote` SHA, S3 ETags, crawl hash) and skips unchanged sources; a spec change forces a full re-process.
6. **Auto-tune:** if measured recall < `minimumRecallPercent` and auto-tune is enabled, it adjusts effective chunking (grow overlap, then shrink chunk size), clears the spec hash to force a re-index, and retries up to `maxAttempts`; if still short it goes `Degraded`.
7. **Deletion:** a finalizer runs a `cleanup` Job that drops the remote collection before the object is removed.

## Project layout

```
api/v1alpha1/          CRD Go types (+ generated DeepCopy)
internal/controller/   reconcilers (knowledgebase, retriever, vectorindex),
                       jobs / scheduling / metrics, unit + envtest integration tests
cmd/main.go            manager entrypoint (3 controllers, leader election)
worker/rag_worker/     Python data plane: sources, stores, chunking, embeddings,
                       ingest, evaluate, cleanup
worker/retriever/      FastAPI retrieval + generation server
worker/tests/          Python unit tests
config/crd|rbac|manager   generated CRDs, RBAC, operator Deployment
config/samples/        runnable examples (Qdrant, pgvector, MinIO, Ollama, Gemini, …)
.github/workflows/     CI (Go build/vet/fmt/unit/integration + Python tests)
```

## Quick start (local)

```bash
make test               # Go unit tests
make test-integration   # envtest integration tests (downloads kube-apiserver/etcd)
make test-py            # Python worker tests

make install            # install CRDs
make run                # run the operator against your current kubeconfig
make sample             # Qdrant + eval dataset + KnowledgeBase + Retriever

kubectl get kb,vi,rtr
```

In-cluster (prebuilt images are published to GHCR by CI, so you can skip the build):

```bash
# ghcr.io/furkandogmus/{kuberag,kuberag-worker,kuberag-retriever}:latest
make deploy             # CRDs + RBAC + manager (pulls the published images)

# or build your own first:
make docker-build-all   # operator + worker + retriever images
```

For a tenant-isolated Helm installation that watches only one namespace:

```bash
helm upgrade --install kuberag deploy/helm/kuberag \
  --namespace kuberag-system --create-namespace \
  --set rbac.scope=namespace \
  --set rbac.watchNamespace=tenant-a
```

The operator creates a dedicated least-privilege worker ServiceAccount for each
KnowledgeBase. Set `spec.ingestion.serviceAccountName` only when supplying an
external identity such as IRSA.

Namespace scope limits the running operator; CRD installation itself remains a
cluster-level action and therefore still requires cluster-admin installation
permissions.

For a hardened baseline with PSS, quotas, NetworkPolicies, Prometheus,
OIDC/TLS, external Qdrant credentials, and Redis-backed distributed rate
limiting, start from
`config/samples/production-values.yaml` and
`config/samples/production-reference.yaml`. Replace all `sha-REPLACE_ME`
placeholders with signed release image tags first.

Or run the whole thing on a throwaway k3d cluster with one command:

```bash
make demo               # k3d up -> deploy -> ingest a repo -> query
```

### Fully local RAG with Ollama (no API keys)

```bash
ollama pull nomic-embed-text && ollama pull qwen2.5:3b
# expose Ollama to the cluster: OLLAMA_HOST=0.0.0.0, then point baseURL at the host
kubectl apply -f config/samples/ollama.yaml
kubectl port-forward svc/ollama-docs-retriever 8000:8000
curl -s localhost:8000/query -d '{"query":"what is this about?"}' | jq
# -> {"answer": "...", "results": [{"docPath": "...", "score": 0.7, ...}]}
```

See `config/samples/providers.yaml` for a Gemini-embeddings + hosted-LLM example.

## Status & observability

```bash
$ kubectl get kb
NAME           PHASE   MODEL              CHUNKS   RECALL   LASTINDEXED   AGE
company-docs   Ready   bge-small          742      86       2m            5m

$ kubectl get vi
NAME                 HEALTH    POINTS   DIM   AGE
company-docs-index   Healthy   742      384   5m
```

- `status.conditions`: `Ready`, `Ingesting`, `Evaluated` (KB); `Ready` (VectorIndex); `Available` (Retriever).
- **Events** on every transition (`IngestionStarted/Complete/Failed`, `Evaluating`, `RecallMet`, `AutoTuning`, `RecallBelowTarget`, `Cleanup`).
- **Prometheus metrics** on `:8080`: `rag_knowledgebase_ingestions_total`, `rag_knowledgebase_indexed_chunks`, `rag_knowledgebase_recall_percent`, `rag_knowledgebase_autotune_attempts`.

## Design notes / limitations

- One in-flight Job per KnowledgeBase (ingest *or* eval) keeps reconciliation deterministic.
- Auto-tune adjusts chunking only; it does not change the embedding model.
- VectorIndex health probing is implemented for Qdrant (HTTP); other stores report `Unknown` and rely on ingestion success.
- Recall is computed as recall@TopK over a user-provided query dataset (expected source paths).
- The API group is `rag.furkan.dev` (independent of the repository name).

## License

MIT — see [LICENSE](LICENSE).
