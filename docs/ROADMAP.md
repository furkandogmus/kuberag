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

## Production readiness

Items required before kuberag is safe to recommend for production deployment
with sensitive data. Some of these are tracked elsewhere on this roadmap
("Near term" / "Later") and called out here for visibility.

### API maturity

- **`v1` API with conversion webhook** — `v1alpha1` carries no upgrade
  guarantee. A `v1beta1` → `v1` transition needs a conversion webhook to
  migrate stored objects without data loss. Today the API group
  `rag.furkan.dev` is also owned by a personal domain; for a real release
  it should move to a project-owned domain (`rag.kuberag.io` or similar)
  before the API gets locked.
- **Validating admission webhooks** — CEL rules in the CRD catch most
  schema-level issues, but cross-field invariants (e.g. secret key exists
  in the referenced Secret, embedding model dimension matches the target
  store, `VectorIndex` references an existing `KnowledgeBase`) are only
  enforced at reconcile time, after the user has already applied the CR.
  Webhooks would fail fast at admission.
- **Defaulting webhooks** — common derived defaults (e.g. `collection =
  metadata.name` when omitted, `nodeSelector` propagated from a shared
  ConfigMap) are today inlined in the controller, which makes them
  invisible to `kubectl explain`.
- **Namespace-scope operator mode** — the operator is currently
  cluster-scoped (`ClusterRole`). Multi-tenant clusters need a
  `WATCH_NAMESPACE` env var that restricts reconciliation and RBAC to
  one or more namespaces, with the operator dropping its leader-election
  ClusterRole when locked down.
- **Per-KB worker ServiceAccount** — today all workers share a single
  `kuberag-worker` ServiceAccount in each namespace. A KB can use
  `spec.ingestion.serviceAccountName` to point at a different SA, but the
  default is global. In multi-tenant deployments, the per-KB SA should
  be auto-provisioned and bound to a per-KB RBAC role so one KB cannot
  touch another's ConfigMaps or Jobs.
- **API review against Kubernetes SIG conventions** — before `v1`,
  publish a public API review covering field immutability, default
  semantics, list semantics (`MaxItems` for Sources is 5 and
  undocumented; `MaxItems=20` for web URLs is also undocumented),
  deprecation policy, and `ListMeta`/`LabelSelector`/`Watch` ergonomics.

### Security

- **Security disclosure policy and supported versions** — `SECURITY.md`
  is 863 bytes today. Production OSS needs: contact email / PGP key,
  disclosure timeline, supported-version matrix, CVE history.
- **Network policy egress allowlist per store** — `NetworkPolicy`
  defaults to "DNS, API server, vector stores, external APIs". For a
  locked-down deployment, each KB should be able to declare the
  endpoints it needs to reach (e.g. `s3.amazonaws.com`,
  `github.com`, `qdrant.svc.cluster.local`) and have the operator
  reconcile the egress allowlist automatically. Today the policy is
  cluster-wide and must be hand-edited per environment.
- **Auth on the retriever** — the FastAPI server has no authentication.
  Production needs at least an API-key check (header or bearer) and
  ideally integration with OIDC or an auth-proxy sidecar.
- **TLS for the retriever** — there is no cert-manager integration; the
  Service is HTTP-only. Production needs automatic cert provisioning
  for the retriever (and optionally the operator metrics endpoint).
- **Rate limiting on `/query`** — the FastAPI server has no per-client
  rate limit. With LLM-backed generation on the hot path, a single
  client can exhaust worker / GPU budget. Need per-token buckets with
  429 responses.
- **Dependency pinning policy** — `setuptools==80.10.2` is pinned because
  pymilvus breaks on 81+. That pin carries known CVEs (currently
  accepted via `pip-audit || true` in CI). Production should either
  upgrade pymilvus to a version that supports setuptools 81+ or pin
  an older pymilvus that doesn't need setuptools 80.10+.
- **Per-KB network egress** — the web crawler SSRF guard validates
  per-request, but DNS resolution is per-request and not pinned to a
  specific IP. With DNS-rebinding protection added (resolved IPs
  cached for 60s), the protection is best-effort, not bulletproof.
  Production should additionally pin connections at the transport
  layer (custom `HTTPAdapter` that connects to the resolved IP but
  sets `Host` to the original hostname for SNI / cert verification).

### Observability & operations

- **Code coverage measurement** — no `go test -cover` in the Makefile or
  CI. Without coverage data, "what are we not testing?" is unanswerable.
  Production should publish a coverage report (Codecov / coveralls) and
  gate new code on minimum coverage per package.
- **Load / benchmark suite** — no benchmarks for ingest Job throughput
  (chunks/sec, MB/sec source), retriever p50/p95/p99 latency under
  concurrent load, or controller reconcile throughput. Defaults
  (`BatchSize=64`, `ActiveDeadline=7200s`, `resources: 1 CPU / 4Gi`) are
  unverified. Production needs a k6 / vegeta harness backed by
  Prometheus assertions.
- **Property-based testing** — `chunking.py` and `embeddings.py` accept
  arbitrary text inputs (empty, 10 MB, pure whitespace, control
  characters). No Hypothesis / `testing/quick` coverage. Production
  should fuzz the chunking and embed boundary conditions.
