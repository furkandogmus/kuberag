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
- **API-key authentication**: optional Secret-backed Bearer or `X-API-Key`
  protection on all endpoints except `/healthz`; Secret rotation rolls pods
- **Ingress, TLS, and OIDC**: optional managed Ingress with cert-manager
  ClusterIssuer annotation and an oauth2-proxy sidecar for generic OIDC login
- **Runtime overload protection**: optional per-client token-bucket rate
  limiting plus per-pod concurrency and streaming request-body limits
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
- Retriever Prometheus endpoint with request/error/latency/saturation metrics,
  SLO dashboard panels, bundled PrometheusRule alerts, and a concurrent
  dependency-free load-test harness

### CRD design
- Six resources: `KnowledgeBase` (`kb`), `Retriever` (`rtr`), `VectorIndex`
  (`vi`), `IngestionRun`, `Backup`, and `Restore`
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
- **Managed PodDisruptionBudget** for Retriever deployments and Helm-managed
  PDB for the operator
- **Retriever HPA**: optional CPU-based autoscaling with configurable min/max
  replicas and utilization target
- **Startup/liveness/readiness probes** on operator and retriever
- **Topology spread** (zone anti-skew) on retriever
- **Secured images**: distroless operator, `USER 65532` on worker/retriever,
  readOnlyRootFilesystem, drop ALL capabilities, runtime default seccomp
- Multi-arch images (amd64, arm64) published to GHCR via release workflow
- Leader election with coordination.k8s.io leases
- Namespace-scoped cache/RBAC mode via `WATCH_NAMESPACE` and Helm
  `rbac.scope=namespace`
- Per-KB worker ServiceAccount + Role/RoleBinding with temporary,
  resource-name-scoped ConfigMap access

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
- **Multi-namespace scoped Helm mode** — the binary accepts comma-separated
  `WATCH_NAMESPACE`, while Helm's namespaced Role mode targets one namespace.
  Multiple tenant namespaces currently require one release per namespace or
  manually composed RoleBindings.

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
- **Custom-metric Retriever autoscaling** — CPU-based HPA is managed by the
  operator and request/latency/saturation metrics are now exported. Wiring
  Prometheus Adapter metrics into HPA remains.
- **Streaming generation (SSE)** — server-sent events for token-by-token LLM
  output, giving users progressive answer rendering.
- **OpenShift Route support** — Kubernetes Ingress with TLS and OIDC is managed
  by the operator. Native OpenShift Route generation is still not implemented.

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
- **Store-level tracing** — OTLP propagation now connects reconcile → Job →
  worker and Retriever query spans. Individual vector-store HTTP/SQL operations
  still need child spans.
- **Worker Job logs aggregation** — surface worker logs in `kubectl describe kb`
  or status conditions for faster debugging when an ingestion fails.
- **Per-KnowledgeBase freshness SLO** — namespace-level successful-ingestion
  timestamps, dashboarding, and stale-ingestion alerts are bundled. A bounded
  per-KB view requires an opt-in label allowlist or status exporter.

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
- **Cross-namespace tenant administration** — namespace-scoped mode and
  per-KB worker identities are available. A centralized multi-namespace
  installation still needs automated tenant onboarding/offboarding and
  per-namespace RoleBinding provisioning.
- **API review against Kubernetes SIG conventions** — before `v1`,
  publish a public API review covering field immutability, default
  semantics, list semantics (`MaxItems` for Sources is 5 and
  undocumented; `MaxItems=20` for web URLs is also undocumented),
  deprecation policy, and `ListMeta`/`LabelSelector`/`Watch` ergonomics.

### Security

- **Network policy egress allowlist per store** — `NetworkPolicy`
  defaults to "DNS, API server, vector stores, external APIs". For a
  locked-down deployment, each KB should be able to declare the
  endpoints it needs to reach (e.g. `s3.amazonaws.com`,
  `github.com`, `qdrant.svc.cluster.local`) and have the operator
  reconcile the egress allowlist automatically. Today the policy is
  cluster-wide and must be hand-edited per environment.
