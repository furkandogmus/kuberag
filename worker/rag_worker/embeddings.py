"""Embedding backends: local (fastembed) and OpenAI-compatible (OpenAI, Gemini,
OpenRouter-style, or any custom /embeddings endpoint)."""
from __future__ import annotations

import os

# Local fastembed models -> (fastembed id, dimension).
LOCAL_MODELS = {
    "bge-small": ("BAAI/bge-small-en-v1.5", 384),
    "bge-large": ("BAAI/bge-large-en-v1.5", 1024),
}

# Known hosted model dimensions (used to skip the runtime probe).
HOSTED_DIMS = {
    "text-embedding-3-small": 1536,
    "text-embedding-3-large": 3072,
    "text-embedding-004": 768,
    "text-embedding-005": 768,
    "gemini-embedding-001": 3072,
}

# Per-provider default OpenAI-compatible base URLs.
PROVIDER_BASE_URLS = {
    "openai": "https://api.openai.com/v1",
    "gemini": "https://generativelanguage.googleapis.com/v1beta/openai/",
    # openai-compatible has no default; baseURL must be supplied.
}


class Embedder:
    """Embeds documents/queries with a consistent dimension across providers."""

    def __init__(self, model: str, provider: str = "local", base_url: str = "", dimension: int = 0):
        self.model = model
        self.provider = provider or "local"
        self._client = None
        self._fe = None

        if self.provider == "local":
            from fastembed import TextEmbedding

            if model not in LOCAL_MODELS:
                raise ValueError(f"unknown local model: {model}")
            fe_id, self.dim = LOCAL_MODELS[model]
            self._fe = TextEmbedding(model_name=fe_id)
            return

        # Hosted, OpenAI-compatible.
        from openai import OpenAI

        url = base_url or PROVIDER_BASE_URLS.get(self.provider, "")
        if not url:
            raise ValueError(f"provider {self.provider!r} requires a baseURL")
        # Local OpenAI-compatible servers (Ollama, LM Studio, vLLM, TEI) need no
        # key; the SDK still requires a non-empty value, so default a placeholder.
        api_key = os.environ.get("EMBEDDING_API_KEY") or "no-key"
        self._client = OpenAI(base_url=url, api_key=api_key)

        # Resolve dimension: explicit override, known table, or runtime probe.
        if dimension and dimension > 0:
            self.dim = dimension
        elif model in HOSTED_DIMS:
            self.dim = HOSTED_DIMS[model]
        else:
            self.dim = len(self._embed_remote(["dimension probe"])[0])

    def _embed_remote(self, texts: list[str]) -> list[list[float]]:
        resp = self._client.embeddings.create(model=self.model, input=texts)
        return [d.embedding for d in resp.data]

    def embed_documents(self, texts: list[str]) -> list[list[float]]:
        if self._fe is not None:
            return [list(map(float, v)) for v in self._fe.embed(texts)]
        return self._embed_remote(texts)

    def embed_query(self, text: str) -> list[float]:
        return self.embed_documents([text])[0]


def from_spec(embedding: dict) -> Embedder:
    """Build an Embedder from a KnowledgeBase embedding spec dict."""
    return Embedder(
        embedding["model"],
        embedding.get("provider", "local"),
        embedding.get("baseURL", ""),
        int(embedding.get("dimension", 0) or 0),
    )