- **Race-condition tests** — `recordBest`, `applyAutoTune`, and
  `userEditedSpec` mutate `Status` from a single reconciler, but two
  concurrent Job events can race. No integration test simulates this.
- **OTel tracing** — already on the "Near term" list. Cross-process
  traces (operator → worker → store) are required to debug latency
  regressions in production.
- **VectorIndex probe batching** — each `VectorIndex` probes its store
  every 60s. With 1000 KBs, that's 17 req/s of small HTTP calls. Should
  be batched (e.g. one probe per store, returning health for all
  collections).
- **Auto-tune timing surfaced in status** — `Status.AutoTuneAttempts`
  increments but the total wall-clock time spent in the auto-tune loop
  is not recorded. Need a `Status.AutoTuneStartedAt` and a Prometheus
  histogram of tune-loop duration for SLI tracking.
- **SLO dashboards** — already on "Near term". Need: ingestion
  freshness (time since last successful index), recall percentiles,
  retriever p99 latency, error rates, saturation. Without SLOs
  defined, on-call can't make paging decisions.
- **Audit log shipping** — operator lifecycle events (KB created /
  modified / deleted, eval run, secret rotation observed) should be
  shipped to an external log aggregator with the actor's identity
  (today Kubernetes events are emitted but not forwarded).
- **CHANGELOG** — none. v0.1 → v0.2 → v0.3 transitions are invisible to
  consumers. Production needs a real changelog (per release, with
  `Added / Changed / Deprecated / Removed / Fixed / Security`).

### Resilience

- **TTL / deadline configurable per KB** — recently added. Default
  300s/7200s is fine for small corpora but too short for
  multi-million-chunk KBs. Make sure the new fields are tested under
  load.
- **Secret rotation independent of re-index** — recently added
  (`ObservedSecretsHash` separated from `ObservedSpecHash`). Verify
  with an integration test that rotates a Secret and asserts the
  store is untouched.
- **Stale Job detection** — if the operator dies mid-reconcile, an
  `ActiveJob` may be left set forever. Need a TTL-based auto-cleanup
  for `kb.Status.ActiveJob` (e.g. "if `ActiveJob` was set more than
  N× ActiveDeadline ago with no corresponding Job, clear it").
- **Worker pod preemption tolerance** — workers run with `RestartPolicy
  = Never` and no checkpoint. A 2-hour spot preemption wastes 2 hours
  of embedding cost. Need checkpoint / resume so an interrupted Job
  can pick up from the last persisted offset.
- **Backpressure on overlapping ingestions** — if a freshness cron
  fires while a manual ingestion is running, today the manual one
  wins (it's the `ActiveJob`). The cron should defer to the next tick,
  not get dropped silently. Need a queueing strategy.

### Multi-tenant & deployment

- **Single deployment surface** — today both `make deploy` (Kustomize)
  and the Helm chart exist; the chart is the recommended one but
  Kustomize is what the local demo uses. Pick one and deprecate the
  other; current state is "neither is fully tested in CI".
- **Cross-namespace references** — `Retriever.Spec.KnowledgeBaseRef`
  must be same-namespace, with no CEL or webhook enforcement (just a
  comment). For multi-tenant deployments, allow a Retriever in
  namespace A to mount a KB in namespace B, gated by an explicit
  `crossNamespaceRefs: true` flag and per-tenant RBAC.
- **Disaster recovery / backup** — the vector store is the source of
  truth, but there's no documented procedure for rebuilding from
  scratch. Need a `kuberag backup` / `restore` workflow that exports
  collection state to object storage and re-ingests on demand.
- **Worker ServiceAccount isolation** — see "API maturity". A
  compromised KB should not be able to delete another KB's
  ConfigMap or IngestionRun.

### Documentation

- **CONTRIBUTING.md details** — 2 KB today. Need: code-style guide,
  PR review process, how to run unit + integration + e2e, how to add
  a new source / store / provider, release checklist.
- **API reference generated from code** — `docs/API.md` is hand-written
  and out of date every time a field is added. Generate from
  `controller-gen` + `gen-crd-api-reference-docs` so the published
  reference is always in sync with the CRDs.
- **Operations runbook** — there is no `RUNBOOK.md`. On-call needs:
  "what does `PhaseFailed` mean", "how to re-trigger ingestion
  without losing state", "how to roll back a generation", "how to
  scale the retriever", "how to drain and restart workers".
- **Versioning and deprecation policy** — once `v1` lands, document
  which fields are deprecated, how long they'll be supported, and
  what the upgrade path is.
- **Reference architecture** — for a real production deployment, a
  diagram and example manifests covering: external Postgres for
  pgvector, external Qdrant cluster (HA), OIDC-fronted retriever, S3
  source with IRSA, secrets in External Secrets Operator. Today only
  the in-cluster k3d demo is documented.

### What's already done (from this work)

- Real Qdrant + pgvector integration tests in CI (Milvus nightly-only)
- pip-audit + Trivy container scan in CI
- Secret values separated from corpus `specHash`
- Configurable `TTL`, `ActiveDeadline`, model-cache size per KB
- Controller split into 5 focused files
- DNS rebinding mitigation + Psycopg `sql.Identifier` for table names