- **Fine-grained authorization policy** — generic OIDC login and group/email
  restrictions are available through the managed oauth2-proxy sidecar.
  Per-route/per-document authorization and policy engines such as OPA remain
  external.
- **Operator metrics TLS/auth** — Retriever traffic can use managed
  Ingress/cert-manager TLS, but the operator metrics endpoint is still plain
  HTTP and should be isolated or protected by the platform.
- **Distributed rate limiting** — the default bounded token bucket remains
  per-pod, while an optional Secret-backed Redis backend applies one atomic
  server-time quota across all replicas and fails closed on backend outages.
  Redis Cluster/Sentinel topology drills remain.
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

- **Ingest/controller benchmark suite** — Retriever concurrency, throughput,
  error rate, and p50/p95/p99 latency now have a repeatable load harness and
  Prometheus metrics. Ingest chunks/sec, MB/sec, and controller reconcile
  throughput benchmarks are still missing.
- **Property-based testing** — `chunking.py` and `embeddings.py` accept
  arbitrary text inputs (empty, 10 MB, pure whitespace, control
  characters). No Hypothesis / `testing/quick` coverage. Production
  should fuzz the chunking and embed boundary conditions.
- **OTel tracing** — already on the "Near term" list. Cross-process
  traces (operator → worker → store) are required to debug latency
  regressions in production.
- **VectorIndex probe batching** — each `VectorIndex` probes its store
  every 60s. With 1000 KBs, that's 17 req/s of small HTTP calls. Should
  be batched (e.g. one probe per store, returning health for all
  collections).
- **Per-KnowledgeBase freshness SLO** — Retriever and namespace-level ingestion
  SLOs now have dashboard panels and alerts. Per-KB freshness remains excluded
  by the default cardinality budget.
