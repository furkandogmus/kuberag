# Changelog

All notable changes to kuberag are documented in this file. The
format is loosely based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
as soon as `v1` ships. Until then, `v0.x.y` versions may include
breaking changes between minors.

## [Unreleased]

### Added
- **Retriever API-key authentication** through
  `Retriever.spec.apiKeySecretRef`. Protected retrievers accept either
  `Authorization: Bearer` or `X-API-Key`; `/healthz` remains open for
  Kubernetes probes, and Secret rotation triggers an automatic rollout.
- **Retriever production guards**: optional per-client token-bucket rate
  limiting, bounded rate-limit state, per-pod concurrency caps, streaming
  request-body limits, and standard 429/503 `Retry-After` responses.
- **Controller-managed Retriever PDB and CPU HPA**, plus an operator PDB in the
  Helm chart. Missing or empty referenced Secrets now keep the Retriever
  `Pending` and remove its serving workload until configuration is repaired.
- **Kustomize render validation** in CI. The previously broken `config/crd/`
  directory reference now has its own Kustomization, so `kubectl apply -k
  config/` renders successfully.
- **SBOM and provenance attestations** for all operator, worker, and retriever
  images published by the release workflow.
- **Managed Retriever Ingress/TLS and OIDC login**. `spec.ingress` creates an
  owned Kubernetes Ingress with optional cert-manager ClusterIssuer metadata;
  `spec.oidc` adds a pinned oauth2-proxy sidecar, Secret-backed client/cookie
  credentials, optional email-domain/group restrictions, and an owned
  NetworkPolicy that prevents in-cluster clients from bypassing the proxy.
- **Namespace-scoped operator mode** through `WATCH_NAMESPACE` and Helm
  `rbac.scope=namespace`, which renders namespaced Role/RoleBinding resources.
- **Per-KnowledgeBase worker identity**. Default worker Jobs receive an owned
  ServiceAccount and RBAC limited to the current result ConfigMap (and eval
  dataset when needed); access is removed when the Job finishes. Custom
  ServiceAccounts remain supported and are validated before Job creation.
- **Production Readiness section** in `ROADMAP.md` cataloguing the
  gaps between `v1alpha1` and a production-viable operator (API
  maturity, security, observability, resilience, multi-tenancy,
  documentation).
- **`ObservedSecretsHash`** status field. Secret *value* changes no
  longer trigger re-indexing; they only cancel in-flight Jobs and
  pick up new credentials on the next run. Secret *references*
  (name/key) still hash into the corpus spec.
- **Configurable ingestion tuning** per `KnowledgeBase`:
  `spec.ingestion.ttlSecondsAfterFinished`,
  `spec.ingestion.activeDeadlineSeconds`,
  `spec.ingestion.modelCacheSizeLimit`. Defaults preserve prior
  behavior (300s, 7200s, 2Gi).
- **Real Qdrant + pgvector integration tests** in CI
  (`worker/tests/test_stores_integration.py`), run against service
  containers.
- **Milvus nightly integration tests** in `.github/workflows/nightly.yaml`
  (heavy; not on every PR).
- **`pip-audit`** for both worker and retriever dependency files
  (advisory; `setuptools 80.10.2` pin is intentionally accepted
  until `pymilvus` drops the constraint).
- **Trivy container vulnerability scan** for all three images
  (operator distroless = fail-on-HIGH; worker/retriever = advisory).
- **`docs/RUNBOOK.md`** — operator on-call handbook: phase meaning,
  re-trigger, rollback, scaling, drain/restart.
- **Expanded `CONTRIBUTING.md`** with code style, PR process,
  release checklist, and how to add new sources/stores/providers.
- **Expanded `SECURITY.md`** with disclosure timeline, supported
  versions, severity classification, known accepted risks.
- **Reference architecture** for a production deployment
  (external Postgres, HA Qdrant, OIDC-fronted retriever, IRSA, ESO).
- **Versioning & deprecation policy** (`docs/VERSIONING.md`).
- **Controller split**: `knowledgebase_controller.go` (1032 → 247
  lines) now imports focused files for ingest, evaluation, deletion,
  vector-index helpers, and decision helpers.
