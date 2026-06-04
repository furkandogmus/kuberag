# API Reference

API group `rag.furkan.dev`, version `v1alpha1`. Short names: `kb`, `rtr`, `vi`.

## KnowledgeBase (`kb`)

The desired knowledge state.

### `spec`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `sources[]` | list | — (≥1) | Where documents come from. See [Source](#source). |
| `chunking.strategy` | enum `semantic`/`recursive`/`fixed` | `semantic` | How documents are split. |
| `chunking.maxTokens` | int | `800` | Upper bound per chunk (≈ tokens). |
| `chunking.overlap` | int | `80` | Token overlap between adjacent chunks. |
| `embedding.model` | string | `bge-small` | Embedding model. Changing it triggers a full re-embed. |
| `embedding.provider` | enum `local`/`openai`/`gemini`/`openai-compatible` | `local` | Backend. See [Providers](PROVIDERS.md). |
| `embedding.baseURL` | string | — | API base URL (required for `openai-compatible`). |
| `embedding.dimension` | int | auto | Override vector dimension (else known table or auto-detected). |
| `embedding.apiKeySecretRef` | SecretKeyRef | — | API key for hosted providers. |
| `vectorStore.type` | enum `qdrant`/`pgvector`/`milvus` | `qdrant` | Vector database. |
| `vectorStore.endpoint` | string | — | URL or DSN. |
| `vectorStore.collection` | string | KB name | Collection/table name. |
| `vectorStore.distance` | enum `cosine`/`dot`/`euclid` | `cosine` | Distance metric. |
| `vectorStore.credentialsSecretRef` | SecretKeyRef | — | Password/API key for the store. |
| `ingestion.mode` | enum `full`/`incremental` | `incremental` | Re-process everything vs skip unchanged sources. |
| `ingestion.resources.cpu` / `.memory` | string | `250m` / `4Gi` limit | Worker pod resources. |
| `ingestion.serviceAccountName` | string | `kuberag-worker` | SA for ingestion Jobs (e.g. for IRSA). |
| `freshness.schedule` | cron (5-field) | — | Periodic re-sync; empty disables. |
| `retrievalQuality` | object | — | Eval + auto-tune. See [below](#retrievalquality). |
| `workerImage` | string | built-in | Override the worker image. |
| `suspend` | bool | `false` | Pause all reconciliation actions. |

#### Source

`name` (unique, for incremental tracking) + `type` + one matching block:

- **github**: `repo` (`owner/name`), `ref`, `includeGlobs[]` (gitignore-style, `**` aware), `tokenSecretRef`.
- **s3**: `bucket`, `prefix`, `region`, `endpoint` (for MinIO/compatible), `includeGlobs[]`, `accessKeySecretRef`, `secretKeySecretRef`.
- **web**: `urls[]`, `maxDepth` (default 1), `sameDomainOnly` (default true), `maxPages` (default 200).

#### retrievalQuality

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Run evaluations. |
| `evalSchedule` | cron | — | When to evaluate; empty = once after data exists. |
| `datasetRef.name` | string | — | ConfigMap with key `dataset.jsonl` (`{query, expectedSources[]}` per line). |
| `topK` | int | `8` | Retrieval depth during eval. |
| `minimumRecallPercent` | int 0–100 | — | Target recall@TopK. |
| `autoTune.enabled` | bool | `false` | Adjust chunking to chase the target. |
| `autoTune.maxAttempts` | int | `3` | Tuning iterations before `Degraded`. |

### `status`

`phase` (`Pending`/`Ingesting`/`Ready`/`Degraded`/`Failed`/`Suspended`),
`observedSpecHash`, `observedEmbeddingModel`, `effectiveChunking` (auto-tune
override), `autoTuneAttempts`, `evalRound`, `lastIndexedTime`, `indexedChunks`,
`sources[]` (per-source revision + chunk count), `evaluation`
(`recallPercent`, `p95LatencyMillis`, `queries`, `time`), `activeJob`,
`conditions[]` (`Ready`, `Ingesting`, `Evaluated`).

## Retriever (`rtr`)

A serving endpoint over a KnowledgeBase.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `knowledgeBaseRef.name` | string | — | KnowledgeBase to serve (same namespace). |
| `topK` | int | `8` | Default chunks per query. |
| `scoreThresholdPercent` | int 0–100 | `0` | Drop results below this similarity. |
| `rerank.enabled` | bool | `false` | Cross-encoder rerank of candidates. |
| `rerank.model` | string | `bge-reranker-base` | Reranker model. |
| `replicas` | int | `1` | Server replicas (scale subresource enabled). |
| `image` | string | built-in | Override the retriever image. |
| `generation` | object | — | Optional LLM answer synthesis. See [below](#generation). |

#### generation

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Synthesize an answer from retrieved chunks. |
| `provider` | enum `openai`/`openrouter`/`groq`/`gemini`/`openai-compatible` | — | Chat backend (all via OpenAI-compatible API). |
| `model` | string | — | Chat model name. |
| `baseURL` | string | — | Override (required for `openai-compatible`, e.g. Ollama). |
| `apiKeySecretRef` | SecretKeyRef | — | API key; optional for local servers. |
| `maxTokens` | int | `512` | Answer length cap. |
| `systemPrompt` | string | built-in | Override the grounding instruction. |

`status`: `phase`, `serviceEndpoint`, `readyReplicas`, `conditions[]` (`Available`).
`/query` returns `{query, results[]{text,source,docPath,score}, answer?}`.

## VectorIndex (`vi`)

Auto-created per KnowledgeBase (owned). `spec`: `knowledgeBaseRef`, `store`,
`dimension`, `probeIntervalSeconds` (default 60). `status`: `health`
(`Healthy`/`Degraded`/`Missing`/`Unknown`), `pointCount`, `dimension`,
`message`, `lastProbeTime`.
