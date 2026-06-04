# Configuration & tuning guide

API.md lists *every* field; this page is the opinionated companion: **what the
knobs do, what values exist, and which to reach for.** Each section ends with a
copy-pasteable YAML fragment. All examples target `apiVersion:
rag.furkan.dev/v1alpha1`.

- [Chunking strategy](#chunking-strategy) — `KnowledgeBase.spec.chunking`
- [Embedding model & provider](#embedding-model--provider) — `spec.embedding`
- [Vector store & distance metric](#vector-store--distance-metric) — `spec.vectorStore`
- [Ingestion mode & freshness](#ingestion-mode--freshness) — `spec.ingestion`, `spec.freshness`
- [Retrieval quality & auto-tune](#retrieval-quality--auto-tune) — `spec.retrievalQuality`
- [Serving knobs](#serving-knobs-retriever) — `Retriever.spec`
- [Generation](#generation) — `Retriever.spec.generation`
- [Ready-made profiles](#ready-made-profiles)
- [What lives on which CRD](#what-lives-on-which-crd)

---

## Chunking strategy

`spec.chunking` controls how each document is split before embedding. Three
fields:

| Field | Default | Meaning |
|-------|---------|---------|
| `strategy` | `semantic` | How block boundaries are chosen. |
| `maxTokens` | `800` | Upper bound per chunk (tokens ≈ words × 1.3). |
| `overlap` | `80` | Tokens repeated between adjacent chunks. Must be `< maxTokens`. |

### The three strategies

| `strategy` | What it actually does | Reach for it when |
|------------|----------------------|-------------------|
| **`semantic`** | Splits on Markdown headings (`#`–`######`) and blank-line paragraph boundaries first, *then* packs words up to `maxTokens` within each block. Chunks stay aligned to document structure. | Markdown / structured docs (the common case). **Default, start here.** |
| **`recursive`** | Recursively splits on a separator hierarchy — paragraph (`\n\n`) → line (`\n`) → sentence (`. `) → word — choosing the coarsest boundary that keeps a piece under `maxTokens`, then greedily merges adjacent pieces (carrying `overlap`). Breaks land on natural boundaries without needing headings. | Prose / mixed text with no reliable heading structure. |
| **`fixed`** | A uniform sliding window of `maxTokens` words over the whole document with `overlap`, ignoring all structure. | You want predictable, uniform, structure-blind chunks. |

All three honour `maxTokens` and `overlap`; they differ only in *where* the cuts
land. `semantic` and `recursive` respect boundaries (headings vs. a separator
hierarchy); `fixed` does not. The implementation lives in
`worker/rag_worker/chunking.py`.

### Sizing `maxTokens` / `overlap`

- **Smaller chunks** (e.g. `300–500`) → more precise retrieval, finer-grained
  matches, more vectors (higher store cost), each result carries less context.
- **Larger chunks** (e.g. `800–1200`) → more context per hit, fewer vectors,
  but a single chunk may bury the relevant sentence and dilute the embedding.
- **Overlap** stops answers from being cut across a chunk boundary. ~10% of
  `maxTokens` is a sane start (the `800/80` default). Raising it improves recall
  at the cost of duplicate text and more vectors.

> Changing chunking re-processes the affected sources on the next reconcile (the
> `specHash` covers chunking). It does **not** force a model re-embed.

```yaml
chunking:
  strategy: semantic   # semantic | recursive | fixed
  maxTokens: 800
  overlap: 80
```

---

## Embedding model & provider

`spec.embedding` picks the model that turns chunks into vectors. **Changing
`model` triggers a full re-embed** (the whole collection is rebuilt), so choose
deliberately.

| `provider` | Runs where | `baseURL` | Auth | Example `model` |
|------------|-----------|-----------|------|-----------------|
| `local` | in the worker pod (fastembed) | — | none | `bge-small` (384d), `bge-large` (1024d) |
| `openai` | OpenAI API | preset | `apiKeySecretRef` | `text-embedding-3-small` (1536d) |
| `gemini` | Gemini (OpenAI-compatible) | preset | `apiKeySecretRef` | `gemini-embedding-001` (3072d) |
| `openai-compatible` | anything | **required** | optional | `nomic-embed-text`, vLLM/TEI models |

How to choose:

- **No keys / fully offline / cheap** → `local` `bge-small`. Great default for
  dev and most internal docs.
- **Higher quality, don't mind an API key/cost** → `openai` or `gemini`.
- **Self-hosted (Ollama, vLLM, LM Studio, TEI)** → `openai-compatible` + a
  `baseURL`. Dimension is auto-detected from a probe embedding.
- `dimension` only needs setting for an unknown model whose size can't be
  inferred — otherwise leave it (built-in table or auto-detect).

```yaml
embedding:
  model: bge-small
  provider: local
# hosted alternative:
# embedding:
#   model: text-embedding-3-small
#   provider: openai
#   apiKeySecretRef: { name: openai, key: apiKey }
```

See [PROVIDERS.md](PROVIDERS.md) for the full backend matrix and the
fully-local Ollama recipe.

---

## Vector store & distance metric

| Field | Values | Notes |
|-------|--------|-------|
| `type` | `qdrant` · `pgvector` · `milvus` | `qdrant` has full health probing + payload indexes for hybrid search. |
| `endpoint` | URL or DSN | `http://qdrant:6333`, `postgresql://…`, `http://milvus:19530`. |
| `collection` | string | Defaults to the KB name. |
| `distance` | `cosine` · `dot` · `euclid` | `cosine` is the right default for normalized embeddings. |

- **`cosine`** — default; correct for the normalized embeddings these models
  produce. Use it unless you have a specific reason not to.
- **`dot`** — when your model is tuned for inner-product similarity.
- **`euclid`** — L2 distance; rarely needed for text embeddings.

```yaml
vectorStore:
  type: qdrant
  endpoint: http://qdrant:6333
  collection: company-docs
  distance: cosine
```

---

## Ingestion mode & freshness

```yaml
ingestion:
  mode: incremental    # incremental | full
freshness:
  schedule: "0 */6 * * *"   # standard 5-field cron; empty = no scheduled re-sync
```

- **`incremental`** (default) — the worker probes each source's revision (git
  SHA, S3 ETag-set hash, crawl hash) and **skips unchanged sources**; only
  changed/added chunks are re-embedded and removed ones deleted. A spec change
  still forces a full re-process of the affected fields.
- **`full`** — recreates the collection every run. Use when you want a clean
  rebuild or suspect drift between the store and the sources.
- **`freshness.schedule`** — cron for periodic re-sync. Pair it with
  `incremental` so scheduled runs are cheap when nothing changed.

`ingestion` also carries pod-placement knobs (`resources`, `serviceAccountName`,
`nodeSelector`, `tolerations`, `affinity`) — see API.md.

---

## Retrieval quality & auto-tune

This is the headline feature: measure recall against a labelled dataset, and —
if you opt in — let the operator **tune chunking automatically** to hit a target.

```yaml
retrievalQuality:
  enabled: true
  evalSchedule: "0 * * * *"          # cron; empty = evaluate once after data exists
  datasetRef: { name: company-docs-eval }   # ConfigMap, key dataset.jsonl
  topK: 8                            # retrieval depth used during eval
  minimumRecallPercent: 80           # recall@TopK target (0–100)
  autoTune:
    enabled: true
    maxAttempts: 3
```

The dataset is a ConfigMap with key `dataset.jsonl`, one JSON object per line:

```json
{"query": "how do I configure incremental ingest?", "expectedSources": ["docs/ingest.md"]}
```

Recall is **recall@TopK**: the fraction of queries whose `expectedSources`
appear in the top-`topK` retrieved chunks.

### How auto-tune actually adjusts chunking

When measured recall < `minimumRecallPercent` and `autoTune.enabled` is true, on
each attempt the operator (see `applyAutoTune` in the controller):

1. **Grows overlap** by `+40` tokens.
2. Once overlap exceeds half of `maxTokens`, it **shrinks `maxTokens` by `200`**
   (floor 300) and resets overlap to `maxTokens / 5` — i.e. finer-grained chunks.
3. Clears the spec hash to **force a re-index** with the tuned chunking, then
   re-evaluates.

It repeats up to `maxAttempts`. If still short, the KB goes **`Degraded`**. The
tuned values land in `status.effectiveChunking` (your `spec.chunking` is left
untouched). Editing `spec.chunking` yourself discards the override and resets the
attempt counter.

> Auto-tune only touches **chunking** — never the embedding model. If recall is
> capped by model quality, switch models manually.

---

## Serving knobs (Retriever)

A `Retriever` is a Deployment+Service over a KnowledgeBase. Query-shaping fields:

| Field | Default | Effect |
|-------|---------|--------|
| `topK` | `8` | Chunks returned per query (overridable per request). |
| `hybrid` | `false` | Default **every** query to hybrid retrieval (dense vector + lexical search fused with RRF). Turn on when exact keywords/identifiers matter as much as semantics. |
| `hybridDensePercent` | `50` | When hybrid is active, weights dense vs lexical in RRF (`70` → 0.7 dense / 0.3 lexical; `0` → pure lexical, `100` → pure dense). Lower it when keyword/identifier matches matter more; raise it for paraphrase-heavy queries. |
| `scoreThresholdPercent` | `0` | Drop results below this similarity (0–100). Raise to cut weak matches. |
| `rerank.enabled` / `rerank.model` | `false` / `bge-reranker-base` | Cross-encoder re-ranks candidates for precision at extra latency. |
| `rerank.candidatePoolSize` | `0` (auto) | How many candidates to fetch *before* reranking; the reranker returns the top `topK`. Bigger pool → better quality, more latency. `0` = `max(4×topK, 20)`. |
| `replicas` | `1` | Server replicas (scale subresource enabled). |

**Hybrid: spec default vs per-request.** `spec.hybrid` sets the default for the
endpoint; an individual `/query` can always override it with its own `hybrid`
field (e.g. default on, but force `hybrid: false` for a latency-sensitive call).

**Reranking flow.** With `rerank.enabled`, the server fetches
`candidatePoolSize` candidates (auto = `max(4×topK, 20)`), runs the cross-encoder
over them, and returns the best `topK`. Widen the pool when the right chunk is
being retrieved but ranked just outside `topK`.

Per-request, `/query` also accepts `hybrid`, `source` / `docPath` /
`docPathPrefix` filters, and `history[]`.

```yaml
apiVersion: rag.furkan.dev/v1alpha1
kind: Retriever
metadata: { name: company-docs }
spec:
  knowledgeBaseRef: { name: company-docs }
  topK: 8
  hybrid: true                # default vector+lexical for every query
  hybridDensePercent: 60      # lean slightly toward semantic similarity
  scoreThresholdPercent: 20
  rerank:
    enabled: true
    candidatePoolSize: 50     # rerank 50 candidates, return top 8
```

---

## Generation

Add `spec.generation` to a Retriever to make `/query` return a synthesized
`answer` grounded in the retrieved chunks (full RAG). Without it, `/query`
returns ranked chunks only.

```yaml
generation:
  enabled: true
  provider: openai-compatible   # openai | openrouter | groq | gemini | openai-compatible
  model: qwen2.5:3b
  baseURL: http://host.docker.internal:11434/v1   # required for openai-compatible
  maxTokens: 512
  # systemPrompt: "Answer only from the provided context…"
  # apiKeySecretRef: { name: openai, key: apiKey }
```

`maxTokens`, `temperature`, and `systemPrompt` can be overridden per request on
`/query`. Provider/base-URL presets are in [PROVIDERS.md](PROVIDERS.md).

---

## Ready-made profiles

**Fast local dev — no keys, cheap, offline:**

```yaml
chunking:    { strategy: semantic, maxTokens: 800, overlap: 80 }
embedding:   { model: bge-small, provider: local }
vectorStore: { type: qdrant, endpoint: http://qdrant:6333 }
ingestion:   { mode: incremental }
```

**High-recall documentation — chase a quality target:**

```yaml
chunking:    { strategy: semantic, maxTokens: 500, overlap: 100 }
embedding:   { model: bge-large, provider: local }
vectorStore: { type: qdrant, endpoint: http://qdrant:6333 }
retrievalQuality:
  enabled: true
  datasetRef: { name: docs-eval }
  minimumRecallPercent: 85
  autoTune: { enabled: true, maxAttempts: 4 }
# Retriever: { topK: 10, hybrid: true, scoreThresholdPercent: 15, rerank: { enabled: true, candidatePoolSize: 60 } }
```

**Cost-sensitive hosted — fewer, larger chunks, hosted model:**

```yaml
chunking:    { strategy: semantic, maxTokens: 1000, overlap: 100 }
embedding:   { model: text-embedding-3-small, provider: openai, apiKeySecretRef: { name: openai, key: apiKey } }
vectorStore: { type: pgvector, endpoint: postgresql://… }
ingestion:   { mode: incremental }
freshness:   { schedule: "0 3 * * *" }
```

---

## What lives on which CRD

A quick map of which knobs belong where:

| Concern | CRD | Field |
|---------|-----|-------|
| Sources, chunking, embedding, store, freshness, ingest mode, eval, auto-tune | **KnowledgeBase** (`kb`) | `spec.*` (above) |
| `topK`, score threshold, rerank, replicas, generation, pod placement | **Retriever** (`rtr`) | `spec.*` |
| Collection health, point count, dimension | **VectorIndex** (`vi`) | auto-created & owned — **read-only**; you don't author it. `spec` is set by the operator (`knowledgeBaseRef`, `store`, `dimension`, `probeIntervalSeconds`). |

So: **KnowledgeBase** answers *what knowledge and how it's built*, **Retriever**
answers *how it's served and answered*, and **VectorIndex** is pure
observability the operator maintains for you.

For the exhaustive field-by-field reference, see [API.md](API.md).
