"""FastAPI retrieval (and optional generation) server deployed by the Retriever.

Config comes entirely from env (set by the operator):
  VECTORSTORE_TYPE, VECTORSTORE_ENDPOINT, VECTORSTORE_COLLECTION, VECTORSTORE_CREDENTIAL
  EMBEDDING_MODEL, EMBEDDING_PROVIDER, EMBEDDING_BASE_URL, EMBEDDING_DIMENSION, EMBEDDING_API_KEY
  TOPK, SCORE_THRESHOLD, RERANK_ENABLED, RERANK_MODEL
  GEN_ENABLED, GEN_PROVIDER, GEN_MODEL, GEN_BASE_URL, GEN_API_KEY, GEN_MAX_TOKENS, GEN_SYSTEM_PROMPT
"""
from __future__ import annotations

import os

from contextlib import asynccontextmanager
from fastapi import FastAPI
from pydantic import BaseModel

from rag_worker.embeddings import from_spec
from rag_worker.stores import make_store

@asynccontextmanager
async def lifespan(app: FastAPI):
    yield
    global _store
    if _store is not None:
        try:
            _store.close()
        except Exception:
            pass

app = FastAPI(title="kuberag-retriever", lifespan=lifespan)

_DEFAULT_TOPK = int(os.environ.get("TOPK", "8"))
_SCORE_THRESHOLD = int(os.environ.get("SCORE_THRESHOLD", "0")) / 100.0
_RERANK = os.environ.get("RERANK_ENABLED", "false").lower() == "true"
_GEN_ENABLED = os.environ.get("GEN_ENABLED", "false").lower() == "true"

# OpenAI-compatible chat base URLs per provider.
_GEN_BASE_URLS = {
    "openai": "https://api.openai.com/v1",
    "openrouter": "https://openrouter.ai/api/v1",
    "groq": "https://api.groq.com/openai/v1",
    "gemini": "https://generativelanguage.googleapis.com/v1beta/openai/",
}
_DEFAULT_SYSTEM_PROMPT = (
    "You are a helpful assistant. Answer the question using ONLY the provided "
    "context. If the context is insufficient, say so. Cite sources by their path."
)


def _spec_from_env() -> dict:
    return {
        "vectorStore": {
            "type": os.environ["VECTORSTORE_TYPE"],
            "endpoint": os.environ["VECTORSTORE_ENDPOINT"],
            "collection": os.environ.get("VECTORSTORE_COLLECTION", ""),
            "distance": os.environ.get("DISTANCE", "cosine"),
        }
    }


# Lazy singletons so the import (and probes) succeed before the store is ready.
_embedder = None
_store = None
_reranker = None
_gen_client = None


def _embedding_spec() -> dict:
    return {
        "model": os.environ["EMBEDDING_MODEL"],
        "provider": os.environ.get("EMBEDDING_PROVIDER", "local") or "local",
        "baseURL": os.environ.get("EMBEDDING_BASE_URL", ""),
        "dimension": int(os.environ.get("EMBEDDING_DIMENSION", "0") or 0),
    }


def _ensure() -> None:
    global _embedder, _store, _reranker, _gen_client
    if _embedder is None:
        _embedder = from_spec(_embedding_spec())
    if _store is None:
        os.environ.setdefault("KB_NAME", os.environ.get("VECTORSTORE_COLLECTION", "kb"))
        _store = make_store(_spec_from_env())
    if _RERANK and _reranker is None:
        from fastembed.rerank.cross_encoder import TextCrossEncoder

        model = os.environ.get("RERANK_MODEL") or "Xenova/ms-marco-MiniLM-L-6-v2"
        _reranker = TextCrossEncoder(model_name=model)
    if _GEN_ENABLED and _gen_client is None:
        from openai import OpenAI

        provider = os.environ.get("GEN_PROVIDER", "openai")
        base_url = os.environ.get("GEN_BASE_URL") or _GEN_BASE_URLS.get(provider)
        if not base_url:
            raise ValueError(f"generation provider {provider!r} requires GEN_BASE_URL")
        _gen_client = OpenAI(base_url=base_url, api_key=os.environ.get("GEN_API_KEY") or "no-key")


