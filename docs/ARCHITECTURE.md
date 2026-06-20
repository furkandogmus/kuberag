# Architecture

kuberag splits cleanly into a **control plane** (a Go operator that decides
*what* should happen and *when*) and a **data plane** (Python workers and a
serving process that do the actual work). They communicate only through the
Kubernetes API — Jobs, ConfigMaps, and CRD status.

```
                         ┌──────────────────────── control plane (Go) ────────────────────────┐
 kubectl apply  ─────▶   │  KnowledgeBaseReconciler   RetrieverReconciler   VectorIndexReconciler │
                         └───────┬───────────────────────────┬───────────────────────┬──────────┘
                                 │ creates / watches         │ creates               │ probes
                                 ▼                           ▼                       ▼
                         Job (ingest|eval|cleanup)   Deployment + Service     vector store HTTP
                                 │  ▲ result ConfigMap         │ (FastAPI /query)
                                 ▼  │                          ▼
                  ┌──────────── data plane (Python) ───────────────────────┐
                  │  rag_worker: sources → chunk → embed → vector store      │
                  │  retriever:  query → embed → search → (optional) LLM      │
                  └─────────────────────────────────────────────────────────┘
```

## Why two planes

Embedding and ingestion are heavy (ONNX runtimes, model downloads, large repos).
Keeping that out of the operator process means:

- the operator stays small, fast, and crash-safe;
- ingestion runs as ordinary Kubernetes **Jobs** — retried, resource-bounded,
  scheduled, and observable like any other workload;
- the ML dependency surface lives in one image, independent of the operator.

The operator never imports an ML library; the workers never watch the API.

## KnowledgeBase reconcile

The `KnowledgeBase` reconciler is a level-triggered state machine. Each pass:

1. **Finalizer / deletion.** On create, attach a finalizer. On delete, run a
   `cleanup` Job that drops the remote collection, then release the finalizer.
2. **Ensure `VectorIndex`.** Create/patch an owned `VectorIndex` describing the
   collection (store, dimension) so health is tracked independently.
