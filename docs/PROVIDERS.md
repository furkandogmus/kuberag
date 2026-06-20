# Providers & backends

## Sources

| Type | Config | Incremental marker | Notes |
|------|--------|--------------------|-------|
| `github` | `repo`, `ref`, `includeGlobs[]`, `tokenSecretRef` | commit SHA (`git ls-remote`) | Blobless + sparse clone: only matching paths are fetched. |
| `s3` | `bucket`, `prefix`, `region`, `endpoint`, `includeGlobs[]`, key secrets | sorted object ETags hash | Works with AWS S3 and S3-compatible stores (MinIO) via path-style addressing. |
| `web` | `urls[]`, `maxDepth`, `sameDomainOnly`, `maxPages` | crawl content hash | Bounded static-HTML crawler; normalizes URLs, deduplicates links, enforces redirect domains, and strips HTML to text. |

`includeGlobs` are gitignore-style with real `**` semantics (`docs/**/*.md`
matches nested *and* top-level). Empty globs fall back to known text extensions.

The web source is intended for documentation sites that render useful content
in the initial HTML response. It does not execute JavaScript or act as a
general-purpose browser crawler. `maxPages` is a fetch budget, so error pages and
empty responses cannot cause an unbounded crawl.

## Vector stores

| Type | `endpoint` | Health probe | Notes |
|------|-----------|--------------|-------|
| `qdrant` | `http://host:6333` | ✅ HTTP (points, dim) | Payload indexes on `source`, `doc_path`, and `text` for deletes, metadata filters, and hybrid text search. |
| `pgvector` | `postgresql://…` DSN | ⚠️ `Unknown` | Auto-creates table + `vector` extension; `<=>`/`<#>`/`<->` per metric. |
| `milvus` | `http://host:19530` | ⚠️ `Unknown` | Eventually-consistent counts; string PK. |

All stores implement: ensure/recreate collection, per-source delete (for
incremental), batched upsert, count, search, drop (finalizer cleanup).

## Embeddings

Set on `KnowledgeBase.spec.embedding`.

| Provider | Example model | Dimension | Auth |
|----------|---------------|-----------|------|
| `local` | `bge-small`, `bge-large` | 384 / 1024 | none (fastembed, in-process) |
| `openai` | `text-embedding-3-small` | 1536 | `apiKeySecretRef` |
| `gemini` | `gemini-embedding-001` | 3072 | `apiKeySecretRef` |
| `openai-compatible` | anything (`nomic-embed-text`, …) | auto-detected | optional (`baseURL` required) |

All hosted providers speak the OpenAI `/embeddings` API; `openai`/`gemini` are
presets that fill `baseURL`. Unknown model dimensions are auto-detected from a
probe embedding at ingest time. The query is always embedded with the same
provider used for ingestion.

## Retrieval tuning

Surfaced on `Retriever.spec` and overridable per `/query` request.

### Hybrid search (RRF)

When enabled, the retriever runs both dense vector and lexical text search, then
fuses results with Reciprocal Rank Fusion. The dense/lexical weight is
configurable:

| Field | Default | Description |
|-------|---------|-------------|
| `hybrid` | `false` | Enable hybrid (vector + lexical) retrieval by default. |
| `hybridDensePercent` | `50` | Dense (vector) weight in RRF (0 = pure lexical, 100 = pure dense). |

Per-request: set `hybrid` and `hybridDensePercent` on `/query` to override.

### Reranking

Optional cross-encoder reranking (fastembed in-process) re-scores candidates
before returning the top K:

| Field | Default | Description |
|-------|---------|-------------|
| `rerank.enabled` | `false` | Enable cross-encoder reranking. |
| `rerank.model` | `bge-reranker-base` | Reranker model name. |
| `rerank.candidatePoolSize` | `0` (auto: max(4×topK, 20)) | How many candidates to retrieve before reranking. |

A larger candidate pool gives the reranker more to work with at the cost of
latency. Per-request: set `rerank: false` to opt out of reranking on a
rerank-enabled Retriever.

### Per-request tuning knobs

Every retrieval and generation parameter can be overridden per `/query` call:

| Field | Description |
|-------|-------------|
| `topK` | Number of chunks returned (1-100). |
| `hybrid` / `hybridDensePercent` | Override hybrid search config. |
| `source` / `docPath` / `docPathPrefix` | Metadata filters. |
| `scoreThresholdPercent` | Drop results below this similarity (0-100). |
| `rerank` | Opt out of reranking (`false`). |
| `temperature` / `maxTokens` / `systemPrompt` | Override generation config. |
| `history` | Multi-turn conversation (list of `{role, content}`). |

The response includes a `meta` object with diagnostics: `topK`, `hybrid`,
`hybridDensePercent`, `scoreThresholdPercent`, `reranked`, `candidates`,
`returned`, `tookMillis`.

### Playground UI

The retriever serves an interactive HTML playground at `/` that lets you
experiment with every knob live, ingest files/URLs for ad-hoc testing, and see
per-query diagnostics — no redeploy needed.

## Generation (full RAG)

Optional, set on `Retriever.spec.generation`. After retrieval the server asks an
OpenAI-compatible chat model to answer grounded in the chunks; `/query` returns
`{answer, results}`.

| Provider | Base URL preset | Example model |
|----------|-----------------|---------------|
| `openai` | `api.openai.com/v1` | `gpt-4o-mini` |
| `openrouter` | `openrouter.ai/api/v1` | `google/gemini-2.0-flash-exp:free` |
| `groq` | `api.groq.com/openai/v1` | `llama-3.3-70b-versatile` |
| `gemini` | `generativelanguage.googleapis.com/v1beta/openai/` | `gemini-2.0-flash` |
| `openai-compatible` | your `baseURL` | e.g. Ollama `qwen2.5:3b` |

## Fully local with Ollama (no keys, no quotas)

Ollama exposes an OpenAI-compatible API, so it slots into `openai-compatible`
for **both** embedding and generation — no code changes, just a `baseURL`.

```bash
ollama pull nomic-embed-text && ollama pull qwen2.5:3b
# make Ollama reachable from the cluster (host networking varies by setup):
#   OLLAMA_HOST=0.0.0.0  ->  baseURL: http://host.docker.internal:11434/v1
kubectl apply -f config/samples/ollama.yaml
```

```yaml
embedding:
  provider: openai-compatible
  model: nomic-embed-text
  baseURL: http://host.docker.internal:11434/v1     # dimension auto-detected (768)
# Retriever:
generation:
  provider: openai-compatible
  model: qwen2.5:3b
  baseURL: http://host.docker.internal:11434/v1
```

## Credentials

Every secret is referenced, never inlined: `tokenSecretRef`,
`accessKeySecretRef`/`secretKeySecretRef`, `embedding.apiKeySecretRef`,
`vectorStore.credentialsSecretRef`, `generation.apiKeySecretRef`. The operator
injects them into worker/retriever pods as env from the named `Secret` keys.
Scope each Secret to least privilege.
