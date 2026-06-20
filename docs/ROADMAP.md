# Roadmap

kuberag is `v1alpha1`. This is a rough, non-binding direction — issues and PRs
welcome.

## Validated today

### Sources & ingestion
- **GitHub**: public/private repos via token, blobless + sparse clone
- **S3/MinIO**: S3-compatible buckets, path-style addressing, ETag-based revision
- **Web crawl**: bounded HTML crawler, same-domain enforcement, URL normalization,
  fail-loud on seed errors (connection, non-200, cross-domain redirect, no text)
- **Incremental ingest**: per-source revision probes (`git ls-remote`, S3 ETags,
  crawl hash) skip unchanged sources; spec/embedding change forces full re-ingest
- **Freshness cron**: re-sync on schedule; `full` always for spec changes,
  `incremental` for freshness re-syncs
- **Finalizer cleanup**: delete triggers a `cleanup` Job that drops the remote
  vector store collection before releasing the finalizer

### Chunking & embedding
- **Three chunking strategies**: `semantic` (heading/paragraph split), `recursive`
  (separator hierarchy), `fixed` (sliding window)
- **Local embeddings**: fastembed in-process (`bge-small`, `bge-large`)
- **OpenAI-compatible embeddings**: OpenAI, Gemini, Ollama, vLLM, LM Studio, TEI
  with auto-detected dimension and optional API key
- **PDF parsing**: native pypdf extraction for GitHub, S3, web, and playground
  file upload sources

### Vector stores
- **Qdrant**: collection-level HTTP health probe (points, dimension, status)
- **pgvector**: SQL table-existence + point-count health probe via pgx driver
- **Milvus**: HTTP health check + collection-describe entity count probe
- All stores: ensure/recreate collection, per-source delete, batched upsert,
  count, vector + lexical search, drop

### Retrieval & serving
- **FastAPI retriever**: Deployment + Service managed by Retriever controller
- **Vector search** with score threshold filtering
- **Hybrid search**: dense vector + lexical text search fused with Reciprocal
  Rank Fusion (RRF), configurable dense/lexical weight per-Retriever or per-request
- **Reranking**: cross-encoder (fastembed) with configurable candidate pool size
- **Metadata filtering**: per-query filter by `source`, `docPath`, `docPathPrefix`
- **Conversational RAG**: multi-turn history injection into LLM prompt
- **Per-request tuning**: `topK`, `hybrid`, `hybridDensePercent`,
  `scoreThresholdPercent`, `rerank`, `temperature`, `maxTokens`, `systemPrompt`
  — all overridable per `/query` without redeploy
- **Playground UI**: interactive HTML at `/` for experimenting with every knob,
  ad-hoc file/URL ingest, per-query diagnostics in response `meta`

### Generation (full RAG)
- OpenAI-compatible chat: `openai`, `openrouter`, `groq`, `gemini`,
  `openai-compatible` (Ollama, vLLM, LM Studio)
- System prompt override, maxTokens, temperature per-request
- Best-effort: generation errors never fail retrieval

### Auto-tune
- **Ladder exploration**: grow overlap → shrink chunk size → rotate split strategy
  (semantic → recursive → fixed) with configurable max attempts
- **Revert-to-best**: `recordBest` snapshots best recall per config;
  `settleOnBest` lands on optimal configuration on exhaustion
- **Empty-dataset guard**: 0 queries → `NoDataset` condition, skips recall gate
- **Spec-edit safety**: `PendingRetune` drives re-index without clearing
  `ObservedSpecHash`, so user spec edits are detected mid-tune

### Observability
- Prometheus metrics: `ingestions_total`, `indexed_chunks`, `recall_percent`,
  `autotune_attempts`, `autotune_best_recall`
- Kubernetes events on every lifecycle transition
- Conditions: `Ready`, `Ingesting`, `Evaluated` (KB); `Ready` (VI);
  `Available` (Retriever)
- Status print columns: Phase, Model, Chunks, Recall, LastIndexed (KB);
  Health, Points, Dim (VI); KB, Phase, Endpoint (Retriever)
- Grafana dashboard + ServiceMonitor + Prometheus scrape annotations

### CRD design
- Three resources: `KnowledgeBase` (`kb`), `Retriever` (`rtr`), `VectorIndex` (`vi`)
- CEL validations: overlap < maxTokens, unique source names, backend block
  exclusivity, openai-compatible requires baseURL, cron pattern enforcement,
  Repo owner/name pattern, URL https scheme
- Enum-typed fields with defaults for all major configurations
- `+kubebuilder:subresource:status` on all CRDs; `scale` subresource on Retriever

### Deployment & operations
- **Helm chart** (`deploy/helm/kuberag/`): full install with values.yaml
- **Kustomize** base (`config/kustomization.yaml`)
- **NetworkPolicy**: default-deny ingress + egress whitelist (DNS, API server,
  vector stores, external APIs)
- **PriorityClass** (`kuberag-system`, 1M) on operator + retriever + worker jobs
- **PodDisruptionBudget** template for Retriever deployments
- **Startup/liveness/readiness probes** on operator and retriever
- **Topology spread** (zone anti-skew) on retriever
- **Secured images**: distroless operator, `USER 65532` on worker/retriever,
  readOnlyRootFilesystem, drop ALL capabilities, runtime default seccomp
- Multi-arch images (amd64, arm64) published to GHCR via release workflow
- Leader election with coordination.k8s.io leases

### Testing & CI
- **Go unit tests** for all pure-logic helpers (hashing, auto-tune, chunking,
  embedding dimensions, secret checksums, scheduling, security context)
- **Go envtest integration tests**: lifecycle, failed retry, finalizer cleanup,
  auto-tune loop + revert, empty-dataset guard, CEL admission
- **Python unit tests** (43 tests): chunking (3 strategies), globs, web crawl
  hardening, PDF parsing, Milvus literal escaping, retriever RRF, history,
  per-request overrides, metadata filters
- **CI pipeline**: golangci-lint → gofmt → vet → build → generated-artifacts
  stale-check → unit → envtest integration → Python → **k3d e2e** (ingest +
  query + chunk assertion)

## Near term

- **Validating/defaulting webhooks** (today: CEL validation catches most issues,
  webhooks would add cross-field defaults and richer validation).
- **Incremental at file granularity** (today: skip is per-source via revision
  probe; could diff individual files for finer-grained updates).
- **More sources**: Confluence/Notion API, generic Git (non-GitHub), local PVC
  mounts.
- **Retriever PDB managed by controller** (today: static template).

## Later

- **`KnowledgeBase` composition** — multiple vector stores, multi-tenant
  namespaces, cross-namespace references.
- **Cost/usage accounting** — track token consumption for hosted embedding &
  generation providers.
- **Eval suites** beyond recall@k — faithfulness (LLM-as-judge), answer
  relevance, latency SLOs, drift detection over time.
- **Autoscaling** — scale ingestion workers by queue depth; GPU-aware scheduling
  for embedding/reranking workloads.
- **v1 API** — stabilization, deprecation policy, conversion webhook, formal
  upgrade guarantees.

See [issues](https://github.com/furkandogmus/kuberag/issues) to propose or pick up work.