3. **Suspend.** If `spec.suspend`, mark `Suspended` and stop.
4. **Compute `specHash`** over the *desired* state: sources, **spec** chunking
   (user intent), embedding model, and store. (See [Hashing](#hashing).)
5. **Reset stale auto-tune.** If the spec hash changed while an auto-tune
   override is stored, drop the override so the new ingest honours the spec.
6. **Finalize an in-flight Job.** If `status.activeJob` is set, read its state;
   on completion consume the [result ConfigMap](#result-configmaps) and update
   status; otherwise requeue.
7. **Decide work** (single in-flight Job at a time):
   - **Ingest** if needed — first run, spec drift, model change, or a freshness
     cron tick. Ingest takes priority over evaluation.
   - **Evaluate** if `retrievalQuality` is enabled and an eval is due.
   - Otherwise **requeue** near the next freshness/eval fire.

```
                 ┌─────────┐  spec/model/sources change   ┌──────────────┐
   create ─────▶ │ Pending │ ───────────────────────────▶ │  Ingesting   │
                 └─────────┘                               └──────┬───────┘
                      ▲                                           │ job complete
        freshness /   │                                           ▼
        eval cron     │                                     ┌──────────┐
                      └──────────── re-ingest ◀──────────── │  Ready   │
                                                            └────┬─────┘
                                                  eval below tgt │  (auto-tune)
                                                                 ▼
                                                          ┌────────────┐
                                                          │  Degraded  │
                                                          └────────────┘
```

## Ingestion

`buildIngestJob` renders a Job running `python -m rag_worker ingest`. The Job:

- runs under a dedicated worker ServiceAccount (least privilege: ConfigMaps only);
- receives the spec as `KB_SPEC_JSON`, the prior per-source revisions as
  `PRIOR_SOURCES_JSON`, and `INGEST_MODE` (`full` or `incremental`);
- has secret-backed env injected for source/store/embedding credentials;
- is resource-bounded (defaults: 1Gi request / 4Gi limit — ONNX + batches);
- has a short `ttlSecondsAfterFinished` so finished Jobs don't collide with the
  next scheduled run.

**Mode selection.** Incremental is only safe when the *reason* is a freshness
re-sync (the spec is unchanged, so a source skips iff its upstream revision is
unchanged). Any spec change, model change, or first run forces `full`.

**Incremental skip.** The worker computes a cheap revision per source
(`git ls-remote` SHA, sorted S3 ETags, crawl content hash). If it matches the
revision the operator last recorded and the mode is incremental, the source's
chunks are left untouched.

**Streaming.** Chunks are embedded and upserted in bounded batches via a
generator, so peak memory is one batch regardless of corpus size.

## Evaluation & auto-tune

When `retrievalQuality.enabled`, once the KB is `Ready` with data, the reconciler
runs an `eval` Job over a user-supplied dataset (a ConfigMap of
`{query, expectedSources}` lines). The worker measures recall@TopK and p95 latency.

- recall ≥ target → `RecallMet`, stays `Ready`.
- recall < target and `autoTune.enabled` and attempts remain → **AutoTuning**:
  the operator adjusts effective chunking (grow overlap, then shrink chunk size),
  stores it in `status.effectiveChunking`, sets `PendingRetune` to force a
  re-index (without disturbing `observedSpecHash`), clears the last evaluation so
  it re-evaluates, and bumps the attempt counter.
- attempts exhausted → **Revert to best**: the operator lands the KB on the
  chunking configuration that achieved the highest recall across all attempts
  (`settleOnBest`), forces one final re-index + re-eval, and if still below
  target goes `Degraded`. This prevents settling on the last (arbitrary) ladder
  step when a prior configuration performed better.
- **Empty dataset guard**: evaluations with `queries=0` (missing/empty dataset)
  are recorded with a `NoDataset` condition but skip the recall gate and
  auto-tune — a meaningless 0% recall won't churn the loop.

### Auto-tune ladder

`nextChunking` drives a structured exploration:

1. **Grow overlap** (+40 tokens per step) to reduce answer cuts across chunk
   boundaries.
2. Once overlap dominates the chunk, **shrink chunk size** (-200 tokens, floor
   300) and reset overlap to 20% — finer-grained, more precise chunks.
3. At the floor with max overlap, **rotate the split strategy** (semantic →
   recursive → fixed → semantic) and reset size/overlap — attack the corpus with
   a different boundary model rather than shrinking further.

`recordBest` snapshots the effective chunking + recall on every evaluation,
keeping the best by recall (ties keep the earlier, cheaper, larger-chunk config).
This memory lets `settleOnBest` revert to the optimal configuration on exhaustion.

Eval Jobs are named with an incrementing `status.evalRound`; ingest Jobs include
`ingestRound`, `autoTuneAttempts`, and a `chunkFingerprint` to guarantee unique
names across retries, auto-tune steps, and settle/revert re-indices — even before
the TTL expires.

## Job naming & collision avoidance

Ingest Job names carry three disambiguators so every run, retry, or auto-tune
step gets a unique name — even before `ttlSecondsAfterFinished` (300s) expires:

- **`ingestRound`** — increments on every ingestion attempt (initial, retry,
  freshness re-sync), ensuring no two sequential runs collide.
- **`autoTuneAttempts`** — identifies which tune iteration this ingest belongs to.
- **`chunkFingerprint`** — a hash of `(strategy, maxTokens, overlap)`, keeping a
  settle/revert re-index (same attempt counter, different chunking) distinct from
  the prior attempt.

On completion the operator immediately deletes the finished Job (rather than
relying on TTL), so the next run always gets a clean slate.

## Web crawl hardening

The web crawler treats seed URLs as authoritative and fails **loud** on any seed
error — connection failure, non-200 HTTP status, non-HTML response, cross-domain
redirect, no indexable text — producing a clear error rather than silently
producing an empty knowledge base. Discovered pages are more lenient (404s and
benign redirects are silently skipped), but retryable errors (429, 5xx) on any
page still cause the crawl to fail so the operator can retry.

## Hashing

`specHash` fingerprints the user's **spec** (sources + defaulted spec chunking +
model + store endpoint/type). Consequences:

- editing the spec (including chunking) changes the hash → re-ingest;
- an auto-tune override lives in status and does **not** change the hash —
  auto-tune triggers its own re-ingest by clearing `observedSpecHash`;
- so a user spec edit always wins over a stale auto-tune override.

## Result ConfigMaps

envtest/k8s has no way for a Job to return a value, so each worker writes a
small ConfigMap (`<job>-result`, key `result.json`). The operator reads it on
Job completion to learn the chunk total and per-source revisions, then deletes
it. The worker reports `max(store.count(), upserted)` so eventually-consistent
stores (Milvus) still surface an accurate total.

## Retriever

The `Retriever` reconciler resolves the referenced `KnowledgeBase`, then
creates/updates a `Deployment` + `Service` running the FastAPI server, wiring
the store, embedding provider, and optional generation provider via env (secrets
injected as needed). It mirrors the Deployment's readiness into
`status.readyReplicas`/`phase` and exposes the in-cluster endpoint.

`/query` embeds the query with the **same** provider used for ingestion, searches
the store, optionally reranks, and — if generation is configured — asks an
OpenAI-compatible chat model to synthesize an answer grounded in the retrieved
chunks, returning `{answer, results}`.

### Retrieval features

- **Hybrid search (RRF).** When enabled (per-Retriever default or per-request
  override), the server runs both dense vector search and lexical text search,
  then fuses results with Reciprocal Rank Fusion. The dense/lexical weight is
  configurable via `hybridDensePercent` (0 = pure lexical, 100 = pure dense,
  default 50).
- **Reranking.** An optional cross-encoder reranker re-scores retrieved candidates
  before returning the top K, with a configurable candidate pool size.
- **Per-request overrides.** Every tuning knob — `topK`, `hybrid`,
  `hybridDensePercent`, `scoreThresholdPercent`, `rerank`, `temperature`,
  `maxTokens`, `systemPrompt` — can be set per `/query` request without
  redeploying. This powers the built-in [Playground UI](#playground).
- **Metadata filtering.** Queries can filter by `source`, exact `docPath`, or
  `docPathPrefix`.
- **Playground UI.** A built-in HTML playground (`/`) lets you experiment with
  every retrieval and generation knob interactively, file/URL ingest for ad-hoc
  testing, and view per-query diagnostics (`meta` with candidate count, latency,
  threshold).

## VectorIndex

One `VectorIndex` is created per KnowledgeBase (owned, GC'd with it). Its
reconciler periodically probes the collection: for Qdrant it queries the HTTP
API for point count and dimension and reports `Healthy`/`Degraded`/`Missing`;
other stores report `Unknown` and rely on ingestion success. A dimension
mismatch between the store and the expected embedding dimension is surfaced as
`Degraded`.