def _generate(
    question: str,
    chunks: list[Chunk],
    history: list[Message] | None = None,
    temperature: float | None = None,
    system_prompt: str | None = None,
    max_tokens: int | None = None,
) -> str:
    context = "\n\n".join(f"[{c.docPath}]\n{c.text}" for c in chunks)
    system = system_prompt or os.environ.get("GEN_SYSTEM_PROMPT") or _DEFAULT_SYSTEM_PROMPT
    messages = [{"role": "system", "content": system}]
    if history:
        for msg in history:
            messages.append({"role": msg.role, "content": msg.content})
    messages.append({"role": "user", "content": f"Context:\n{context}\n\nQuestion: {question}"})
    
    kwargs = {
        "model": os.environ["GEN_MODEL"],
        "max_tokens": max_tokens or int(os.environ.get("GEN_MAX_TOKENS", "512")),
        "messages": messages,
    }
    if temperature is not None:
        kwargs["temperature"] = temperature

    resp = _gen_client.chat.completions.create(**kwargs)
    return resp.choices[0].message.content or ""


def rrf(vector_hits: list[dict], text_hits: list[dict], k: int = 60) -> list[dict]:
    scores = {}
    payloads = {}
    
    def make_key(h):
        p = h["payload"]
        return (p.get("source", ""), p.get("doc_path", ""), p.get("text", ""))
    
    for rank, h in enumerate(vector_hits):
        key = make_key(h)
        scores[key] = scores.get(key, 0.0) + 1.0 / (k + rank + 1)
        payloads[key] = h["payload"]
        
    for rank, h in enumerate(text_hits):
        key = make_key(h)
        scores[key] = scores.get(key, 0.0) + 1.0 / (k + rank + 1)
        payloads[key] = h["payload"]
        
    fused = []
    for key, score in scores.items():
        fused.append({
            "score": score,
            "payload": payloads[key]
        })
    fused.sort(key=lambda x: x["score"], reverse=True)
    return fused


class Message(BaseModel):
    role: str
    content: str


class QueryRequest(BaseModel):
    query: str
    topK: int | None = None
    source: str | None = None
    history: list[Message] | None = None
    docPath: str | None = None
    docPathPrefix: str | None = None
    hybrid: bool | None = False
    temperature: float | None = None
    systemPrompt: str | None = None
    maxTokens: int | None = None


class Chunk(BaseModel):
    text: str
    source: str
    docPath: str
    score: float


class QueryResponse(BaseModel):
    query: str
    results: list[Chunk]
    answer: str | None = None


@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok"}


@app.post("/query", response_model=QueryResponse)
def query(req: QueryRequest) -> QueryResponse:
    _ensure()
    topk = req.topK or _DEFAULT_TOPK
    fetch_k = topk * 4 if _RERANK else (max(topk * 3, 20) if req.hybrid else topk)
    
    if req.hybrid:
        qv = _embedder.embed_query(req.query)
        vector_hits = _store.search(
            qv, fetch_k, source=req.source, doc_path=req.docPath, doc_path_prefix=req.docPathPrefix
        )
        text_hits = _store.search_text(
            req.query, fetch_k, source=req.source, doc_path=req.docPath, doc_path_prefix=req.docPathPrefix
        )
        hits = rrf(vector_hits, text_hits)
    else:
        qv = _embedder.embed_query(req.query)
        hits = _store.search(
            qv, fetch_k, source=req.source, doc_path=req.docPath, doc_path_prefix=req.docPathPrefix
        )

    if _RERANK and hits:
        scores = list(_reranker.rerank(req.query, [h["payload"].get("text", "") for h in hits]))
        for h, s in zip(hits, scores):
            h["score"] = float(s)
        hits.sort(key=lambda h: h["score"], reverse=True)

    results = []
    for h in hits[:topk]:
        is_rrf_score = req.hybrid and not _RERANK
        if not is_rrf_score and h["score"] < _SCORE_THRESHOLD:
            continue
        p = h["payload"]
        results.append(Chunk(
            text=p.get("text", ""),
            source=p.get("source", ""),
            docPath=p.get("doc_path", ""),
            score=round(float(h["score"]), 4),
        ))

    answer = None
    if _GEN_ENABLED and results:
        try:
            answer = _generate(
                req.query,
                results,
                req.history,
                temperature=req.temperature,
                system_prompt=req.systemPrompt,
                max_tokens=req.maxTokens,
            )
        except Exception as e:
            # Generation is best-effort: never fail retrieval because the LLM
            # call errored (quota, timeout, bad model). Surface the reason.
            answer = f"[generation unavailable: {type(e).__name__}: {e}]"
    return QueryResponse(query=req.query, results=results, answer=answer)