- **Audit log shipping** — operator lifecycle events (KB created /

### Resilience

- **Worker pod preemption tolerance** — full ingestions persist completed-source
  checkpoints and the next Job resumes without re-embedding those sources.
  File/batch-level checkpoints are still needed to avoid repeating the current
  large source after preemption.
- **Backpressure on overlapping ingestions** — freshness triggers that arrive
  during an active Job are recorded and run after it settles instead of being
  silently dropped. A general multi-trigger queue is still out of scope.

### Multi-tenant & deployment

- **Single deployment surface** — Helm is the recommended production surface;
  Helm contracts and Kustomize rendering are both tested in CI. Kustomize
  remains the development/demo base, so versioned release support and
  deprecation ownership still need to be explicit.
- **Cross-namespace references** — `Retriever.Spec.KnowledgeBaseRef`
  must be same-namespace, with no CEL or webhook enforcement (just a
  comment). For multi-tenant deployments, allow a Retriever in
  namespace A to mount a KB in namespace B, gated by an explicit
  `crossNamespaceRefs: true` flag and per-tenant RBAC.
- **Disaster recovery / backup** — `Backup` and `Restore` CRDs export vector
  points to S3-compatible object storage and restore through a verified staging
  collection with atomic promotion. Scheduling, retention, encryption/KMS,
  cross-cluster portability, and a real-store end-to-end restore drill remain.
- **Worker network identity** — Kubernetes API access is isolated per KB, but
  network egress is still shared unless per-KB NetworkPolicies are configured.

### Testing gaps

Items that the existing test pyramid (unit / envtest integration /
Python mock / k3d e2e) does not cover. These are not about new
features; they are about confidence in what's already built.

- **Helm chart test** — `helm lint` is in CI (`make lint-helm`);
  rendered resource contracts now verify cluster/namespace RBAC scope,
  WATCH_NAMESPACE wiring, image overrides, restricted security defaults,
  PDB/PriorityClass, NetworkPolicies, ServiceMonitors, and PrometheusRule
  alerts. A chart-testing install test against a disposable cluster remains.
- **Upgrade test** — envtest covers resource round-trips, stored-version/status
  behavior, active-Job recovery, and Helm render/RBAC upgrade contracts. A true
  previous-release → current-release cluster upgrade with old CRDs and binaries
  remains.
- **Multi-replica operator** — lease objects and reconcile recovery are covered,
  but no integration/e2e test runs two live managers and kills the elected
  leader during ingestion. True leader handoff remains unverified.
- **pgvector e2e** — the k3d e2e test only deploys Qdrant.
  pgvector (and Milvus) are not exercised end-to-end.
- **Auto-tune e2e assertion** — the e2e test waits for
  `Phase=Ready` but does not assert that the recall target
  was met or that `AutoTuneAttempts` stabilised at a
  reasonable value. An auto-tune loop could silently regress
  without the e2e catching it.
- **Secret rotation e2e** — rotate a referenced `Secret` and
  assert `ObservedSpecHash` is unchanged. Currently only a
  unit-level test covers this.
- **Multi-source e2e** — GitHub + S3 + Web sources in the
  same `KnowledgeBase`. The e2e only indexes one GitHub repo.
  Source-index cross-talk (e.g. S3 key mangles GitHub
  revision) is untested.
- **Chaos test** — envtest simulates failed/stale ingestion and pre-existing
  cleanup Jobs, verifying state/finalizer recovery. Process-level operator
  kills, mid-evaluation recovery, node loss, and network partitions still need
  a disposable-cluster chaos suite.
- **Multi-arch e2e** — arm64 worker/retriever images are
  built and published but never tested on arm64 hardware or
  emulation.
- **Air-gapped test** — run the worker without internet
  access (model already cached, sources locally reachable).
  Today HuggingFace model downloads are the only cache path;
  no PVC-backed persistent model cache is tested.

### Supply chain & operational safety

- **`govulncheck`** — Go dependency vulnerability scan. pip-audit
  covers Python; Go's `stdlib` and `k8s.io/*` dependencies are
  not scanned. `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
  is a single CI step.
- **Image signing and policy enforcement** — release builds publish BuildKit
  SBOM and maximum-mode provenance attestations and keylessly sign all three
  image digests with `cosign`. A Kyverno v1.18+
  `ImageValidatingPolicy` example verifies the exact GitHub Actions workflow
  identity, requires Rekor transparency-log validation, mutates tags to
  digests, and rejects unsigned images. An end-to-end admission test remains.
- **Operator HA validation** — the Helm chart installs an operator PDB, leader
  election is enabled, and lease/recovery behavior has envtest coverage. A
  two-manager process-level leader-handoff test remains before claiming HA.
- **Pod Security Standards** — generated ingest/backup/restore Jobs,
  Retriever Deployments (including the OIDC sidecar), Helm defaults, and the
  static manager manifest are regression-tested for the `restricted` profile:
  non-root execution, RuntimeDefault seccomp, no privilege escalation, dropped
  capabilities, read-only roots, and no host namespace/hostPath use. A real
  namespace with `pod-security.kubernetes.io/enforce=restricted` remains as an
  end-to-end admission test.
- **Metrics cardinality policy** — operator metrics now aggregate by namespace
  and Retriever metrics use only bounded result/reason labels. The series
  budget is documented, operator descriptors are regression-tested for
  forbidden labels, and unexpected Retriever values collapse to `other`.
- **Log sampling / rate limiting** — worker logs use a bounded token bucket
  (30-message burst per 10 seconds) with regression coverage. Structured log
  levels and a configurable policy remain future improvements.
- **CRD pruning** — envtest submits an unstructured KnowledgeBase with a
  typo'd field, verifies API-server pruning on read-back, and confirms the
  controller still reconciles the resource.
- **Resource quota compatibility** — quota and LimitRange create failures are
  surfaced as a `ResourceQuotaExceeded` condition with bounded retries instead
  of controller-loop errors. Unit coverage verifies the ConfigMap rejection
  path and distinguishes quota failures from ordinary RBAC denials. A real
  namespace-level ResourceQuota end-to-end test remains.
- **ConfigMap 1MB limit** — ingestion now fails fast with a
  `SpecConfigTooLarge` condition and suppresses retries until the
  `KnowledgeBase` generation changes. Eval/restore serialize only embedding
  and vector-store fields; cleanup/backup serialize only vector-store fields,
  so large source lists do not affect those Jobs. Admission-time size feedback
  and an end-to-end oversized-object test remain.
#### Quick wins (this iteration, sub-1-hour each)
- **`govulncheck` CI step** — Go vulnerability scanning (advisory).
- **`helm lint` in CI** — + `make lint-helm` Makefile target.
- **Operator PDB template** — `config/rbac/operator-pdb.yaml`,
  `minAvailable=1` against voluntary disruptions.
- **`BatchSize` configurable** — `spec.ingestion.batchSize` CRD
  field (default 64). Worker reads it from the spec JSON.

### What's already done (from this work)

#### From previous iteration (control/data plane separation + initial hardening)
- Real Qdrant + pgvector integration tests in CI (Milvus nightly-only)
- pip-audit + Trivy container scan in CI
- Secret values separated from corpus `specHash`
- Configurable `TTL`, `ActiveDeadline`, model-cache size per KB
- Controller split into 5 focused files
- DNS rebinding mitigation + Psycopg `sql.Identifier` for table names

#### Documentation (all completed)
- `CONTRIBUTING.md` — code style (Go/Python/YAML), PR process, release
  checklist, how to add new source/store/provider
- `SECURITY.md` — disclosure timeline (90-day), supported versions,
  severity classification, known accepted risks, CVE history table
- `docs/CHANGELOG.md` — Keep-a-Changelog format with `[Unreleased]`
  section
- `docs/RUNBOOK.md` — on-call handbook: phase meanings, re-trigger,
  rollback, scaling, drain/restart, common failure modes, jq queries
- `docs/VERSIONING.md` — SemVer + Kubernetes API conventions,
  deprecation policy, supported-versions matrix
- `docs/ARCHITECTURE-reference.md` — production deployment: HA, IRSA,
  ingress with OIDC, NetworkPolicy, observability, DR procedure
- `docs/API.md` — auto-generated from CRD YAML via `hack/gen-api-docs.py`;
  CI checks staleness on every push

#### Controller & observability (this iteration)
- **Code coverage in CI** — `go test -coverprofile` with `MIN_COVERAGE`
  gate (default %10). Coverage artifact uploaded. `make test-coverage`.
  Current: 19.7% total, 29.8% controller package.
- **Sources `MaxItems` documented** — `Sources` (5) and `WebSource.URLs`
  (20) now carry kubebuilder descriptions explaining the limit and
  how to scale past it (split into multiple KBs).
- **Stale Job detection** — new `Status.ActiveJobStartedAt` +
  `isActiveJobTimedOut()`. Clears `ActiveJob` if the Job is past
  `ActiveDeadlineSeconds + 10 min` grace. Defensive cleanup for lost
  watch events / leader handoffs. Sets `IngestionStuck` condition.
- **Auto-tune timing** — new `Status.AutoTuneStartedAt` +
  `rag_knowledgebase_autotune_duration_seconds` Prometheus histogram.
  `recordAutoTuneDuration()` observes on converge / exhaust / reset.
- **Secret rotation verified** — unit test that rotates a referenced
  Secret and asserts `ObservedSpecHash` is unchanged while
  `ObservedSecretsHash` updates. Bug fixed: `ObservedSecretsHash` was
  only set on first reconcile, now updated on every reconcile when the
  computed value differs.
- **Race condition test** — documents the `statusUpdate` wholesale-replace
  semantics. controller-runtime WorkQueue serialises per-object-key so
  this is safe in practice; test is a tripwire if
  `MaxConcurrentReconciles` is ever raised above 1 without first
  converting to a JSON merge patch.