- **Generated API reference** via `gen-crd-api-reference-docs` in
  CI; `docs/API.md` is now auto-generated.

### Changed
- **Controller binary image** (`kuberag`): still distroless, still
  runs as `USER 65532`. No new security risks.
- **`pip-audit` invocation** in CI now uses the correct severity
  filtering flags. Advisory mode (`|| true`) for the `setuptools`
  pin; mandatory mode for the rest.
- **Trivy action** bumped from `0.28.0` (didn't exist) to `v0.36.0`.
- **Dependency versions** (resolve HIGH CVEs):
  - `requests` 2.32.3 → 2.33.0
  - `pypdf` 5.1.0 → 6.7.3
  - `fastapi` 0.115.6 → 0.115.12 (transitively bumps `starlette` to
    a non-vulnerable version)
  - `uvicorn` 0.34.0 → 0.38.0
  - `python-multipart` 0.0.9 → 0.0.31
  - `pillow` pinned to `>=12.1.1` (transitively pulled)

### Fixed
- **Secrets hash label length** — `computeSecretsHash` previously
  returned 64 hex characters, exceeding the 63-character Kubernetes
  label-value limit and breaking Job creation with `metadata.labels:
  Invalid value`. Truncated to 8 hex chars (32 bits of entropy is
  sufficient for change detection).
- **PgVectorStore missing `self.collection`** — `staging_name` and
  other base-class methods referenced `self.collection`, which the
  PgVectorStore constructor never set. Fixed; added a regression
  test via the integration suite.
- **Web crawler DNS amplification** — added a 60s TTL cache for
  resolved addresses so repeat hostname lookups don't hit the
  resolver (also reduces DoS surface and rebinding window).
- **PgVectorStore SQL injection surface** — replaced all f-string
  table-name interpolations with `psycopg.sql.Identifier` and
  `psycopg.sql.SQL` composables.
- **Web crawl `list.pop(0)`** replaced with `collections.deque.popleft()`.
- **`_replay_into` temp directory leak** — `tempfile.mkdtemp()` was
  not cleaned up on error; now wrapped in `try/finally` with
  `shutil.rmtree`.
- **`go test -cover`** — coverage data is now collected in CI.
- **Generated-artifacts stale check** — `make manifests generate` was
  failing CI because the CRD was out of date after the new
  IngestionSpec fields. Re-ran controller-gen and committed.
- **Integration tests running before services were up** — the
  `worker tests` step was running `python -m unittest discover`,
  which found `test_stores_integration.py` and tried to connect
  before the Qdrant/pgvector service containers were ready. Fixed
  by listing the unit-test modules explicitly.

## [0.3.x] - prior

This changelog starts with the most recent work. Earlier history is
in `git log --oneline`. Major themes in the recent past:

- **Hardened ingestion lifecycle**: atomic ingestion, active state
  in Job resolution, S3 consistency via ETag versioning, orphan
  cleanup, and generation error isolation.
- **Architectural fix-up**: resolved a result-race condition where
  the operator could miss a Job completion, the cleanup Job could
  orphan a finalizer, the generation handler could fail the whole
  query on an LLM-side error, and embedding prefixes were being
  applied inconsistently.
- **Versioned physical collection + fixed alias design** in the
  Qdrant path (one-time migration from the legacy physical-name
  layout baked in for safety).
- **P0/P1 audit findings**: alias correctness, atomicity of
  collection swap, `ActiveJob` state machine, SSRF protection in
  the web crawler, `pruneIngestionRuns` semantics, and Helm
  `ingestionruns` RBAC.
- **`IngestionRun` CRD**: immutable per-run audit record, controller
  sets `Phase: Running → Succeeded | Failed`.
- **golangci-lint cleanup**: errcheck, staticcheck, unused.
- **Embedding prefix handling**: the worker's
  `documentPrefix`/`queryPrefix` is now applied per-call, not at
  the Embedder level, so prefix/suffix matches between ingestion
  and query are guaranteed.

[Unreleased]: https://github.com/furkandogmus/kuberag/compare/main...HEAD
