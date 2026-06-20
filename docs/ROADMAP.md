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

### Operator maturity
- **Validating/defaulting webhooks** — admission webhooks would add cross-field
  defaults (e.g. auto-derive `collection` from KB name at admission time),
  richer validation (secret key existence, endpoint reachability pre-check),
  and immutability guards. Today CEL validation + controller-side error handling
  covers most cases.
- **Conversion webhook** — required before introducing `v1beta1` or `v1`. Must
  convert stored objects between API versions without data loss.
- **Namespace-scoped operator mode** — today the operator is cluster-scoped
  (ClusterRole); an option restricted to a watch namespace reduces blast radius
  for multi-tenant clusters.

### Ingestion improvements
- **Incremental at file granularity** — today skip is per-source via revision
  probe (git SHA, S3 ETags, crawl hash). File-level diffing would detect exactly
  which documents changed and re-embed only those, dramatically reducing ingestion
  time for large repos on freshness runs.
- **Webhook-driven sync** — GitHub webhooks / S3 event notifications could trigger
  ingestion immediately instead of waiting for the cron tick. Complements cron
  freshness as a push-based alternative.
- **More sources**:
  - **Confluence / Notion** — REST API clients for common knowledge management
    platforms with pagination, attachment handling, and incremental sync via
    `updatedAt` cursors.
  - **Generic Git** — today GitHub's token auth is baked in; support any Git
    remote (GitLab, Bitbucket, Gitea) via SSH key or generic HTTP token.
  - **Local PVC** — mount an existing PersistentVolumeClaim and index its
    contents, enabling air-gapped / on-premise document stores.
  - **Google Drive / SharePoint** — cloud document storage APIs with OAuth2.
- **Web crawl depth and rate limiting** — per-domain rate limits, `robots.txt`
  compliance, `sitemap.xml` discovery, JavaScript rendering for SPAs.

### Serving & retrieval
- **Retriever HPA** — horizontal pod autoscaling on CPU/memory or custom metrics
  (requests per second, query latency p95). Today replicas are static.
- **Retriever PDB managed by controller** — the operator should create and own a
  PodDisruptionBudget for each Retriever instead of requiring a manual template.
- **Streaming generation (SSE)** — server-sent events for token-by-token LLM
  output, giving users progressive answer rendering.
- **Retriever ingress/route** — optionally create an Ingress or OpenShift Route
  for the retriever Service, with TLS and auth annotations.

### Evaluation & tuning
- **Auto-generated eval datasets** — sample questions from ingested chunks
  (synthetic query generation via LLM) so users don't need to manually write
  `{query, expectedSources}` pairs.
- **Multi-metric evaluation** — beyond recall@k: answer faithfulness
  (LLM-as-judge comparing answer to retrieved context), context relevance,
  answer completeness, hallucination detection.
- **Chunking strategy auto-selection** — today auto-tune explores overlap, size,
  and strategy rotation; the operator could pre-select the optimal starting
  strategy based on document structure analysis (heading density, paragraph
  length distribution).
- **Embedding model benchmarking** — test multiple embedding models against the
  same dataset and compare recall/latency/cost, surfacing the best pick.

### Observability & operations
- **Distributed tracing** — OpenTelemetry spans across reconcile → Job → worker
  → store, surfacing end-to-end ingestion and query latency.
- **Worker Job logs aggregation** — surface worker logs in `kubectl describe kb`
  or status conditions for faster debugging when an ingestion fails.
- **SLO dashboards** — pre-built Grafana SLO panels for ingestion freshness
  (time since last successful index) and retrieval latency percentiles.

## Later

### Multi-tenancy & federation
- **`KnowledgeBase` composition** — a single `KnowledgeBase` spanning multiple
  vector stores (e.g. Qdrant for fast search + pgvector for analytical queries)
  or multiple embedding models on the same content.
- **Cross-namespace references** — today `KnowledgeBaseRef` must be same-namespace;
  support referencing a KB from another namespace (with RBAC validation).
- **Federated retrieval** — query across multiple KnowledgeBases and merge/fuse
  results, with per-KB weighting.

### Cost & resource management
- **Token usage accounting** — track embedding and generation token consumption
  per KnowledgeBase, exposed as Prometheus metrics and status fields. Break down
  by provider and model for cost attribution.
- **Ingestion cost estimation** — predict ingestion cost (API calls, compute time)
  before running, based on source size and embedding model pricing.
- **Budget enforcement** — `maxTokensPerPeriod` or `maxCostPerMonth` on
  embedding/generation specs, pausing ingestion/generation when exceeded.
- **Spot-instance friendly workers** — worker Jobs tolerant of preemption with
  checkpoint/resume so interrupted ingestions don't restart from zero.

### Advanced retrieval
- **Query rewriting / expansion** — automatically rewrite user queries (synonym
  expansion, HyDE, multi-query) before retrieval to improve recall on vague or
  short queries.
- **Semantic caching** — cache query embeddings and results with a similarity
  threshold; near-duplicate queries hit the cache, skipping the expensive
  embed→search→generate pipeline.
- **Multi-modal RAG** — embed images, diagrams, and tables from documents along
  with text, using vision embedding models. Retrieve visual context alongside
  text chunks.
- **Knowledge graphs** — extract entities and relationships from chunks, store
  in a graph alongside vectors, and traverse during retrieval for richer context.

### Evaluation & quality
- **A/B testing framework** — run two chunking/embedding configurations side by
  side on the same dataset, compare metrics, and promote the winner.
- **Drift detection & alerting** — monitor recall/latency over time; alert when
  metrics regress below a configurable threshold between scheduled evaluations.
- **Answer grounding verification** — check that every claim in a generated
  answer is supported by a retrieved chunk (citation grounding score).
- **User feedback loop** — accept thumbs-up/down on query responses, feed back
  into eval metrics and auto-tune decisions.

### Platform & ecosystem
- **OLM integration** — Operator Lifecycle Manager bundle for OpenShift and
  vanilla OLM clusters, with automatic upgrades and dependency management.
- **`kuberag` CLI** — `kubectl` plugin for common operations: `kuberag query`
  (search without port-forward), `kuberag ingest` (trigger manual ingestion),
  `kuberag eval` (run evaluation ad-hoc), `kuberag diff` (preview what a spec
  change would re-index).
- **KnowledgeBase templates** — a catalog of pre-configured KnowledgeBases for
  common use cases: "index my GitHub docs", "RAG over a website", "company
  handbook search".
- **v1 API** — stabilization after real-world usage, deprecation policy for
  `v1alpha1` fields, conversion webhook, formal upgrade guarantees, API review
  with Kubernetes SIG conventions.

See [issues](https://github.com/furkandogmus/kuberag/issues) to propose or pick up work.
