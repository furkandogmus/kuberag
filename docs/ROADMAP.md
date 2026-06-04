# Roadmap

kuberag is `v1alpha1`. This is a rough, non-binding direction — issues and PRs
welcome.

## Validated today

- Sources: GitHub (sparse clone), S3/MinIO, web crawl
- Stores: Qdrant, pgvector, Milvus
- Embeddings: local (fastembed) + OpenAI-compatible (OpenAI, Gemini, Ollama, …)
- Generation: OpenAI-compatible chat (OpenAI/OpenRouter/Groq/Gemini/Ollama)
- Incremental ingest, freshness cron, finalizer cleanup
- Eval + closed-loop chunking auto-tune
- In-cluster deploy, leader election, RBAC, events, Prometheus metrics
- CI (unit + envtest integration + lint) and multi-arch (amd64/arm64) images

## Near term

- **Helm chart / kustomize overlays** for install (currently raw manifests).
- **Health probing for pgvector and Milvus** (today only Qdrant is probed).
- **Validating/defaulting webhooks** (today: CEL validation only).
- **Incremental at file granularity** (skip is per-source; could diff changed files).
- **More sources**: Confluence/Notion, generic Git (non-GitHub), local PVC.
- **Rerank + hybrid search** options surfaced on `Retriever`.

## Later

- **`KnowledgeBase` composition** — multiple stores / multi-tenant namespaces.
- **Cost/usage accounting** for hosted embedding & generation providers.
- **Eval suites** beyond recall@k (faithfulness, latency SLOs, drift detection).
- **Autoscaling** ingestion workers by queue depth; GPU-aware scheduling.
- Progress toward a stable `v1` API.

See [issues](https://github.com/furkandogmus/kuberag/issues) to propose or pick up work.
